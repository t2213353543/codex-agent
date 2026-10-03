package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func nativeWSFixture(t *testing.T, handler http.HandlerFunc) (*Proxy, *httptest.Server) {
	t.Helper()
	cert, _, ca := nativeCaptureCertificate(t)
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	t.Setenv("CODEX_CA_CERTIFICATE", ca)
	p := newTestProxy(t, 3)
	p.BaseURL, p.TokenURL = server.URL, server.URL+"/oauth/token"
	realGatewayNativePool(t, p)
	p.NativeWebSocket = true
	enableGatewayWS(t, p, "")
	return p, server
}

func TestGatewayNativeActualWebSocketToolTurns(t *testing.T) {
	var handshakes, turns, pongs atomic.Int32
	p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		if r.TLS == nil || r.ProtoMajor != 1 || !strings.Contains(r.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
			t.Error("native WebSocket omitted TLS/upgrade/deflate")
		}
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("native WebSocket changed account or leaked downstream secrets")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for turn := 0; turn < 2; turn++ {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			turns.Add(1)
			if turn == 1 && !strings.Contains(string(payload), "function_call_output") {
				t.Error("tool result disappeared")
			}
			if turn == 0 {
				go func() {
					if conn.Ping(r.Context()) == nil {
						pongs.Add(1)
					}
				}()
			}
			output := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("native-stream-", 6000) + `"}]}`
			if turn == 0 {
				output = `{"type":"function_call","name":"synthetic_tool","call_id":"tool-1","arguments":"{}"}`
			}
			event := fmt.Sprintf(`{"type":"response.completed","response":{"id":"native-ws-%d","status":"completed","output":[%s],"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80}}}}`, turn, output)
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
				return
			}
		}
		_, _, _ = conn.Read(r.Context())
	})
	guard := &nativeCLIFallbackGuard{}
	p.Client.Transport = guard
	gw := httptest.NewServer(p)
	defer gw.Close()
	conn, _ := gatewayWSDial(t, gw.URL, "native-ws-tools")
	conn.SetReadLimit(maxBody)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		input := `[]`
		if i == 1 {
			input = `[{"type":"function_call_output","call_id":"tool-1","output":"ok"}]`
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":`+input+`}`)); err != nil {
			t.Fatal(err)
		}
		got := gatewayWSRead(t, conn)
		if !strings.Contains(string(got), fmt.Sprintf("native-ws-%d", i)) {
			t.Fatalf("native WS output missing: length=%d", len(got))
		}
	}
	deadline := time.Now().Add(time.Second)
	for pongs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_ = conn.CloseNow()
	waitGatewayWSReleased(t, p)
	if handshakes.Load() != 1 || turns.Load() != 2 || guard.calls.Load() != 0 || pongs.Load() != 1 {
		t.Fatalf("native WS routing/ping mismatch: handshakes=%d turns=%d fallback=%d pong=%d", handshakes.Load(), turns.Load(), guard.calls.Load(), pongs.Load())
	}
	records := p.Metrics.RecentForAccount("test", 10)
	if len(records) != 2 {
		t.Fatal("native WS lost per-turn usage")
	}
	for _, rec := range records {
		if rec.InputTokens != 100 || rec.OutputTokens != 7 || rec.CachedTokens != 80 || rec.Status != "ok" {
			t.Fatal("native WS usage changed")
		}
	}
}

func TestGatewayNativeActualWebSocket401Recovery(t *testing.T) {
	var handshakes, refreshes, accepted atomic.Int32
	p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"native-ws-rotated","refresh_token":"native-ws-rotated-rt"}`, "test-account", "fixture-user"))
			return
		}
		if handshakes.Add(1) == 1 {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "Bearer native-ws-rotated" {
			t.Error("native WS did not use refreshed bearer")
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.CloseNow()
		if _, _, err = c.Read(r.Context()); err != nil {
			return
		}
		accepted.Add(1)
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"native-ws-recovered","status":"completed","output":[]}}`))
		_, _, _ = c.Read(r.Context())
	})
	gw := httptest.NewServer(p)
	defer gw.Close()
	c, _ := gatewayWSDial(t, gw.URL, "native-ws-recovery")
	if err := c.Write(t.Context(), websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if raw := gatewayWSRead(t, c); !strings.Contains(string(raw), "native-ws-recovered") {
		t.Fatal("native WS recovery missing")
	}
	_ = c.CloseNow()
	waitGatewayWSReleased(t, p)
	if handshakes.Load() != 2 || refreshes.Load() != 1 || accepted.Load() != 1 {
		t.Fatal("native handshake recovery replayed or skipped accepted turn")
	}
	a, err := p.Store.Get("test")
	if err != nil || a.RefreshToken != "native-ws-rotated-rt" {
		t.Fatal("native WS refresh RT not saved")
	}
}

func TestGatewayNativeActualWebSocket429Preserved(t *testing.T) {
	var calls atomic.Int32
	p, _ := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "17")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(429)
	})
	gw := httptest.NewServer(p)
	defer gw.Close()
	_, response, err := websocket.Dial(t.Context(), gw.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"native-ws-limited"}}})
	if err == nil || response == nil || response.StatusCode != 429 || response.Header.Get("Retry-After") != "17" || calls.Load() != 1 {
		t.Fatal("native WS rate-limit handshake was lost or replayed")
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
}

func TestGatewayNativeActualWebSocketSOCKSAndProxyFailure(t *testing.T) {
	for _, wrongPassword := range []bool{false, true} {
		t.Run(fmt.Sprint("proxy_auth_failure=", wrongPassword), func(t *testing.T) {
			var upstreamCalls atomic.Int32
			p, upstream := nativeWSFixture(t, func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				if r.URL.Path == "/oauth/token" {
					t.Error("proxy failure triggered OAuth")
					w.WriteHeader(500)
					return
				}
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"native-ws-socks","status":"completed","output":[]}}`))
				_, _, _ = conn.Read(r.Context())
			})
			socks, entry := nativeSOCKSTestServer(t, upstream.Listener.Addr().String(), "fixture-user", "fixture-password")
			entry.Name = "native WS SOCKS fixture"
			if wrongPassword {
				entry.Password = "wrong"
			}
			stored, err := p.Proxies.Create(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(account *Account) error { account.Proxies = []string{stored.ID}; return nil }); err != nil {
				t.Fatal(err)
			}
			guard := &nativeCLIFallbackGuard{}
			p.Client.Transport = guard
			gw := httptest.NewServer(p)
			defer gw.Close()
			if wrongPassword {
				conn, response, err := websocket.Dial(t.Context(), gw.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"native-ws-proxy-failed"}}})
				if conn != nil {
					_ = conn.CloseNow()
				}
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				status := 0
				if response != nil {
					status = response.StatusCode
				}
				// Existing WS transport failures use 503, unlike HTTP's 502.
				if err == nil || status != 503 || upstreamCalls.Load() != 0 || socks.dials.Load() != 1 || guard.calls.Load() != 0 {
					t.Fatalf("WS proxy failure: status=%d upstream=%d SOCKS=%d fallback=%d", status, upstreamCalls.Load(), socks.dials.Load(), guard.calls.Load())
				}
				return
			}
			conn, _ := gatewayWSDial(t, gw.URL, "native-ws-socks")
			if err := conn.Write(t.Context(), websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			if raw := gatewayWSRead(t, conn); !strings.Contains(string(raw), "native-ws-socks") {
				t.Fatal("native WS SOCKS lost response")
			}
			_ = conn.CloseNow()
			waitGatewayWSReleased(t, p)
			if upstreamCalls.Load() != 1 || socks.dials.Load() != 1 || guard.calls.Load() != 0 {
				t.Fatal("native WS SOCKS path not exclusively used")
			}
		})
	}
}
