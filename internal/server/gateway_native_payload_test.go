package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func nativePayloadStart(sequence uint64, typ websocket.MessageType, total uint64) []byte {
	raw := make([]byte, 17)
	binary.BigEndian.PutUint64(raw, sequence)
	raw[8] = byte(typ)
	binary.BigEndian.PutUint64(raw[9:], total)
	return raw
}

func nativePayloadPair(t *testing.T) (*gatewayNativeWS, net.Conn) {
	t.Helper()
	client, helper := net.Pipe()
	ws := newGatewayNativeWSWithPolicy(t.Context(), client, func() {}, true)
	t.Cleanup(func() { _ = helper.Close(); _ = ws.CloseNow() })
	return ws, helper
}

func TestGatewayNativePayloadWS33MiBRoundTrip(t *testing.T) {
	for _, typ := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
		t.Run(map[websocket.MessageType]string{websocket.MessageText: "text", websocket.MessageBinary: "binary"}[typ], func(t *testing.T) {
			ws, helper := nativePayloadPair(t)
			ws.SetReadLimit(-1)
			if ws.readLimit.Load() != -1 {
				t.Fatal("unbounded read policy was clamped")
			}
			value := byte('x')
			if typ == websocket.MessageBinary {
				value = 255
			}
			payload := bytes.Repeat([]byte{value}, 33<<20)
			done := make(chan error, 1)
			go func() {
				read := func() (byte, []byte, error) {
					var prefix [5]byte
					if _, err := io.ReadFull(helper, prefix[:]); err != nil {
						return 0, nil, err
					}
					size := binary.BigEndian.Uint32(prefix[1:])
					if size > nativeWSChunkSize {
						return 0, nil, errNativeWSProtocol
					}
					raw := make([]byte, size)
					_, err := io.ReadFull(helper, raw)
					return prefix[0], raw, err
				}
				kind, raw, err := read()
				if err != nil || kind != nativeWSSendStart || !bytes.Equal(raw, nativePayloadStart(1, typ, uint64(len(payload)))) {
					done <- errNativeWSProtocol
					return
				}
				var received []byte
				for len(received) < len(payload) {
					kind, raw, err = read()
					if err != nil || kind != nativeWSSendChunk || len(raw) == 0 {
						done <- errNativeWSProtocol
						return
					}
					received = append(received, raw...)
				}
				kind, raw, err = read()
				if err != nil || kind != nativeWSSendFinish || !bytes.Equal(raw, []byte{0, 0, 0, 0, 0, 0, 0, 1}) || !bytes.Equal(received, payload) {
					done <- errNativeWSProtocol
					return
				}
				if err = nativeWSTestPacket(helper, nativeWSAck, raw); err != nil {
					done <- err
					return
				}
				if err = nativeWSTestPacket(helper, nativeWSStart, nativePayloadStart(1, typ, uint64(len(payload)))); err != nil {
					done <- err
					return
				}
				for offset := 0; offset < len(payload); offset += nativeWSChunkSize {
					if err = nativeWSTestPacket(helper, nativeWSChunk, payload[offset:min(offset+nativeWSChunkSize, len(payload))]); err != nil {
						done <- err
						return
					}
				}
				done <- nativeWSTestPacket(helper, nativeWSFinish, []byte{0, 0, 0, 0, 0, 0, 0, 1})
			}()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := ws.Write(ctx, typ, payload); err != nil {
				t.Fatal(err)
			}
			gotType, got, err := ws.Read(ctx)
			if err != nil || gotType != typ || !bytes.Equal(got, payload) {
				t.Fatalf("message changed: type=%v bytes=%d err=%v", gotType, len(got), err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if ws.queued.Load() != 0 {
				t.Fatal("message accounting leaked")
			}
		})
	}
}

func TestGatewayNativePayloadWSMalformedChunksAndExplicitLimit(t *testing.T) {
	cases := []struct {
		name   string
		total  uint64
		frames []struct {
			kind byte
			raw  []byte
		}
		limit int64
	}{
		{name: "short", total: 2, frames: []struct {
			kind byte
			raw  []byte
		}{{nativeWSFinish, []byte{0, 0, 0, 0, 0, 0, 0, 1}}}, limit: -1},
		{name: "overshoot", total: 1, frames: []struct {
			kind byte
			raw  []byte
		}{{nativeWSChunk, []byte{1, 2}}}, limit: -1},
		{name: "wrong_sequence", total: 0, frames: []struct {
			kind byte
			raw  []byte
		}{{nativeWSFinish, []byte{0, 0, 0, 0, 0, 0, 0, 2}}}, limit: -1},
		{name: "empty_chunk", total: 1, frames: []struct {
			kind byte
			raw  []byte
		}{{nativeWSChunk, nil}}, limit: -1},
		{name: "nested_start", total: 1, frames: []struct {
			kind byte
			raw  []byte
		}{{nativeWSStart, nativePayloadStart(2, websocket.MessageBinary, 0)}}, limit: -1},
		{name: "arithmetic_overflow", total: ^uint64(0), limit: -1},
		{name: "explicit_read_limit", total: 2, limit: 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ws, helper := nativePayloadPair(t)
			ws.SetReadLimit(test.limit)
			go func() {
				_ = nativeWSTestPacket(helper, nativeWSStart, nativePayloadStart(1, websocket.MessageBinary, test.total))
				for _, frame := range test.frames {
					_ = nativeWSTestPacket(helper, frame.kind, frame.raw)
				}
			}()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, _, err := ws.Read(ctx)
			want := errNativeWSProtocol
			if test.limit >= 0 {
				want = errNativeWSLimit
			}
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			if ws.queued.Load() != 0 {
				t.Fatal("partial payload accounting leaked")
			}
		})
	}
}

func TestGatewayNativePayloadWSCancellationDuringMoreThanUint32Message(t *testing.T) {
	ws, helper := nativePayloadPair(t)
	started := make(chan struct{})
	go func() {
		_ = nativeWSTestPacket(helper, nativeWSStart, nativePayloadStart(1, websocket.MessageBinary, uint64(1)<<32+1))
		_ = nativeWSTestPacket(helper, nativeWSChunk, []byte{42})
		close(started)
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := ws.Read(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = ws.CloseNow()
	if ws.queued.Load() != 0 {
		t.Fatal("cancelled payload accounting leaked")
	}
}

func TestGatewayNativePayloadHTTPLargeBodyAndOldHelperNegotiation(t *testing.T) {
	for _, supports := range []bool{false, true} {
		t.Run(map[bool]string{false: "old_helper", true: "unbounded_helper"}[supports], func(t *testing.T) {
			pool := nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
				original := options.command
				options.command = func(path string) *exec.Cmd {
					cmd := original(path)
					if supports {
						cmd.Env = append(cmd.Env, "GATEWAY_NATIVE_FIXTURE_CAPABILITIES=unbounded_payload_v1,websocket_message_v2")
					}
					return cmd
				}
			})
			ctx := withGatewayUnboundedPayload(t.Context(), true)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/inspect", bytes.NewReader(bytes.Repeat([]byte{'x'}, 33<<20)))
			if err != nil {
				t.Fatal(err)
			}
			response, err := pool.RoundTripper(nil, time.Second).RoundTrip(req)
			if !supports {
				if !errors.Is(err, errGatewayNativeUnboundedReady) {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var got struct {
				BodyLength int `json:"body_length"`
			}
			if err = json.NewDecoder(response.Body).Decode(&got); err != nil || got.BodyLength != 33<<20 {
				t.Fatalf("body truncated: %d %v", got.BodyLength, err)
			}
		})
	}
}

func TestGatewayNativePayloadWSOldHelperIsRejectedBeforeHandshake(t *testing.T) {
	p := newTestProxy(t, 1)
	p.NativeHTTP = nativeHTTPFixturePool(t, nil)
	defer func() { p.NativeHTTP = nil }()
	ctx := withGatewayUnboundedPayload(t.Context(), true)
	_, response, err := p.dialNativeGatewayWebSocket(ctx, make(http.Header), nil)
	if response != nil || !errors.Is(err, errGatewayNativeUnboundedReady) {
		t.Fatalf("old helper accepted v2: %v", err)
	}
}

func TestGatewayNativePayloadWSCompressionProfileRequiresNegotiation(t *testing.T) {
	p := newTestProxy(t, 1)
	p.NativeHTTP = nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
		original := options.command
		options.command = func(path string) *exec.Cmd {
			cmd := original(path)
			cmd.Env = append(cmd.Env, "GATEWAY_NATIVE_FIXTURE_CAPABILITIES=websocket_message_v1,websocket_message_v2,unbounded_payload_v1")
			return cmd
		}
	})
	defer func() { p.NativeHTTP = nil }()
	if gatewayNativeWebSocketCompressionDisabled(t.Context()) {
		t.Fatal("Responses default compression changed")
	}
	ctx := withGatewayNativeWebSocketCompressionDisabled(withGatewayUnboundedPayload(t.Context(), true))
	_, response, err := p.dialNativeGatewayWebSocket(ctx, make(http.Header), nil)
	if response != nil || !errors.Is(err, errNativeWSNoCompressionReady) {
		t.Fatalf("helper lacking compression profile accepted request: %v", err)
	}
}
