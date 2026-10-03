package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Only local compatibility mutation branches are called. Inference, models,
// privacy, refresh, OAuth exchange and import-from-URL are never dispatched.
func TestPerformanceCompatLocalWrites(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic performance runner required")
	}
	p, _ := adminPGTestProxy(t)
	deny := new(localWriteNoUpstream)
	p.Client = &http.Client{Transport: deny}
	p.BaseURL = "http://127.0.0.1:1/forbidden-provider"
	server := httptest.NewServer(webHandler(p, t.TempDir()))
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	functionalOnly := os.Getenv("PERFORMANCE_LOCAL_WRITE_FUNCTIONAL") == "1"
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 5, 1, 20)
	type sample struct {
		MS     float64 `json:"ms"`
		Status int     `json:"status"`
		Error  string  `json:"error,omitempty"`
	}
	measurements := map[string][]sample{}
	routes := map[string]map[string]string{}
	request := func(name, method, path string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+webTestKey)
		r.Header.Set("Content-Type", "application/json")
		start := time.Now()
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		content, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		s := sample{MS: float64(time.Since(start).Microseconds()) / 1000, Status: response.StatusCode}
		value := map[string]any{}
		if readErr != nil || response.StatusCode != http.StatusOK || json.Unmarshal(content, &value) != nil {
			s.Error = "http_or_json_failure"
		}
		measurements[name] = append(measurements[name], s)
		routes[name] = map[string]string{"method": method, "path": path}
		if s.Error != "" {
			t.Fatalf("%s failed: %d %s", name, s.Status, content)
		}
		return value
	}
	data := func(value map[string]any) map[string]any {
		t.Helper()
		if value["code"] != float64(0) {
			t.Fatalf("Sub2 envelope: %v", value)
		}
		out, ok := value["data"].(map[string]any)
		if !ok {
			t.Fatalf("Sub2 data not object: %v", value)
		}
		return out
	}
	assertEncrypted := func(id, secret string) {
		t.Helper()
		var raw []byte
		if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT credentials_cipher FROM account_credentials WHERE account_id=$1", id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("stored credentials contain plaintext token")
		}
		plain, err := p.Store.DB.Decrypt(raw)
		if err != nil || !bytes.Contains(plain, []byte(secret)) {
			t.Fatal("stored encrypted credentials failed round trip")
		}
	}
	for iteration := range samples {
		filename := fmt.Sprintf("compat-http-%d.json", iteration)
		token := fmt.Sprintf("synthetic-cpa-http-%d", iteration)
		request("cpa_upload", http.MethodPost, "/v0/management/auth-files?name="+url.QueryEscape(filename), cpaTestPayload(token))
		row, err := p.cpaFind(t.Context(), filename, "")
		if err != nil || row.Account.AccessToken != token || row.Record.FileName != filename {
			t.Fatalf("CPA upload not durable: %v", err)
		}
		assertEncrypted(row.Account.ID, token)
		request("cpa_status_patch", http.MethodPatch, "/v0/management/auth-files/status", map[string]any{"name": filename, "auth_index": compatAuthIndex(row.Account.ID), "disabled": true})
		row, err = p.compatGetAccount(t.Context(), row.Account.ID)
		if err != nil || row.Account.Enabled {
			t.Fatal("CPA disable not durable")
		}
		request("cpa_fields_patch", http.MethodPatch, "/v0/management/auth-files/fields", map[string]any{"name": filename, "priority": 9, "note": "compat local write", "custom.value": 7})
		row, err = p.compatGetAccount(t.Context(), row.Account.ID)
		if err != nil || row.Account.Enabled || row.Account.Priority != 9 || row.Account.Notes != "compat local write" || !strings.Contains(string(row.Record.CPA["custom"]), `"value":7`) {
			t.Fatal("CPA fields not preserved")
		}
		request("cpa_delete", http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(filename), nil)
		if _, err := p.Store.GetMetadata(row.Account.ID); !errors.Is(err, errNotFound) {
			t.Fatalf("CPA delete not durable: %v", err)
		}
		base := "/api/v1/admin"
		group := data(request("sub2_group_create", http.MethodPost, base+"/groups", map[string]any{"name": fmt.Sprintf("compat-group-%d", iteration), "platform": "openai"}))
		proxy := data(request("sub2_proxy_create", http.MethodPost, base+"/proxies", map[string]any{"name": fmt.Sprintf("compat-proxy-%d", iteration), "protocol": "http", "host": "127.0.0.1", "port": 1, "username": "synthetic-user", "password": "synthetic-proxy-password"}))
		groupID, proxyID := sub2ID(group), sub2ID(proxy)
		group = data(request("sub2_group_update", http.MethodPut, base+"/groups/"+groupID, map[string]any{"name": fmt.Sprintf("compat-group-updated-%d", iteration), "description": "updated description"}))
		if group["description"] != "updated description" {
			t.Fatal("Sub2 group update missing")
		}
		proxy = data(request("sub2_proxy_update", http.MethodPut, base+"/proxies/"+proxyID, map[string]any{"name": fmt.Sprintf("compat-proxy-updated-%d", iteration), "host": "127.0.0.2"}))
		if proxy["host"] != "127.0.0.2" {
			t.Fatal("Sub2 proxy update missing")
		}
		name := fmt.Sprintf("compat-sub2-%d@example.invalid", iteration)
		payload := sub2Fixture(name)
		payload["group_ids"] = []any{group["id"]}
		payload["proxy_id"] = proxy["id"]
		account := data(request("sub2_account_create", http.MethodPost, base+"/accounts", payload))
		numeric := sub2ID(account)
		nativeID, err := p.compatResolveAccount(t.Context(), numeric)
		if err != nil {
			t.Fatal(err)
		}
		native, err := p.Store.Get(nativeID)
		if err != nil || native.AccessToken != "access-"+name || len(native.Groups) != 1 || len(native.Proxies) != 1 || fmt.Sprint(compatNumericID(native.Groups[0])) != groupID || fmt.Sprint(compatNumericID(native.Proxies[0])) != proxyID {
			t.Fatal("Sub2 credentials/bindings not durable")
		}
		assertEncrypted(nativeID, native.AccessToken)
		metadataRaw, _ := json.Marshal(account)
		if bytes.Contains(metadataRaw, []byte(native.AccessToken)) {
			t.Fatal("Sub2 mutation response leaked credentials")
		}
		data(request("sub2_account_update", http.MethodPut, base+"/accounts/"+numeric, map[string]any{"priority": 1, "notes": "sub2 changed", "extra": map[string]any{"team_oauth_verified": true, "label": "synthetic"}}))
		after, err := p.Store.Get(nativeID)
		if err != nil || after.Priority != -1 || after.Notes != "sub2 changed" || after.CredentialVersion != native.CredentialVersion || !reflect.DeepEqual(after.Groups, native.Groups) || !reflect.DeepEqual(after.Proxies, native.Proxies) {
			t.Fatal("Sub2 metadata changed credentials or bindings")
		}
		data(request("sub2_schedulable", http.MethodPost, base+"/accounts/"+numeric+"/schedulable", map[string]bool{"schedulable": false}))
		after, err = p.Store.Get(nativeID)
		if err != nil || after.Enabled {
			t.Fatal("Sub2 schedulable not durable")
		}
		data(request("sub2_clear_error", http.MethodPost, base+"/accounts/"+numeric+"/clear-error", map[string]any{}))
		after, err = p.Store.Get(nativeID)
		if err != nil || after.LastError != "" || after.CredentialStatus != "unverified" || after.Enabled {
			t.Fatal("Sub2 clear-error altered pause state")
		}
		request("sub2_group_delete", http.MethodDelete, base+"/groups/"+groupID, nil)
		after, err = p.Store.GetMetadata(nativeID)
		if err != nil || len(after.Groups) != 0 || len(after.Proxies) != 1 {
			t.Fatal("group deletion did not detach just group relation")
		}
		request("sub2_proxy_delete", http.MethodDelete, base+"/proxies/"+proxyID, nil)
		after, err = p.Store.GetMetadata(nativeID)
		if err != nil || len(after.Groups) != 0 || len(after.Proxies) != 0 {
			t.Fatal("proxy deletion did not detach relation")
		}
		request("sub2_account_delete", http.MethodDelete, base+"/accounts/"+numeric, nil)
		if _, err := p.Store.GetMetadata(nativeID); !errors.Is(err, errNotFound) {
			t.Fatalf("Sub2 delete not durable: %v", err)
		}
	}
	if deny.calls.Load() != 0 {
		t.Fatalf("compat local routes attempted upstream %d times", deny.calls.Load())
	}
	var accounts, credentials, groups, proxies int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM accounts),(SELECT count(*) FROM account_credentials),(SELECT count(*) FROM account_groups),(SELECT count(*) FROM account_proxies)`).Scan(&accounts, &credentials, &groups, &proxies); err != nil || accounts != 0 || credentials != 0 || groups != 0 || proxies != 0 {
		t.Fatalf("compat cleanup left entities %d/%d/%d/%d err=%v", accounts, credentials, groups, proxies, err)
	}
	names := make([]string, 0, len(measurements))
	for name := range measurements {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		values := measurements[name]
		latencies := make([]float64, len(values))
		over := 0
		for i, v := range values {
			latencies[i] = v.MS
			if v.MS >= 1000 {
				over++
			}
		}
		sort.Float64s(latencies)
		percentile := func(p int) float64 { return latencies[(len(latencies)*p+99)/100-1] }
		performanceSLOEmit(t, "WRITE_RESULT", map[string]any{"name": name, "route": routes[name], "samples": values, "failed": 0, "over_slo": over, "threshold_ms": 1000, "max_ms": latencies[len(latencies)-1], "p50_ms": percentile(50), "p95_ms": percentile(95), "p99_ms": percentile(99), "functional_only": functionalOnly, "scope": "independent synthetic PG schema; real compat HTTP lifecycle; no provider"})
		if !functionalOnly && over > 0 {
			t.Errorf("%s exceeded 1s in %d samples", name, over)
		}
	}
}
