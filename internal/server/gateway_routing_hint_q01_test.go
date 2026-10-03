package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestQualityQ01RoutingHintScopeAndControls(t *testing.T) {
	const base = "https://chatgpt.com/backend-api/codex"
	account := Account{AccessToken: "synthetic-oauth", AccountID: "synthetic-workspace"}
	for _, tc := range []struct {
		name, base, path, body, want, outcome string
		account                               Account
	}{
		{"model_only", base, "/responses", `{"model":"test"}`, "model=test", "generated", account},
		{"priority", base, "/responses", `{"model":"test","service_tier":"priority","reasoning":{"effort":"xhigh"}}`, "model=test;tier=priority", "generated", account},
		{"null_tier", base, "/responses", `{"model":"test","service_tier":null}`, "model=test", "generated", account},
		{"explicit_default_retained", base, "/responses", `{"model":"test","service_tier":"default"}`, "model=test;tier=default", "generated", account},
		{"future_tier_retained", base, "/responses", `{"model":"test","service_tier":"future-tier"}`, "model=test;tier=future-tier", "generated", account},
		{"default_port_case", "https://CHATGPT.COM:443/backend-api/codex/", "/responses", `{"model":"provider/test-1.2"}`, "model=provider/test-1.2", "generated", account},
		{"no_body_prewarm", base, "/responses", "", "", "deferred", account},
		{"guardian", base, "/guardian", `{"model":"test"}`, "", "not_applicable", account},
		{"models", base, "/models", `{"model":"test"}`, "", "not_applicable", account},
		{"legacy_compact", base, "/responses/compact", `{"model":"test"}`, "", "not_applicable", account},
		{"custom_host", "https://proxy.example/backend-api/codex", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"suffix_trick", "https://chatgpt.com.example/backend-api/codex", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"subdomain", "https://api.chatgpt.com/backend-api/codex", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"nonstandard_port", "https://chatgpt.com:8443/backend-api/codex", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"http", "http://chatgpt.com/backend-api/codex", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"public_api", "https://api.openai.com/v1", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"query", base + "?hint=x", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"empty_query", base + "?", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"empty_fragment", base + "#", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"escaped_path", "https://chatgpt.com/backend-api/%63odex", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"userinfo", "https://caller@chatgpt.com/backend-api/codex", "/responses", `{"model":"test"}`, "", "not_applicable", account},
		{"missing_oauth_workspace", base, "/responses", `{"model":"test"}`, "", "not_applicable", Account{AccessToken: "synthetic"}},
		{"missing_token", base, "/responses", `{"model":"test"}`, "", "not_applicable", Account{AccountID: "synthetic"}},
		{"missing_model", base, "/responses", `{}`, "", "unrepresentable_controls", account},
		{"null_model", base, "/responses", `{"model":null}`, "", "unrepresentable_controls", account},
		{"wrong_tier_type", base, "/responses", `{"model":"test","service_tier":123}`, "", "unrepresentable_controls", account},
		{"empty_tier", base, "/responses", `{"model":"test","service_tier":""}`, "", "unrepresentable_controls", account},
		{"header_injection", base, "/responses", `{"model":"test\r\nX-Injected: bad"}`, "", "unrepresentable_controls", account},
		{"tier_injection", base, "/responses", `{"model":"test;tier=priority"}`, "", "unrepresentable_controls", account},
		{"unicode", base, "/responses", `{"model":"测试"}`, "", "unrepresentable_controls", account},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := make(http.Header)
			body := []byte(tc.body)
			if got := applyGatewayRoutingHint(header, tc.base, tc.path, tc.account, body); got != tc.outcome {
				t.Errorf("disposition=%q, want %q", got, tc.outcome)
			}
			if got := header.Get(gatewayRoutingHintHeader); got != tc.want {
				t.Errorf("hint=%q, want %q", got, tc.want)
			}
			if string(body) != tc.body {
				t.Fatal("request controls were changed")
			}
		})
	}
}

func TestQualityQ01RoutingHintPreservesExplicitExtensionsAndConflicts(t *testing.T) {
	account := Account{AccessToken: "synthetic-oauth", AccountID: "synthetic-workspace"}
	body := []byte(`{"model":"test","service_tier":"priority"}`)
	for _, tc := range []struct {
		values []string
		match  bool
	}{
		{[]string{"model=test;tier=priority"}, true},
		{[]string{"future=keep;model=test;tier=priority;opaque=a=b;c"}, true},
		{[]string{"model=different;tier=priority"}, false},
		{[]string{"model=test;tier=flex"}, false},
		{[]string{"model=test"}, false},
		{[]string{"opaque_future_format"}, false},
		{[]string{"model=test;model=test;tier=priority"}, false},
		{[]string{"model=test;tier=priority", "future=keep"}, false},
		{[]string{""}, false},
	} {
		header := http.Header{"x-codex-routing-hint": tc.values, "X-Codex-Other": {"unchanged"}}
		before := header.Clone()
		result := applyGatewayRoutingHint(header, "https://chatgpt.com/backend-api/codex", "/responses", account, body)
		if (result == "preserved_match") != tc.match {
			t.Errorf("hint %q: %s", tc.values, result)
		}
		if !reflect.DeepEqual(before, header) {
			t.Errorf("explicit hint was rewritten: %v -> %v", before, header)
		}
	}
}

// Preserve the production BaseURL for policy decisions, then route all actual
// I/O to the test server. No DNS lookup or connection to ChatGPT is performed.
type q01RoutingHintLoopbackTransport struct {
	t       *testing.T
	base    *url.URL
	wrapped http.RoundTripper
}

func (rt q01RoutingHintLoopbackTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != "chatgpt.com" || request.URL.Scheme != "https" {
		rt.t.Errorf("unexpected upstream: %s", request.URL.Redacted())
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = rt.base.Scheme, rt.base.Host
	return rt.wrapped.RoundTrip(clone)
}

func q01RoutingHintProxy(t *testing.T, upstream http.HandlerFunc) *Proxy {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, 5)
	p.BaseURL = "https://chatgpt.com/backend-api/codex"
	p.Client = &http.Client{Transport: q01RoutingHintLoopbackTransport{t: t, base: base, wrapped: server.Client().Transport}}
	settings := defaultSettings()
	settings.BuiltinPromptEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQualityQ01RoutingHintHTTPFinalPreparedRequest(t *testing.T) {
	for _, existing := range []string{"", "model=test;tier=priority;extension=retained", "model=other;tier=flex;extension=retained"} {
		t.Run(existing, func(t *testing.T) {
			called := false
			p := q01RoutingHintProxy(t, func(w http.ResponseWriter, r *http.Request) {
				called = true
				want := existing
				if want == "" {
					want = "model=test;tier=priority"
				}
				if got := r.Header.Get(gatewayRoutingHintHeader); got != want {
					t.Errorf("wire hint=%q want=%q", got, want)
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["reasoning"]) != `{"effort":"xhigh"}` || string(body["service_tier"]) != `"priority"` {
					t.Errorf("body controls changed: %v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"test","service_tier":"priority","reasoning":{"effort":"xhigh"},"input":[],"stream":true}`)
			if existing != "" {
				r.Header.Set(gatewayRoutingHintHeader, existing)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusOK || !called {
				t.Fatalf("request changed acceptance: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestQualityQ01RoutingHintDoesNotFollowCompactAdapterRoute(t *testing.T) {
	for _, route := range []string{"/v1/responses/compact", "/v1/models", "/v1/guardian", "/v1/guardian-classifier"} {
		t.Run(route, func(t *testing.T) {
			called := false
			p := q01RoutingHintProxy(t, func(w http.ResponseWriter, r *http.Request) {
				called = true
				if got := r.Header.Get(gatewayRoutingHintHeader); got != "" {
					t.Errorf("non-Responses ingress acquired a routing hint: %q", got)
				}
				if route == "/v1/responses/compact" && r.URL.Path != "/backend-api/codex/responses" {
					t.Errorf("test did not exercise compact-v2 adapter: %s", r.URL.Path)
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"synthetic rejection"}}`)
			})
			method := http.MethodPost
			if route == "/v1/models" {
				method = http.MethodGet
			}
			r := proxyRequest(method, route, `{"model":"test","service_tier":"priority","input":[]}`)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !called {
				t.Fatalf("route did not reach loopback: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestQualityQ01RoutingHintWebSocketLazyAndPreconnectReuse(t *testing.T) {
	for _, earlySession := range []bool{false, true} {
		t.Run(fmt.Sprint(earlySession), func(t *testing.T) {
			var connections atomic.Int32
			hints := make(chan string, 1)
			p := q01RoutingHintProxy(t, func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				hints <- r.Header.Get(gatewayRoutingHintHeader)
				conn, err := websocket.Accept(w, r, nil)
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
					var body struct {
						Model string `json:"model"`
						Tier  string `json:"service_tier"`
					}
					_ = json.Unmarshal(raw, &body)
					if (i == 0 && (body.Model != "first" || body.Tier != "priority")) || (i == 1 && (body.Model != "second" || body.Tier != "flex")) {
						t.Errorf("subsequent model/tier was changed: %s", raw)
					}
					event := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[]}}`, i)
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
						t.Error(err)
						return
					}
				}
				_, _, _ = conn.Read(r.Context())
			})
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			session := ""
			if earlySession {
				session = "routing-session"
			}
			conn, _ := gatewayWSDial(t, server.URL, session)
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, body := range []string{
				`{"type":"response.create","model":"first","service_tier":"priority","input":[],"prompt_cache_key":"routing-session"}`,
				`{"type":"response.create","model":"second","service_tier":"flex","input":[],"prompt_cache_key":"routing-session","previous_response_id":"resp_0"}`,
			} {
				if err := conn.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
					t.Fatal(err)
				}
				if result := gatewayWSRead(t, conn); !strings.Contains(string(result), "response.completed") {
					t.Fatalf("response rejected: %s", result)
				}
			}
			if got := <-hints; (earlySession && got != "") || (!earlySession && got != "model=first;tier=priority") {
				t.Errorf("handshake hint=%q early=%v", got, earlySession)
			}
			if connections.Load() != 1 {
				t.Fatalf("model/tier change reconnected: %d", connections.Load())
			}
		})
	}
}
