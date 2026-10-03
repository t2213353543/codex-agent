package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayGuardianWebSocketEndpointTransparent(t *testing.T) {
	for _, endpoint := range []string{"/guardian", "/guardian-classifier"} {
		for _, prefix := range []string{"", "/v1"} {
			t.Run(prefix+endpoint, func(t *testing.T) {
				gatewayGuardianWebSocketRoundTrip(t, endpoint, prefix, false)
			})
		}
	}
}

func TestGatewayNativeActualGuardianWebSocketEndpointTransparent(t *testing.T) {
	for _, endpoint := range []string{"/guardian", "/guardian-classifier"} {
		t.Run(endpoint, func(t *testing.T) { gatewayGuardianWebSocketRoundTrip(t, endpoint, "/v1", true) })
	}
}

func gatewayGuardianWebSocketRoundTrip(t *testing.T, endpoint, prefix string, native bool) {
	t.Helper()
	payload := []byte(`{ "type":"response.create", "model":"guardian-fixture", "instructions":"client review only", "input":[{"type":"message","role":"user","content":"keep source"}], "prompt_cache_key":"default", "store":true, "client_metadata":{"parent_response_id":"client-parent","opaque":9007199254740993}, "unknown_future":{"preserve":true} }`)
	reply := []byte(`{"type":"response.completed","response":{"id":"guardian-result","model":"guardian-fixture","status":"completed","output":[],"usage":{"input_tokens":4,"output_tokens":2}}}`)
	var handshakes atomic.Int32
	var gotPath string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		handshakes.Add(1)
		if r.URL.Path != "/api-prefix"+endpoint {
			t.Errorf("wrong upstream endpoint %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Error("account authorization lost")
		}
		if r.Header.Get("Session-Id") != "guardian-client-session" {
			t.Error("Responses cache policy changed Guardian session header")
		}
		if r.Header.Get("X-Codex-Routing-Hint") != "" {
			t.Error("Responses routing hint injected into Guardian")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, body, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		if string(body) != string(payload) {
			t.Errorf("Guardian message was rewritten:\n%s", body)
		}
		_ = conn.Write(r.Context(), websocket.MessageText, reply)
		_, _, _ = conn.Read(r.Context())
	}
	var p *Proxy
	if native {
		var upstream *httptest.Server
		p, upstream = nativeWSFixture(t, handler)
		p.BaseURL = upstream.URL + "/api-prefix"
	} else {
		upstream := httptest.NewServer(http.HandlerFunc(handler))
		t.Cleanup(upstream.Close)
		p = newTestProxy(t, 1)
		p.BaseURL = upstream.URL + "/api-prefix"
	}
	enableGatewayWS(t, p, "SHOULD NOT ENTER GUARDIAN")
	gw := httptest.NewServer(p)
	defer gw.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	headers := http.Header{"Authorization": {"Bearer test-client-key"}, "Session-Id": {"guardian-client-session"}}
	conn, res, err := websocket.Dial(ctx, gw.URL+prefix+endpoint, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatalf("Guardian WebSocket upgrade failed: %v %v", err, res)
	}
	defer conn.CloseNow()
	if err = conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.Read(ctx)
	if err != nil || string(body) != string(reply) {
		t.Fatalf("Guardian response changed: %v %s", err, body)
	}
	_ = conn.CloseNow()
	waitGatewayWSReleased(t, p)
	if handshakes.Load() != 1 || gotPath != "/api-prefix"+endpoint {
		t.Fatal("upstream handshake was rerouted")
	}
	p.Metrics.mu.Lock()
	defer p.Metrics.mu.Unlock()
	if p.Metrics.recentAt != 1 {
		t.Fatalf("records=%d", p.Metrics.recentAt)
	}
	record := p.Metrics.recent[0]
	if record.Endpoint != strings.TrimPrefix(endpoint, "/") || record.Model != "guardian-fixture" {
		t.Fatalf("Guardian attribution lost: endpoint=%s model=%s", record.Endpoint, record.Model)
	}
	if len(record.Attempts) == 0 || record.Attempts[0].Endpoint != endpoint {
		t.Fatalf("handshake attempt attributed to responses: %+v", record.Attempts)
	}
}

func TestGatewayGuardianWebSocketAllowlistAndNativeBoundary(t *testing.T) {
	p := &Proxy{NativeHTTP: &gatewayNativePool{}}
	for _, path := range []string{"/responses/compact", "/v1/guardian", "/guardian/other", "https://example.invalid/guardian", "/guardian?secret=x"} {
		_, _, err := p.dialNativeGatewayWebSocketEndpointLifetime(t.Context(), t.Context(), nil, nil, path)
		if !errors.Is(err, errNativeWSProtocol) {
			t.Fatalf("native endpoint accepted %q: %v", path, err)
		}
	}
	for _, path := range []string{"/guardian", "/v1/guardian", "/guardian-classifier", "/v1/guardian-classifier", "/responses", "/v1/responses"} {
		got, err := gatewayWSModelEndpoint(path)
		if err != nil || got != strings.TrimPrefix(path, "/v1") {
			t.Fatalf("endpoint %q: %s %v", path, got, err)
		}
	}
}
