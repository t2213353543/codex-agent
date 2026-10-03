package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func appServerIdentityBridgeFixture(t *testing.T) *Proxy {
	t.Helper()
	p := appServerAuxiliaryTestProxy(t)
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.Identity = &managedAccountIdentity{UserID: "upstream-user", WorkspaceID: a.AccountID, PlanType: "plus", Email: "private@example.test"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	settings, _ := p.Settings.Get()
	settings.StickySessionsEnabled = false
	settings.WebsocketEnabled = true
	_ = p.Settings.Set(settings)
	return p
}

func TestAppServerClientIdentityUsesAccountMetadataAndNoCredentialExport(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	req := httptest.NewRequest("GET", appServerClientIdentityPath, nil)
	req.Header.Set("Authorization", "Bearer test-client-key")
	out := httptest.NewRecorder()
	p.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("identity failed %d %s", out.Code, out.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(out.Body.Bytes(), &body)
	if body["chatgpt_user_id"] != "upstream-user" || body["chatgpt_account_id"] != "test-account" || body["chatgpt_plan_type"] != "plus" {
		t.Fatalf("identity not sourced from account: %v", body)
	}
	for _, secret := range []string{"test-upstream-token", "test-client-key", "private@example.test", "refresh_token"} {
		if strings.Contains(out.Body.String(), secret) {
			t.Fatal("unnecessary identity data exposed")
		}
	}
	if owner, _ := p.sessionAccount(t.Context(), "admin", appServerClientIdentitySession, ""); owner == "" {
		t.Fatal("identity owner not retained with sticky disabled")
	}
	unauthorized := httptest.NewRecorder()
	p.ServeHTTP(unauthorized, httptest.NewRequest("GET", appServerClientIdentityPath, nil))
	if unauthorized.Code != 401 {
		t.Fatal("unauthenticated identity exposed")
	}
}

func TestAppServerDelegatedModelAndBackendKeepOneOwner(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	identity := httptest.NewRequest("GET", appServerClientIdentityPath, nil)
	identity.Header.Set("Authorization", "Bearer test-client-key")
	p.ServeHTTP(httptest.NewRecorder(), identity)
	raw, _ := json.Marshal(map[string]any{"access_token": "higher-priority-token", "account_id": "other-account", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("other", raw, AccountDefaults{Concurrency: 2, Priority: 100}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	modelCalls, backendCalls := 0, 0
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Fatal("delegated request moved to unrelated pool account")
		}
		content := "application/json"
		response := `{"enabled":true}`
		if r.URL.Path == "/backend-api/codex/responses" {
			modelCalls++
			content = "text/event-stream"
			response = "data: " + `{"type":"response.completed","response":{"id":"resp_delegated","status":"completed","output":[]}}` + "\n\n"
		} else if r.URL.Path == "/backend-api/wham/settings/user" {
			backendCalls++
		} else {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{content}}, Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})}
	request := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"fixture","stream":false,"input":[{"type":"additional_tools","role":"developer","tools":[]}]}`))
	request.Header.Set("Authorization", "Bearer test-client-key")
	request.Header.Set("ChatGPT-Account-Id", "test-account")
	request.Header.Set("Session-Id", "model-conversation")
	out := httptest.NewRecorder()
	p.ServeHTTP(out, request)
	if out.Code != 200 {
		t.Fatalf("delegated model %d %s", out.Code, out.Body)
	}
	backend := httptest.NewRequest("GET", "/backend-api/wham/settings/user", nil)
	backend.Header.Set("Authorization", "Bearer test-client-key")
	out = httptest.NewRecorder()
	p.ServeHTTP(out, backend)
	if out.Code != 200 || modelCalls != 1 || backendCalls != 1 {
		t.Fatalf("backend path failed %d", out.Code)
	}
	request.Header.Set("ChatGPT-Account-Id", "other-account")
	out = httptest.NewRecorder()
	p.ServeHTTP(out, request)
	if out.Code != 409 || modelCalls != 1 {
		t.Fatal("claimed account changed delegated owner")
	}
}

func TestAppServerBackendMCPPreservesWireWithoutClientAccountHeader(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	identity := httptest.NewRequest("GET", appServerClientIdentityPath, nil)
	identity.Header.Set("Authorization", "Bearer test-client-key")
	p.ServeHTTP(httptest.NewRecorder(), identity)
	input := `{ "jsonrpc":"2.0", "id":9007199254740993, "method":"tools/call", "params":{"name":"fixture","arguments":{"opaque":"<>&"}} }`
	result := "event: message\ndata: " + `{"jsonrpc":"2.0","id":9007199254740993,"result":{"content":[{"type":"text","text":"fixture-result"}]}}` + "\n\n"
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != input || r.Header.Get("Mcp-Session-Id") != "client-mcp-session" || r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("X-Openai-Product-Sku") != "codex" {
			t.Fatal("MCP input contract changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "Mcp-Session-Id": []string{"upstream-mcp-session"}}, Body: io.NopCloser(strings.NewReader(result)), Request: r}, nil
	})}
	req := httptest.NewRequest("POST", "/backend-api/ps/mcp", strings.NewReader(input))
	req.Header.Set("Authorization", "Bearer test-client-key")
	req.Header.Set("Mcp-Session-Id", "client-mcp-session")
	req.Header.Set("X-Openai-Product-Sku", "codex")
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	p.ServeHTTP(out, req)
	if out.Code != 200 || out.Body.String() != result || out.Header().Get("Mcp-Session-Id") != "upstream-mcp-session" {
		t.Fatalf("MCP output changed %d %s", out.Code, out.Body)
	}
}

func TestAppServerBackendObservedWritePathsStayOwnerBoundAndByteExact(t *testing.T) {
	p := appServerIdentityBridgeFixture(t)
	identity := httptest.NewRequest("GET", appServerClientIdentityPath, nil)
	identity.Header.Set("Authorization", "Bearer test-client-key")
	p.ServeHTTP(httptest.NewRecorder(), identity)
	cases := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/backend-api/wham/accounts/send_add_credits_nudge_email", `{"opaque":9007199254740993}`},
		{http.MethodPost, "/backend-api/wham/tasks", `{"model":"fixture","prompt":"opaque"}`},
		{http.MethodDelete, "/backend-api/public/plugins/workspace/plugin-1", ""},
	}
	p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer test-upstream-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Fatal("observed backend write moved delegated owner")
		}
		if r.URL.Path != cases[0].path && r.URL.Path != cases[1].path && r.URL.Path != cases[2].path {
			t.Fatalf("unexpected backend write path %s", r.URL.Path)
		}
		for _, item := range cases {
			if r.URL.Path == item.path && string(body) != item.body {
				t.Fatalf("backend write body changed for %s: %q", item.path, body)
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Request: r}, nil
	})}
	for _, item := range cases {
		req := httptest.NewRequest(item.method, item.path, strings.NewReader(item.body))
		req.Header.Set("Authorization", "Bearer test-client-key")
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		p.ServeHTTP(out, req)
		if out.Code != http.StatusOK {
			t.Fatalf("backend write %s %s returned %d: %s", item.method, item.path, out.Code, out.Body)
		}
	}
}
