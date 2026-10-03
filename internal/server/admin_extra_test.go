package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdminProxyDiagnosticStages(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		err         error
		ok          bool
		stage, code string
	}{
		{"IPv4", 200, "fl=x\nip=203.0.113.8\n", nil, true, "complete", ""},
		{"IPv6", 200, "ip=2001:db8::1\n", nil, true, "complete", ""},
		{"forbidden", 403, "ip=203.0.113.8\n", nil, false, "http", "http_status"},
		{"authentication", 407, "", nil, false, "authentication", "proxy_authentication_failed"},
		{"rate limit", 429, "", nil, false, "http", "http_status"},
		{"redirect", 302, "ip=203.0.113.8\n", nil, false, "http", "http_status"},
		{"invalid IP", 200, "ip=not-an-ip\n", nil, false, "exit_ip", "invalid_exit_ip"},
		{"missing IP", 204, "", nil, false, "exit_ip", "invalid_exit_ip"},
		{"CONNECT auth error", 0, "", errors.New("proxyconnect tcp http://user:private-password@host:407: Proxy Authentication Required"), false, "authentication", "proxy_authentication_failed"},
		{"connection error", 0, "", errors.New("dial http://user:private-password@host failed"), false, "connection", "connection_failed"},
		{"SOCKS authentication", 0, "", errors.New("socks5 authentication rejected"), false, "authentication", "proxy_authentication_failed"},
		{"CONNECT forbidden", 403, "", errors.New("Forbidden"), false, "http", "http_status"},
		{"response interrupted", 200, "", errors.New("unexpected EOF"), false, "exit_ip", "response_read_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := proxyDiagnosticResult(test.status, []byte(test.body), test.err, 12*time.Millisecond)
			if result.OK != test.ok || result.Stage != test.stage || result.ErrorCode != test.code || result.LatencyMS != 12 {
				t.Fatalf("unexpected result: %+v", result)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "private-password") {
				t.Fatal("diagnostic leaked password")
			}
		})
	}
}

func TestAdminProxyPasswordRedactionAndBlankPreservation(t *testing.T) {
	p, handler := webTestSetup(t)
	response := webTestRequest(handler, http.MethodPost, "/api/proxies", `{"name":"test","type":"http","host":"127.0.0.1","port":8080,"password":"private-proxy-password"}`, "Bearer "+webTestKey)
	if response.Code != 200 || strings.Contains(response.Body.String(), "private-proxy-password") || !strings.Contains(response.Body.String(), `"has_password":true`) {
		t.Fatalf("unsafe create: %d %s", response.Code, response.Body.String())
	}
	var entry ProxyEntry
	if err := json.Unmarshal(response.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/proxies", "/api/accounts"} {
		response = webTestRequest(handler, http.MethodGet, path, "", "Bearer "+webTestKey)
		if response.Code != 200 || strings.Contains(response.Body.String(), "private-proxy-password") {
			t.Fatalf("unsafe list: %s", response.Body.String())
		}
	}
	response = webTestRequest(handler, http.MethodPut, "/api/proxies/"+entry.ID, `{"password":"","name":"updated"}`, "Bearer "+webTestKey)
	if response.Code != 200 {
		t.Fatalf("update failed %s", response.Body.String())
	}
	stored, err := p.Proxies.GetForAdmin(entry.ID)
	if err != nil || stored.Password != "private-proxy-password" {
		t.Fatal("blank edit discarded proxy password")
	}
	response = webTestRequest(handler, http.MethodPut, "/api/proxies/"+entry.ID, `{"clear_password":true}`, "Bearer "+webTestKey)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"has_password":false`) {
		t.Fatalf("clear failed %s", response.Body.String())
	}
	stored, err = p.Proxies.GetForAdmin(entry.ID)
	if err != nil || stored.Password != "" {
		t.Fatal("explicit clear did not persist")
	}
}

func TestAdminProxyConnectResponseClassification(t *testing.T) {
	_, handler := webTestSetup(t)
	for _, status := range []int{403, 407, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					t.Errorf("unexpected method %s", r.Method)
				}
				w.WriteHeader(status)
			}))
			defer proxy.Close()
			parsed, _ := url.Parse(proxy.URL)
			port, _ := strconv.Atoi(parsed.Port())
			body, _ := json.Marshal(map[string]any{"name": "synthetic", "type": "http", "host": parsed.Hostname(), "port": port, "username": "user", "password": "private-test-password"})
			response := webTestRequest(handler, http.MethodPost, "/api/proxies/test", string(body), "Bearer "+webTestKey)
			var result proxyDiagnostic
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.OK || result.HTTPCode != status {
				t.Fatalf("CONNECT failure misclassified: %d %s", response.Code, response.Body.String())
			}
			if status == 407 && result.Authentication != "failed" {
				t.Fatal("proxy authentication failure was not recognized")
			}
			if status != 407 && (result.Stage != "http" || result.Authentication != "not_checked") {
				t.Fatal("HTTP proxy rejection asserted an unobserved authentication result")
			}
			if strings.Contains(response.Body.String(), "private-test-password") {
				t.Fatal("CONNECT diagnostic leaked password")
			}
		})
	}
}

func TestAdminImportReportsConflictAndPreviewWithoutMutation(t *testing.T) {
	p, handler := webTestSetup(t)
	initial := `{"accounts":[{"name":"collision","credentials":{"access_token":"token-one"}},{"name":"collision","credentials":{"access_token":"token-two"}}]}`
	response := webTestRequest(handler, http.MethodPost, "/api/accounts/import/preview", initial, "Bearer "+webTestKey)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"dry_run":true`) {
		t.Fatalf("preview: %s", response.Body.String())
	}
	_, before, err := p.Store.List()
	if err != nil || len(before) != 1 {
		t.Fatal("preview wrote accounts")
	}
	response = webTestRequest(handler, http.MethodPost, "/api/accounts", initial, "Bearer "+webTestKey)
	var report struct {
		Added, Duplicate, Replaced, Failed, Imported int
		Results                                      []ImportResult
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &report) != nil || report.Added != 1 || report.Imported != 1 || report.Duplicate+report.Failed != 1 {
		t.Fatalf("conflict report: %d %s", response.Code, response.Body.String())
	}
	a, err := p.Store.Get("collision")
	if err != nil || a.AccessToken != "token-one" {
		t.Fatal("same-name input silently replaced first account")
	}
	response = webTestRequest(handler, http.MethodPost, "/api/accounts", `{"name":"collision","credentials":{"access_token":"token-two"},"replace":true}`, "Bearer "+webTestKey)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"replaced":1`) {
		t.Fatalf("explicit replacement failed: %s", response.Body.String())
	}
	a, err = p.Store.Get("collision")
	if err != nil || a.AccessToken != "token-two" {
		t.Fatal("explicit replacement not persisted")
	}
}

func TestAdminRawImportRejectsMalformedJSON(t *testing.T) {
	p, handler := webTestSetup(t)
	request, _ := json.Marshal(map[string]string{"raw": "{\n\"tokens\": {\"access_token\":\"never-import-this\"\n}"})
	response := webTestRequest(handler, http.MethodPost, "/api/accounts", string(request), "Bearer "+webTestKey)
	if response.Code != 400 || strings.Contains(response.Body.String(), "never-import-this") {
		t.Fatalf("unsafe malformed import result: %d %s", response.Code, response.Body.String())
	}
	_, accounts, err := p.Store.List()
	if err != nil || len(accounts) != 1 {
		t.Fatal("malformed JSON was imported")
	}
}

func TestAdminStableIDVersionAndDetail(t *testing.T) {
	p, handler := webTestSetup(t)
	a, err := p.Store.GetMetadata("test")
	if err != nil {
		t.Fatal(err)
	}
	response := webTestRequest(handler, http.MethodPut, "/api/accounts/"+a.ID, fmt.Sprintf(`{"version":%d,"name":"renamed","notes":"note","tags":["one"]}`, a.Version), "Bearer "+webTestKey)
	if response.Code != 200 {
		t.Fatalf("stable ID update failed: %s", response.Body.String())
	}
	response = webTestRequest(handler, http.MethodPut, "/api/accounts/"+a.ID, fmt.Sprintf(`{"version":%d,"notes":"stale"}`, a.Version), "Bearer "+webTestKey)
	if response.Code != 409 {
		t.Fatalf("stale version returned %d: %s", response.Code, response.Body.String())
	}
	response = webTestRequest(handler, http.MethodGet, "/api/accounts/"+a.ID, "", "Bearer "+webTestKey)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"name":"renamed"`) || strings.Contains(response.Body.String(), `"access_token"`) {
		t.Fatalf("detail failure: %s", response.Body.String())
	}
	fresh, err := p.Store.GetMetadata(a.ID)
	if err != nil || fresh.Notes != "note" || fresh.Version != a.Version+1 {
		t.Fatal("conflicting update changed persisted data")
	}
}

func TestAdminInvalidPaginationAndOverviewRange(t *testing.T) {
	_, handler := webTestSetup(t)
	for _, path := range []string{"/api/accounts?limit=501", "/api/logs?page=101", "/api/logs?cursor=broken", "/api/logs?from=10&to=2", "/api/logs?page_size=501", "/api/overview?from=1&to=9223372036854775807", "/api/overview?from=100&to=20", "/api/overview?step=1"} {
		response := webTestRequest(handler, http.MethodGet, path, "", "Bearer "+webTestKey)
		if response.Code != 400 {
			t.Fatalf("%s returned %d: %s", path, response.Code, response.Body.String())
		}
	}
}

func TestAdminRotationPersistenceFailurePreservesOldKey(t *testing.T) {
	p, handler := webTestSetup(t)
	p.KeyFile = t.TempDir() // replacement of a directory must fail
	response := webTestRequest(handler, http.MethodPost, "/api/settings/rotate-key", `{"request_id":"failed-rotation"}`, "Bearer "+webTestKey)
	if response.Code != 500 || p.adminKey() != webTestKey {
		t.Fatalf("failed rotation changed active key: %d", response.Code)
	}
	response = webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	if response.Code != 200 {
		t.Fatal("old key rejected after failed rotation")
	}
}

func TestAdminSettingsAndKeyVersionConflict(t *testing.T) {
	p, handler := webTestSetup(t)
	initial, err := p.Settings.Latest()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"admin_api":false,"settings_version":%d}`, initial.Version)
	response := webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
	if response.Code != 200 {
		t.Fatalf("settings save failed: %s", response.Body.String())
	}
	response = webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
	if response.Code != 409 {
		t.Fatalf("stale settings accepted: %d %s", response.Code, response.Body.String())
	}
	response = webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	if response.Code != 200 || !strings.Contains(response.Body.String(), fmt.Sprintf(`"settings_version":%d`, initial.Version+1)) {
		t.Fatalf("settings version missing: %s", response.Body.String())
	}
	key, err := p.Keys.Create("version-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	body = fmt.Sprintf(`{"name":"renamed-key","version":%d}`, key.Version)
	response = webTestRequest(handler, http.MethodPut, "/api/keys/"+key.ID, body, "Bearer "+webTestKey)
	if response.Code != 200 {
		t.Fatalf("key save failed: %s", response.Body.String())
	}
	response = webTestRequest(handler, http.MethodPut, "/api/keys/"+key.ID, body, "Bearer "+webTestKey)
	if response.Code != 409 {
		t.Fatalf("stale key accepted: %d %s", response.Code, response.Body.String())
	}
}
