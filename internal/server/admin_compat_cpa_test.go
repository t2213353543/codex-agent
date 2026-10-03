package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func cpaTestRequest(t *testing.T, handler http.Handler, method, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	raw := cpaRaw(body)
	r := webTestRequest(handler, method, path, string(raw), "Bearer "+webTestKey)
	if r.Code != status {
		t.Fatalf("%s %s status=%d want=%d: %s", method, path, r.Code, status, r.Body.String())
	}
	return r
}

func cpaTestUpload(t *testing.T, h http.Handler, files map[string]string, defaults string, status int) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range files {
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(part, value)
	}
	if defaults != "" {
		_ = writer.WriteField("default_websockets", defaults)
	}
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Authorization", "Bearer "+webTestKey)
	req.Header.Set("X-Management-Key", webTestKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != status {
		t.Fatalf("multipart status %d want %d: %s", resp.Code, status, resp.Body.String())
	}
	return resp
}

func cpaTestPayload(token string) map[string]any {
	return map[string]any{"type": "codex", "email": "synthetic@example.invalid", "account_id": "workspace-synthetic", "access_token": token, "refresh_token": "refresh-synthetic", "id_token": "synthetic-id-token", "expired": "2033-05-18T03:33:20Z", "priority": 5, "disabled": false, "note": "CPA imported", "custom": map[string]any{"keep": true}}
}

func TestCPACompatLifecycle(t *testing.T)   { exerciseCPACompatLifecycle(t, webTestSetup) }
func TestPGCPACompatLifecycle(t *testing.T) { exerciseCPACompatLifecycle(t, adminPGTestProxy) }

func exerciseCPACompatLifecycle(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	h := p.compatRoutes()
	payload := cpaTestPayload("first-access-synthetic")
	cpaTestUpload(t, h, map[string]string{"synthetic@example.invalid.json": string(cpaRaw(payload))}, "", 200)
	var listing struct {
		Files []map[string]json.RawMessage `json:"files"`
	}
	list := cpaTestRequest(t, h, "GET", "/v0/management/auth-files?name=synthetic%40example.invalid.json", nil, 200)
	if err := json.Unmarshal(list.Body.Bytes(), &listing); err != nil || len(listing.Files) != 1 {
		t.Fatalf("list %s", list.Body.String())
	}
	id, index := cpaString(listing.Files[0], "id"), cpaString(listing.Files[0], "auth_index")
	if id == "" || len(index) != 16 {
		t.Fatalf("missing stable identity: %s", list.Body.String())
	}
	for _, secret := range []string{"first-access-synthetic", "refresh-synthetic", "synthetic-id-token"} {
		if strings.Contains(list.Body.String(), secret) {
			t.Fatalf("list leaked %s", secret)
		}
	}
	row, err := p.compatGetAccount(context.Background(), id)
	if err != nil || row.Account.Priority != 5 || row.Account.ExpiresAt != 2000000000 || row.Account.AccountID != "workspace-synthetic" {
		t.Fatalf("native import %#v %v", row.Account, err)
	}
	if len(row.Record.CPA["access_token"]) != 0 || len(row.Record.CPA["refresh_token"]) != 0 {
		t.Fatal("duplicate secret metadata")
	}
	download := cpaTestRequest(t, h, "GET", "/v0/management/auth-files/download?name=synthetic%40example.invalid.json", nil, 200)
	if !strings.Contains(download.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("download is not attachment")
	}
	var downloaded map[string]json.RawMessage
	_ = json.Unmarshal(download.Body.Bytes(), &downloaded)
	if cpaString(downloaded, "id_token") != "synthetic-id-token" || cpaString(downloaded, "access_token") != "first-access-synthetic" || len(downloaded["custom"]) == 0 {
		t.Fatal("auth file round trip lost metadata")
	}
	cpaTestRequest(t, h, "PATCH", "/v0/management/auth-files/status", map[string]any{"name": id, "auth_index": "wrong", "disabled": true}, 404)
	row, _ = p.compatGetAccount(context.Background(), id)
	if !row.Account.Enabled {
		t.Fatal("wrong auth_index changed account")
	}
	cpaTestRequest(t, h, "PATCH", "/v0/management/auth-files/status", map[string]any{"name": id, "auth_index": index, "disabled": true}, 200)
	cpaTestRequest(t, h, "PATCH", "/v0/management/auth-files/fields", map[string]any{"name": id, "priority": 9, "note": "changed", "custom.value": 7}, 200)
	row, _ = p.compatGetAccount(context.Background(), id)
	if row.Account.Enabled || row.Account.Priority != 9 || row.Account.Notes != "changed" {
		t.Fatal("fields not reflected in native account")
	}
	payload["access_token"] = "rotated-access-synthetic"
	delete(payload, "disabled")
	delete(payload, "priority")
	delete(payload, "note")
	cpaTestRequest(t, h, "POST", "/v0/management/auth-files?name=synthetic%40example.invalid.json", payload, 200)
	rotated, _ := p.compatGetAccount(context.Background(), id)
	if rotated.Account.AccessToken != "rotated-access-synthetic" || rotated.Account.Enabled || rotated.Account.Priority != 9 || compatAuthIndex(rotated.Account.ID) != index {
		t.Fatal("reimport lost identity or management state")
	}
	cpaTestRequest(t, h, "PATCH", "/v0/management/auth-files/fields", map[string]any{"name": id, "access_token": "fields-access-synthetic", "expired": "2036-07-18T13:20:00Z", "disabled": false}, 200)
	row, _ = p.compatGetAccount(context.Background(), id)
	if row.Account.AccessToken != "fields-access-synthetic" || !row.Account.Enabled || row.Account.ExpiresAt != 2100000000 {
		t.Fatalf("credential mutation failed: %#v", row.Account)
	}
	settings, err := p.settingsValue()
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range []map[string]any{
		{"name": id, "headers": map[string]string{"X-Test": "value"}},
		{"name": id, "prefix": "team"},
		{"name": id, "websockets": !settings.WebsocketEnabled},
		{"name": id, "models": []string{"made-up-model"}},
		{"name": id, "priority": 7, "disabled": "not-bool"},
	} {
		cpaTestRequest(t, h, "PATCH", "/v0/management/auth-files/fields", patch, 400)
	}
	current, _ := p.compatGetAccount(context.Background(), id)
	if current.Account.Version != row.Account.Version {
		t.Fatal("invalid patch mutated native account")
	}
	cpaTestRequest(t, h, "DELETE", "/v0/management/auth-files?name=synthetic%40example.invalid.json", nil, 200)
	cpaTestRequest(t, h, "GET", "/v0/management/auth-files/download?name=synthetic%40example.invalid.json", nil, 404)
	list = cpaTestRequest(t, h, "GET", "/v0/management/auth-files?name=synthetic%40example.invalid.json", nil, 200)
	if !strings.Contains(list.Body.String(), `"files":[]`) {
		t.Fatal("deleted auth is still listed")
	}
}

func TestCPACompatMultipartPartialAndValidation(t *testing.T) {
	p, _ := webTestSetup(t)
	h := p.compatRoutes()
	resp := cpaTestUpload(t, h, map[string]string{"valid.json": string(cpaRaw(cpaTestPayload("valid-access"))), "bad.json": `{"type":"claude","access_token":"bad"}`}, "", 207)
	if !strings.Contains(resp.Body.String(), `"uploaded":1`) || !strings.Contains(resp.Body.String(), `"status":"partial"`) {
		t.Fatalf("partial response %s", resp.Body.String())
	}
	cpaTestUpload(t, h, map[string]string{"duplicate.json": `{"type":"codex","access_token":"one","access_token":"two"}`}, "", 400)
	cpaTestRequest(t, h, "POST", "/v0/management/auth-files?name=..%2Fbad.json", cpaTestPayload("traversal-access"), 400)
	cpaTestRequest(t, h, "PATCH", "/v0/management/auth-files/status", map[string]any{"name": "valid.json"}, 400)
	cpaTestRequest(t, h, "DELETE", "/v0/management/auth-files", map[string]any{"names": []string{"valid.json", "missing.json"}}, 207)
}

func TestCPACompatAPICallProxyAndModels(t *testing.T) {
	p, _ := webTestSetup(t)
	h := p.compatRoutes()
	a := compatStoreFixture(t, p, "api-call")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+a.Account.AccessToken {
			t.Error("token placeholder not substituted")
		}
		if strings.Contains(r.URL.Path, "models") {
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-test","display_name":"Test"}]}`)
			return
		}
		if r.Method != "POST" || r.Header.Get("X-Test") != "request-header" {
			t.Error("request method/header not forwarded")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"hello":true}` {
			t.Error("data string not forwarded")
		}
		w.Header().Set("X-Upstream", "synthetic")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":"quota"}`)
	}))
	defer upstream.Close()
	resp := cpaTestRequest(t, h, "POST", "/v0/management/api-call", map[string]any{"authIndex": compatAuthIndex(a.Account.ID), "ensureFreshToken": true, "method": "POST", "url": upstream.URL, "header": map[string]string{"Authorization": "Bearer $TOKEN$", "X-Test": "request-header"}, "data": `{"hello":true}`}, 200)
	var result struct {
		Status int         `json:"status_code"`
		Header http.Header `json:"header"`
		Body   string      `json:"body"`
	}
	_ = json.Unmarshal(resp.Body.Bytes(), &result)
	if result.Status != 429 || result.Body != `{"error":"quota"}` || result.Header.Get("X-Upstream") != "synthetic" {
		t.Fatalf("wrong response envelope: %s", resp.Body.String())
	}
	p.BaseURL = upstream.URL
	catalog := cpaTestRequest(t, h, "GET", "/v0/management/auth-files/models?name="+url.QueryEscape(a.Account.ID), nil, 200)
	if !strings.Contains(catalog.Body.String(), `"id":"gpt-test"`) {
		t.Fatal("models are not based on account upstream")
	}
	cpaTestRequest(t, h, "POST", "/v0/management/api-call", map[string]any{"method": "GET", "url": upstream.URL, "header": map[string]string{"Authorization": "Bearer $TOKEN$"}}, 400)
	cpaTestRequest(t, h, "POST", "/v0/management/api-call", map[string]any{"auth_index": "unknown", "method": "GET", "url": upstream.URL}, 404)
	cpaTestRequest(t, h, "POST", "/v0/management/api-call", map[string]any{"method": "GET", "url": "ftp://example.invalid/file"}, 400)
	proxyHit := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit = true
		_, _ = io.WriteString(w, `{"proxied":true}`)
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	var port int
	_, _ = fmt.Sscan(u.Port(), &port)
	entry, err := p.Proxies.Create(ProxyEntry{Name: "CPA Test", Type: "http", Host: u.Hostname(), Port: port, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	cpaTestRequest(t, h, "PATCH", "/v0/management/auth-files/fields", map[string]any{"name": a.Account.ID, "proxy_url": proxy.URL}, 200)
	row, _ := p.compatGetAccount(context.Background(), a.Account.ID)
	if len(row.Account.Proxies) != 1 || row.Account.Proxies[0] != entry.ID {
		t.Fatal("proxy not bound natively")
	}
	cpaTestRequest(t, h, "POST", "/v0/management/api-call", map[string]any{"auth_index": compatAuthIndex(a.Account.ID), "method": "GET", "url": "http://synthetic.example.invalid/"}, 200)
	if !proxyHit {
		t.Fatal("account proxy bypassed")
	}
}

func TestCPACompatAPICallCancellationAndResponseLimit(t *testing.T) {
	p, _ := webTestSetup(t)
	h := p.compatRoutes()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", cpaResponseLimit+1)), cpaResponseLimit+1)
			return
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("POST", "/v0/management/api-call", bytes.NewReader(cpaRaw(map[string]any{"method": "GET", "url": upstream.URL}))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+webTestKey)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != 502 {
		t.Fatalf("canceled call %d", response.Code)
	}
	cpaTestRequest(t, h, "POST", "/v0/management/api-call", map[string]any{"method": "GET", "url": upstream.URL + "/large"}, 502)
}
