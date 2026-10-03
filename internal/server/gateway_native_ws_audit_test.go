package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayNativeWSAuditHandshakeMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{"repeated_opaque_headers", func(h map[string]any) {
			h["headers"] = [][]string{{"X-Test", "YQ=="}, {"X-Test", "/w=="}, {"X-Tab", "CQ=="}}
		}, true},
		{"short_header_tuple", func(h map[string]any) { h["headers"] = [][]string{{"X-Test"}} }, false},
		{"long_header_tuple", func(h map[string]any) { h["headers"] = [][]string{{"X-Test", "YQ==", "ignored"}} }, false},
		{"control_header", func(h map[string]any) { h["headers"] = [][]string{{"X-Test", "AQ=="}} }, false},
		{"delete_header", func(h map[string]any) { h["headers"] = [][]string{{"X-Test", "fw=="}} }, false},
		{"bad_header_name", func(h map[string]any) { h["headers"] = [][]string{{"X:Test", "YQ=="}} }, false},
		{"bad_header_base64", func(h map[string]any) { h["headers"] = [][]string{{"X-Test", "%%%"}} }, false},
		{"none_with_body", func(h map[string]any) { h["body_scope"] = "none" }, false},
		{"none_truncated", func(h map[string]any) {
			h["body_scope"], h["body_base64"], h["tail_truncated"] = "none", "", true
		}, false},
		{"none_empty", func(h map[string]any) { h["body_scope"], h["body_base64"] = "none", "" }, true},
		{"accepted_body", func(h map[string]any) { h["status"] = 101 }, false},
		{"accepted_tail_truncated", func(h map[string]any) {
			h["status"], h["body_base64"], h["tail_truncated"] = 101, "", true
		}, false},
		{"rejected_opaque_tail", func(h map[string]any) { h["tail_truncated"] = true }, true},
		{"false_completeness", func(h map[string]any) { h["body_complete"] = false }, false},
		{"missing_completeness", func(h map[string]any) { delete(h, "body_complete") }, false},
		{"tail_limit", func(h map[string]any) { h["body_base64"] = base64.StdEncoding.EncodeToString(make([]byte, (16<<10)+1)) }, false},
		{"metadata_limit", func(h map[string]any) { h["extra"] = strings.Repeat("x", gatewayNativeHeadLimit) }, false},
		{"invalid_status", func(h map[string]any) { h["status"] = 600 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := map[string]any{"status": 429, "headers": [][]string{}, "body_scope": "handshake_tail", "body_complete": nil, "body_base64": "/wB4", "tail_truncated": false}
			tc.change(head)
			raw, err := json.Marshal(head)
			if err != nil {
				t.Fatal(err)
			}
			response, err := decodeNativeWSHandshake(raw)
			if !tc.valid {
				if !errors.Is(err, errNativeWSProtocol) || response != nil {
					t.Fatalf("invalid handshake accepted: response=%v err=%v", response, err)
				}
				return
			}
			if err != nil || response.StatusCode != 429 || response.ContentLength != -1 {
				t.Fatalf("valid rejection lost status or unknown completeness: response=%v err=%v", response, err)
			}
			body, _ := io.ReadAll(response.Body)
			want, _ := base64.StdEncoding.DecodeString(head["body_base64"].(string))
			if !bytes.Equal(body, want) {
				t.Fatal("opaque rejection tail changed")
			}
			if tc.name == "repeated_opaque_headers" && !bytes.Equal([]byte(response.Header.Values("X-Test")[1]), []byte{0xff}) {
				t.Fatal("opaque header bytes changed")
			}
		})
	}
}

func TestGatewayNativeWSAuditRemoteErrorPhaseAndTimeout(t *testing.T) {
	for _, code := range []string{"ws_handshake_timeout", "ws_write_timeout", "ws_ipc_write_timeout", "ws_ipc_read_timeout", "ws_close_timeout"} {
		t.Run(code, func(t *testing.T) {
			for _, accepted := range []bool{false, true} {
				phase := "handshake"
				if accepted {
					phase = "message"
				}
				raw, _ := json.Marshal(map[string]any{"phase": phase, "code": code, "accepted": accepted})
				err := nativeWSRemoteError(raw, accepted)
				var transport net.Error
				if !errors.As(err, &transport) || !transport.Timeout() {
					t.Fatalf("timeout lost net.Error semantics: %v", err)
				}
			}
		})
	}
	for _, raw := range []string{
		`{"phase":"handshake","code":"ws_transport","accepted":true}`,
		`{"phase":"message","code":"ws_transport","accepted":false}`,
		`{"phase":"message","code":"ws_transport"}`,
		`{"phase":"message","code":"ws_transport","accepted":null}`,
		`{"phase":"message","code":"unknown","accepted":true}`,
	} {
		if err := nativeWSRemoteError([]byte(raw), true); !errors.Is(err, errNativeWSProtocol) {
			t.Fatalf("contradictory or incomplete metadata accepted: %s -> %v", raw, err)
		}
	}
	if err := nativeWSRemoteError([]byte(`{"phase":"message","code":"ws_transport","accepted":false}`), false); !errors.Is(err, errNativeWSProtocol) {
		t.Fatalf("pre-handshake message phase accepted: %v", err)
	}
	if err := nativeWSRemoteError([]byte(`{"phase":"message","code":"ws_message_limit","accepted":true}`), true); !errors.Is(err, errNativeWSLimit) {
		t.Fatalf("message limit classification changed: %v", err)
	}
}

func TestGatewayNativeWSAuditFrameBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
		size uint32
		want error
	}{
		{"unknown_kind", 81, 0, errNativeWSProtocol},
		{"short_ack", nativeWSAck, 7, errNativeWSProtocol},
		{"long_ack", nativeWSAck, 9, errNativeWSProtocol},
		{"nonempty_end", nativeWSEnd, 1, errNativeWSProtocol},
		{"short_message", nativeWSMessage, 8, errNativeWSProtocol},
		{"hard_message_limit", nativeWSMessage, gatewayNativeBodyLimit + 10, errNativeWSProtocol},
		{"caller_message_limit", nativeWSMessage, 26, errNativeWSLimit},
		{"metadata_limit", nativeWSClose, gatewayNativeHeadLimit + 1, errNativeWSProtocol},
		{"exact_message_limit", nativeWSMessage, 25, nil},
		{"empty_message", nativeWSMessage, 9, nil},
		{"exact_ack", nativeWSAck, 8, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prefix [5]byte
			prefix[0] = tc.kind
			binary.BigEndian.PutUint32(prefix[1:], tc.size)
			_, _, err := readNativeWSFrameHeader(bytes.NewReader(prefix[:]), 16)
			if !errors.Is(err, tc.want) {
				t.Fatalf("prefix classification=%v want=%v", err, tc.want)
			}
		})
	}
}

type nativeWSAuditErrorReader struct{ err error }

func (r nativeWSAuditErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestGatewayNativeWSAuditReadErrorIdentity(t *testing.T) {
	var prefix [5]byte
	prefix[0] = nativeWSAck
	binary.BigEndian.PutUint32(prefix[1:], 8)
	for _, stage := range []string{"header", "payload"} {
		t.Run(stage, func(t *testing.T) {
			for _, cause := range []error{os.ErrDeadlineExceeded, net.ErrClosed, io.EOF} {
				reader := io.Reader(nativeWSAuditErrorReader{cause})
				if stage == "payload" {
					reader = io.MultiReader(bytes.NewReader(prefix[:]), bytes.NewReader([]byte{1, 2}), reader)
				}
				_, _, err := readNativeWSFrame(reader, gatewayNativeBodyLimit)
				want := cause
				if cause == io.EOF {
					want = io.ErrUnexpectedEOF
				}
				if !errors.Is(err, want) {
					t.Fatalf("read cause lost: got=%v want=%v", err, want)
				}
			}
		})
	}
	ws, _, _ := nativeWSTestPair(t)
	if err := ws.conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	var transport net.Error
	if !errors.As(err, &transport) || !transport.Timeout() {
		t.Fatalf("pump erased IPC timeout: %v", err)
	}
}

func TestGatewayNativeWSAuditCloseUnicodeAndPinnedCodes(t *testing.T) {
	for _, tc := range []struct {
		name, raw, reason string
		code              websocket.StatusCode
	}{
		{"invalid_utf8", "{\"code\":1000,\"reason\":\"\xff\"}", "", -1},
		{"lone_high_surrogate", `{"code":1000,"reason":"\ud800"}`, "", -1},
		{"lone_low_surrogate", `{"code":1000,"reason":"\udc00"}`, "", -1},
		{"unpaired_high_surrogate", `{"code":1000,"reason":"\ud800\u0061"}`, "", -1},
		{"surrogate_pair", `{"code":1000,"reason":"\ud83d\ude00"}`, "😀", 1000},
		{"escaped_literal", `{"code":1000,"reason":"\\ud800"}`, `\ud800`, 1000},
		{"replacement_character", `{"code":1000,"reason":"�"}`, "�", 1000},
		{"missing_reason", `{"code":1000}`, "", -1},
		{"null_reason", `{"code":1000,"reason":null}`, "", -1},
		{"no_status", `{"code":null,"reason":""}`, "", websocket.StatusNoStatusRcvd},
		{"no_status_with_reason", `{"code":null,"reason":"invalid"}`, "", -1},
		{"pinned_fork_bad_1014", `{"code":1014,"reason":""}`, "", -1},
		{"reserved_1015", `{"code":1015,"reason":""}`, "", -1},
		{"application_code", `{"code":4999,"reason":"application"}`, "application", 4999},
		{"utf8_byte_limit", `{"code":1000,"reason":"` + strings.Repeat("界", 42) + `"}`, "", -1},
		{"utf8_exact_byte_limit", `{"code":1000,"reason":"` + strings.Repeat("界", 41) + `"}`, strings.Repeat("界", 41), 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, helper, released := nativeWSTestPair(t)
			go func() { _ = nativeWSTestPacket(helper, nativeWSClose, []byte(tc.raw)) }()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, _, err := ws.Read(ctx)
			if tc.code == -1 {
				if !errors.Is(err, errNativeWSProtocol) {
					t.Fatalf("invalid close metadata accepted: %v", err)
				}
			} else {
				var closeErr websocket.CloseError
				if !errors.As(err, &closeErr) || closeErr.Code != tc.code || closeErr.Reason != tc.reason {
					t.Fatalf("valid close changed: %#v want code=%d reason=%q", err, tc.code, tc.reason)
				}
			}
			_ = ws.CloseNow()
			if released.Load() != 1 || ws.queued.Load() != 0 {
				t.Fatal("close metadata path leaked capacity")
			}
		})
	}
	for _, code := range []int{-1, 999, 1004, 1005, 1006, 1014, 1015, 1999, 2000, 2999, 5000} {
		if nativeWSAllowedCloseCode(code) {
			t.Fatalf("pinned fork rejects close code %d", code)
		}
	}
}

func TestGatewayNativeWSAuditCancelledReadKeepsQueuedMessage(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	go func() {
		_ = nativeWSTestPacket(helper, nativeWSMessage, nativeWSTestMessage(1, websocket.MessageText, "queued"))
		_ = nativeWSTestPacket(helper, nativeWSClose, []byte(`{"code":1000,"reason":""}`))
	}()
	select {
	case <-ws.pumpDone:
	case <-time.After(time.Second):
		t.Fatal("fixture did not finish")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 16 {
		typ, data, err := ws.Read(ctx)
		if !errors.Is(err, context.Canceled) || typ != 0 || data != nil {
			t.Fatalf("already-cancelled Read consumed queued output: type=%v bytes=%q err=%v", typ, data, err)
		}
	}
	if ws.queued.Load() != 6 || len(ws.messages) != 1 {
		t.Fatal("cancelled Read consumed queue credit")
	}
	_ = ws.CloseNow()
	if ws.queued.Load() != 0 || released.Load() != 1 {
		t.Fatal("cancelled reader cleanup leaked capacity")
	}
}

func TestGatewayNativeWSAuditReadLimitReducedDuringPayload(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	started, resume := make(chan struct{}), make(chan struct{})
	go func() {
		var prefix [5]byte
		prefix[0] = nativeWSMessage
		binary.BigEndian.PutUint32(prefix[1:], 17)
		if nativeWSWriteAll(helper, prefix[:]) != nil || nativeWSWriteAll(helper, nativeWSTestMessage(1, websocket.MessageBinary, "")[:9]) != nil {
			return
		}
		close(started)
		<-resume
		_ = nativeWSWriteAll(helper, []byte("12345678"))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("fixture did not enter payload read")
	}
	ws.SetReadLimit(4)
	close(resume)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); !errors.Is(err, errNativeWSLimit) {
		t.Fatalf("mid-frame limit reduction reported wrong error: %v", err)
	}
	_ = ws.CloseNow()
	if ws.queued.Load() != 0 || released.Load() != 1 {
		t.Fatal("mid-frame limit path leaked capacity")
	}
}

func TestGatewayNativeWSAuditOutboundValidationBeforeIPC(t *testing.T) {
	ws, _, released := nativeWSTestPair(t)
	for _, tc := range []struct {
		typ  websocket.MessageType
		data []byte
		want error
	}{
		{websocket.MessageText, []byte{0xff}, errNativeWSProtocol},
		{websocket.MessageType(3), []byte("x"), errNativeWSProtocol},
		{websocket.MessageBinary, make([]byte, gatewayNativeBodyLimit+1), errNativeWSLimit},
	} {
		if err := ws.Write(t.Context(), tc.typ, tc.data); !errors.Is(err, tc.want) {
			t.Fatalf("outbound validation returned %v want %v", err, tc.want)
		}
	}
	if ws.sequence != 0 || ws.waitingAck.Load() != 0 || released.Load() != 0 {
		t.Fatal("invalid caller message mutated IPC state")
	}
}

func TestGatewayNativeWSAuditHelperTimeoutInterruptsWrite(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	go func() {
		var prefix [14]byte
		if _, err := io.ReadFull(helper, prefix[:]); err != nil {
			return
		}
		// Leave payload blocked while the helper publishes its exact cause.
		_ = nativeWSTestPacket(helper, nativeWSError, []byte(`{"phase":"message","code":"ws_ipc_read_timeout","accepted":true}`))
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for range 2 {
		err := ws.Write(ctx, websocket.MessageText, []byte("blocked payload"))
		var transport net.Error
		if !errors.As(err, &transport) || !transport.Timeout() || err != gatewayNativeError("ws_ipc_read_timeout") {
			t.Fatalf("helper timeout was replaced with local socket error: %v", err)
		}
	}
	_ = ws.CloseNow()
	if released.Load() != 1 || ws.sequence != 1 {
		t.Fatal("failed socket was written again or capacity leaked")
	}
}

func TestGatewayNativeWSAuditRejectedConfigurationBeforePool(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		header     http.Header
	}{
		{"query", "https://example.invalid/backend?key=value", nil},
		{"empty_query", "https://example.invalid/backend?", nil},
		{"empty_hostname", "https://:443/backend", nil},
		{"bad_header", "https://example.invalid/backend", http.Header{"X-Test": {"\x01"}}},
		{"too_many_values", "https://example.invalid/backend", http.Header{"X-Test": make([]string, 129)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{BaseURL: tc.base, NativeHTTP: &gatewayNativePool{}}
			conn, response, err := p.dialNativeGatewayWebSocket(t.Context(), tc.header, nil)
			if err == nil || conn != nil || response != nil {
				t.Fatal("malformed request reached process startup")
			}
		})
	}
}

func TestGatewayNativeWSAuditConcurrentWaitersCancelTogether(t *testing.T) {
	ws, _, released := nativeWSTestPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			_, _, err := ws.Read(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("reader lost shared cancellation: %v", err)
			}
		})
		workers.Go(func() {
			err := ws.Write(ctx, websocket.MessageText, []byte("x"))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("writer lost shared cancellation: %v", err)
			}
		})
	}
	cancel()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read/write lock waiters did not wake on cancellation")
	}
	_ = ws.CloseNow()
	if released.Load() != 1 || ws.waitingAck.Load() > 1 {
		t.Fatal("concurrent cancellation duplicated release or sequence")
	}
}

func BenchmarkGatewayNativeWSAuditMessageBudget(b *testing.B) {
	ws := &gatewayNativeWS{done: make(chan struct{}), budgetWake: make(chan struct{}, 1), messages: make(chan nativeWSPacket, 16)}
	ws.readLimit.Store(gatewayNativeBodyLimit)
	message := nativeWSPacket{websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"x"}`)}
	b.ReportAllocs()
	b.SetBytes(int64(len(message.payload)))
	for b.Loop() {
		if err := ws.reserveMessageBudget(int64(len(message.payload))); err != nil {
			b.Fatal(err)
		}
		if err := ws.enqueueMessage(message); err != nil {
			b.Fatal(err)
		}
		ws.releaseMessageBudget(int64(len((<-ws.messages).payload)))
	}
}
