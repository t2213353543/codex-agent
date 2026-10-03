package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise previously unsampled local read routes on an existing synthetic
// population. No migrations or historical event/log mutations happen here.
func TestPerformanceSLOAdditionalReads(t *testing.T) {
	schema, dsn := os.Getenv("PERFORMANCE_SLO_SCHEMA"), os.Getenv("TEST_DATABASE_URL")
	if os.Getenv("PERFORMANCE_SLO") != "1" || !regexp.MustCompile(`^perf_slo_[a-z0-9_]{8,60}$`).MatchString(schema) || dsn == "" {
		t.Skip("use the retained isolated performance fixture")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path != "/codex_agent_integration" || u.User == nil || u.User.Username() != "codex_agent_test" || u.Port() != "25432" || !stringIn(u.Hostname(), "localhost", "127.0.0.1", "::1") {
		t.Fatal("dedicated loopback integration database required")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := OpenDatabase(t.Context(), u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32)), "slo-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var eventCount int64
	var ready bool
	if err := db.Pool.QueryRow(t.Context(), "SELECT rows,ready FROM performance_slo_fixture").Scan(&eventCount, &ready); err != nil || !ready {
		t.Fatalf("retained fixture unavailable: %v", err)
	}
	metrics, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewPGKeyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	proxies, err := NewPGProxyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Proxies: proxies, Keys: keys, Settings: NewPGSettingsStore(db), Metrics: metrics, AdminKey: webTestKey, startedAt: time.Now()}
	account, err := p.Store.GetMetadata("slo_a_1")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := p.Groups.List()
	if err != nil || len(groups) == 0 {
		t.Fatalf("groups: %v", err)
	}
	proxyRows, err := proxies.managedMetadata()
	if err != nil || len(proxyRows) == 0 {
		t.Fatalf("proxies: %v", err)
	}
	var accountCount int
	if err := db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM accounts").Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	state := p.managementState()
	now := time.Now().Unix()
	seed := func(namespace, key string, value any, expires int64) {
		t.Helper()
		if err := state.Update(t.Context(), namespace, key, func(entry *managementStateEntry) error {
			var err error
			entry.Payload, err = json.Marshal(value)
			entry.ExpiresAt = expires
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("account-auto-reset", account.ID, managedAutoResetPolicy{Version: 1, Threshold: 95, Interval: 15, Phase: "off", CredentialVersion: account.CredentialVersion}, 0)
	seed("account-leases", account.ID, managedAccountLease{Version: 1, ExpiresAt: now + 3600}, 0)
	planID := account.ID + "_s_performance"
	plans := managedScheduleDocument{Plans: map[string]*managedScheduleState{}}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("%s_s_perf_%d", account.ID, i)
		if i == 0 {
			id = planID
		}
		row := &managedScheduleState{Plan: managedAccountSchedule{ID: id, AccountID: account.ID, Version: 1, Kind: "models", Cron: "0 * * * *", Timezone: "UTC", MaxResults: 100}, Results: []managedScheduleRun{}}
		for j := 0; j < 100; j++ {
			row.Results = append(row.Results, managedScheduleRun{ID: fmt.Sprintf("run_%d", j), Status: "ok", ScheduledAt: now - int64(j)})
		}
		plans.Plans[id] = row
	}
	seed("account-schedules", account.ID, plans, 0)
	seed("account-upstream", account.ID, map[string]managedUpstreamSnapshot{"models": {ID: account.ID, Version: account.Version, CredentialVersion: account.CredentialVersion, Kind: "models", OK: true, CheckedAt: now, Source: "synthetic_saved_observation", Data: map[string]any{"models": []string{"synthetic"}}}}, 0)
	owner := "bearer:" + adminKeyHash(webTestKey)
	oauthID := "performance-local-oauth"
	seed(managedOAuthNamespace, oauthID, managedOAuthSession{Owner: owner, Method: "oauth", Phase: "pending", TargetID: account.ID, TargetVersion: account.Version}, now+3600)
	receiptID := "performance-local-lease-receipt"
	seed("lease-restore", receiptID, storedManagedLeaseReceipt{managedLeaseRestoreReceipt: managedLeaseRestoreReceipt{ID: receiptID, ExpiresAt: now + 3600, Items: []managedLeaseRestoreItem{{ID: account.ID, Name: account.Name, Status: "restored"}}}, Owner: adminKeyHash(owner)}, now+3600)
	server := httptest.NewServer(webHandler(p, t.TempDir()))
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 8}}
	defer client.CloseIdleConnections()
	type readCase struct {
		name, path, field string
		count             int
		expected          any
	}
	base := "/api/accounts/" + url.PathEscape(account.ID)
	cases := []readCase{
		{"native_account_detail", base, "account.id", -1, account.ID},
		{"legacy_accounts_50", "/api/accounts?limit=50", "accounts", 50, nil},
		{"legacy_accounts_500", "/api/accounts?limit=500", "accounts", 500, nil},
		{"legacy_accounts_name", "/api/accounts?search=" + url.QueryEscape(account.Name), "accounts", -2, nil},
		{"saved_upstream_snapshot", base + "/upstream", "snapshots.models.source", -1, "synthetic_saved_observation"},
		{"account_auto_reset", base + "/auto-reset", "policy.phase", -1, "off"},
		{"account_schedules", base + "/schedules", "plans", 20, nil},
		{"account_lease", base + "/lease", "lease.version", -1, float64(1)},
		{"schedule_results_50", "/api/account-schedules/" + planID + "/results?limit=50", "results", 50, nil},
		{"schedule_results_100", "/api/account-schedules/" + planID + "/results?limit=100", "results", 100, nil},
		{"lease_receipt", "/api/accounts/import-leases/receipts/" + receiptID, "receipt_id", -1, receiptID},
		{"oauth_saved_pending", "/api/account-oauth/" + oauthID, "phase", -1, "pending"},
		{"cpa_accounts_all", "/v0/management/auth-files", "files", accountCount, nil},
		{"cpa_account_id", "/v0/management/auth-files?name=" + url.QueryEscape(account.ID), "files", 1, nil},
		{"cpa_account_filename", "/v0/management/auth-files?name=" + url.QueryEscape(compatFileName(account, adminCompatRecord{})), "files", 1, nil},
		{"cpa_account_auth_index", "/v0/management/auth-files?auth_index=" + compatAuthIndex(account.ID), "files", 1, nil},
		{"sub2_accounts_50", "/api/v1/admin/accounts?page_size=50", "data.items", 50, nil},
		{"sub2_accounts_500", "/api/v1/admin/accounts?page_size=500", "data.items", 500, nil},
		{"sub2_account_detail", "/api/v1/admin/accounts/" + fmt.Sprint(compatNumericID(account.ID)), "data.id", -1, float64(compatNumericID(account.ID))},
		{"sub2_groups", "/api/v1/admin/groups?page_size=100", "data.items", len(groups), nil},
		{"sub2_groups_all", "/api/v1/admin/groups/all", "data", len(groups), nil},
		{"sub2_group_detail", "/api/v1/admin/groups/" + fmt.Sprint(compatNumericID(groups[0].ID)), "data.id", -1, float64(compatNumericID(groups[0].ID))},
		{"sub2_proxies", "/api/v1/admin/proxies?page_size=100", "data.items", len(proxyRows), nil},
		{"sub2_proxies_all", "/api/v1/admin/proxies/all", "data", len(proxyRows), nil},
		{"sub2_proxy_detail", "/api/v1/admin/proxies/" + fmt.Sprint(compatNumericID(proxyRows[0].ID)), "data.id", -1, float64(compatNumericID(proxyRows[0].ID))},
		{"review_errors_unfiltered", "/api/logs/errors?page_size=50", "items", 50, nil},
		{"review_errors_unresolved", "/api/logs/errors?page_size=50&resolved=false", "items", 50, nil},
	}
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 20, 1, 200)
	threshold := float64(1000)
	if eventCount >= 10000000 {
		threshold = 3000
	}
	type sample struct {
		MS     float64 `json:"ms"`
		Status int     `json:"status"`
		Bytes  int     `json:"bytes"`
		Error  string  `json:"error,omitempty"`
	}
	measure := func(c readCase) sample {
		start := time.Now()
		request, _ := http.NewRequest(http.MethodGet, server.URL+c.path, nil)
		request.Header.Set("Authorization", "Bearer "+webTestKey)
		response, err := client.Do(request)
		if err != nil {
			return sample{MS: float64(time.Since(start).Microseconds()) / 1000, Error: err.Error()}
		}
		raw, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		out := sample{MS: float64(time.Since(start).Microseconds()) / 1000, Status: response.StatusCode, Bytes: len(raw)}
		if readErr != nil || response.StatusCode != 200 {
			out.Error = "http_or_body_failure"
			return out
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			out.Error = "invalid_json"
			return out
		}
		for _, field := range strings.Split(c.field, ".") {
			object, ok := value.(map[string]any)
			if !ok {
				out.Error = "missing_object_" + field
				return out
			}
			value = object[field]
		}
		if c.count >= 0 {
			items, ok := value.([]any)
			if !ok || len(items) != c.count {
				out.Error = fmt.Sprintf("items=%d want=%d", len(items), c.count)
			}
		} else if c.count == -2 {
			items, ok := value.([]any)
			if !ok || len(items) < 1 {
				out.Error = "no_matching_accounts"
			}
		} else if value != c.expected {
			out.Error = fmt.Sprintf("field %s differs", c.field)
		}
		return out
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := measure(c)
			performanceSLOEmit(t, "FIRST", map[string]any{"name": c.name, "path": c.path, "sample": first, "threshold_ms": threshold, "slo_pass": first.Error == "" && first.MS < threshold})
			if first.Error != "" || first.MS >= threshold {
				t.Errorf("first request %+v", first)
				return
			}
			for _, concurrency := range []int{1, 4} {
				measured := make([]sample, samples)
				jobs := make(chan int)
				var workers sync.WaitGroup
				before := db.Pool.Stat().AcquireCount()
				for range concurrency {
					workers.Go(func() {
						for i := range jobs {
							measured[i] = measure(c)
						}
					})
				}
				for i := range samples {
					jobs <- i
				}
				close(jobs)
				workers.Wait()
				latencies := make([]float64, samples)
				failed, over := 0, 0
				statuses := map[int]int{}
				for i, s := range measured {
					latencies[i] = s.MS
					statuses[s.Status]++
					if s.Error != "" {
						failed++
					}
					if s.MS >= threshold {
						over++
					}
				}
				sort.Float64s(latencies)
				percentile := func(p int) float64 { return latencies[(len(latencies)*p+99)/100-1] }
				performanceSLOEmit(t, "RESULT", map[string]any{"name": c.name, "path": c.path, "concurrency": concurrency, "samples": measured, "failed": failed, "over_slo": over, "threshold_ms": threshold, "statuses": statuses, "p50_ms": percentile(50), "p95_ms": percentile(95), "p99_ms": percentile(99), "max_ms": latencies[len(latencies)-1], "db_acquires_per_request": float64(db.Pool.Stat().AcquireCount()-before) / float64(samples)})
				if failed > 0 || over > 0 {
					t.Errorf("concurrency=%d failed=%d over=%d", concurrency, failed, over)
					return
				}
			}
		})
	}
}
