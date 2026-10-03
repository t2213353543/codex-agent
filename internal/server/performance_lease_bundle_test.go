package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Full loopback HTTP, including auth, encrypted receipt persistence and response
// JSON. Setup is outside timing; lease policy never calls an upstream provider.
func TestPerformanceLeaseBundlePaths(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic performance runner required")
	}
	p, handler := adminPGTestProxy(t)
	deny := new(localWriteNoUpstream)
	p.Client = &http.Client{Transport: deny}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 40 * time.Second}
	defer client.CloseIdleConnections()
	functionalOnly := os.Getenv("PERFORMANCE_LOCAL_WRITE_FUNCTIONAL") == "1"
	interference := os.Getenv("PERFORMANCE_TEST_INTERFERENCE")
	sizes := []int{100, 1000, 10000}
	if raw := os.Getenv("PERFORMANCE_IMPORT_SIZES"); raw != "" {
		sizes = nil
		for _, part := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(part)
			if err != nil || n < 100 || n > maxAccountImportRows {
				t.Fatal("lease fixture sizes must be within 100..10000")
			}
			sizes = append(sizes, n)
		}
	}
	perform := func(name, path string, body any, validate func([]byte) bool) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+webTestKey)
		request.Header.Set("Content-Type", "application/json")
		before := p.Store.DB.Pool.Stat().AcquireCount()
		start := time.Now()
		response, err := client.Do(request)
		var data []byte
		status, failure := 0, ""
		if err != nil {
			failure = "http_failure"
		} else {
			status = response.StatusCode
			data, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || status != http.StatusOK {
				failure = "http_or_read_failure"
			}
		}
		ms := float64(time.Since(start).Microseconds()) / 1000
		acquires := p.Store.DB.Pool.Stat().AcquireCount() - before
		if failure == "" && !validate(data) {
			failure = "lease_contract_changed"
		}
		performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{
			"name": name, "method": http.MethodPost, "path": path, "request_bytes": len(raw), "response_bytes": len(data),
			"sample":       map[string]any{"ms": ms, "status": status, "error": failure, "db_acquires": acquires},
			"threshold_ms": 1000, "functional_only": functionalOnly, "interference": interference,
			"slo_pass": !functionalOnly && interference == "" && failure == "" && ms < 1000,
		})
		if failure != "" {
			t.Fatalf("%s: %s HTTP%d", name, failure, status)
		}
		if !functionalOnly && interference == "" && ms >= 1000 {
			t.Errorf("%s %.3fms exceeds 1s", name, ms)
		}
	}
	for _, count := range sizes {
		items := make([]ImportItem, count)
		bundle := managedAccountBundle{Type: "codex-agent-accounts", Version: 1, Management: &managedBundleManagement{Version: 1}}
		for i := range items {
			name := fmt.Sprintf("lease-scale-%d-%d", count, i)
			raw, _ := json.Marshal(map[string]any{"access_token": "synthetic-" + name, "refresh_token": "synthetic-refresh-" + name, "expires_at": int64(2100000000)})
			items[i] = ImportItem{Name: name, Credentials: raw}
			bundle.Accounts = append(bundle.Accounts, managedAccountBackup{Name: name, Enabled: true, Concurrency: 1, Weight: 1, WindowDays: 1, Credentials: raw})
			bundle.Management.Leases = append(bundle.Management.Leases, managedBundleLease{AccountIndex: i + 1, ExpiresAt: 2100000000, AutoDisable: true, RestoreOnExtend: true})
		}
		report, err := p.Store.ImportDetailedContext(t.Context(), items, AccountDefaults{}, ImportOptions{})
		if err != nil || report.Added != count {
			t.Fatal("lease fixture accounts", report.Added, err)
		}
		selection := make([]managedAccountItem, count)
		rows := make([][]any, count)
		for i, result := range report.Results {
			selection[i] = managedAccountItem{ID: result.ID, Version: 1}
			key, _ := managementStateKey("account-leases", result.ID)
			policy, _ := json.Marshal(managedAccountLease{Version: 1, ExpiresAt: 2100000000, AutoDisable: true, RestoreOnExtend: true})
			entry, _ := json.Marshal(managementStateEntry{Version: 1, UpdatedAt: time.Now().Unix(), Payload: policy})
			cipher, err := p.Store.DB.Encrypt(entry)
			if err != nil {
				t.Fatal(err)
			}
			envelope, _ := json.Marshal(map[string]any{"ciphertext": cipher})
			rows[i] = []any{key, envelope}
		}
		if copied, err := p.Store.DB.Pool.CopyFrom(t.Context(), pgx.Identifier{"settings"}, []string{"key", "value"}, pgx.CopyFromRows(rows)); err != nil || copied != int64(count) {
			t.Fatal("lease fixture policy", copied, err)
		}
		for iteration := range envInt("PERFORMANCE_SLO_SAMPLES", 3, 1, 20) {
			var imported struct {
				ImportReport
				LeaseRestore managedLeaseRestoreReceipt `json:"lease_restore"`
			}
			perform(fmt.Sprintf("lease_prepare_%d", count), "/api/accounts/import-bundle", map[string]any{"data": bundle}, func(data []byte) bool {
				if json.Unmarshal(data, &imported) != nil || imported.Duplicate != count || imported.Failed != 0 || imported.LeaseRestore.ID == "" || len(imported.LeaseRestore.Items) != count {
					return false
				}
				for _, item := range imported.LeaseRestore.Items {
					if item.Status != "pending" || item.LeaseVersion < 1 || !item.Replaces {
						return false
					}
				}
				return true
			})
			targets := make([]managedLeaseRestoreTarget, 100)
			for i, item := range imported.LeaseRestore.Items[count-100:] {
				targets[i] = managedLeaseRestoreTarget{item.AccountIndex, item.ID, item.AccountVersion, item.LeaseVersion}
			}
			perform(fmt.Sprintf("lease_restore_100_receipt_%d", count), "/api/accounts/import-leases", map[string]any{"receipt_id": imported.LeaseRestore.ID, "items": targets, "confirm_restore": true, "confirm_replace": true}, func(data []byte) bool {
				var result managedLeaseRestoreReceipt
				if json.Unmarshal(data, &result) != nil || len(result.Items) != 100 {
					return false
				}
				for _, item := range result.Items {
					if item.Status != "applied" || item.ResultVersion != 1 || item.ResultLease != int64(iteration+2) {
						return false
					}
				}
				return true
			})
			perform(fmt.Sprintf("lease_export_%d", count), "/api/accounts/export/credentials", map[string]any{"items": selection, "include_leases": true}, func(data []byte) bool {
				var result managedAccountBundle
				return json.Unmarshal(data, &result) == nil && len(result.Accounts) == count && result.Management != nil && len(result.Management.Leases) == count
			})
		}
	}
	if deny.calls.Load() != 0 {
		t.Fatal("lease endpoint attempted provider request")
	}
}
