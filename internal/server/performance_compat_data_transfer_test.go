package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Valid complete data travels through the real authenticated HTTP handlers.
// A second isolated database receives the exported portable bundle so field
// preservation is checked beyond counts. No model/provider call is allowed.
func TestPerformanceCompatDataTransfer(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic performance runner required")
	}
	for _, count := range []int{100, 1000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			p, _ := adminPGTestProxy(t)
			deny := new(localWriteNoUpstream)
			p.Client = &http.Client{Transport: deny}
			p.BaseURL = "http://127.0.0.1:1/forbidden-provider"
			server := httptest.NewServer(webHandler(p, t.TempDir()))
			defer server.Close()
			client := &http.Client{Timeout: 90 * time.Second}
			defer client.CloseIdleConnections()
			var slow []string
			request := func(proxy *Proxy, base, name, method, path string, payload any, validate func([]byte) error) []byte {
				t.Helper()
				var raw []byte
				if payload != nil {
					var err error
					raw, err = json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
				}
				req, err := http.NewRequest(method, base+path, bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+webTestKey)
				req.Header.Set("Content-Type", "application/json")
				acquires := proxy.Store.DB.Pool.Stat().AcquireCount()
				started := time.Now()
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(res.Body)
				res.Body.Close()
				elapsed := float64(time.Since(started).Microseconds()) / 1000
				acquires = proxy.Store.DB.Pool.Stat().AcquireCount() - acquires
				failure := ""
				if readErr != nil || res.StatusCode != 200 {
					failure = "http_transport_or_status"
				} else if res.Header.Get("Cache-Control") != "no-store" {
					failure = "missing_no_store"
				} else if err = validate(body); err != nil {
					failure = err.Error()
				}
				performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{"name": fmt.Sprintf("%s_%d", name, count), "method": method, "path": path, "accounts": count, "proxies": count, "request_bytes": len(raw), "response_bytes": len(body), "sample": map[string]any{"ms": elapsed, "status": res.StatusCode, "error": failure, "db_acquires": acquires}, "threshold_ms": 1000, "slo_pass": failure == "" && elapsed < 1000, "scope": "b3 runtime; authenticated loopback HTTP; isolated PG; valid synthetic portable data; provider denied"})
				if failure != "" {
					t.Fatalf("%s failed semantic validation: %s", name, failure)
				}
				if elapsed >= 1000 {
					slow = append(slow, fmt.Sprintf("%s %.3fms", name, elapsed))
				}
				return body
			}
			input := sub2DataPayload{Type: "sub2api-data", Version: 1, Proxies: []map[string]json.RawMessage{}, Accounts: []map[string]json.RawMessage{}}
			expected := map[string]string{}
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("data-%d-%04d@example.invalid", count, i)
				key := fmt.Sprintf("portable-proxy-%d", i)
				input.Proxies = append(input.Proxies, map[string]json.RawMessage{"name": sub2Raw(fmt.Sprintf("data-proxy-%04d", i)), "protocol": sub2Raw("http"), "host": sub2Raw("127.0.0.1"), "port": sub2Raw(10000 + i), "username": sub2Raw(fmt.Sprintf("synthetic-user-%d", i)), "password": sub2Raw(fmt.Sprintf("synthetic-password-%d", i)), "status": sub2Raw("active"), "proxy_key": sub2Raw(key)})
				payload := sub2Fixture(name)
				payload["notes"] = "portable synthetic metadata"
				payload["proxy_key"] = key
				// Local group bindings deliberately do not travel in portable data.
				payload["group_ids"] = []int64{999999}
				encoded, _ := json.Marshal(payload)
				var item map[string]json.RawMessage
				_ = json.Unmarshal(encoded, &item)
				input.Accounts = append(input.Accounts, item)
				expected[name] = "access-" + name
			}
			countsOK := func(body []byte) error {
				var result struct {
					Code int
					Data struct {
						ProxyCreated   int `json:"proxy_created"`
						ProxyReused    int `json:"proxy_reused"`
						ProxyFailed    int `json:"proxy_failed"`
						AccountCreated int `json:"account_created"`
						AccountFailed  int `json:"account_failed"`
					}
				}
				if json.Unmarshal(body, &result) != nil || result.Code != 0 || result.Data.ProxyCreated != count || result.Data.AccountCreated != count || result.Data.ProxyFailed != 0 || result.Data.AccountFailed != 0 {
					return fmt.Errorf("portable_import_counts")
				}
				return nil
			}
			request(p, server.URL, "sub2_data_import", http.MethodPost, "/api/v1/admin/accounts/data", map[string]any{"data": input, "skip_default_group_bind": true}, countsOK)
			checkDurable := func(proxy *Proxy) []Account {
				t.Helper()
				_, accounts, err := proxy.Store.List()
				if err != nil || len(accounts) != count {
					t.Fatal("imported account durability/count", err)
				}
				proxies, err := proxy.Proxies.List()
				if err != nil || len(proxies) != count {
					t.Fatal("imported proxy durability/count", err)
				}
				byID := map[string]ProxyEntry{}
				for _, v := range proxies {
					byID[v.ID] = v
				}
				for _, a := range accounts {
					if a.AccessToken != expected[a.Name] || len(a.Groups) != 0 || len(a.Proxies) != 1 || !a.Enabled || a.Priority != -3 {
						t.Fatal("portable account credentials/policy/group/proxy mismatch")
					}
					px, ok := byID[a.Proxies[0]]
					if !ok || !px.Enabled || !strings.HasPrefix(px.Password, "synthetic-password-") {
						t.Fatal("portable credential binding missing")
					}
				}
				var n int
				if err := proxy.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM account_proxies").Scan(&n); err != nil || n != count {
					t.Fatal("association durability mismatch", err)
				}
				var credentialRows, accountAudits, proxyAudits, accountTransactions, proxyTransactions int
				if err := proxy.Store.DB.Pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM account_credentials),
 (SELECT count(*) FROM audit_logs WHERE action='account.compat.update'),
 (SELECT count(*) FROM audit_logs WHERE action='proxy.create'),
 (SELECT count(DISTINCT xmin::text) FROM accounts),
 (SELECT count(DISTINCT xmin::text) FROM proxies)`).Scan(&credentialRows, &accountAudits, &proxyAudits, &accountTransactions, &proxyTransactions); err != nil || credentialRows != count || accountAudits != count || proxyAudits != count || accountTransactions != count || proxyTransactions != count {
					t.Fatalf("portable per-resource durability/audit changed: credentials=%d audits=%d/%d transactions=%d/%d err=%v", credentialRows, accountAudits, proxyAudits, accountTransactions, proxyTransactions, err)
				}
				return accounts
			}
			accounts := checkDurable(p)
			var exported sub2DataPayload
			exportOK := func(body []byte) error {
				var envelope struct {
					Code int
					Data sub2DataPayload
				}
				if json.Unmarshal(body, &envelope) != nil || envelope.Code != 0 || len(envelope.Data.Accounts) != count || len(envelope.Data.Proxies) != count {
					return fmt.Errorf("portable_export_count")
				}
				keys := map[string]bool{}
				for _, px := range envelope.Data.Proxies {
					key := sub2String(px, "proxy_key", "")
					if key == "" || sub2String(px, "password", "") == "" {
						return fmt.Errorf("portable_export_proxy_credentials")
					}
					keys[key] = true
				}
				for _, a := range envelope.Data.Accounts {
					var credentials map[string]json.RawMessage
					if json.Unmarshal(a["credentials"], &credentials) != nil || sub2String(credentials, "access_token", "") != expected[sub2String(a, "name", "")] || !keys[sub2String(a, "proxy_key", "")] {
						return fmt.Errorf("portable_export_account_identity")
					}
					if _, ok := a["group_ids"]; ok {
						return fmt.Errorf("portable_export_local_binding_leak")
					}
				}
				exported = envelope.Data
				return nil
			}
			for sample := 0; sample < 3; sample++ {
				request(p, server.URL, "sub2_data_export", http.MethodGet, "/api/v1/admin/accounts/data", nil, exportOK)
			}
			selection := make([]managedAccountItem, len(accounts))
			for i, a := range accounts {
				selection[i] = managedAccountItem{ID: a.ID, Version: a.Version}
			}
			request(p, server.URL, "native_metadata_export", http.MethodPost, "/api/accounts/export", map[string]any{"items": selection}, func(body []byte) error {
				var v struct {
					Type     string
					Accounts []json.RawMessage
				}
				if json.Unmarshal(body, &v) != nil || v.Type != "codex-agent-account-metadata" || len(v.Accounts) != count {
					return fmt.Errorf("native_metadata_count")
				}
				for _, secret := range expected {
					if bytes.Contains(body, []byte(secret)) {
						return fmt.Errorf("native_metadata_secret_leak")
					}
				}
				return nil
			})
			sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
			filename := accounts[count-1].Name + ".json"
			request(p, server.URL, "cpa_download", http.MethodGet, "/v0/management/auth-files/download?name="+url.QueryEscape(filename), nil, func(body []byte) error {
				var fields map[string]json.RawMessage
				if json.Unmarshal(body, &fields) != nil || cpaString(fields, "access_token") != expected[accounts[count-1].Name] {
					return fmt.Errorf("cpa_download_credential_identity")
				}
				return nil
			})
			// A separate synthetic destination proves exported proxy-key bindings
			// and credentials can be restored through the public portable route.
			destination, _ := adminPGTestProxy(t)
			destination.Client = &http.Client{Transport: deny}
			destination.BaseURL = p.BaseURL
			restoreServer := httptest.NewServer(webHandler(destination, t.TempDir()))
			defer restoreServer.Close()
			request(destination, restoreServer.URL, "sub2_data_reimport", http.MethodPost, "/api/v1/admin/accounts/data", map[string]any{"data": exported, "skip_default_group_bind": true}, countsOK)
			checkDurable(destination)
			if deny.calls.Load() != 0 {
				t.Fatal("local data route attempted provider request")
			}
			if len(slow) > 0 {
				t.Errorf("valid local data transfer exceeds1s: %s", strings.Join(slow, "; "))
			}
		})
	}
}
