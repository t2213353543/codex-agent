package server

import (
	"bytes"
	"context"
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

const resourceDurabilityValidationSQL = `SELECT
 (SELECT count(*) FROM groups WHERE id=ANY($1::text[]))+(SELECT count(*) FROM proxies WHERE id=ANY($1::text[])),
 (SELECT count(*) FROM audit_logs WHERE entity_id=ANY($1::text[]) AND action IN ('group.create','proxy.restore')),
 (SELECT count(DISTINCT x) FROM (SELECT xmin::text AS x FROM groups WHERE id=ANY($1::text[]) UNION ALL SELECT xmin::text FROM proxies WHERE id=ANY($1::text[])) q)`

func resourceValidationError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

// This is phase-4 evidence only: it exercises migration 018 and the new
// per-item-commit pipeline. Earlier frozen artifact measurements do not prove
// its behavior or latency. No provider request is needed or allowed.
func TestPerformanceResourceBundleImports(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic performance runner required")
	}
	p, handler := adminPGTestProxy(t)
	deny := new(localWriteNoUpstream)
	p.Client = &http.Client{Transport: deny}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	functionalOnly := os.Getenv("PERFORMANCE_LOCAL_WRITE_FUNCTIONAL") == "1"
	interference := os.Getenv("PERFORMANCE_TEST_INTERFERENCE")
	sizes := []int{100, 1000, 10000}
	if raw := os.Getenv("PERFORMANCE_RESOURCE_IMPORT_SIZES"); raw != "" {
		sizes = nil
		for _, part := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(part)
			if err != nil || n < 2 || n > 10000 {
				t.Fatal("PERFORMANCE_RESOURCE_IMPORT_SIZES must contain counts from 2 through 10000")
			}
			sizes = append(sizes, n)
		}
	}
	for _, count := range sizes {
		for _, kind := range []string{"groups", "proxies", "mixed"} {
			for iteration := range envInt("PERFORMANCE_SLO_SAMPLES", 3, 1, 20) {
				groups, proxies := count, 0
				if kind == "proxies" {
					groups, proxies = 0, count
				} else if kind == "mixed" {
					groups, proxies = count/2, count-count/2
				}
				prefix := fmt.Sprintf("perf-r-%s-%d-%d", kind, count, iteration)
				input := managedResourcePipelineFixture(groups, proxies, prefix)
				raw, _ := json.Marshal(input)
				request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/accounts/import-resources", bytes.NewReader(raw))
				request.Header.Set("Authorization", "Bearer "+webTestKey)
				request.Header.Set("Content-Type", "application/json")
				before := p.Store.DB.Pool.Stat().AcquireCount()
				start := time.Now()
				response, err := client.Do(request)
				var result managedBundleResourceReply
				var data []byte
				status, failure := 0, ""
				if err != nil {
					failure = "http_failure"
				} else {
					status = response.StatusCode
					var readErr error
					data, readErr = io.ReadAll(response.Body)
					response.Body.Close()
					if readErr != nil || status != http.StatusOK || json.Unmarshal(data, &result) != nil {
						failure = "http_or_json_failure"
					}
				}
				ms := float64(time.Since(start).Microseconds()) / 1000
				acquires := p.Store.DB.Pool.Stat().AcquireCount() - before
				if failure == "" && (result.Succeeded != count || result.Failed != 0 || len(result.Results) != count || len(result.GroupMapping) != groups || len(result.ProxyMapping) != proxies) {
					failure = "resource_result_mismatch"
				}
				if failure == "" && (bytes.Contains(data, []byte("synthetic-resource-password")) || bytes.Contains(data, []byte("synthetic-resource-user"))) {
					failure = "response_credentials_leaked"
				}
				ids := make([]string, 0, len(result.Results))
				for _, row := range result.Results {
					if row.Status != "created" || row.ID == "" {
						failure = "resource_item_not_created"
					}
					ids = append(ids, row.ID)
				}
				var stored, auditRows, transactions int
				var validationOriginalMS, validationFreshMS float64
				var validationOriginalError, validationFreshError string
				validationRecovered := false
				if failure == "" {
					validationCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					validationStart := time.Now()
					validationErr := p.Store.DB.Pool.QueryRow(validationCtx, resourceDurabilityValidationSQL, ids).Scan(&stored, &auditRows, &transactions)
					cancel()
					validationOriginalMS = float64(time.Since(validationStart).Microseconds()) / 1000
					validationOriginalError = resourceValidationError(validationErr)
					if validationErr != nil || stored != count || auditRows != count || transactions != count {
						freshStored, freshAudits, freshTransactions := 0, 0, 0
						freshCtx, freshCancel := context.WithTimeout(context.Background(), 20*time.Second)
						freshStart := time.Now()
						freshErr := p.Store.DB.Pool.QueryRow(freshCtx, resourceDurabilityValidationSQL, pgx.QueryExecModeExec, ids).Scan(&freshStored, &freshAudits, &freshTransactions)
						freshCancel()
						validationFreshMS = float64(time.Since(freshStart).Microseconds()) / 1000
						validationFreshError = resourceValidationError(freshErr)
						if freshErr == nil && freshStored == count && freshAudits == count && freshTransactions == count {
							stored, auditRows, transactions = freshStored, freshAudits, freshTransactions
							validationRecovered = true
						} else {
							failure = "durable_count_audit_or_commit_boundary_mismatch"
						}
					}
					for _, original := range input.Resources.Proxies {
						p.Proxies.mu.RLock()
						cached := p.Proxies.index[result.ProxyMapping[original.ID]]
						valid := cached != nil && cached.Password == original.Password && cached.Username == original.Username && cached.Enabled == original.Enabled
						p.Proxies.mu.RUnlock()
						if !valid {
							failure = "runtime_proxy_cache_mismatch"
							break
						}
					}
				}
				performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{
					"name": fmt.Sprintf("resource_import_%s_%d", kind, count), "method": http.MethodPost, "path": "/api/accounts/import-resources",
					"sample":       map[string]any{"ms": ms, "status": status, "error": failure, "db_acquires": acquires, "validation_original_ms": validationOriginalMS, "validation_fresh_ms": validationFreshMS, "validation_original_error": validationOriginalError, "validation_fresh_error": validationFreshError, "validation_recovered": validationRecovered, "observed_stored": stored, "observed_audits": auditRows, "observed_transactions": transactions},
					"threshold_ms": 1000, "slo_pass": !functionalOnly && interference == "" && failure == "" && ms < 1000,
					"functional_only": functionalOnly, "interference": interference, "request_bytes": len(raw),
					"rows": count, "groups": groups, "proxies": proxies, "commit_semantics": "one independent transaction per item",
				})
				if failure != "" {
					t.Fatalf("resource import %s %d: %s (HTTP %d)", kind, count, failure, status)
				}
				if !functionalOnly && interference == "" && ms >= 1000 {
					t.Errorf("resource import %s %d %.3fms exceeds 1s", kind, count, ms)
				}
			}
		}
	}
	if deny.calls.Load() != 0 {
		t.Fatal("resource import attempted provider requests")
	}
}

// A single locally rejected or explicitly mapped row must not turn the other
// independent proxy creates into one full catalog decrypt per item.
func TestPerformanceResourceBundleMixedValidity(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic performance runner required")
	}
	sizes := []int{100, 1000}
	if raw := os.Getenv("PERFORMANCE_RESOURCE_IMPORT_SIZES"); raw != "" {
		sizes = nil
		for _, part := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(part)
			if err != nil || n < 2 || n > 10000 {
				t.Fatal("invalid import size")
			}
			sizes = append(sizes, n)
		}
	}
	for _, count := range sizes {
		for _, variant := range []string{"invalid", "mapped", "conflict"} {
			t.Run(fmt.Sprintf("%s_%d", variant, count), func(t *testing.T) {
				p, handler := adminPGTestProxy(t)
				deny := new(localWriteNoUpstream)
				p.Client = &http.Client{Transport: deny}
				input := managedResourcePipelineFixture(0, count, fmt.Sprintf("mixedvalid-%s-%d", variant, count))
				mid := count / 2
				expectedCreated, expectedSucceeded, expectedCode := count-1, count-1, "invalid_resource"
				if variant == "invalid" {
					input.Resources.Proxies[mid].Port = 0
				} else {
					existing, err := p.Proxies.Create(ProxyEntry{Name: input.Resources.Proxies[mid].Name, Type: "http", Host: "127.0.0.1", Port: 4321, Password: "existing-synthetic"})
					if err != nil {
						t.Fatal(err)
					}
					if variant == "mapped" {
						input.ProxyMapping = map[string]string{input.Resources.Proxies[mid].ID: existing.ID}
						expectedSucceeded = count
						expectedCode = ""
					} else {
						expectedCode = "name_conflict"
					}
				}
				server := httptest.NewServer(handler)
				defer server.Close()
				client := &http.Client{Timeout: 30 * time.Second}
				defer client.CloseIdleConnections()
				raw, _ := json.Marshal(input)
				request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/accounts/import-resources", bytes.NewReader(raw))
				request.Header.Set("Authorization", "Bearer "+webTestKey)
				request.Header.Set("Content-Type", "application/json")
				before := p.Store.DB.Pool.Stat().AcquireCount()
				started := time.Now()
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				ms := float64(time.Since(started).Microseconds()) / 1000
				acquires := p.Store.DB.Pool.Stat().AcquireCount() - before
				var result managedBundleResourceReply
				if err != nil || response.StatusCode != 200 || json.Unmarshal(body, &result) != nil || len(result.Results) != count || result.Succeeded != expectedSucceeded || result.Failed != count-expectedSucceeded {
					t.Fatalf("mixed-validity response mismatch: status=%d result=%+v err=%v", response.StatusCode, result, err)
				}
				if result.Results[mid].Code != expectedCode || variant == "mapped" && result.Results[mid].Status != "mapped" {
					t.Fatalf("selected row changed: %+v", result.Results[mid])
				}
				ids := make([]string, 0, expectedCreated)
				for i, row := range result.Results {
					if i == mid {
						continue
					}
					if row.Status != "created" || row.ID == "" {
						t.Fatalf("independent row%dfailed: %+v", i, row)
					}
					ids = append(ids, row.ID)
				}
				var stored, audits, transactions int
				if err := p.Store.DB.Pool.QueryRow(t.Context(), resourceDurabilityValidationSQL, pgx.QueryExecModeExec, ids).Scan(&stored, &audits, &transactions); err != nil || stored != expectedCreated || audits != expectedCreated || transactions != expectedCreated {
					t.Fatalf("durability or independent commits changed: stored=%d audits=%d tx=%d err=%v", stored, audits, transactions, err)
				}
				if deny.calls.Load() != 0 || bytes.Contains(body, []byte("synthetic-resource-password")) {
					t.Fatal("unrelated upstream call or credential leak")
				}
				performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{"name": fmt.Sprintf("resource_mixed_validity_%s_%d", variant, count), "method": http.MethodPost, "path": "/api/accounts/import-resources", "rows": count, "variant": variant, "sample": map[string]any{"ms": ms, "status": response.StatusCode, "error": "", "db_acquires": acquires, "observed_stored": stored, "observed_audits": audits, "observed_transactions": transactions}, "threshold_ms": 1000, "slo_pass": ms < 1000, "commit_semantics": "one independent transaction per created item"})
				if ms >= 1000 {
					t.Errorf("mixed-validity import %s/%d %.3fms exceeds1s", variant, count, ms)
				}
			})
		}
	}
}
