package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// These tests exercise the public management handler as an API client. Every
// credential and account is synthetic; refresh and proxy probes terminate at
// local fixtures and never contact a real provider.
type adminKeyAPIFixture struct {
	p       *Proxy
	handler http.Handler
	group   Group
	key     APIKey
	proxy   ProxyEntry
	account Account
	calls   atomic.Int64
}

func newAdminKeyAPIFixture(t *testing.T) *adminKeyAPIFixture {
	t.Helper()
	f := &adminKeyAPIFixture{p: newTestProxy(t, 1)}
	f.p.setAdminKey(webTestKey)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected local upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"access_token": "synthetic-refreshed-access", "refresh_token": "synthetic-refreshed-refresh", "expires_in": 3600})
	}))
	t.Cleanup(upstream.Close)
	f.p.TokenURL = upstream.URL + "/oauth/token"
	f.p.Client = upstream.Client()
	var err error
	f.group, err = f.p.Groups.Create("fixture-group", "unassociated route fixture")
	if err != nil {
		t.Fatal(err)
	}
	f.key, err = f.p.Keys.Create("fixture-key", []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	port, _ := strconv.Atoi(u.Port())
	f.proxy, err = f.p.Proxies.Create(ProxyEntry{Name: "fixture-proxy", Type: "http", Host: u.Hostname(), Port: port, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	f.account, err = f.p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	f.handler = f.p.adminRoutes()
	return f
}

func adminKeyAPIJSON(t *testing.T, response *httptest.ResponseRecorder, status int) map[string]json.RawMessage {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP %d, expected %d; response=%s", response.Code, status, response.Body.String())
	}
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("management response headers: %v", response.Header())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result == nil {
		t.Fatalf("management response is not a JSON object: %s", response.Body.String())
	}
	if status >= 400 {
		var failure struct{ Type, Code, Message string }
		if json.Unmarshal(result["error"], &failure) != nil || failure.Type != "proxy_error" || failure.Code == "" || failure.Message == "" {
			t.Fatalf("missing structured management error: %s", response.Body.String())
		}
	}
	return result
}

func adminKeyAPICall(t *testing.T, handler http.Handler, method, path string, body any, status int) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	return adminKeyAPIJSON(t, webTestRequest(handler, method, path, string(raw), "Bearer "+webTestKey), status)
}

func TestAdminKeyAPIAllRoutesAuthorization(t *testing.T) {
	routes := []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/session", `{"admin_key":"$admin"}`, 503}, // Cookie login requires Redis; bearer management does not.
		{"GET", "/api/session", "", 200},
		{"DELETE", "/api/session", "", 200},
		{"GET", "/api/overview", "", 200},
		{"GET", "/api/diagnostics", "", 200},
		{"GET", "/api/audit-logs", "", 200},
		{"GET", "/api/accounts", "", 200},
		{"POST", "/api/accounts", `{"name":"new-account","credentials":{"access_token":"synthetic-new-access"}}`, 200},
		{"GET", "/api/accounts/$account", "", 200},
		{"PUT", "/api/accounts/$account", `{"notes":"changed through bearer API"}`, 200},
		{"DELETE", "/api/accounts/$account", "", 200},
		{"GET", "/api/accounts/$account/requests", "", 200},
		{"POST", "/api/accounts/refresh", `{"id":"$account"}`, 200},
		{"POST", "/api/accounts/import/preview", `{"name":"preview-account","credentials":{"access_token":"synthetic-preview-access"}}`, 200},
		{"POST", "/api/accounts/batch", `{"items":[{"id":"$account","patch":{"notes":"batch through bearer API"}}]}`, 200},
		{"GET", "/api/groups", "", 200},
		{"POST", "/api/groups", `{"name":"created-group","description":"created through bearer API"}`, 200},
		{"PUT", "/api/groups/$group", `{"name":"renamed-group","description":"updated"}`, 200},
		{"DELETE", "/api/groups/$group", "", 200},
		{"GET", "/api/keys", "", 200},
		{"POST", "/api/keys", `{"name":"created-key","groups":["$group"]}`, 200},
		{"PUT", "/api/keys/$key", `{"enabled":false}`, 200},
		{"DELETE", "/api/keys/$key", "", 200},
		{"GET", "/api/proxies", "", 200},
		{"POST", "/api/proxies", `{"name":"created-proxy","type":"http","host":"127.0.0.1","port":8080}`, 200},
		{"PUT", "/api/proxies/$proxy", `{"enabled":false}`, 200},
		{"DELETE", "/api/proxies/$proxy", "", 200},
		{"POST", "/api/proxies/test", `{"id":"$proxy"}`, 200},
		{"GET", "/api/logs", "", 200},
		{"GET", "/api/settings", "", 200},
		{"POST", "/api/settings", `{"websocket_enabled":true}`, 200},
		{"POST", "/api/settings/rotate-key", `{"request_id":"route-contract-rotation"}`, 200},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			f := newAdminKeyAPIFixture(t)
			replace := strings.NewReplacer("$account", f.account.ID, "$group", f.group.ID, "$key", f.key.ID, "$proxy", f.proxy.ID, "$admin", webTestKey)
			path, body := replace.Replace(route.path), replace.Replace(route.body)
			for _, token := range []string{"", "invalid-admin-key", f.key.Key} {
				deniedBody := body
				if path == "/api/session" && route.method == http.MethodPost {
					encoded, _ := json.Marshal(map[string]string{"admin_key": token})
					deniedBody = string(encoded)
				}
				response := webTestRequest(f.handler, route.method, path, deniedBody, "Bearer "+token)
				adminKeyAPIJSON(t, response, http.StatusUnauthorized)
			}
			if f.calls.Load() != 0 {
				t.Fatal("rejected management request contacted upstream")
			}
			response := webTestRequest(f.handler, route.method, path, body, "Bearer "+webTestKey)
			result := adminKeyAPIJSON(t, response, route.status)
			if path == "/api/proxies/test" {
				var diagnostic proxyDiagnostic
				_ = json.Unmarshal(response.Body.Bytes(), &diagnostic)
				if diagnostic.OK || diagnostic.HTTPCode != 407 || diagnostic.Authentication != "failed" || f.calls.Load() != 1 {
					t.Fatalf("local proxy probe result: %s", response.Body.String())
				}
			}
			if path == "/api/accounts/refresh" {
				stored, err := f.p.Store.Get(f.account.ID)
				if err != nil || stored.AccessToken != "synthetic-refreshed-access" || stored.RefreshToken != "synthetic-refreshed-refresh" || f.calls.Load() != 1 {
					t.Fatal("authorized refresh failed to persist local provider result")
				}
			}
			if path == "/api/accounts/import/preview" && string(result["dry_run"]) != "true" {
				t.Fatal("preview endpoint did not force dry_run")
			}
		})
	}
}

func TestAdminKeyAPIHTTPErrorContract(t *testing.T) {
	f := newAdminKeyAPIFixture(t)
	for _, test := range []struct{ path, allow string }{
		{"/api/session", "DELETE, GET, HEAD, POST"},
		{"/api/overview", "GET, HEAD"},
		{"/api/diagnostics", "GET, HEAD"},
		{"/api/audit-logs", "GET, HEAD"},
		{"/api/accounts", "GET, HEAD, POST"},
		{"/api/accounts/" + f.account.ID, "DELETE, GET, HEAD, PUT"},
		{"/api/accounts/" + f.account.ID + "/requests", "GET, HEAD"},
		{"/api/accounts/import/preview", "POST"},
		{"/api/groups", "GET, HEAD, POST"},
		{"/api/groups/" + f.group.ID, "DELETE, PUT"},
		{"/api/keys", "GET, HEAD, POST"},
		{"/api/keys/" + f.key.ID, "DELETE, PUT"},
		{"/api/proxies", "GET, HEAD, POST"},
		{"/api/proxies/" + f.proxy.ID, "DELETE, PUT"},
		{"/api/proxies/test", "DELETE, POST, PUT"},
		{"/api/logs", "GET, HEAD"},
		{"/api/settings", "GET, HEAD, POST"},
		{"/api/settings/rotate-key", "POST"},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := webTestRequest(f.handler, http.MethodPatch, test.path, `{}`, "Bearer "+webTestKey)
			adminKeyAPIJSON(t, response, http.StatusMethodNotAllowed)
			if response.Header().Get("Allow") != test.allow {
				t.Fatalf("Allow=%q, expected %q", response.Header().Get("Allow"), test.allow)
			}
		})
	}
	for _, path := range []string{"/api/unknown", "/api/accounts/not/a/route"} {
		adminKeyAPIJSON(t, webTestRequest(f.handler, http.MethodGet, path, "", "Bearer "+webTestKey), http.StatusNotFound)
		adminKeyAPIJSON(t, webTestRequest(f.handler, http.MethodGet, path, "", "Bearer "+f.key.Key), http.StatusUnauthorized)
	}
	for _, body := range []string{`[]`, `null`, `{`, `{"unknown":true}`, `{"websocket_enabled":true} {}`} {
		adminKeyAPIJSON(t, webTestRequest(f.handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey), http.StatusBadRequest)
	}
}

func TestAdminKeyAPIStorageErrorsAreStructured(t *testing.T) {
	for _, test := range []struct {
		name, method, path, body string
		status                   int
		breakStore               func(*Proxy, string)
	}{
		{"groups", "GET", "/api/groups", "", 500, func(p *Proxy, path string) { p.Groups.store.path = path }},
		{"keys", "GET", "/api/keys", "", 500, func(p *Proxy, path string) { p.Keys.store.path = path }},
		{"proxies", "GET", "/api/proxies", "", 500, func(p *Proxy, path string) { p.Proxies.store.path = path }},
		{"accounts", "GET", "/api/accounts", "", 500, func(p *Proxy, path string) { p.Store.Path = path }},
		{"settings", "GET", "/api/settings", "", 500, func(p *Proxy, path string) { p.Settings.store.path = path; p.Settings.loaded = false }},
		{"missing settings", "POST", "/api/settings", `{"websocket_enabled":true}`, 503, func(p *Proxy, _ string) { p.Settings = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdminKeyAPIFixture(t)
			// Reading a directory as a store reliably fails without file damage.
			privatePath := t.TempDir()
			test.breakStore(f.p, privatePath)
			response := webTestRequest(f.handler, test.method, test.path, test.body, "Bearer "+webTestKey)
			adminKeyAPIJSON(t, response, test.status)
			if strings.Contains(response.Body.String(), privatePath) || strings.Contains(response.Body.String(), webTestKey) {
				t.Fatal("storage error disclosed a local path or admin credential")
			}
		})
	}
}

func TestAdminKeyAPILifecycleAndPartialSettings(t *testing.T) {
	f := newAdminKeyAPIFixture(t)
	call := func(method, path string, body any, status int) map[string]json.RawMessage {
		return adminKeyAPICall(t, f.handler, method, path, body, status)
	}
	created := call("POST", "/api/groups", map[string]any{"name": "lifecycle-group", "description": "initial"}, 200)
	var groupID string
	_ = json.Unmarshal(created["id"], &groupID)
	if groupID == "" {
		t.Fatal("created group omitted ID")
	}
	call("PUT", "/api/groups/"+groupID, map[string]any{"version": json.RawMessage(created["version"]), "name": "lifecycle-updated", "description": "changed"}, 200)
	groupList := call("GET", "/api/groups", nil, 200)
	if !strings.Contains(string(groupList["groups"]), `"name":"lifecycle-updated"`) {
		t.Fatal("group update did not appear in list")
	}
	proxyResult := call("POST", "/api/proxies", map[string]any{"name": "lifecycle-proxy", "type": "http", "host": "127.0.0.1", "port": 8888, "password": "synthetic-lifecycle-password"}, 200)
	var proxyID string
	_ = json.Unmarshal(proxyResult["id"], &proxyID)
	if string(proxyResult["has_password"]) != "true" || len(proxyResult["password"]) != 0 {
		t.Fatal("proxy create must report password presence without exposing password")
	}
	call("PUT", "/api/proxies/"+proxyID, map[string]any{"version": json.RawMessage(proxyResult["version"]), "password": "", "name": "lifecycle-proxy-updated"}, 200)
	storedProxy, err := f.p.Proxies.GetForAdmin(proxyID)
	if err != nil || storedProxy.Password != "synthetic-lifecycle-password" {
		t.Fatal("partial proxy update discarded password")
	}
	keyResult := call("POST", "/api/keys", map[string]any{"name": "lifecycle-key", "groups": []string{groupID}}, 200)
	var keyID, gatewayKey string
	_ = json.Unmarshal(keyResult["id"], &keyID)
	_ = json.Unmarshal(keyResult["key"], &gatewayKey)
	if keyID == "" || !strings.HasPrefix(gatewayKey, "sk-") {
		t.Fatal("key create did not return one-time key")
	}
	updatedKey := call("PUT", "/api/keys/"+keyID, map[string]any{"version": json.RawMessage(keyResult["version"]), "enabled": false}, 200)
	if strings.Contains(string(mustAdminKeyAPIJSON(t, updatedKey)), gatewayKey) {
		t.Fatal("updating key configuration disclosed existing key material")
	}
	if _, allowed := f.p.Keys.Lookup(gatewayKey); allowed {
		t.Fatal("disabled gateway key retained access")
	}
	keyList := call("GET", "/api/keys", nil, 200)
	if strings.Contains(string(keyList["keys"]), gatewayKey) {
		t.Fatal("key list disclosed complete key material")
	}
	importBody := map[string]any{"name": "lifecycle-account", "credentials": map[string]string{"access_token": "synthetic-lifecycle-access", "refresh_token": "synthetic-lifecycle-refresh"}, "defaults": map[string]any{"groups": []string{groupID}}}
	preview := call("POST", "/api/accounts/import/preview", importBody, 200)
	if string(preview["added"]) != "1" || string(preview["dry_run"]) != "true" {
		t.Fatal("valid import preview did not predict one addition")
	}
	if _, err := f.p.Store.Get("lifecycle-account"); err == nil {
		t.Fatal("preview persisted an account")
	}
	imported := call("POST", "/api/accounts", importBody, 200)
	if string(imported["added"]) != "1" || string(imported["failed"]) != "0" {
		t.Fatal("account import report did not reflect persistence")
	}
	a, err := f.p.Store.GetMetadata("lifecycle-account")
	if err != nil {
		t.Fatal(err)
	}
	call("PUT", "/api/accounts/"+a.ID, map[string]any{"version": a.Version, "name": "lifecycle-account-renamed", "notes": "preserve me", "proxies": []string{proxyID}}, 200)
	call("PUT", "/api/accounts/"+a.ID, map[string]any{"version": a.Version, "notes": "stale overwrite"}, 409)
	detail := call("GET", "/api/accounts/"+a.ID, nil, 200)
	var account accountSummary
	_ = json.Unmarshal(detail["account"], &account)
	if account.ID != a.ID || account.Name != "lifecycle-account-renamed" || account.Notes != "preserve me" || account.Version <= a.Version {
		t.Fatal("stable account identity/optimistic concurrency contract failed")
	}
	if strings.Contains(string(detail["account"]), "synthetic-lifecycle") || strings.Contains(string(detail["account"]), `"access_token"`) {
		t.Fatal("account detail exposed credentials")
	}
	batch := call("POST", "/api/accounts/batch", map[string]any{"items": []any{map[string]any{"id": a.ID, "version": account.Version, "patch": map[string]any{"enabled": false}}, map[string]any{"id": "missing-account", "patch": map[string]any{"notes": "must fail"}}}}, 200)
	if string(batch["succeeded"]) != "1" || string(batch["failed"]) != "1" {
		t.Fatal("batch did not distinguish independent success and missing account")
	}
	settings := call("GET", "/api/settings", nil, 200)
	call("POST", "/api/settings", map[string]any{"settings_version": json.RawMessage(settings["settings_version"]), "admin_api": false, "builtin_prompt": "fixed policy", "websocket_enabled": true}, 200)
	changed := call("GET", "/api/settings", nil, 200)
	call("POST", "/api/settings", map[string]any{"settings_version": json.RawMessage(changed["settings_version"]), "builtin_prompt_enabled": false}, 200)
	call("POST", "/api/settings", map[string]any{"settings_version": json.RawMessage(settings["settings_version"]), "websocket_enabled": false}, 409)
	current := call("GET", "/api/settings", nil, 200)
	if string(current["admin_api"]) != "false" || string(current["websocket_enabled"]) != "true" || string(current["builtin_prompt"]) != `"fixed policy"` || string(current["builtin_prompt_enabled"]) != "false" || string(current["admin_key"]) != `""` {
		t.Fatal("partial settings discarded fields, disclosed admin key, or disabled management access")
	}
	if _, _, allowed := f.p.identifyKey(webTestKey); allowed {
		t.Fatal("admin_api=false did not disable admin key's gateway access")
	}
	// Management remains usable when the admin key's inference access is off.
	call("DELETE", "/api/accounts/"+a.ID, nil, 200)
	call("GET", "/api/accounts/"+a.ID, nil, 404)
	call("DELETE", "/api/keys/"+keyID, nil, 200)
	call("DELETE", "/api/proxies/"+proxyID, nil, 200)
	call("DELETE", "/api/groups/"+groupID, nil, 200)
	if _, err := f.p.Proxies.GetForAdmin(proxyID); err == nil {
		t.Fatal("deleted proxy remains in management storage")
	}
	if _, err := f.p.Groups.Get(groupID); err == nil {
		t.Fatal("deleted group remains in management storage")
	}
	if f.calls.Load() != 0 {
		t.Fatal("CRUD lifecycle unexpectedly contacted upstream")
	}
}

func mustAdminKeyAPIJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAdminKeyAPIRotationRevokesPreviousBearer(t *testing.T) {
	for _, path := range []string{"/api/settings/rotate-key", "/api/settings"} {
		t.Run(path, func(t *testing.T) {
			f := newAdminKeyAPIFixture(t)
			body := `{"request_id":"synthetic-rotation"}`
			if path == "/api/settings" {
				body = `{"request_id":"synthetic-rotation","regenerate_key":true}`
			}
			rotated := adminKeyAPIJSON(t, webTestRequest(f.handler, "POST", path, body, "Bearer "+webTestKey), 200)
			var fresh, requestID string
			_ = json.Unmarshal(rotated["admin_key"], &fresh)
			_ = json.Unmarshal(rotated["request_id"], &requestID)
			if !strings.HasPrefix(fresh, "ca_") || fresh == webTestKey || requestID != "synthetic-rotation" {
				t.Fatal("rotation response did not provide a fresh administrator credential")
			}
			for _, target := range []string{"/api/accounts", "/api/settings", "/api/session"} {
				adminKeyAPIJSON(t, webTestRequest(f.handler, "GET", target, "", "Bearer "+webTestKey), 401)
				response := webTestRequest(f.handler, "GET", target, "", "Bearer "+fresh)
				adminKeyAPIJSON(t, response, 200)
				if strings.Contains(response.Body.String(), fresh) {
					t.Fatal("ordinary management read disclosed current admin key")
				}
			}
			if f.p.adminKey() != fresh {
				t.Fatal("rotated credential did not become active")
			}
		})
	}
}
