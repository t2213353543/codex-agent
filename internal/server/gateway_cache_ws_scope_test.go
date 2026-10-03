package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewaySub2CacheWSFreezesKindAcrossPartialContinuations(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, initialMetadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%t/session_kind=%t", native, initialMetadata), func(t *testing.T) {
				captured := make(chan []byte, 3)
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					for i := 1; i <= 3; i++ {
						_, raw, err := conn.Read(r.Context())
						if err != nil {
							return
						}
						captured <- raw
						_ = conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"scoped-%d","status":"completed","output":[]}}`, i)))
					}
				})
				enableGatewayWS(t, p, "")
				if native {
					realGatewayNativePool(t, p)
					p.NativeWebSocket = true
				}
				server := httptest.NewServer(p)
				defer server.Close()
				conn, _ := gatewayWSDial(t, server.URL, "session")
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				metadata := func(enabled bool) string {
					if enabled {
						return `,"client_metadata":{"session_id":"session"}`
					}
					return ""
				}
				var firstKey string
				for i := 1; i <= 3; i++ {
					previous := ""
					if i > 1 {
						previous = fmt.Sprintf(`,"previous_response_id":"scoped-%d"`, i-1)
					}
					meta := initialMetadata
					if i > 1 {
						meta = !initialMetadata
					}
					raw := fmt.Sprintf(`{"type":"response.create","model":"test","prompt_cache_key":"session","input":[]%s%s}`, metadata(meta), previous)
					if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
						t.Fatal(err)
					}
					if reply := gatewayWSRead(t, conn); !strings.Contains(string(reply), `"response.completed"`) {
						t.Fatalf("partial continuation rejected: %s", reply)
					}
					wire := <-captured
					key := gatewayPromptCacheKeyValue(wire)
					if i == 1 {
						firstKey = key
					}
					if key != firstKey || len(key) != 36 {
						t.Fatal("partial continuation changed original scoped cache key")
					}
					if i > 1 && !strings.Contains(string(wire), previous[1:]) {
						t.Fatal("continuation did not preserve previous_response_id")
					}
				}
			})
		}
	}
}

func TestGatewaySub2CacheWSAcceptsUnknownExplicitContinuation(t *testing.T) {
	for _, defaults := range []bool{false, true} {
		t.Run(fmt.Sprintf("default=%t", defaults), func(t *testing.T) {
			captured := make(chan []byte, 1)
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_, raw, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				captured <- raw
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"new","status":"completed","output":[]}}`))
			})
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			conn, _ := gatewayWSDial(t, server.URL, "root")
			defer conn.CloseNow()
			metadata := ""
			if defaults {
				metadata = `,"client_metadata":{"session_id":"cache"}`
			}
			raw := fmt.Sprintf(`{"type":"response.create","model":"test","prompt_cache_key":"cache","previous_response_id":"unknown-response","input":[]%s}`, metadata)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := conn.Write(ctx, websocket.MessageText, []byte(raw)); err != nil {
				t.Fatal(err)
			}
			reply := gatewayWSRead(t, conn)
			if defaults {
				if !strings.Contains(string(reply), "cache policy for previous_response_id is unavailable") {
					t.Fatalf("unknown default chain was not rejected: %s", reply)
				}
				select {
				case <-captured:
					t.Fatal("unknown default chain reached upstream")
				default:
				}
				return
			}
			if !strings.Contains(string(reply), `"response.completed"`) {
				t.Fatalf("unknown explicit chain was rejected: %s", reply)
			}
			if wire := <-captured; !strings.Contains(string(wire), `"previous_response_id":"unknown-response"`) || len(gatewayPromptCacheKeyValue(wire)) != 36 {
				t.Fatal("explicit continuation was not forwarded with scoped key and original response identity")
			}
		})
	}
}
