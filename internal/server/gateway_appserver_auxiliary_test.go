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
)

func appServerAuxiliaryTestProxy(t *testing.T) *Proxy {
	t.Helper()
	p := newTestProxy(t, 1)
	p.RuntimeModes = true
	p.AppServerCLI = &appServerCLI{}
	settings, _ := p.Settings.Get()
	settings.InferenceMode = inferenceCodexCLI
	settings.AutomaticTokenRefreshEnabled = false
	settings.Upstream401RecoveryEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	return p
}
func appServerAuxiliaryTestRequest(path, body, key string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Session-Id", "search-session")
	r.Header.Set("Originator", "codex_cli_rs")
	r.Header.Set("User-Agent", "codex_cli_rs/synthetic")
	return r
}

func TestAppServerAuxiliaryExactJSONAndAdmittedIdentity(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	raw := `{ "id":"search-session","model":"fixture","input":[{"encrypted_content":"opaque"}],"commands":{"search_query":[{"q":"synthetic"}]},"unknown":{"large":9007199254740993} }`
	response := `{"output":"exact search result","encrypted_output":"opaque-result","results":[{"url":"https://example.invalid"}],"usage":{"input_tokens":9,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}`
	calls := 0
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		got, _ := io.ReadAll(r.Body)
		if string(got) != raw {
			t.Error("search request body changed")
		}
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/alpha/search" {
			t.Errorf("wrong target %s", r.URL)
		}
		if r.Header.Get("Authorization") == "Bearer test-client-key" || r.Header.Get("Authorization") == "" {
			t.Error("client key reached upstream")
		}
		if r.Header.Get("Content-Encoding") != "" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("search HTTP framing changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"search-upstream"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	for _, path := range []string{"/v1/alpha/search", "/alpha/search"} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, appServerAuxiliaryTestRequest(path, raw, "test-client-key"))
		if w.Code != 200 || w.Body.String() != response {
			t.Fatalf("response changed: %d %s", w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatal("wrong upstream calls")
	}
}

func TestAppServerAuxiliaryKeyMethodPathAndConcurrency(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"output":"ok"}`))}, nil
	})}
	bad := httptest.NewRecorder()
	p.ServeHTTP(bad, appServerAuxiliaryTestRequest("/v1/alpha/search", `{"model":"fixture"}`, "bad"))
	if bad.Code != 401 || calls.Load() != 0 {
		t.Fatal("bad key admitted")
	}
	method := appServerAuxiliaryTestRequest("/v1/alpha/search", `{}`, "test-client-key")
	method.Method = "GET"
	w := httptest.NewRecorder()
	p.ServeHTTP(w, method)
	if w.Code != 405 {
		t.Fatal("wrong method allowed")
	}
	unknown := httptest.NewRecorder()
	p.ServeHTTP(unknown, appServerAuxiliaryTestRequest("/v1/alpha/private", `{}`, "test-client-key"))
	if unknown.Code != 404 {
		t.Fatal("arbitrary alpha path exposed")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.ServeHTTP(httptest.NewRecorder(), appServerAuxiliaryTestRequest("/v1/alpha/search", `{"model":"fixture"}`, "test-client-key"))
	}()
	<-entered
	second := httptest.NewRecorder()
	p.ServeHTTP(second, appServerAuxiliaryTestRequest("/v1/alpha/search", `{"model":"fixture"}`, "test-client-key"))
	if second.Code != 429 || calls.Load() != 1 {
		t.Errorf("account concurrency bypassed: %d", second.Code)
	}
	close(release)
	<-done
}

func TestAppServerAuxiliaryAccountFailoverAndOwnerCAS(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "switch", true: "CAS-conflict"}[conflict], func(t *testing.T) {
			p := appServerAuxiliaryTestProxy(t)
			_ = p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; a.AccessToken = "bad"; a.RefreshToken = ""; return nil })
			raw, _ := json.Marshal(map[string]any{"access_token": "good", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("good", raw, AccountDefaults{Concurrency: 1, Priority: 0}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			calls := []string{}
			body := `{"model":"fixture","id":"search-session","commands":{"search_query":[{"q":"exact"}]}}`
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
				identity := r.Header.Get("Authorization")
				calls = append(calls, identity)
				got, _ := io.ReadAll(r.Body)
				if string(got) != body {
					t.Error("failover changed search body")
				}
				status, payload := 401, `{"error":{"code":"unauthorized"}}`
				if identity == "Bearer good" {
					status, payload = 200, `{"output":"GOOD-SEARCH"}`
					if conflict {
						owner, _ := p.sessionAccount(r.Context(), "admin", "search-session", "")
						_, _, _ = p.switchSessionAccount(r.Context(), "admin", "search-session", owner, "concurrent-owner")
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, appServerAuxiliaryTestRequest("/v1/alpha/search", body, "test-client-key"))
			if len(calls) != 2 || calls[0] != "Bearer bad" || calls[1] != "Bearer good" {
				t.Fatalf("failover wrong: %v", calls)
			}
			if conflict && (w.Code != 409 || strings.Contains(w.Body.String(), "GOOD-SEARCH")) {
				t.Fatal("CAS conflict leaked search result")
			}
			if !conflict && (w.Code != 200 || !strings.Contains(w.Body.String(), "GOOD-SEARCH")) {
				t.Fatalf("search failover failed %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAppServerAuxiliaryNativeDispatchGuardTracksCurrentAccount(t *testing.T) {
	p := appServerAuxiliaryTestProxy(t)
	// A fake pool enables the production authorization plumbing. Injected
	// transport below performs its final guard as the real native helper does.
	p.CodexTransport = &Proxy{NativeHTTP: &gatewayNativePool{}}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	var admitted string
	ctx := context.WithValue(t.Context(), gatewayNativeDispatchAuthorizerKey{}, gatewayNativeDispatchAuthorizer(func(_ context.Context, current Account) error { admitted = current.ID; return nil }))
	ctx = withInferenceMode(ctx, inferenceCodexCLI)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://chatgpt.com/backend-api/codex/alpha/search", strings.NewReader(`{}`))
	_, err = p.doAccountRequest(&http.Client{}, req, account, func(_ *http.Client, r *http.Request) (*http.Response, error) {
		if err := checkGatewayNativeDispatch(r.Context()); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
	}, nil)
	if err != nil || admitted != account.ID {
		t.Fatal("native guard lost current account")
	}
}
