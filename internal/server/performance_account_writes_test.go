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
)

// Independent schema, synthetic credentials, no provider transport. This file
// only calls public pre-existing handlers so it can overlay the 41d1 baseline.
func TestPerformanceAccountBulkWrites(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated performance runner required")
	}
	p, handler := adminPGTestProxy(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 3, 1, 20)
	functionalOnly := os.Getenv("PERFORMANCE_LOCAL_WRITE_FUNCTIONAL") == "1"
	sizes := []int{100, 1000}
	if raw := os.Getenv("PERFORMANCE_IMPORT_SIZES"); raw != "" {
		sizes = nil
		for _, value := range strings.Split(raw, ",") {
			count, err := strconv.Atoi(value)
			if err != nil || count < 1 || count > maxAccountImportRows {
				t.Fatal("PERFORMANCE_IMPORT_SIZES must contain supported import counts")
			}
			sizes = append(sizes, count)
		}
	}
	type sample struct {
		MS           float64 `json:"ms"`
		Status       int     `json:"status"`
		Error        string  `json:"error,omitempty"`
		ReadAcquires int64   `json:"db_acquires"`
	}
	perform := func(name, path string, body any, added, duplicates, failed int) (map[string]any, sample) {
		t.Helper()
		raw, _ := json.Marshal(body)
		request, _ := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+webTestKey)
		request.Header.Set("Content-Type", "application/json")
		before := p.Store.DB.Pool.Stat().AcquireCount()
		start := time.Now()
		response, err := client.Do(request)
		s := sample{}
		if err != nil {
			s.Error = err.Error()
			return nil, s
		}
		data, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		s.MS = float64(time.Since(start).Microseconds()) / 1000
		s.Status = response.StatusCode
		s.ReadAcquires = p.Store.DB.Pool.Stat().AcquireCount() - before
		var value map[string]any
		if readErr != nil || response.StatusCode != http.StatusOK || json.Unmarshal(data, &value) != nil {
			s.Error = "http_or_json_failure"
		} else if value["added"] != float64(added) || value["duplicate"] != float64(duplicates) || value["failed"] != float64(failed) {
			s.Error = "import_semantics_changed"
		}
		performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{"name": name, "method": "POST", "path": path, "sample": s, "threshold_ms": 1000, "slo_pass": !functionalOnly && s.Error == "" && s.MS < 1000, "functional_only": functionalOnly, "interference": os.Getenv("PERFORMANCE_TEST_INTERFERENCE"), "request_bytes": len(raw)})
		if s.Error != "" {
			t.Errorf("%s: %+v body=%s", name, s, string(data))
		}
		if !functionalOnly && s.MS >= 1000 {
			t.Errorf("%s %.2fms exceeds 1s", name, s.MS)
		}
		return value, s
	}
	for _, count := range sizes {
		for iteration := range samples {
			rows := make([]map[string]any, count)
			for i := range rows {
				name := fmt.Sprintf("http-import-%d-%d-%d", count, iteration, i)
				rows[i] = map[string]any{"name": name, "access_token": "synthetic-" + name, "refresh_token": "refresh-" + name}
			}
			body := map[string]any{"accounts": rows}
			var before int
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM accounts").Scan(&before); err != nil {
				t.Fatal(err)
			}
			perform(fmt.Sprintf("import_preview_%d", count), "/api/accounts/import/preview", body, count, 0, 0)
			var after int
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM accounts").Scan(&after); err != nil || after != before {
				t.Fatalf("preview changed accounts %d/%d %v", before, after, err)
			}
			perform(fmt.Sprintf("import_new_%d", count), "/api/accounts", body, count, 0, 0)
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM accounts").Scan(&after); err != nil || after != before+count {
				t.Fatalf("bulk import missing rows %d/%d %v", before, after, err)
			}
			perform(fmt.Sprintf("import_duplicate_%d", count), "/api/accounts", body, 0, count, 0)
			mixed := append([]map[string]any(nil), rows...)
			name := fmt.Sprintf("http-mixed-new-%d-%d", count, iteration)
			mixed[len(mixed)-1] = map[string]any{"name": name, "access_token": "synthetic-" + name, "refresh_token": "refresh-" + name}
			perform(fmt.Sprintf("import_mixed_%d", count), "/api/accounts", map[string]any{"accounts": mixed}, 1, count-1, 0)
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM accounts").Scan(&after); err != nil || after != before+count+1 {
				t.Fatalf("mixed batch row count=%d want=%d err=%v", after, before+count+1, err)
			}
		}
	}
}

func TestPerformanceAccountBulkMetadata(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated performance runner required")
	}
	p, handler := adminPGTestProxy(t)
	for i := range 100 {
		raw, _ := json.Marshal(map[string]string{"access_token": fmt.Sprintf("synthetic-metadata-%d", i)})
		if err := p.Store.Import(fmt.Sprintf("bulk-metadata-%d", i), raw); err != nil {
			t.Fatal(err)
		}
	}
	_, accounts, err := p.Store.ListMetadata()
	if err != nil || len(accounts) != 100 {
		t.Fatalf("fixture: %d %v", len(accounts), err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	for iteration := range envInt("PERFORMANCE_SLO_SAMPLES", 3, 1, 20) {
		for _, kind := range []string{"actions_update_100", "batch_update_100"} {
			items := make([]map[string]any, len(accounts))
			patch := map[string]any{"enabled": iteration%2 == 0, "notes": fmt.Sprintf("metadata revision %d", iteration)}
			path := "/api/accounts/actions"
			for i, a := range accounts {
				items[i] = map[string]any{"id": a.ID, "version": a.Version}
				if kind == "batch_update_100" {
					items[i]["patch"] = patch
				}
			}
			body := map[string]any{"action": "update", "items": items, "patch": patch}
			if kind == "batch_update_100" {
				body = map[string]any{"items": items}
				path = "/api/accounts/batch"
			}
			raw, _ := json.Marshal(body)
			request, _ := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(raw))
			request.Header.Set("Authorization", "Bearer "+webTestKey)
			request.Header.Set("Content-Type", "application/json")
			start := time.Now()
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			ms := float64(time.Since(start).Microseconds()) / 1000
			var result struct {
				Succeeded int `json:"succeeded"`
				Failed    int `json:"failed"`
			}
			valid := readErr == nil && response.StatusCode == 200 && json.Unmarshal(data, &result) == nil && result.Succeeded == 100 && result.Failed == 0
			performanceSLOEmit(t, "ACCOUNT_WRITE", map[string]any{"name": kind, "method": "POST", "path": path, "sample": map[string]any{"ms": ms, "status": response.StatusCode}, "threshold_ms": 1000, "semantic_pass": valid, "slo_pass": valid && ms < 1000})
			if !valid {
				t.Fatalf("metadata result invalid: %d %s", response.StatusCode, data)
			}
			if ms >= 1000 {
				t.Errorf("%s %.2fms exceeds1s", kind, ms)
			}
			_, next, err := p.Store.ListMetadata()
			if err != nil {
				t.Fatal(err)
			}
			byID := map[string]Account{}
			for _, a := range accounts {
				byID[a.ID] = a
			}
			for _, a := range next {
				prior := byID[a.ID]
				if a.Version != prior.Version+1 || a.Notes != patch["notes"] || a.Enabled != patch["enabled"] {
					t.Fatal("metadata not durably persisted")
				}
			}
			accounts = next
		}
	}
}
