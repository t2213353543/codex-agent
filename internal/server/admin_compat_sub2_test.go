package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func sub2TestRequest(t *testing.T, h http.Handler, method, path string, body any, status int) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	request.Header.Set("x-api-key", webTestKey)
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request)
	if w.Code != status {
		t.Fatalf("%s %s got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("compat response may be cached")
	}
	var result map[string]any
	if json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("invalid envelope %s", w.Body.String())
	}
	if status == 200 && result["code"] != float64(0) {
		t.Fatalf("invalid success code %v", result)
	}
	return result
}
func sub2Fixture(name string) map[string]any {
	return map[string]any{"name": name, "platform": "openai", "type": "oauth", "credentials": map[string]any{"access_token": "access-" + name, "refresh_token": "refresh-" + name, "id_token": "id-secret-" + name, "chatgpt_account_id": "workspace-" + name, "client_id": "fixture-client", "expires_at": 2000000000}, "extra": map[string]any{"email": name, "team_oauth_verified": true, "team_oauth_verified_workspace_id": "workspace-" + name}, "priority": 3, "concurrency": 10}
}
func sub2Data(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	v, ok := result["data"].(map[string]any)
	if !ok {
		t.Fatalf("data is not object: %v", result)
	}
	return v
}
func sub2ID(v map[string]any) string { return fmt.Sprintf("%.0f", v["id"]) }

func TestSub2CompatAccountLifecycle(t *testing.T) { runSub2CompatAccountLifecycle(t, webTestSetup) }
func TestPGSub2CompatAccountLifecycle(t *testing.T) {
	runSub2CompatAccountLifecycle(t, adminPGTestProxy)
}
func runSub2CompatAccountLifecycle(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	h := webHandler(p, t.TempDir())
	base := "/api/v1/admin"
	group := sub2Data(t, sub2TestRequest(t, h, "POST", base+"/groups", map[string]any{"name": "sync-group", "platform": "openai"}, 200))
	proxy := sub2Data(t, sub2TestRequest(t, h, "POST", base+"/proxies", map[string]any{"name": "sync-proxy", "protocol": "http", "host": "127.0.0.1", "port": 8765, "password": "private-proxy-password"}, 200))
	fixture := sub2Fixture("member@example.invalid")
	fixture["group_ids"] = []any{group["id"]}
	fixture["proxy_id"] = proxy["id"]
	fixture["expires_at"] = int64(2100000000)
	account := sub2Data(t, sub2TestRequest(t, h, "POST", base+"/accounts", fixture, 200))
	id := sub2ID(account)
	path := base + "/accounts/" + id
	if account["id"].(float64) <= 0 || account["id"].(float64) > 9007199254740991 {
		t.Fatal("ID not JavaScript safe")
	}
	raw, _ := json.Marshal(account)
	if strings.Contains(string(raw), "access-member") || strings.Contains(string(raw), "id-secret-") {
		t.Fatalf("account DTO leaked credentials: %s", raw)
	}
	if account["expires_at"] != float64(2100000000) {
		t.Fatal("management expiry missing")
	}
	account = sub2Data(t, sub2TestRequest(t, h, "PUT", path, map[string]any{"priority": 1, "status": "active"}, 200))
	if account["priority"] != float64(1) {
		t.Fatal("post-create priority finalize ignored")
	}
	list := sub2Data(t, sub2TestRequest(t, h, "GET", base+"/accounts?search=member%40example.invalid&page_size=100", nil, 200))
	if list["total"] != float64(1) {
		t.Fatalf("sync search failed: %v", list)
	}
	item := list["items"].([]any)[0].(map[string]any)
	if sub2ID(item) != id || item["extra"].(map[string]any)["team_oauth_verified"] != true {
		t.Fatal("ID or verification metadata lost")
	}
	account = sub2Data(t, sub2TestRequest(t, h, "POST", path+"/schedulable", map[string]bool{"schedulable": false}, 200))
	if account["schedulable"] != false || account["status"] != "active" {
		t.Fatal("scheduling state conflated with status")
	}
	nativeID, err := p.compatResolveAccount(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	native, err := p.Store.Get(nativeID)
	if err != nil || native.Enabled {
		t.Fatal("pause did not reach scheduler")
	}
	if native.Priority != -1 {
		t.Fatal("Sub2API small-first priority not mapped to native large-first priority")
	}
	if native.ExpiresAt != 2000000000 {
		t.Fatal("management expiry replaced token expiry")
	}
	account = sub2Data(t, sub2TestRequest(t, h, "PUT", path, map[string]any{"credentials": map[string]any{"access_token": "rotated-access", "chatgpt_account_id": "workspace-member@example.invalid"}}, 200))
	native, err = p.Store.Get(nativeID)
	if err != nil || native.AccessToken != "rotated-access" || native.RefreshToken != "refresh-member@example.invalid" || native.Enabled {
		t.Fatal("credential replacement lost refresh token or scheduling state")
	}
	if native.ExpiresAt != 0 {
		t.Fatal("new opaque access token inherited old token expiry")
	}
	sub2TestRequest(t, h, "PUT", path, map[string]any{"credentials": map[string]any{"access_token": "must-rollback"}, "extra": []string{"invalid"}}, 400)
	native, _ = p.Store.Get(nativeID)
	if native.AccessToken != "rotated-access" {
		t.Fatal("invalid metadata partially committed credentials")
	}
	sub2TestRequest(t, h, "POST", path+"/clear-error", map[string]any{}, 200)
	sub2TestRequest(t, h, "POST", path+"/schedulable", map[string]bool{"schedulable": true}, 200)
	native, _ = p.Store.Get(nativeID)
	if !native.Enabled {
		t.Fatal("resume not persisted")
	}
	account = sub2Data(t, sub2TestRequest(t, h, "PUT", path, map[string]any{"proxy_id": 0, "group_ids": []int64{}}, 200))
	if account["proxy_id"] != nil || len(account["group_ids"].([]any)) != 0 {
		t.Fatal("explicit relation clear ignored")
	}
	sub2TestRequest(t, h, "DELETE", path, nil, 200)
	sub2TestRequest(t, h, "GET", path, nil, 404)
	sub2TestRequest(t, h, "DELETE", base+"/proxies/"+sub2ID(proxy), nil, 200)
	sub2TestRequest(t, h, "DELETE", base+"/groups/"+sub2ID(group), nil, 200)
}

func TestSub2CompatDataAndUnsupportedSettings(t *testing.T)   { runSub2CompatData(t, webTestSetup) }
func TestPGSub2CompatDataAndUnsupportedSettings(t *testing.T) { runSub2CompatData(t, adminPGTestProxy) }
func runSub2CompatData(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	h := webHandler(p, t.TempDir())
	base := "/api/v1/admin/accounts"
	row := sub2Fixture("export-account")
	sub2TestRequest(t, h, "POST", base, row, 200)
	data := sub2Data(t, sub2TestRequest(t, h, "GET", base+"/data?include_proxies=false&search=export-account", nil, 200))
	accounts := data["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatal("export missed account")
	}
	item := accounts[0].(map[string]any)
	credentials := item["credentials"].(map[string]any)
	if credentials["refresh_token"] != "refresh-export-account" || credentials["id_token"] != "id-secret-export-account" {
		t.Fatal("explicit export did not preserve credentials")
	}
	item["name"] = "import-account"
	credentials["access_token"] = "import-access"
	invalid := sub2Fixture("unsupported-provider")
	invalid["platform"] = "anthropic"
	result := sub2Data(t, sub2TestRequest(t, h, "POST", base+"/data", map[string]any{"data": map[string]any{"type": "sub2api-data", "version": 1, "proxies": []any{}, "accounts": []any{item, invalid}}}, 200))
	if result["account_created"] != float64(1) || result["account_failed"] != float64(1) {
		t.Fatalf("partial import counts wrong: %v", result)
	}
	bad := sub2Fixture("bad-model-map")
	bad["credentials"].(map[string]any)["model_mapping"] = map[string]string{"a": "a"}
	sub2TestRequest(t, h, "POST", base, bad, 400)
	bad = sub2Fixture("bad-provider")
	bad["platform"] = "anthropic"
	sub2TestRequest(t, h, "POST", base, bad, 400)
	sub2TestRequest(t, h, "POST", base+"/data", map[string]any{"data": map[string]any{"accounts": []any{}}}, 400)
	sub2TestRequest(t, h, "POST", "/api/v1/admin/proxies", map[string]any{"name": "unsupported", "protocol": "https", "host": "proxy.invalid", "port": 443}, 400)
}

func TestSub2CompatSSETruth(t *testing.T)   { runSub2CompatSSE(t, webTestSetup) }
func TestPGSub2CompatSSETruth(t *testing.T) { runSub2CompatSSE(t, adminPGTestProxy) }
func runSub2CompatSSE(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	h := webHandler(p, t.TempDir())
	var fail atomic.Bool
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer access-stream-account" {
			t.Error("test did not use account credentials")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if fail.Load() {
			fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"fixture failure\"}}}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"works\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	account := sub2Data(t, sub2TestRequest(t, h, "POST", "/api/v1/admin/accounts", sub2Fixture("stream-account"), 200))
	path := "/api/v1/admin/accounts/" + sub2ID(account) + "/test"
	for _, failure := range []bool{false, true} {
		fail.Store(failure)
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"model_id":"fixture-model","mode":"default","prompt":""}`))
		r.Header.Set("x-api-key", webTestKey)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
			t.Fatalf("SSE unavailable: %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"type":"test_complete"`) || !strings.Contains(w.Body.String(), fmt.Sprintf(`"success":%t`, !failure)) {
			t.Fatalf("SSE terminal wrong: %s", w.Body.String())
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("expected real upstream requests, got %d", requests.Load())
	}
}

func TestSub2CompatPrivacyAndPortableProxy(t *testing.T) {
	runSub2PrivacyAndPortableProxy(t, webTestSetup)
}
func TestPGSub2CompatPrivacyAndPortableProxy(t *testing.T) {
	runSub2PrivacyAndPortableProxy(t, adminPGTestProxy)
}
func runSub2PrivacyAndPortableProxy(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	h := webHandler(p, t.TempDir())
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "PATCH" || r.URL.Path != "/backend-api/settings/user" || r.URL.Query().Get("value") != "false" {
			t.Errorf("wrong privacy request: %s %s", r.Method, r.URL.String())
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	p.BaseURL = upstream.URL + "/backend-api/codex"
	base := "/api/v1/admin"
	fixture := sub2Fixture("private-roundtrip")
	fixture["credentials"].(map[string]any)["accessToken"] = "unrecognized-secret-alias"
	fixture["credentials"].(map[string]any)["nested"] = map[string]string{"token": "nested-secret"}
	created := sub2Data(t, sub2TestRequest(t, h, "POST", base+"/accounts", fixture, 200))
	path := base + "/accounts/" + sub2ID(created)
	privacy := sub2Data(t, sub2TestRequest(t, h, "POST", path+"/set-privacy", map[string]any{}, 200))
	if calls.Load() != 1 || privacy["extra"].(map[string]any)["privacy_mode"] != "training_off" {
		t.Fatal("privacy success was not tied to upstream acknowledgement")
	}
	list := sub2TestRequest(t, h, "GET", base+"/accounts?search=private-roundtrip", nil, 200)
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "unrecognized-secret-alias") || strings.Contains(string(raw), "nested-secret") {
		t.Fatalf("credential alias leaked: %s", raw)
	}
	item := sub2Data(t, list)["items"].([]any)[0].(map[string]any)
	if item["credentials_status"].(map[string]any)["has_access_token"] != true || item["credentials"].(map[string]any)["chatgpt_account_id"] != "workspace-private-roundtrip" {
		t.Fatal("list/detail credential metadata disagrees")
	}
	proxy := sub2Data(t, sub2TestRequest(t, h, "POST", base+"/proxies", map[string]any{"name": "portable-proxy", "protocol": "socks5", "host": "127.0.0.1", "port": 23456, "username": "proxy-user", "password": "proxy-secret"}, 200))
	sub2TestRequest(t, h, "PUT", path, map[string]any{"proxy_id": proxy["id"]}, 200)
	bundle := sub2Data(t, sub2TestRequest(t, h, "GET", base+"/accounts/data?ids="+sub2ID(created), nil, 200))
	if len(bundle["proxies"].([]any)) != 1 || bundle["proxies"].([]any)[0].(map[string]any)["password"] != "proxy-secret" {
		t.Fatal("portable proxy credentials missing")
	}
	sub2TestRequest(t, h, "DELETE", path, nil, 200)
	sub2TestRequest(t, h, "DELETE", base+"/proxies/"+sub2ID(proxy), nil, 200)
	restored := sub2Data(t, sub2TestRequest(t, h, "POST", base+"/accounts/data", map[string]any{"data": bundle}, 200))
	if restored["account_created"] != float64(1) || restored["proxy_created"] != float64(1) {
		t.Fatalf("portable restore failed: %v", restored)
	}
	restoredList := sub2Data(t, sub2TestRequest(t, h, "GET", base+"/accounts?search=private-roundtrip", nil, 200))
	account := restoredList["items"].([]any)[0].(map[string]any)
	if account["proxy_id"] == nil || account["proxy_id"] == proxy["id"] {
		t.Fatal("portable proxy_key was not rebound to a new local ID")
	}
	sub2TestRequest(t, h, "POST", base+"/accounts/"+sub2ID(account)+"/schedulable", map[string]any{"schedulable": nil}, 400)
}
