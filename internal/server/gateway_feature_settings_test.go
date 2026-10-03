package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewayFeatureSettingsTransportGates(t *testing.T) {
	for _, test := range []struct {
		name, method, path, body string
		set                      func(*Settings, bool)
	}{
		{"stream", "POST", "/v1/responses", `{"model":"test","stream":true}`, func(s *Settings, v bool) { s.HTTPStreamEnabled = v }},
		{"json", "POST", "/responses", `{"model":"test","stream":false}`, func(s *Settings, v bool) { s.HTTPJSONEnabled = v }},
		{"compact", "POST", "/v1/responses/compact", `{"model":"test"}`, func(s *Settings, v bool) { s.CompactEnabled = v }},
		{"models", "GET", "/v1/models", "", func(s *Settings, v bool) { s.ModelsEnabled = v }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/responses" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"ok\",\"output\":[]}}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"output":[],"models":[]}`)
				}
			})
			for _, enabled := range []bool{false, true, false} {
				s, err := p.Settings.Get()
				if err != nil {
					t.Fatal(err)
				}
				test.set(&s, enabled)
				if err := p.Settings.Set(s); err != nil {
					t.Fatal(err)
				}
				before := calls.Load()
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(test.method, test.path, test.body))
				want, wantCalls := http.StatusForbidden, before
				if enabled {
					want, wantCalls = http.StatusOK, before+1
				}
				if w.Code != want || calls.Load() != wantCalls || p.inFlight("test") != 0 {
					t.Fatalf("enabled=%v status=%d calls=%d inflight=%d body=%s", enabled, w.Code, calls.Load(), p.inFlight("test"), w.Body.String())
				}
			}
		})
	}
}

func TestGatewayFeatureSettingsPromptModes(t *testing.T) {
	for _, test := range []struct{ mode, instructions, want string }{
		{"prepend", "client", "gateway\n\nclient"},
		{"append", "client", "client\n\ngateway"},
		{"if_empty", "client", "client"},
		{"if_empty", " \n ", "gateway"},
		{"prepend", "", "gateway"},
		{"append", "", "gateway"},
	} {
		t.Run(test.mode+"/"+test.instructions, func(t *testing.T) {
			s := defaultSettings()
			s.BuiltinPrompt, s.BuiltinPromptMode = "gateway", test.mode
			s.PromptCacheKeyEnabled = false
			raw, _ := json.Marshal(map[string]any{"instructions": test.instructions, "input": []any{map[string]string{"type": "reasoning", "encrypted_content": "opaque"}}})
			r := httptest.NewRequest("POST", "/responses", nil)
			first, _, err := applyGatewayPolicy(r, raw, "test", s)
			if err != nil || policyString(t, first, "instructions") != test.want {
				t.Fatalf("first=%s err=%v", first, err)
			}
			second, _, err := applyGatewayPolicy(r, first, "test", s)
			if err != nil || string(second) != string(first) {
				t.Fatalf("non-idempotent second=%s err=%v", second, err)
			}
		})
	}
}

func TestGatewayFeatureSettingsCompatibilitySwitch(t *testing.T) {
	const raw = `{"model":"test","temperature":0.2,"max_output_tokens":100,"input":[{"role":"system","content":"policy"},{"type":"reasoning","encrypted_content":"opaque"}],"tools":[{"type":"function","name":"call","parameters":{"temperature":{"type":"number"}}}]}`
	s := defaultSettings()
	s.BuiltinPromptEnabled, s.PromptCacheKeyEnabled = false, false
	r := httptest.NewRequest("POST", "/responses", nil)
	s.CompatibilityNormalizationEnabled = false
	unchanged, _, err := applyGatewayPolicy(r, []byte(raw), "test", s)
	if err != nil || string(unchanged) != raw {
		t.Fatalf("disabled normalization changed payload: %s %v", unchanged, err)
	}
	s.CompatibilityNormalizationEnabled = true
	normalized, _, err := applyGatewayPolicy(r, []byte(raw), "test", s)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(normalized, &fields)
	if fields["temperature"] != nil || fields["max_output_tokens"] != nil || !strings.Contains(string(fields["input"]), `"developer"`) || !strings.Contains(string(fields["tools"]), `"temperature"`) || !strings.Contains(string(fields["input"]), `"opaque"`) {
		t.Fatalf("normalization changed unrelated content: %s", normalized)
	}
}

func TestGatewayFeatureSettingsHeaderDeadlineAndActiveBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-header" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if r.URL.Path == "/active-body" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		_, _ = io.WriteString(w, "complete")
	}))
	defer server.Close()
	base := server.Client()
	original := base.Transport.(*http.Transport)
	configured, cleanup := gatewayClientWithHeaderTimeout(base, 30*time.Millisecond)
	defer cleanup()
	if configured == base || original.ResponseHeaderTimeout != 0 {
		t.Fatal("custom setting mutated shared client")
	}
	res, err := configured.Get(server.URL + "/slow-header")
	if res != nil {
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("response header timeout was not enforced")
	}
	res, err = configured.Get(server.URL + "/active-body")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "complete" {
		t.Fatalf("header timeout incorrectly bounded body: %q %v", body, err)
	}
	reused, done := gatewayClientWithHeaderTimeout(configured, 30*time.Millisecond)
	defer done()
	if reused != configured {
		t.Fatal("same timeout discarded connection pool")
	}
}

func TestGatewayFeatureSettingsTimeoutsReachUpstream(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(60 * time.Millisecond):
		}
		if r.URL.Path == "/responses" {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"ok\",\"output\":[]}}\n\n")
		} else {
			_, _ = io.WriteString(w, `{"output":[],"models":[]}`)
		}
	})
	base := p.Client.Transport.(*http.Transport)
	base.ResponseHeaderTimeout = 10 * time.Millisecond
	s, _ := p.Settings.Get()
	s.UpstreamResponseHeaderTimeoutSeconds, s.UpstreamCompactTimeoutSeconds = 5, 30
	if err := p.Settings.Set(s); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/responses", "/models", "/responses/compact"} {
		method := "POST"
		if path == "/models" {
			method = "GET"
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, proxyRequest(method, path, `{"model":"test","stream":false}`))
		if w.Code != http.StatusOK {
			t.Fatalf("%s did not use configured timeout: %d %s", path, w.Code, w.Body.String())
		}
	}
	if base.ResponseHeaderTimeout != 10*time.Millisecond {
		t.Fatal("changed shared transport")
	}
	p.closeGatewayClients()
}

func TestGatewayFeatureSettingsHTTPDisabledKeepsWebSocket(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, _, err = conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"ws-only","output":[]}}`))
		_, _, _ = conn.Read(r.Context())
	})
	s, _ := p.Settings.Get()
	s.WebsocketEnabled = true
	s.HTTPStreamEnabled, s.HTTPJSONEnabled, s.ModelsEnabled, s.CompactEnabled = false, false, false, false
	if err := p.Settings.Set(s); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	conn, _ := gatewayWSDial(t, server.URL, "ws-only-session")
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if raw := gatewayWSRead(t, conn); !strings.Contains(string(raw), "ws-only") {
		t.Fatalf("HTTP gates affected WebSocket: %s", raw)
	}
	_ = conn.CloseNow()
	waitGatewayWSReleased(t, p)
	p.closeGatewayClients()
}
