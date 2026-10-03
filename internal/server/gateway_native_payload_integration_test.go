package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

func TestGatewayNativePayloadActualHTTP33MiBPlainAndZstd(t *testing.T) {
	p := newTestProxy(t, 1)
	realGatewayNativePool(t, p)
	payload := append([]byte(`{"text":"`), bytes.Repeat([]byte{'x'}, 33<<20)...)
	payload = append(payload, []byte(`"}`)...)
	want := sha256.Sum256(payload)
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "zstd"}[compressed], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var reader io.Reader = r.Body
				if compressed {
					if r.Header.Get("Content-Encoding") != "zstd" {
						t.Error("compression header absent")
						w.WriteHeader(400)
						return
					}
					decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					defer decoder.Close()
					reader = decoder
				} else if r.Header.Get("Content-Encoding") != "" {
					t.Error("plain request was encoded")
					w.WriteHeader(400)
					return
				}
				hash := sha256.New()
				count, err := io.Copy(hash, reader)
				if err != nil || count != int64(len(payload)) || !bytes.Equal(hash.Sum(nil), want[:]) {
					t.Errorf("actual helper changed body: bytes=%d err=%v", count, err)
					w.WriteHeader(400)
					return
				}
				_, _ = io.WriteString(w, hex.EncodeToString(hash.Sum(nil)))
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(withGatewayUnboundedPayload(t.Context(), true), 30*time.Second)
			defer cancel()
			if compressed {
				ctx = withGatewayNativeRequestCompression(ctx)
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/responses", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			response, err := p.NativeHTTP.RoundTripper(nil, time.Second*10).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != hex.EncodeToString(want[:]) {
				t.Fatalf("native HTTP transfer failed: status=%d err=%v", response.StatusCode, err)
			}
		})
	}
}

func TestGatewayNativePayloadActualWebSocket33MiBTextAndBinary(t *testing.T) {
	for _, typ := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
		t.Run(map[websocket.MessageType]string{websocket.MessageText: "text", websocket.MessageBinary: "binary"}[typ], func(t *testing.T) {
			value := byte('x')
			if typ == websocket.MessageBinary {
				value = 255
			}
			payload := bytes.Repeat([]byte{value}, 33<<20)
			want := sha256.Sum256(payload)
			p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Sec-WebSocket-Extensions") != "" {
					t.Error("Realtime profile advertised a compression extension")
				}
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				conn.SetReadLimit(-1)
				gotType, got, err := conn.Read(r.Context())
				if err != nil || gotType != typ || sha256.Sum256(got) != want {
					t.Errorf("upstream received changed message: type=%d bytes=%d err=%v", gotType, len(got), err)
					return
				}
				if err = conn.Write(r.Context(), gotType, got); err != nil {
					t.Error(err)
					return
				}
				_, _, _ = conn.Read(r.Context())
			})
			ctx, cancel := context.WithTimeout(withGatewayUnboundedPayload(t.Context(), true), 30*time.Second)
			defer cancel()
			ctx = withGatewayNativeWebSocketCompressionDisabled(ctx)
			ws, response, err := p.dialNativeGatewayWebSocket(ctx, make(http.Header), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.CloseNow()
			if response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatal(response.StatusCode)
			}
			ws.SetReadLimit(-1)
			if err = ws.Write(ctx, typ, payload); err != nil {
				t.Fatal(err)
			}
			gotType, got, err := ws.Read(ctx)
			if err != nil || gotType != typ || sha256.Sum256(got) != want {
				t.Fatalf("downstream received changed message: type=%d bytes=%d err=%v", gotType, len(got), err)
			}
		})
	}
}
