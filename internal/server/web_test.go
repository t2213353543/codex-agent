package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const webTestKey = "test-client-key-with-at-least-32-characters"

func webTestSetup(t *testing.T) (*Proxy, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>Account management</title>"), 0600); err != nil {
		t.Fatal(err)
	}
	proxy := newTestProxy(t, 3)
	proxy.AdminKey = webTestKey
	return proxy, webHandler(proxy, dir)
}

func webTestRequest(handler http.Handler, method, path, body, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestWebAPIRequiresBearer(t *testing.T) {
	_, handler := webTestSetup(t)
	for _, path := range []string{"/api/overview", "/api/accounts", "/api/groups", "/api/keys", "/api/proxies", "/api/logs", "/api/settings", "/api/unknown"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			for _, auth := range []string{"", "Basic " + webTestKey, "Bearer incorrect"} {
				response := webTestRequest(handler, method, path, `{}`, auth)
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("%s %s auth=%q returned %d: %s", method, path, auth, response.Code, response.Body.String())
				}
				if response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("API authentication response can be cached")
				}
			}
		}
	}
	proxy, handler := webTestSetup(t)
	proxy.setAdminKey("")
	response := webTestRequest(handler, http.MethodGet, "/api/accounts", "", "Bearer ")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("an unset admin key bypassed authentication: %d", response.Code)
	}
}

func TestWebAccountListRedactsCredentials(t *testing.T) {
	proxy, handler := webTestSetup(t)
	if err := proxy.Store.Import("one", []byte(`{"access_token":"private-access-token","refresh_token":"private-refresh-token","account_id":"private-account-id","client_id":"private-client-id","expires_at":1900000000}`)); err != nil {
		t.Fatal(err)
	}
	response := webTestRequest(handler, http.MethodGet, "/api/accounts", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("list returned %d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"private-access-token", "private-refresh-token", "private-account-id", "private-client-id", "access_token", "account_id", "client_id"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("account summary exposed %q", secret)
		}
	}
	var result struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var account map[string]any
	for _, candidate := range result.Accounts {
		if candidate["name"] == "one" {
			account = candidate
		}
	}
	if account == nil || account["enabled"] != true {
		t.Fatalf("incorrect list response: %+v", result)
	}
	if account["expires_at"] != float64(1900000000) || account["has_refresh_token"] != true {
		t.Fatalf("unexpected account summary: %+v", account)
	}
	for _, key := range []string{"group_names", "usage", "status", "quota_percent", "concurrency"} {
		if _, ok := account[key]; !ok {
			t.Fatalf("account summary missing %q: %+v", key, account)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("unsafe summary headers: %v", response.Header())
	}
}

func TestWebAccountCRUD(t *testing.T) {
	proxy, handler := webTestSetup(t)
	for _, request := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/accounts", `{"name":"first","credentials":{"access_token":"first-token"}}`},
		{http.MethodPost, "/api/accounts", `{"accounts":[{"name":"second","credentials":{"tokens":{"access_token":"second-token"}}},{"name":"third","credentials":{"access_token":"third-token"}}],"defaults":{"concurrency":5,"priority":7,"weight":3,"quota":10000,"window_days":7}}`},
	} {
		response := webTestRequest(handler, request.method, request.path, request.body, "Bearer "+webTestKey)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"error"`) {
			t.Fatalf("%s %s failed: %d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	_, accounts, err := proxy.Store.List()
	if err != nil || len(accounts) != 4 { // fixture account "test" plus first/second/third
		t.Fatalf("imports did not persist: %+v %v", accounts, err)
	}
	var third Account
	for _, account := range accounts {
		if account.Name == "third" {
			third = account
		}
	}
	if third.Concurrency != 5 || third.Priority != 7 || third.Weight != 3 || third.Quota != 10000 || third.WindowDays != 7 || third.WindowStart == 0 {
		t.Fatalf("batch defaults were not applied: %+v", third)
	}
	response := webTestRequest(handler, http.MethodPut, "/api/accounts/second", `{"enabled":false,"concurrency":9}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("update failed: %d %s", response.Code, response.Body.String())
	}
	second, err := proxy.Store.Get("second")
	if err != nil || second.Enabled || second.Concurrency != 9 || len(second.Groups) != 0 {
		t.Fatalf("update did not persist: %+v %v", second, err)
	}
	response = webTestRequest(handler, http.MethodDelete, "/api/accounts/second", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("delete failed: %d %s", response.Code, response.Body.String())
	}
	response = webTestRequest(handler, http.MethodDelete, "/api/accounts/missing", "", "Bearer "+webTestKey)
	if response.Code != http.StatusNotFound {
		t.Fatalf("deleting a missing account returned %d", response.Code)
	}
}

func TestWebAccountInvalidBodies(t *testing.T) {
	proxy, handler := webTestSetup(t)
	for _, body := range []string{
		``, `null`, `[]`, `{`, `{}`, `{"name":"x","credentials":null}`,
		`{"name":"x","credentials":{"access_token":"token"},"unknown":true}`,
		`{"name":"x","credentials":{"access_token":"token"}} {}`,
		`{"name":"x","credentials":{"access_token":"` + strings.Repeat("x", 33<<20) + `"}}`,
		`{"accounts":[],"defaults":{}}`,
		`{"accounts":[{"name":"x","credentials":{}}]}`,
	} {
		response := webTestRequest(handler, http.MethodPost, "/api/accounts", body, "Bearer "+webTestKey)
		if response.Code == http.StatusOK {
			var result struct {
				Imported int `json:"imported"`
				Failed   int `json:"failed"`
			}
			if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Imported != 0 || result.Failed < 1 {
				t.Fatalf("invalid body accepted: %s", response.Body.String())
			}
		} else if response.Code != http.StatusBadRequest && response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("invalid body with %d bytes returned %d: %s", len(body), response.Code, response.Body.String())
		}
	}
	_, accounts, err := proxy.Store.List()
	if err != nil || len(accounts) != 1 { // only the fixture account
		t.Fatalf("invalid request modified accounts: %+v %v", accounts, err)
	}
	response := webTestRequest(handler, http.MethodPut, "/api/accounts/x", `{"concurrency":0}`, "Bearer "+webTestKey)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid concurrency accepted: %d", response.Code)
	}
	response = webTestRequest(handler, http.MethodPut, "/api/accounts/x", `{"groups":["nonexistent"]}`, "Bearer "+webTestKey)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown group accepted: %d", response.Code)
	}
}

func TestWebGroupCRUD(t *testing.T) {
	proxy, handler := webTestSetup(t)
	response := webTestRequest(handler, http.MethodGet, "/api/groups", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("group list failed: %d %s", response.Code, response.Body.String())
	}
	response = webTestRequest(handler, http.MethodPost, "/api/groups", `{"name":"Plus 账号","description":"测试分组"}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"g_`) {
		t.Fatalf("group create failed: %d %s", response.Code, response.Body.String())
	}
	var created Group
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.Name != "Plus 账号" {
		t.Fatalf("bad create response: %s", response.Body.String())
	}
	response = webTestRequest(handler, http.MethodPut, "/api/groups/"+created.ID, `{"name":"Plus","description":"改"}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"Plus"`) {
		t.Fatalf("group update failed: %d %s", response.Code, response.Body.String())
	}
	// Legacy storage cannot atomically detach across JSON files. It must reject
	// the operation and preserve every entity; PostgreSQL tests cover atomic detach.
	if err := proxy.Store.ImportWith("acc", []byte(`{"access_token":"t"}`), AccountDefaults{Groups: []string{created.ID}}); err != nil {
		t.Fatal(err)
	}
	key, err := proxy.Keys.Create("k", []string{created.ID})
	if err != nil {
		t.Fatal(err)
	}
	response = webTestRequest(handler, http.MethodDelete, "/api/groups/"+created.ID, "", "Bearer "+webTestKey)
	if response.Code != http.StatusConflict {
		t.Fatalf("legacy associated group deletion returned %d", response.Code)
	}
	if _, err := proxy.Groups.Get(created.ID); err != nil {
		t.Fatal("failed deletion removed group")
	}
	account, err := proxy.Store.Get("acc")
	if err != nil || len(account.Groups) != 1 {
		t.Fatal("failed deletion changed accounts")
	}
	keys, _ := proxy.Keys.List()
	if len(keys) != 1 || len(keys[0].Groups) != 1 || keys[0].Key != key.Key {
		t.Fatal("failed deletion changed keys")
	}
	if err := proxy.Store.UpdateAccount("acc", func(a *Account) error { a.Groups = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Keys.RemoveGroup(created.ID); err != nil {
		t.Fatal(err)
	}
	response = webTestRequest(handler, http.MethodDelete, "/api/groups/"+created.ID, "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("unbound group deletion failed: %d", response.Code)
	}

}

func TestWebKeyCRUDAndMasking(t *testing.T) {
	proxy, handler := webTestSetup(t)
	response := webTestRequest(handler, http.MethodPost, "/api/groups", `{"name":"team"}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("group create failed: %d %s", response.Code, response.Body.String())
	}
	var group Group
	if err := json.Unmarshal(response.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	response = webTestRequest(handler, http.MethodPost, "/api/keys", `{"name":"team-a","groups":["`+group.ID+`"]}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("key create failed: %d %s", response.Code, response.Body.String())
	}
	var created APIKey
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || !strings.HasPrefix(created.Key, "sk-") || len(created.Key) < 40 {
		t.Fatalf("bad key create response: %s", response.Body.String())
	}
	response = webTestRequest(handler, http.MethodGet, "/api/keys", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("key list failed: %d", response.Code)
	}
	if strings.Contains(response.Body.String(), created.Key) {
		t.Fatal("key list exposed the full key material")
	}
	if !strings.Contains(response.Body.String(), "sk-") || !strings.Contains(response.Body.String(), "****") {
		t.Fatal("key list did not return a masked key")
	}
	response = webTestRequest(handler, http.MethodPut, "/api/keys/"+created.ID, `{"enabled":false,"name":"renamed"}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("key update failed: %d %s", response.Code, response.Body.String())
	}
	if _, ok := proxy.Keys.Lookup(created.Key); ok {
		t.Fatal("disabled key still accepted by the gateway")
	}
	response = webTestRequest(handler, http.MethodPost, "/api/keys", `{"name":"bad","groups":["nope"]}`, "Bearer "+webTestKey)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown group accepted for key: %d", response.Code)
	}
	response = webTestRequest(handler, http.MethodDelete, "/api/keys/"+created.ID, "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("key delete failed: %d %s", response.Code, response.Body.String())
	}
}

func TestWebProxyCRUD(t *testing.T) {
	proxy, handler := webTestSetup(t)
	response := webTestRequest(handler, http.MethodPost, "/api/proxies", `{"name":"US-01","type":"socks5","host":"127.0.0.1","port":1080}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("valid proxy create failed: %d %s", response.Code, response.Body.String())
	}
	for _, body := range []string{
		`{"name":"bad","type":"ftp","host":"127.0.0.1","port":21}`,
		`{"name":"bad-port","type":"http","host":"127.0.0.1","port":70000}`,
		`{"name":"bad","type":"socks5","host":"","port":1080}`,
	} {
		response = webTestRequest(handler, http.MethodPost, "/api/proxies", body, "Bearer "+webTestKey)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid proxy accepted: %d %s", response.Code, response.Body.String())
		}
	}
	proxies, err := proxy.Proxies.List()
	if err != nil || len(proxies) != 1 {
		t.Fatalf("invalid proxies were accepted: %+v %v", proxies, err)
	}
	response = webTestRequest(handler, http.MethodPut, "/api/proxies/"+proxies[0].ID, `{"enabled":false,"port":1081}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy update failed: %d %s", response.Code, response.Body.String())
	}
	if _, ok := proxy.Proxies.Get(proxies[0].ID); ok {
		t.Fatal("disabled proxy still resolvable for requests")
	}
	response = webTestRequest(handler, http.MethodDelete, "/api/proxies/"+proxies[0].ID, "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy delete failed: %d %s", response.Code, response.Body.String())
	}
}

func TestWebSettingsAndAdminAPI(t *testing.T) {
	proxy, handler := webTestSetup(t)
	response := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), webTestKey) || !strings.Contains(response.Body.String(), `"admin_key_masked"`) || !strings.Contains(response.Body.String(), `"admin_api":true`) {
		t.Fatalf("settings response incomplete: %d %s", response.Code, response.Body.String())
	}
	response = webTestRequest(handler, http.MethodPost, "/api/settings", `{"admin_api":false}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("settings update failed: %d", response.Code)
	}
	if _, _, ok := proxy.identifyKey(webTestKey); ok {
		t.Fatal("admin key still accepted on the gateway after admin api was disabled")
	}
	// The admin console keeps working while the gateway switch is off.
	response = webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("admin console blocked by admin api switch: %d", response.Code)
	}
	// Regenerate produces a working new key and invalidates the old one.
	response = webTestRequest(handler, http.MethodPost, "/api/settings", `{"regenerate":true,"admin_api":true}`, "Bearer "+webTestKey)
	if response.Code != http.StatusBadRequest || proxy.adminKey() != webTestKey {
		t.Fatal("mixed key rotation must fail without invalidating the key")
	}
	response = webTestRequest(handler, http.MethodPost, "/api/settings", `{"admin_api":true}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatal("separate settings update failed")
	}
	response = webTestRequest(handler, http.MethodPost, "/api/settings", `{"regenerate":true}`, "Bearer "+webTestKey)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"admin_key":"ca_`) {
		t.Fatalf("key regeneration failed: %d %s", response.Code, response.Body.String())
	}
	var regenerated struct {
		AdminKey string `json:"admin_key"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &regenerated)
	if regenerated.AdminKey == "" || regenerated.AdminKey == webTestKey {
		t.Fatal("regenerated key missing or unchanged")
	}
	if proxy.adminKey() != regenerated.AdminKey {
		t.Fatal("regenerated key was not applied to the running proxy")
	}
	response = webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	if response.Code != http.StatusUnauthorized {
		t.Fatal("old admin key still accepted after regeneration")
	}
	response = webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+regenerated.AdminKey)
	if response.Code != http.StatusOK {
		t.Fatal("regenerated key rejected")
	}
}

func TestWebStaticAndUnknownRoutes(t *testing.T) {
	_, handler := webTestSetup(t)
	response := webTestRequest(handler, http.MethodGet, "/", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Account management") {
		t.Fatalf("root did not serve the frontend: %d %s", response.Code, response.Body.String())
	}
	for _, header := range []string{"X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
		if response.Header().Get(header) == "" {
			t.Fatalf("missing static security header: %s", header)
		}
	}
	for _, path := range []string{"/api/unknown", "/unknown", "/accounts", "/private/accounts.json", "/v1/unknown"} {
		response := webTestRequest(handler, http.MethodGet, path, "", "Bearer "+webTestKey)
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "Account management") {
			t.Fatalf("unknown route %s fell back to frontend: %d %s", path, response.Code, response.Body.String())
		}
	}
	response = webTestRequest(handler, http.MethodPost, "/", "", "")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("static endpoint accepted POST: %d", response.Code)
	}
	proxy := newTestProxy(t, 1)
	proxy.AdminKey = webTestKey
	response = webTestRequest(webHandler(proxy, t.TempDir()), http.MethodGet, "/", "", "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing frontend did not return 503: %d", response.Code)
	}
}

func TestWebRefreshWithoutRefreshToken(t *testing.T) {
	proxy, handler := webTestSetup(t)
	accountTestImport(t, proxy.Store, "one", "private-access-token")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("refresh without a refresh token reached the upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	proxy.Client, proxy.TokenURL = upstream.Client(), upstream.URL
	response := webTestRequest(handler, http.MethodPost, "/api/accounts/refresh", `{"name":"one"}`, "Bearer "+webTestKey)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "no refresh token") {
		t.Fatalf("missing refresh token was not reported: %d %s", response.Code, response.Body.String())
	}
	if calls.Load() != 0 || strings.Contains(response.Body.String(), "private-access-token") {
		t.Fatal("failed refresh sent or exposed credentials")
	}
	account, err := proxy.Store.Get("one")
	if err != nil || account.AccessToken != "private-access-token" {
		t.Fatalf("failed refresh modified credentials: %+v %v", account, err)
	}
}

func TestWebStorageErrorsHidePrivatePaths(t *testing.T) {
	proxy, handler := webTestSetup(t)
	accountTestImport(t, proxy.Store, "one", "token")
	lock := proxy.Store.Path + ".lock"
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-accountLockStale - time.Minute)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	response := webTestRequest(handler, http.MethodPut, "/api/accounts/one", `{"enabled":true}`, "Bearer "+webTestKey)
	var decoded any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	encodedPath, _ := json.Marshal(filepath.Dir(proxy.Store.Path))
	if strings.Contains(string(raw), string(encodedPath[1:len(encodedPath)-1])) {
		t.Fatalf("storage error exposed a private path: %s", response.Body.String())
	}
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("storage lock failure returned %d instead of 500: %s", response.Code, response.Body.String())
	}
}

func TestWebOverviewAndLogs(t *testing.T) {
	proxy, handler := webTestSetup(t)
	// Simulate a few requests through the metrics.
	now := time.Now().Unix()
	for i := int64(0); i < 5; i++ {
		proxy.Metrics.Record(RequestRecord{Time: now - 120 + i, Account: "acc", KeyName: "k1", Model: "m", Status: "ok", InputTokens: 100, OutputTokens: 50, CachedTokens: 30, LatencyMs: 500})
	}
	proxy.Metrics.Record(RequestRecord{Time: now - 60, Account: "acc", KeyName: "k1", Model: "m", Status: "error", Error: "upstream returned HTTP 429"})
	response := webTestRequest(handler, http.MethodGet, "/api/overview?from="+itoa(now-300)+"&to="+itoa(now+60)+"&step=60", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("overview failed: %d %s", response.Code, response.Body.String())
	}
	var overview struct {
		Cards  map[string]any `json:"cards"`
		Series []SeriesPoint  `json:"series"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.Cards["requests"] != float64(6) || overview.Cards["tokens"] != float64(750) || overview.Cards["errors"] != float64(1) {
		t.Fatalf("wrong overview cards: %+v", overview.Cards)
	}
	if len(overview.Series) == 0 {
		t.Fatal("overview returned no series")
	}
	response = webTestRequest(handler, http.MethodGet, "/api/logs?page=1&page_size=10", "", "Bearer "+webTestKey)
	if response.Code != http.StatusOK {
		t.Fatalf("logs failed: %d %s", response.Code, response.Body.String())
	}
	var logs LogPage
	if err := json.Unmarshal(response.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	if logs.Total != 6 || len(logs.Items) != 6 {
		t.Fatalf("wrong log page: %+v", logs)
	}
	response = webTestRequest(handler, http.MethodGet, "/api/logs?status=error", "", "Bearer "+webTestKey)
	if err := json.Unmarshal(response.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	if logs.Total != 1 || logs.Items[0].Error != "upstream returned HTTP 429" {
		t.Fatalf("wrong filtered log page: %+v", logs)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
