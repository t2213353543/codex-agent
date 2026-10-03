package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func appServerIdentityAuditHydrate(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, appServerClientIdentityPath, nil)
	r.Header.Set("Authorization", "Bearer test-client-key")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func appServerIdentityAuditAddOther(t *testing.T, p *Proxy) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"access_token": "audit-other-token", "account_id": "audit-other-account", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("audit-other", raw, AccountDefaults{Concurrency: 2, Priority: 200}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("audit-other", func(a *Account) error {
		a.Identity = &managedAccountIdentity{UserID: "audit-other-user", WorkspaceID: a.AccountID, PlanType: "pro"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
}

func TestAppServerIdentityAuditHydratedRequestsWithoutAccountHeader(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	if w := appServerIdentityAuditHydrate(t, p); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	appServerIdentityAuditAddOther(t, p)
	var identities []string
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		identities = append(identities, r.Header.Get("Authorization"))
		result := `{"text":"backend"}`
		if strings.HasSuffix(r.URL.Path, "/responses") {
			result = `data: {"type":"response.completed","response":{"id":"same-owner","status":"completed","output":[]}}` + "\n\n"
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(result))}, nil
	})}
	for _, path := range []string{"/backend-api/codex/responses", "/backend-api/ps/mcp"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"fixture","stream":false,"input":[{"type":"additional_tools","role":"developer","tools":[]}]}`))
		r.Header.Set("Authorization", "Bearer test-client-key")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
		}
	}
	if len(identities) != 2 || identities[0] != "Bearer test-upstream-token" || identities[1] != identities[0] {
		t.Fatalf("hydrated model and MCP used different owners without account header: %v", identities)
	}
}

func TestAppServerIdentityAuditDelegatedAuxiliaryDoesNotCrossOwner(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	if w := appServerIdentityAuditHydrate(t, p); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	appServerIdentityAuditAddOther(t, p)
	var identities []string
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		identities = append(identities, r.Header.Get("Authorization"))
		status, result := 401, `{"error":{"code":"unauthorized"}}`
		if strings.Contains(r.Header.Get("Authorization"), "audit-other") {
			status, result = 200, `{"output":"different-owner"}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(result))}, nil
	})}
	r := httptest.NewRequest(http.MethodPost, "/backend-api/codex/alpha/search", strings.NewReader(`{"id":"owner-search","commands":{"search_query":[{"q":"fixture"}]}}`))
	r.Header.Set("Authorization", "Bearer test-client-key")
	r.Header.Set("ChatGPT-Account-Id", "test-account")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if len(identities) != 1 || w.Code != 503 || strings.Contains(w.Body.String(), "different-owner") {
		t.Fatalf("delegated auxiliary crossed owner after 401: status=%d identities=%v", w.Code, identities)
	}
}

func TestAppServerIdentityAuditInvalidMetadataDoesNotPoisonLogin(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Identity = nil; a.Priority = 1000; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	w := appServerIdentityAuditHydrate(t, p)
	if w.Code != 503 {
		t.Fatalf("invalid identity status=%d", w.Code)
	}
	if owner, _ := p.sessionAccount(t.Context(), "admin", appServerClientIdentitySession, ""); owner != "" {
		t.Fatal("failed metadata hydration permanently bound a unusable identity")
	}
	appServerIdentityAuditAddOther(t, p)
	w = appServerIdentityAuditHydrate(t, p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "audit-other-user") {
		t.Fatalf("valid lower priority identity never selected: %d %s", w.Code, w.Body.String())
	}
}

type appServerIdentityAuditBody struct {
	io.Reader
	once       sync.Once
	beforeRead func()
}

func (r *appServerIdentityAuditBody) Read(p []byte) (int, error) {
	r.once.Do(r.beforeRead)
	return r.Reader.Read(p)
}

func TestAppServerIdentityAuditMCPRevalidatesAfterClientBody(t *testing.T) {
	for _, mutation := range []string{"deleted", "disabled", "groups", "rotated"} {
		t.Run(mutation, func(t *testing.T) {
			p := appServerIdentityBridgeFixture(t)
			key, _ := gatewaySessionAuthorizationFixture(t, p)
			r := httptest.NewRequest(http.MethodGet, appServerClientIdentityPath, nil)
			r.Header.Set("Authorization", "Bearer "+key.Key)
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			writer := gatewayKeyAuthorizationWriter(t, p, key)
			calls := 0
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"result":{}}`))}, nil
			})}
			body := &appServerIdentityAuditBody{Reader: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`), beforeRead: func() { gatewayKeyAuthorizationMutate(t, writer, key, mutation) }}
			r = httptest.NewRequest(http.MethodPost, "/backend-api/ps/mcp", body)
			r.Header.Set("Authorization", "Bearer "+key.Key)
			w = httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if calls != 0 || w.Code < 400 {
				t.Fatalf("revoked MCP operation reached upstream: mutation=%s calls=%d status=%d", mutation, calls, w.Code)
			}
			if p.inFlight("test") != 0 {
				t.Fatal("revoked MCP operation retained account lease")
			}
		})
	}
}

func TestAppServerIdentityAuditExplicitRehydrateLeavesExistingThreadOwner(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	if w := appServerIdentityAuditHydrate(t, p); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	old, _ := p.sessionAccount(t.Context(), "admin", appServerClientIdentitySession, "")
	if _, err := p.sessionAccount(t.Context(), "admin", "old-conversation", old); err != nil {
		t.Fatal(err)
	}
	appServerIdentityAuditAddOther(t, p)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	w := appServerIdentityAuditHydrate(t, p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "audit-other-user") {
		t.Fatalf("explicit identity reload failed: %d %s", w.Code, w.Body.String())
	}
	current, _ := p.sessionAccount(t.Context(), "admin", appServerClientIdentitySession, "")
	thread, _ := p.sessionAccount(t.Context(), "admin", "old-conversation", "")
	if current == old || thread != old {
		t.Fatal("rehydration changed an existing encrypted conversation owner")
	}
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("old conversation reached new hydrated account")
		return nil, nil
	})}
	r := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(`{"model":"fixture","stream":false,"input":[]}`))
	r.Header.Set("Authorization", "Bearer test-client-key")
	r.Header.Set("Session-Id", "old-conversation")
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code < 400 {
		t.Fatal("old thread accepted a new delegated owner")
	}
}

func TestAppServerIdentityAuditUnhydratedProviderRoutingUnchanged(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	appServerIdentityAuditAddOther(t, p)
	called := false
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		called = true
		if appServerDelegatedOwner(r.Context()) != "" || r.Header.Get("Authorization") != "Bearer audit-other-token" {
			t.Error("ordinary API key was given a delegated owner without hydration")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"normal","status":"completed","output":[]}}` + "\n\n"))}, nil
	})}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"fixture","stream":false,"input":[{"type":"additional_tools","role":"developer","tools":[]}]}`))
	r.Header.Set("Authorization", "Bearer test-client-key")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if !called || w.Code != 200 {
		t.Fatalf("ordinary route changed: %d %s", w.Code, w.Body.String())
	}
}
