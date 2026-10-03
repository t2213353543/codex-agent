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
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func nativeWSTestPacket(writer io.Writer, kind byte, raw []byte) error {
	var prefix [5]byte
	prefix[0] = kind
	binary.BigEndian.PutUint32(prefix[1:], uint32(len(raw)))
	if err := nativeWSWriteAll(writer, prefix[:]); err != nil {
		return err
	}
	return nativeWSWriteAll(writer, raw)
}

func nativeWSTestMessage(sequence uint64, typ websocket.MessageType, payload string) []byte {
	raw := make([]byte, 9+len(payload))
	binary.BigEndian.PutUint64(raw, sequence)
	raw[8] = byte(typ)
	copy(raw[9:], payload)
	return raw
}

func nativeWSTestPair(t *testing.T) (*gatewayNativeWS, net.Conn, *atomic.Int32) {
	t.Helper()
	client, helper := net.Pipe()
	released := &atomic.Int32{}
	ws := newGatewayNativeWS(t.Context(), client, func() { released.Add(1) })
	t.Cleanup(func() { _ = helper.Close(); _ = ws.CloseNow() })
	return ws, helper, released
}

func TestGatewayNativeWSIPCUnsolicitedMessageBeforeWriteAck(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	serverDone := make(chan error, 1)
	go func() {
		if err := nativeWSTestPacket(helper, nativeWSMessage, nativeWSTestMessage(1, websocket.MessageText, `{"type":"response.created"}`)); err != nil {
			serverDone <- err
			return
		}
		var prefix [5]byte
		if _, err := io.ReadFull(helper, prefix[:]); err != nil {
			serverDone <- err
			return
		}
		if prefix[0] != nativeWSSend {
			serverDone <- errNativeWSProtocol
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(prefix[1:]))
		if _, err := io.ReadFull(helper, body); err != nil {
			serverDone <- err
			return
		}
		if !bytes.Equal(body, nativeWSTestMessage(1, websocket.MessageText, `{"type":"response.create"}`)) {
			serverDone <- errNativeWSProtocol
			return
		}
		serverDone <- nativeWSTestPacket(helper, nativeWSAck, body[:8])
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	typ, raw, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(raw) != `{"type":"response.created"}` {
		t.Fatal("unsolicited message blocked ACK or changed bytes")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	_ = ws.CloseNow()
	_ = ws.CloseNow()
	if released.Load() != 1 {
		t.Fatal("socket release was not exactly once")
	}
}

func TestGatewayNativeWSIPCQueuedMessageSurvivesPeerClose(t *testing.T) {
	ws, helper, _ := nativeWSTestPair(t)
	go func() {
		_ = nativeWSTestPacket(helper, nativeWSMessage, nativeWSTestMessage(1, websocket.MessageText, `{"type":"response.completed"}`))
		_ = nativeWSTestPacket(helper, nativeWSClose, []byte(`{"code":1000,"reason":"fixture"}`))
	}()
	select {
	case <-ws.pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("close was not observed")
	}
	typ, raw, err := ws.Read(t.Context())
	if err != nil || typ != websocket.MessageText || string(raw) != `{"type":"response.completed"}` {
		t.Fatal("close discarded an earlier complete message")
	}
	_, _, err = ws.Read(t.Context())
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatal("peer close code lost")
	}
}

func TestGatewayNativeWSIPCReadCancellationAndNoReplay(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	readDone := make(chan error, 1)
	go func() { _, _, err := ws.Read(ctx); readDone <- err }()
	cancel()
	if err := <-readDone; !errors.Is(err, context.Canceled) {
		t.Fatal("read cancellation lost")
	}
	var one [1]byte
	_ = helper.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := helper.Read(one[:]); err == nil {
		t.Fatal("cancelled IPC remained open")
	}
	_ = ws.CloseNow()
	if released.Load() != 1 {
		t.Fatal("cancel did not release account transport slot")
	}
}

func TestGatewayNativeWSIPCAmbiguousWriteIsNotSuccess(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	go func() {
		var prefix [5]byte
		_, _ = io.ReadFull(helper, prefix[:])
		_, _ = io.CopyN(io.Discard, helper, int64(binary.BigEndian.Uint32(prefix[1:])))
		_ = helper.Close()
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{}`)); err == nil {
		t.Fatal("missing ACK became write success")
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{}`)); err == nil {
		t.Fatal("dead accepted stream replayed a send")
	}
	_ = ws.CloseNow()
	if released.Load() != 1 {
		t.Fatal("ambiguous write leaked or double released")
	}
}

func TestGatewayNativeWSIPCWrongAckAndInvalidFrames(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    byte
		payload []byte
	}{
		{"unsolicited_ack", nativeWSAck, make([]byte, 8)},
		{"bad_sequence", nativeWSMessage, nativeWSTestMessage(2, websocket.MessageText, `{}`)},
		{"invalid_utf8", nativeWSMessage, nativeWSTestMessage(1, websocket.MessageText, "\xff")},
		{"reserved_close", nativeWSClose, []byte(`{"code":1006,"reason":""}`)},
		{"unscoped_end", nativeWSEnd, nil},
		{"repeated_handshake", nativeWSHandshake, []byte(`{}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ws, helper, released := nativeWSTestPair(t)
			go func() { _ = nativeWSTestPacket(helper, test.kind, test.payload) }()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, _, err := ws.Read(ctx); err == nil {
				t.Fatal("invalid IPC accepted")
			}
			_ = ws.CloseNow()
			if released.Load() != 1 {
				t.Fatal("invalid frame leaked slot")
			}
		})
	}
}

func TestGatewayNativeWSIPCReadLimitBeforeAllocation(t *testing.T) {
	ws, helper, _ := nativeWSTestPair(t)
	ws.SetReadLimit(16)
	go func() {
		var prefix [5]byte
		prefix[0] = nativeWSMessage
		binary.BigEndian.PutUint32(prefix[1:], 0xffffffff)
		_ = nativeWSWriteAll(helper, prefix[:])
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); !errors.Is(err, errNativeWSProtocol) {
		t.Fatal("oversized length was not rejected at prefix")
	}
}

func TestGatewayNativeWSIPCConcurrentWritesSerializeSequences(t *testing.T) {
	ws, helper, _ := nativeWSTestPair(t)
	serverDone := make(chan error, 1)
	go func() {
		for sequence := uint64(1); sequence <= 8; sequence++ {
			var prefix [5]byte
			if _, err := io.ReadFull(helper, prefix[:]); err != nil {
				serverDone <- err
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(prefix[1:]))
			if _, err := io.ReadFull(helper, body); err != nil {
				serverDone <- err
				return
			}
			if prefix[0] != nativeWSSend || len(body) != 11 || binary.BigEndian.Uint64(body) != sequence {
				serverDone <- errNativeWSProtocol
				return
			}
			if err := nativeWSTestPacket(helper, nativeWSAck, body[:8]); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := ws.Write(ctx, websocket.MessageText, []byte(`{}`)); err != nil {
				t.Error("concurrent write failed")
			}
		}()
	}
	group.Wait()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestGatewayNativeWSIPCHandshakeStatusAndOpaqueTail(t *testing.T) {
	for _, status := range []int{101, 401, 429} {
		body := []byte{0xff, 0, 'x'}
		if status == 101 {
			body = nil
		}
		raw, _ := json.Marshal(map[string]any{"status": status, "headers": [][2]string{{"Retry-After", base64.StdEncoding.EncodeToString([]byte("3"))}, {"X-Fixture", base64.StdEncoding.EncodeToString([]byte("a"))}, {"X-Fixture", base64.StdEncoding.EncodeToString([]byte("b"))}, {"Content-Length", base64.StdEncoding.EncodeToString([]byte("9999"))}}, "body_scope": "handshake_tail", "body_complete": nil, "body_base64": base64.StdEncoding.EncodeToString(body), "tail_truncated": false})
		res, err := decodeNativeWSHandshake(raw)
		if err != nil || res.StatusCode != status || res.ContentLength != -1 || res.Header.Get("Retry-After") != "3" || len(res.Header.Values("X-Fixture")) != 2 {
			t.Fatal("handshake status/headers or unknown body length changed")
		}
		got, _ := io.ReadAll(res.Body)
		if !bytes.Equal(got, body) {
			t.Fatal("handshake tail lost non-UTF8 bytes")
		}
	}
	if err := nativeWSRemoteError([]byte(`{"phase":"handshake","code":"ws_transport","accepted":false}`), false); err == nil || strings.Contains(err.Error(), "401") {
		t.Fatal("proxy failure invented target HTTP rejection")
	}
	if _, err := decodeNativeWSHandshake([]byte(`{"status":401,"headers":[],"body_scope":"handshake_tail","body_complete":true,"body_base64":"","tail_truncated":false}`)); err == nil {
		t.Fatal("invented complete body accepted")
	}
}

func TestGatewayNativeWSIPCProxySnapshotGuardBeforePool(t *testing.T) {
	p := newTestProxy(t, 1)
	p.NativeHTTP = &gatewayNativePool{}
	entry := poolProxyEntry(t, p, "http://127.0.0.1:1")
	if _, err := p.Proxies.UpdateVersion(entry.ID, 0, func(v *ProxyEntry) error { v.Password = "synthetic-rotation"; return nil }); err != nil {
		t.Fatal(err)
	}
	_, response, err := p.dialNativeGatewayWebSocket(t.Context(), make(http.Header), &entry)
	if !errors.Is(err, errGatewayProxyUnavailable) || response != nil {
		t.Fatal("stale snapshot reached native process startup")
	}
	p.NativeHTTP = nil
}

func TestGatewayNativeWSIPCBackpressureDoesNotBlockCancellation(t *testing.T) {
	ws, helper, released := nativeWSTestPair(t)
	full := make(chan struct{})
	helperDone := make(chan struct{})
	go func() {
		defer close(helperDone)
		for sequence := uint64(1); sequence <= 17; sequence++ {
			if err := nativeWSTestPacket(helper, nativeWSMessage, nativeWSTestMessage(sequence, websocket.MessageText, `{}`)); err != nil {
				return
			}
		}
		close(full)
		var prefix [5]byte
		if _, err := io.ReadFull(helper, prefix[:]); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(prefix[1:]))
		if _, err := io.ReadFull(helper, body); err != nil {
			return
		}
		_ = nativeWSTestPacket(helper, nativeWSAck, body[:8])
	}()
	select {
	case <-full:
	case <-time.After(time.Second):
		t.Fatal("fixture did not fill bounded queue")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("backpressured late ACK did not preserve deadline")
	}
	_ = ws.CloseNow()
	select {
	case <-helperDone:
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock IPC peer")
	}
	if released.Load() != 1 || len(ws.messages) != 0 || ws.queued.Load() != 0 {
		t.Fatal("bounded queue or release invariant changed")
	}
}

func TestGatewayNativeWSIPCWriteCancellationUnblocksIPCSend(t *testing.T) {
	ws, _, released := nativeWSTestPair(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("blocked IPC send ignored context")
	}
	_ = ws.CloseNow()
	if released.Load() != 1 {
		t.Fatal("blocked write cancellation leaked slot")
	}
}

func TestGatewayNativeWSIPCSessionContextLivesBeyondHandshake(t *testing.T) {
	client, helper := net.Pipe()
	defer helper.Close()
	session, cancel := context.WithCancel(t.Context())
	defer cancel()
	var released atomic.Int32
	ws := newGatewayNativeWS(session, client, func() { released.Add(1) })
	defer ws.CloseNow()
	go func() {
		_ = nativeWSTestPacket(helper, nativeWSMessage, nativeWSTestMessage(1, websocket.MessageText, `{}`))
	}()
	if _, _, err := ws.Read(session); err != nil {
		t.Fatal("live session ended at handshake boundary")
	}
	cancel()
	select {
	case <-ws.pumpDone:
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not stop read pump")
	}
	if released.Load() != 1 {
		t.Fatal("session cancellation did not release helper slot")
	}
}

func TestGatewayNativeWSIPCInterfacePreservesNilGoHandshake(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	request := httptest.NewRequest(http.MethodGet, "/responses", nil)
	conn, response, err := p.dialGatewayWebSocket(t.Context(), request, Account{AccessToken: "synthetic"}, nil, defaultSettings())
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("Go handshake rejection not preserved")
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if conn != nil {
		t.Fatal("failed Go handshake returned a typed-nil upstream interface")
	}
}
