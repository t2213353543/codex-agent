package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func enableGatewayWS(t *testing.T, p *Proxy, prompt string) {
	t.Helper()
	s, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	s.WebsocketEnabled, s.StickySessionsEnabled = true, true
	s.BuiltinPromptEnabled, s.BuiltinPrompt = prompt != "", prompt
	s.PromptCacheKeyEnabled = true
	if err := p.Settings.Set(s); err != nil {
		t.Fatal(err)
	}
}

func gatewayWSDial(t *testing.T, target, session string) (*websocket.Conn, *http.Response) {
	t.Helper()
	headers := http.Header{"Authorization": {"Bearer test-client-key"}, "Cookie": {"client-secret=1"}, "Proxy-Authorization": {"client-proxy-secret"}}
	if session != "" {
		headers.Set("Session-Id", session)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, target+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers, CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		t.Fatalf("WebSocket dial: %v (response %v)", err, response)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn, response
}

func gatewayWSRead(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type = %v", typ)
	}
	return payload
}

func waitGatewayWSReleased(t *testing.T, p *Proxy) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for p.inFlight("test") != 0 {
		select {
		case <-deadline.C:
			t.Fatal("WebSocket retained account concurrency slot")
		case <-tick.C:
		}
	}
}

func TestGatewayWebSocketTLSMultiTurnPolicyAndUsage(t *testing.T) {
	var connections atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		if r.ProtoMajor != 1 || r.TLS == nil || r.TLS.NegotiatedProtocol == "h2" {
			t.Errorf("not HTTP/1.1 WebSocket TLS: %s %+v", r.Proto, r.TLS)
		}
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Error("upstream account credentials missing")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("client credentials leaked to upstream")
		}
		if r.Header.Get("Content-Encoding") != "" {
			t.Error("HTTP compression leaked to WebSocket")
		}
		w.Header().Set("X-Codex-Turn-State", "sticky-upstream-state")
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for i := 0; i < 2; i++ {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				t.Error(err)
				return
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Error(err)
				return
			}
			var instructions string
			_ = json.Unmarshal(body["instructions"], &instructions)
			if instructions != "stable gateway prompt\n\nclient instructions" {
				t.Errorf("instructions = %q", instructions)
			}
			if _, exists := body["stream"]; exists {
				t.Error("HTTP stream flag was inserted into WebSocket payload")
			}
			if string(body["store"]) != "true" || string(body["generate"]) != "false" || string(body["client_metadata"]) != `{"opaque":"keep"}` {
				t.Errorf("WebSocket-specific fields changed: %s", raw)
			}
			if i == 1 && string(body["previous_response_id"]) != `"resp_0"` {
				t.Error("previous_response_id lost")
			}
			if _, exists := body["prompt_cache_key"]; exists {
				t.Error("ordinary Responses invented a prompt cache key")
			}
			result := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80}}}}`, i)
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(result)); err != nil {
				t.Error(err)
				return
			}
		}
		_, _, _ = conn.Read(r.Context())
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	p := newTestProxy(t, 2)
	p.BaseURL, p.Client = upstream.URL, upstream.Client()
	p.Client.Timeout = 50 * time.Millisecond
	baseTransport := p.Client.Transport.(*http.Transport)
	baseTransport.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	enableGatewayWS(t, p, "stable gateway prompt")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	conn, response := gatewayWSDial(t, server.URL, "ws-session")
	if response.Header.Get("X-Codex-Turn-State") != "sticky-upstream-state" {
		t.Fatal("upstream handshake metadata missing")
	}
	if baseTransport.TLSClientConfig.NextProtos[0] != "h2" {
		t.Fatal("shared HTTP TLS config was mutated")
	}
	for i := 0; i < 2; i++ {
		previous := ""
		if i > 0 {
			previous = `,"previous_response_id":"resp_0"`
		}
		payload := `{"type":"response.create","model":"test","input":"` + strings.Repeat("x", 5000) + `","instructions":"client instructions","store":true,"generate":false,"client_metadata":{"opaque":"keep"}` + previous + `}`
		if err := gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		if raw := gatewayWSRead(t, conn); !strings.Contains(string(raw), `"cached_tokens":80`) {
			t.Fatalf("response altered: %s", raw)
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitGatewayWSReleased(t, p)
	if connections.Load() != 1 {
		t.Fatalf("upstream connection count = %d", connections.Load())
	}
	records := p.Metrics.RecentForAccount("test", 10)
	if len(records) != 2 {
		t.Fatalf("turn count = %d", len(records))
	}
	for _, rec := range records {
		if rec.Status != "ok" || rec.InputTokens != 100 || rec.OutputTokens != 7 || rec.CachedTokens != 80 {
			t.Fatalf("incorrect WebSocket usage: %+v", rec)
		}
	}
}

func TestGatewayWebSocketBodySessionUsesExistingAccount(t *testing.T) {
	requests := make(chan string, 2)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if _, _, err = conn.Read(r.Context()); err != nil {
			t.Error(err)
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_body","status":"completed"}}`))
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	_, release, err := p.pickSessionAccount(context.Background(), "test-client-key", "admin", "body-session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := p.Store.ImportWith("other", []byte(`{"access_token":"other-token"}`), AccountDefaults{Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("other", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	conn, _ := gatewayWSDial(t, server.URL, "")
	if err := gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(`{"type":"response.create","model":"test","prompt_cache_key":"body-session","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	_ = gatewayWSRead(t, conn)
	if got := <-requests; got != "Bearer test-upstream-token" {
		t.Fatalf("body session routed to wrong account: %s", got)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitGatewayWSReleased(t, p)
}

func TestGatewayWebSocketDisconnectAbortsTurnAndReleasesAccount(t *testing.T) {
	upstreamClosed := make(chan struct{})
	started := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		defer close(upstreamClosed)
		if _, _, err = conn.Read(r.Context()); err != nil {
			t.Error(err)
			return
		}
		close(started)
		_, _, _ = conn.Read(r.Context())
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	conn, _ := gatewayWSDial(t, server.URL, "cancel-session")
	_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive request")
	}
	_ = conn.CloseNow()
	select {
	case <-upstreamClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream WebSocket not cancelled")
	}
	waitGatewayWSReleased(t, p)
	records := p.Metrics.RecentForAccount("test", 10)
	if len(records) != 1 || records[0].Status != "aborted" {
		t.Fatalf("disconnect metrics: %+v", records)
	}
}

func TestGatewayWebSocketHandshakeFailureDoesNotReplay(t *testing.T) {
	var requests atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream limit"}}`)
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, response, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"failure-session"}}})
	if err == nil || response == nil || response.StatusCode != 429 || response.Header.Get("Retry-After") != "3" {
		t.Fatalf("handshake error = %v, response = %v", err, response)
	}
	if requests.Load() != 1 {
		t.Fatal("failed handshake was replayed")
	}
	waitGatewayWSReleased(t, p)
}

func TestGatewayWebSocketTLSViaHTTPProxy(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if _, _, err = conn.Read(r.Context()); err != nil {
			t.Error(err)
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_proxy","status":"completed"}}`))
		_, _, _ = conn.Read(r.Context())
	}))
	t.Cleanup(upstream.Close)
	var connects atomic.Int32
	httpProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Error("expected proxy CONNECT")
			http.Error(w, "CONNECT required", 405)
			return
		}
		connects.Add(1)
		target, err := net.DialTimeout("tcp", r.Host, 3*time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer target.Close()
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(target, client); _ = target.Close(); close(done) }()
		_, _ = io.Copy(client, target)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(httpProxy.Close)
	p := newTestProxy(t, 2)
	p.BaseURL, p.Client = upstream.URL, upstream.Client()
	u, _ := url.Parse(httpProxy.URL)
	port, _ := strconv.Atoi(u.Port())
	entry, err := p.Proxies.Create(ProxyEntry{Name: "websocket-proxy", Type: "http", Host: u.Hostname(), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Store.UpdateAccount("test", func(a *Account) error { a.Proxies = []string{entry.ID}; return nil }); err != nil {
		t.Fatal(err)
	}
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	conn, _ := gatewayWSDial(t, server.URL, "proxy-session")
	_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`))
	_ = gatewayWSRead(t, conn)
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitGatewayWSReleased(t, p)
	if connects.Load() != 1 {
		t.Fatalf("proxy CONNECT count = %d", connects.Load())
	}
}

func TestGatewayWebSocketClientDoesNotMutateTransport(t *testing.T) {
	transport := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}}
	client := &http.Client{Transport: transport, Timeout: time.Minute}
	wsClient := websocketHTTPClient(client)
	wsTransport := wsClient.Transport.(*http.Transport)
	if wsClient.Timeout != 0 || wsTransport.ForceAttemptHTTP2 || wsTransport.Protocols.HTTP2() || !wsTransport.Protocols.HTTP1() {
		t.Fatal("WebSocket transport is not independent HTTP/1.1")
	}
	if client.Timeout != time.Minute || !transport.ForceAttemptHTTP2 || transport.TLSClientConfig.NextProtos[0] != "h2" {
		t.Fatal("base transport mutated")
	}
	if websocketHTTPClient(nil) == nil {
		t.Fatal("nil client fallback missing")
	}
}

func TestGatewayWebSocketMultiplexUsage(t *testing.T) {
	p := newTestProxy(t, 2)
	turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn)}
	for _, lane := range []string{"a", "b", "a"} {
		if err := turns.start([]byte(`{"model":"test","stream_id":"`+lane+`"}`), Account{Name: "test"}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, lane := range []string{"b", "a", "a"} {
		turns.observe([]byte(`{"type":"response.completed","stream_id":"` + lane + `","response":{"status":"completed","usage":{"input_tokens":20,"output_tokens":2,"input_tokens_details":{"cached_tokens":15}}}}`))
	}
	turns.abort()
	records := p.Metrics.RecentForAccount("test", 10)
	if len(records) != 3 {
		t.Fatalf("multiplex turn count = %d", len(records))
	}
	for _, rec := range records {
		if rec.CachedTokens != 15 || rec.Status != "ok" {
			t.Fatalf("multiplex usage = %+v", rec)
		}
	}
}

func TestGatewayWebSocketPerTurnAdmissionAndIdleRelease(t *testing.T) {
	requests := make(chan []byte, 4)
	complete := make(chan struct{}, 4)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for turn := 0; ; turn++ {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			requests <- raw
			<-complete
			if conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","stream_id":"active","response":{"id":"resp_active_%d","status":"completed"}}`, turn))) != nil {
				return
			}
		}
	})
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Concurrency = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	conn, _ := gatewayWSDial(t, server.URL, "admission-session")
	if p.inFlight("test") != 0 {
		t.Fatal("idle connection consumed inference slot")
	}
	_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(`{"type":"response.create","stream_id":"active","model":"test","input":[]}`))
	select {
	case <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn not forwarded")
	}
	if p.inFlight("test") != 1 {
		t.Fatal("active turn did not acquire inference slot")
	}
	_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(`{"type":"response.create","stream_id":"blocked","model":"test","input":[]}`))
	errBody := gatewayWSRead(t, conn)
	if !strings.Contains(string(errBody), `"status":429`) || !strings.Contains(string(errBody), `"stream_id":"blocked"`) {
		t.Fatalf("missing lane admission error: %s", errBody)
	}
	select {
	case <-requests:
		t.Fatal("over-limit turn reached upstream")
	default:
	}
	complete <- struct{}{}
	_ = gatewayWSRead(t, conn)
	waitGatewayWSReleased(t, p)
	// HTTP can use the idle connection's account without forcing an account switch.
	_, release, err := p.pickSessionAccount(context.Background(), "test-client-key", "admin", "admission-session")
	if err != nil {
		t.Fatal("idle WebSocket blocked HTTP session: ", err)
	}
	release()
	_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(`{"type":"response.create","stream_id":"active","model":"test","input":[]}`))
	select {
	case <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("next turn not admitted")
	}
	complete <- struct{}{}
	_ = gatewayWSRead(t, conn)
	waitGatewayWSReleased(t, p)
	if records := p.Metrics.RecentForAccount("test", 10); len(records) != 2 || records[0].Status != "ok" || records[1].Status != "ok" {
		t.Fatal("both admitted turns must finish successfully before the socket closes")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	waitGatewayWSReleased(t, p)
}

func TestGatewayWebSocketRechecksAuthorizationAndPolicy(t *testing.T) {
	for _, revoke := range []string{"key", "account"} {
		t.Run(revoke, func(t *testing.T) {
			requests := make(chan []byte, 4)
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				for turn := 0; ; turn++ {
					_, raw, err := conn.Read(r.Context())
					if err != nil {
						return
					}
					requests <- raw
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_policy_%d","status":"completed"}}`, turn)))
				}
			})
			enableGatewayWS(t, p, "first policy")
			server := httptest.NewServer(p)
			t.Cleanup(server.Close)
			conn, _ := gatewayWSDial(t, server.URL, "policy-session")
			request := []byte(`{"type":"response.create","model":"test","input":[]}`)
			_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, request)
			_ = gatewayWSRead(t, conn)
			if raw := <-requests; !strings.Contains(string(raw), "first policy") {
				t.Fatalf("initial policy missing: %s", raw)
			}
			s, _ := p.Settings.Get()
			s.BuiltinPromptEnabled = false
			s.PromptCacheKeyEnabled = false
			if err := p.Settings.Set(s); err != nil {
				t.Fatal(err)
			}
			p.sessionMu.Lock()
			for key, route := range p.sessionRoutes {
				route.Expires = time.Now().Add(time.Second)
				p.sessionRoutes[key] = route
			}
			p.sessionMu.Unlock()
			_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, request)
			_ = gatewayWSRead(t, conn)
			if raw := <-requests; strings.Contains(string(raw), "instructions") || strings.Contains(string(raw), "prompt_cache_key") {
				t.Fatalf("disabled policy persisted on existing socket: %s", raw)
			}
			waitGatewayWSReleased(t, p)
			if records := p.Metrics.RecentForAccount("test", 10); len(records) != 2 || records[0].Status != "ok" || records[1].Status != "ok" {
				t.Fatal("both policy turns must finish successfully before authorization is revoked")
			}
			p.sessionMu.Lock()
			for _, route := range p.sessionRoutes {
				if time.Until(route.Expires) < time.Minute {
					t.Error("turn did not renew session TTL")
				}
			}
			p.sessionMu.Unlock()
			if revoke == "key" {
				p.setAdminKey("replacement-key")
			} else {
				if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
					t.Fatal(err)
				}
				p.invalidateAccounts()
			}
			_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, request)
			if raw := gatewayWSRead(t, conn); !strings.Contains(string(raw), `"status":403`) {
				t.Fatalf("revoked access still usable: %s", raw)
			}
			select {
			case raw := <-requests:
				t.Fatalf("revoked request forwarded: %s", raw)
			default:
			}
			_ = conn.CloseNow()
		})
	}
}

func TestGatewayWebSocketRenewsAndDetectsChangedBindings(t *testing.T) {
	p := newTestProxy(t, 1)
	enableGatewayWS(t, p, "")
	turns := gatewayWSTurns{proxy: p, keyID: "admin", bindings: make(map[string]string)}
	if err := turns.bind(context.Background(), "lease-session", Account{Name: "test"}); err != nil {
		t.Fatal(err)
	}
	p.sessionMu.Lock()
	for key, route := range p.sessionRoutes {
		route.Expires = time.Now().Add(time.Second)
		p.sessionRoutes[key] = route
	}
	p.sessionMu.Unlock()
	if err := turns.renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.sessionMu.Lock()
	for key, route := range p.sessionRoutes {
		if time.Until(route.Expires) < time.Minute {
			t.Error("lease not renewed")
		}
		route.Account = "name:other"
		p.sessionRoutes[key] = route
	}
	p.sessionMu.Unlock()
	if err := turns.renew(context.Background()); err == nil {
		t.Fatal("changed account binding accepted")
	}
}

func TestGatewayWebSocketClientDisconnectCancelsBodySessionHandshake(t *testing.T) {
	handshakeStarted := make(chan struct{})
	handshakeCancelled := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(handshakeStarted)
		<-r.Context().Done()
		close(handshakeCancelled)
	})
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	t.Cleanup(server.Close)
	conn, _ := gatewayWSDial(t, server.URL, "")
	_ = gatewayWSWrite(context.Background(), conn, websocket.MessageText, []byte(`{"type":"response.create","model":"test","prompt_cache_key":"dial-session","input":[]}`))
	select {
	case <-handshakeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream dial did not begin")
	}
	_ = conn.CloseNow()
	select {
	case <-handshakeCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel upstream handshake")
	}
	waitGatewayWSReleased(t, p)
}

func TestGatewayWebSocketTurnTrackingIsBounded(t *testing.T) {
	p := newTestProxy(t, 1)
	turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn), bindings: make(map[string]string)}
	for i := 0; i < 256; i++ {
		lane := fmt.Sprintf("lane-%d", i)
		if err := turns.start([]byte(`{"model":"test","stream_id":"`+lane+`"}`), Account{Name: "test"}, nil, nil); err != nil {
			t.Fatal(err)
		}
		turns.observe([]byte(`{"type":"response.completed","stream_id":"` + lane + `","response":{"status":"completed"}}`))
	}
	if len(turns.pending) != 0 || turns.count != 0 {
		t.Fatalf("completed lanes retained: %d", len(turns.pending))
	}
	for i := 0; i < 128; i++ {
		turns.bindings[fmt.Sprint(i)] = "name:test"
	}
	if err := turns.bind(context.Background(), "one-too-many", Account{Name: "test"}); err == nil {
		t.Fatal("unbounded session bindings")
	}
}
