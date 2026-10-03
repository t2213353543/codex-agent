package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func proxyFixture(t *testing.T, upstream http.HandlerFunc) *Proxy {
	t.Helper()
	p := newTestProxy(t, 100)
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	p.Client = server.Client()
	p.BaseURL = server.URL
	p.TokenURL = server.URL + "/oauth/token"
	return p
}

// Successful same-owner OAuth fixtures must actually return owner evidence.
// Keep access-token bytes intact for transport assertions; this helper is only
// used where the test explicitly supplies its existing synthetic workspace/user.
// Unknown-owner, malformed-token and owner-change tests must not use it.
func gatewayKnownOwnerRefreshFixture(raw, workspace, user string) string {
	var reply map[string]json.RawMessage
	if workspace == "" || user == "" || json.Unmarshal([]byte(raw), &reply) != nil || reply == nil {
		panic("invalid known-owner OAuth test fixture")
	}
	var access string
	if json.Unmarshal(reply["access_token"], &access) != nil || access == "" || reply["id_token"] != nil {
		panic("known-owner OAuth test fixture needs an access token and no existing id_token")
	}
	reply["id_token"], _ = json.Marshal(managedIdentityTestJWT(workspace, user))
	encoded, err := json.Marshal(reply)
	if err != nil {
		panic("cannot encode known-owner OAuth test fixture")
	}
	return string(encoded)
}

// newTestProxy builds a Proxy with every store wired to a fresh temp dir,
// one account in the default group, and the test key as the admin key.
func newTestProxy(t *testing.T, concurrency int) *Proxy {
	t.Helper()
	dir := t.TempDir()
	s := Store{Path: filepath.Join(dir, "accounts.json")}
	raw, err := json.Marshal(map[string]any{
		"access_token": "test-upstream-token", "refresh_token": "test-refresh-token",
		"chatgpt_account_id": "test-account", "expires_at": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportWith("test", raw, AccountDefaults{Concurrency: concurrency}); err != nil {
		t.Fatal(err)
	}
	// Most transport fixtures exercise refresh of a known ChatGPT owner.
	// Unknown-owner credential replacement has separate conservative tests.
	if err := s.UpdateAccount("test", func(a *Account) error {
		a.Identity = &managedAccountIdentity{UserID: "fixture-user", WorkspaceID: a.AccountID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	metrics, err := NewMetrics(filepath.Join(dir, "stats"))
	if err != nil {
		t.Fatal(err)
	}
	proxy := &Proxy{
		Store:     s,
		Groups:    NewGroupStore(filepath.Join(dir, "groups.json")),
		Keys:      NewKeyStore(filepath.Join(dir, "keys.json")),
		Proxies:   NewProxyStore(filepath.Join(dir, "proxies.json")),
		Settings:  NewSettingsStore(filepath.Join(dir, "settings.json")),
		Metrics:   metrics,
		AdminKey:  "test-client-key",
		KeyFile:   filepath.Join(dir, "client-key"),
		BaseURL:   "http://unset",
		TokenURL:  "http://unset/oauth/token",
		Client:    newHTTPClient(),
		startedAt: time.Now(),
	}
	t.Cleanup(metrics.Close)
	return proxy
}

func proxyRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-client-key")
	return r
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatalf("invalid actual JSON %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatalf("invalid expected JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}

func TestPrepareBody(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		body, stream, err := prepareBody([]byte(`{"model":"test-model","input":"hello","store":true,"stream":false}`), false)
		if err != nil || stream {
			t.Fatalf("prepareBody: stream=%v err=%v", stream, err)
		}
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatal(err)
		}
		if string(parsed["stream"]) != "true" || string(parsed["store"]) != "false" {
			t.Fatalf("upstream flags = %s", body)
		}
		assertJSONEqual(t, parsed["input"], []byte(`[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]`))
		if _, ok := parsed["instructions"]; !ok {
			t.Error("instructions missing")
		}
	})
	t.Run("compact", func(t *testing.T) {
		body, _, err := prepareBody([]byte(`{"model":"test-model","input":[],"instructions":"keep","store":false,"stream":true}`), true)
		if err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, body, []byte(`{"model":"test-model","input":[],"instructions":"keep"}`))
	})
	t.Run("native tools and outputs", func(t *testing.T) {
		raw := []byte(`{"model":"test-model","stream":true,"instructions":"keep exactly","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: WORD"}}],"input":[{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},{"type":"function_call_output","call_id":"call_1","output":"hello\nworld"},{"type":"custom_tool_call_output","call_id":"call_2","output":"done"},{"type":"reasoning","encrypted_content":"opaque"}],"include":["reasoning.encrypted_content"],"reasoning":{"effort":"high"}}`)
		body, stream, err := prepareBody(raw, false)
		if err != nil || !stream {
			t.Fatalf("prepareBody: stream=%v err=%v", stream, err)
		}
		var before, after map[string]json.RawMessage
		if err := json.Unmarshal(raw, &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &after); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"input", "tools", "instructions", "include", "reasoning"} {
			t.Run(key, func(t *testing.T) { assertJSONEqual(t, after[key], before[key]) })
		}
	})
	for _, raw := range []string{``, `null`, `[]`, `"text"`, `{}`, `{"model":12}`, `{"model":" "}`, `{"model":"test","stream":null}`, `{"model":"test","stream":"true"}`, `{"model":"test"} trailing`} {
		t.Run("invalid "+raw, func(t *testing.T) {
			if _, _, err := prepareBody([]byte(raw), false); err == nil {
				t.Errorf("accepted invalid body %q", raw)
			}
		})
	}
}

func TestProxyStreamHeadersAndBody(t *testing.T) {
	const stream = ": keepalive\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"output\":[]}}\n\n"
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" {
			t.Error("wrong upstream authorization")
		}
		if r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Error("wrong account header")
		}
		for _, key := range []string{"Cookie", "X-API-Key", "Proxy-Authorization", "X-Forwarded-For"} {
			if r.Header.Get(key) != "" {
				t.Errorf("leaked client %s", key)
			}
		}
		for _, key := range []string{"User-Agent", "Originator", "Version"} {
			if r.Header.Get(key) == "" {
				t.Errorf("missing identity header %s", key)
			}
		}
		if r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Codex-Turn-State") != "opaque-turn-state" || r.Header.Get("Session_id") != "session-test" {
			t.Error("unexpected upstream headers")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal(body, &parsed); err != nil || string(parsed["store"]) != "false" || string(parsed["stream"]) != "true" {
			t.Errorf("upstream body = %s, err=%v", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "upstream-request")
		w.Header().Set("X-Codex-Turn-State", "next-state")
		w.Header().Set("Set-Cookie", "upstream-secret=value")
		w.Header().Set("Authorization", "Bearer upstream-secret")
		_, _ = io.WriteString(w, stream)
	})
	r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"test-model","input":[],"stream":true}`)
	for _, key := range []string{"Cookie", "X-API-Key", "Proxy-Authorization", "X-Forwarded-For", "ChatGPT-Account-Id"} {
		r.Header.Set(key, "client-secret")
	}
	r.Header.Set("X-Codex-Turn-State", "opaque-turn-state")
	r.Header.Set("Session_id", "session-test")
	r.Header.Set("User-Agent", "codex-tui/0.149.0 (Ubuntu 22.4.0; x86_64) xterm-256color")
	r.Header.Set("Originator", "codex-tui")
	r.Header.Set("Version", "0.149.0")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != stream || !w.Flushed {
		t.Fatalf("stream result: status=%d flushed=%v body=%q", w.Code, w.Flushed, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "text/event-stream" || w.Header().Get("X-Request-Id") != "upstream-request" || w.Header().Get("X-Codex-Turn-State") != "next-state" || w.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("unexpected response headers: %v", w.Header())
	}
	for _, key := range []string{"Set-Cookie", "Authorization"} {
		if w.Header().Get(key) != "" {
			t.Errorf("leaked upstream header %s", key)
		}
	}
}

func TestProxyUpstreamErrors(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			const body = `{"error":{"message":"quota unavailable","type":"rate_limit_error"}}`
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, body)
			})
			requestBody, _ := json.Marshal(map[string]any{"model": "test", "input": []any{}, "stream": stream})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", string(requestBody)))
			if w.Code != http.StatusTooManyRequests || w.Body.String() != body || w.Header().Get("Retry-After") != "30" {
				t.Fatalf("error forwarding: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	t.Run("failed SSE remains failed SSE", func(t *testing.T) {
		const body = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"unavailable\"}}}\n\n"
		p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, body)
		})
		w := httptest.NewRecorder()
		p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":true}`))
		if w.Code != http.StatusOK || w.Body.String() != body {
			t.Fatalf("SSE failure altered: %d %q", w.Code, w.Body.String())
		}
	})
}

func TestCollectResponse(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done", "response.incomplete"} {
		t.Run(terminal, func(t *testing.T) {
			body := ": ping\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ignored\"}\n\nevent: " + terminal + "\ndata: {\"type\":\"" + terminal + "\",\n" + "data: \"response\":{\"id\":\"resp_test\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}"
			got, err := collectResponse(strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, got, []byte(`{"id":"resp_test","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}]}`))
		})
	}
	for name, body := range map[string]string{
		"truncated":        "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
		"done marker only": "data: [DONE]\n\n",
		"failed":           "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n",
		"error":            "data: {\"type\":\"error\",\"message\":\"unavailable\"}\n\n",
		"missing response": "data: {\"type\":\"response.completed\"}\n\n",
		"null response":    "data: {\"type\":\"response.completed\",\"response\":null}\n\n",
		"invalid JSON":     "data: not-json\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if result, err := collectResponse(strings.NewReader(body)); err == nil || result != nil {
				t.Fatalf("result=%s err=%v, expected failure", result, err)
			}
		})
	}
	t.Run("reader failure", func(t *testing.T) {
		if _, err := collectResponse(errorReader{}); err == nil {
			t.Fatal("reader failure ignored")
		}
	})
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("test reader failed") }

func TestProxyNonStreamingResponse(t *testing.T) {
	for _, test := range []struct {
		name, body string
		code       int
	}{
		{"completed", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"output\":[]}}\n\n", http.StatusOK},
		{"truncated", "data: {\"type\":\"response.created\"}\n\n", http.StatusBadGateway},
		{"failed", "data: {\"type\":\"response.failed\"}\n\n", http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.body)
			})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":false}`))
			if w.Code != test.code || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
			}
			if test.code == http.StatusOK {
				assertJSONEqual(t, w.Body.Bytes(), []byte(`{"id":"resp_test","output":[]}`))
			}
		})
	}
}

func TestProxyCompact(t *testing.T) {
	const result = `{"id":"compact_test","object":"response.compaction","output":[]}`
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/compact" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("compact request: %s %v", r.URL.Path, r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		assertJSONEqual(t, body, []byte(`{"model":"test","input":[]}`))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, result)
	})
	// Test compact transport independently of optional gateway prompt/cache policy.
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.BuiltinPromptEnabled, settings.PromptCacheKeyEnabled = false, false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"test","input":[],"stream":true,"store":true}`))
	if w.Code != http.StatusOK || w.Body.String() != result {
		t.Fatalf("compact response: %d %s", w.Code, w.Body.String())
	}
}

func TestProxyModelsQuery(t *testing.T) {
	for _, version := range []string{"", "0.149.0-alpha.1+test"} {
		t.Run("version="+version, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				want := version
				if want == "" {
					want = clientVersion
				}
				if r.Method != http.MethodGet || r.URL.Path != "/models" || r.URL.Query().Get("client_version") != want || len(r.URL.Query()) != 1 {
					t.Errorf("models request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Accept") != "application/json" {
					t.Errorf("models Accept: %q", r.Header.Get("Accept"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"models":[{"slug":"test-model"}]}`)
			})
			w := httptest.NewRecorder()
			query := url.Values{"client_version": {version}, "client_key": {"never-forward"}}.Encode()
			p.ServeHTTP(w, proxyRequest(http.MethodGet, "/v1/models?"+query, ""))
			if w.Code != http.StatusOK {
				t.Fatalf("models status=%d body=%s", w.Code, w.Body.String())
			}
			assertJSONEqual(t, w.Body.Bytes(), []byte(`{"models":[{"slug":"test-model"}]}`))
		})
	}
}

func TestProxyRouteAndAuthRestrictions(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, test := range []struct {
		method, path, key string
		code              int
		allow             string
	}{
		{http.MethodGet, "/health", "", 200, ""},
		{http.MethodPost, "/responses", "", 401, ""},
		{http.MethodPost, "/responses", "Bearer wrong", 401, ""},
		{http.MethodGet, "/responses", "Bearer test-client-key", 405, "POST"},
		{http.MethodPost, "/models", "Bearer test-client-key", 405, "GET"},
		{http.MethodDelete, "/v1/responses/compact", "Bearer test-client-key", 405, "POST"},
		{http.MethodPost, "/v1/chat/completions", "Bearer test-client-key", 404, ""},
		{http.MethodGet, "/accounts", "Bearer test-client-key", 404, ""},
		{http.MethodGet, "/v1/models/anything", "Bearer test-client-key", 404, ""},
		{http.MethodPost, "/v1/responses/", "Bearer test-client-key", 404, ""},
		{http.MethodPost, "/v1/responses", "Bearer test-client-key", 400, ""},
	} {
		t.Run(test.method+" "+test.path+" "+test.key, func(t *testing.T) {
			r := proxyRequest(test.method, test.path, "not-json")
			r.Header.Set("Authorization", test.key)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != test.code || w.Header().Get("Allow") != test.allow {
				t.Errorf("status=%d Allow=%q body=%s", w.Code, w.Header().Get("Allow"), w.Body.String())
			}
		})
	}
}

func TestProxyClientCancellationCancelsUpstream(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(3 * time.Second):
			t.Error("upstream context was not canceled")
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":true}`).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); p.ServeHTTP(httptest.NewRecorder(), r) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(4 * time.Second):
		t.Fatal("upstream did not observe client cancellation")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not finish after cancellation")
	}
}

func TestRefreshAccount(t *testing.T) {
	for _, test := range []struct {
		name, body, clientID   string
		status                 int
		wantToken, wantRefresh string
	}{
		{"rotated", gatewayKnownOwnerRefreshFixture(`{"access_token":"next-access","refresh_token":"next-refresh","expires_in":3600}`, "test-account", "fixture-user"), "", 200, "next-access", "next-refresh"},
		{"retain refresh token", gatewayKnownOwnerRefreshFixture(`{"access_token":"next-access","expires_in":3600}`, "test-account", "fixture-user"), "custom-client", 200, "next-access", "test-refresh-token"},
		{"rejected", `{"error":"invalid_grant","secret":"not-for-errors"}`, "", 401, "", ""},
		{"invalid JSON", `not-json`, "", 200, "", ""},
		{"missing access", `{"expires_in":3600}`, "", 200, "", ""},
		{"missing expiry", gatewayKnownOwnerRefreshFixture(`{"access_token":"next-access"}`, "test-account", "fixture-user"), "", 200, "next-access", "test-refresh-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
					t.Errorf("refresh request: %s %s", r.Method, r.URL)
				}
				var payload map[string]string
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				wantClient := test.clientID
				if wantClient == "" {
					wantClient = oauthClientID
				}
				if r.Header.Get("Content-Type") != "application/json" || len(payload) != 3 || payload["grant_type"] != "refresh_token" || payload["refresh_token"] != "test-refresh-token" || payload["client_id"] != wantClient {
					t.Error("refresh JSON does not match the CLI's RefreshRequest")
				}
				if !strings.HasPrefix(r.Header.Get("User-Agent"), "codex_cli_rs/") || r.Header.Get("Originator") != "codex_cli_rs" {
					t.Error("refresh identity headers missing")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			a, err := p.Store.Active()
			if err != nil {
				t.Fatal(err)
			}
			a.ClientID = test.clientID
			before := time.Now().Unix()
			updated, err := refreshAccount(context.Background(), p.Client, p.TokenURL, a)
			if test.wantToken == "" {
				if err == nil {
					t.Fatal("refresh unexpectedly succeeded")
				}
				if strings.Contains(err.Error(), "not-for-errors") || strings.Contains(err.Error(), "test-refresh-token") {
					t.Fatalf("refresh error leaked credentials: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expiryOK := updated.ExpiresAt >= before+3600 && updated.ExpiresAt <= time.Now().Unix()+3600
			if test.name == "missing expiry" {
				expiryOK = updated.ExpiresAt == 0
			}
			if updated.AccessToken != test.wantToken || updated.RefreshToken != test.wantRefresh || !expiryOK || updated.AccountID != a.AccountID || updated.Name != a.Name {
				t.Error("refresh did not preserve identity or rotate tokens correctly")
			}
		})
	}
}

func TestConcurrentRequestsRefreshOnce(t *testing.T) {
	var refreshes, requests atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			time.Sleep(20 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`, "test-account", "fixture-user"))
			return
		}
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer rotated-access" {
			t.Error("request used stale access token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	a, err := p.Store.Active()
	if err != nil {
		t.Fatal(err)
	}
	a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	if err := p.Store.UpdateTokens(a.Name, a.AccessToken, a); err != nil {
		t.Fatal(err)
	}
	const workers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodGet, "/models", ""))
			if w.Code != http.StatusOK {
				t.Errorf("concurrent request failed: %d %s", w.Code, w.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()
	if refreshes.Load() != 1 || requests.Load() != workers {
		t.Errorf("refreshes=%d requests=%d, want 1 and %d", refreshes.Load(), requests.Load(), workers)
	}
	stored, err := p.Store.Active()
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "rotated-access" || stored.RefreshToken != "rotated-refresh" || stored.ExpiresAt <= time.Now().Unix() {
		t.Error("rotated credentials were not persisted")
	}
}

func TestRequestHeaderIdentity(t *testing.T) {
	for _, ua := range []string{"", "Mozilla/5.0 test-browser", "curl/8.0.0"} {
		t.Run(ua, func(t *testing.T) {
			source, target := make(http.Header), make(http.Header)
			source.Set("User-Agent", ua)
			source.Set("Originator", "browser")
			source.Set("Version", "old")
			source.Set("OpenAI-Beta", "responses=experimental")
			source.Set("Session_id", "keep-session")
			source.Set("X-Codex-Turn-State", "keep-state")
			copyRequestHeaders(target, source)
			if !strings.HasPrefix(target.Get("User-Agent"), "codex_cli_rs/"+clientVersion) || target.Get("Originator") != "codex_cli_rs" || target.Get("Version") != clientVersion {
				t.Errorf("identity not normalized: %v", target)
			}
			if target.Get("Session_id") != "keep-session" || target.Get("X-Codex-Turn-State") != "keep-state" || target.Get("OpenAI-Beta") != "" {
				t.Errorf("protocol headers not preserved or obsolete beta forwarded: %v", target)
			}
		})
	}
	t.Run("native Codex", func(t *testing.T) {
		source, target := make(http.Header), make(http.Header)
		source.Set("User-Agent", "codex-tui/0.149.0 (Linux 6.8; x86_64) xterm-256color")
		source.Set("Originator", "codex-tui")
		source.Set("Version", "0.149.0")
		copyRequestHeaders(target, source)
		for _, key := range []string{"User-Agent", "Originator", "Version"} {
			if target.Get(key) != source.Get(key) {
				t.Errorf("native header %s changed", key)
			}
		}
	})
}

func TestCanceledClientStillPersistsRefreshRotation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected request %s", r.URL.Path)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			t.Error("rotating refresh request canceled with caller")
			return
		case <-time.After(3 * time.Second):
			t.Error("refresh not released")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"rotated-after-cancel","refresh_token":"refresh-after-cancel","expires_in":3600}`, "test-account", "fixture-user"))
	})
	defer releaseRefresh()
	a, err := p.Store.Active()
	if err != nil {
		t.Fatal(err)
	}
	a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	if err := p.Store.UpdateTokens(a.Name, a.AccessToken, a); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.ensureReady(ctx, a); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	cancel()
	releaseRefresh()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not finish")
	}
	stored, err := p.Store.Active()
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "rotated-after-cancel" || stored.RefreshToken != "refresh-after-cancel" {
		t.Error("refresh rotation lost after caller canceled")
	}
}

func TestUnavailableAccountDoesNotForward(t *testing.T) {
	for _, withRefresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired without refresh", true: "refresh rejected"}[withRefresh], func(t *testing.T) {
			var refreshes atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth/token" {
					t.Error("business request forwarded with invalid account")
				}
				refreshes.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			})
			a, err := p.Store.Active()
			if err != nil {
				t.Fatal(err)
			}
			a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
			if !withRefresh {
				a.RefreshToken = ""
			}
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Store.Import(a.Name, raw); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodGet, "/models", ""))
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("unavailable account status=%d body=%s", w.Code, w.Body.String())
			}
			want := int32(0)
			if withRefresh {
				want = 1
			}
			if refreshes.Load() != want {
				t.Errorf("refresh count=%d want=%d", refreshes.Load(), want)
			}
			stored, err := p.Store.Active()
			if err != nil {
				t.Fatal(err)
			}
			if stored.AccessToken != a.AccessToken || stored.RefreshToken != a.RefreshToken {
				t.Error("failed refresh changed persisted credentials")
			}
		})
	}
}

func TestProxyTruncatedStreamAbortsClientConnection(t *testing.T) {
	const partial = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream does not support hijacking")
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 10000\r\nConnection: close\r\n\r\n" + partial)
		if err := rw.Flush(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(p)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", strings.NewReader(`{"model":"test","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-client-key")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("expected flushed stream headers: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err == nil {
		t.Fatal("truncated upstream stream was returned as a clean EOF")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("stream error=%v, want unexpected EOF", err)
	}
	if string(body) != partial {
		t.Errorf("partial stream = %q, want %q", body, partial)
	}
}
