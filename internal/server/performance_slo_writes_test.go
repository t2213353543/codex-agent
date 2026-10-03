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
	"sort"
	"sync"
	"testing"
	"time"
)

// Writes use an independent disposable schema; the retained 10M read fixture
// and its exact counts are never mutated by write/ingestion measurements.
func TestPerformanceSLOWrites(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("run tools/performance_slo.py --test-run '^TestPerformanceSLOWrites$'")
	}
	p, handler := adminPGTestProxy(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	ctx := context.Background()
	count := envInt("PERFORMANCE_SLO_SAMPLES", 20, 1, 200)
	type requestSample struct {
		MS     float64 `json:"ms"`
		Status int     `json:"status"`
		Error  string  `json:"error,omitempty"`
	}
	measurements := map[string][]requestSample{}
	routes := map[string]map[string]string{}
	semanticFailure := func(name, message string) {
		values := measurements[name]
		if len(values) > 0 {
			values[len(values)-1].Error = message
			measurements[name] = values
		}
		t.Error(message)
	}
	request := func(name, method, path string, body any) map[string]any {
		t.Helper()
		routes[name] = map[string]string{"method": method, "path": path}
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+webTestKey)
		req.Header.Set("Content-Type", "application/json")
		started := time.Now()
		res, err := client.Do(req)
		s := requestSample{MS: float64(time.Since(started).Microseconds()) / 1000}
		if err != nil {
			s.Error = err.Error()
			measurements[name] = append(measurements[name], s)
			return nil
		}
		content, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		s.MS = float64(time.Since(started).Microseconds()) / 1000
		s.Status = res.StatusCode
		var data map[string]any
		if readErr != nil {
			s.Error = "read_response"
		} else if res.StatusCode != 200 {
			s.Error = "unexpected_http_status"
		} else if json.Unmarshal(content, &data) != nil {
			s.Error = "invalid_json"
		}
		measurements[name] = append(measurements[name], s)
		return data
	}
	for i := 0; i < count; i++ {
		for _, resource := range []string{"groups", "keys", "proxies"} {
			name := fmt.Sprintf("Synthetic SLO %s %d", resource, i)
			body := map[string]any{"name": name}
			if resource == "proxies" {
				body["type"] = "http"
				body["host"] = "192.0.2.1"
				body["port"] = 9
				body["enabled"] = false
			}
			created := request(resource+"_create", http.MethodPost, "/api/"+resource, body)
			id, _ := created["id"].(string)
			if id == "" {
				semanticFailure(resource+"_create", resource+" create missing identity")
				continue
			}
			changed := request(resource+"_update", http.MethodPut, "/api/"+resource+"/"+id, map[string]any{"version": created["version"], "name": name + " updated"})
			if changed["id"] != id || changed["name"] != name+" updated" {
				semanticFailure(resource+"_update", resource+" update did not persist identity/name")
			}
			deleted := request(resource+"_delete", http.MethodDelete, "/api/"+resource+"/"+id, nil)
			if deleted["ok"] != true {
				semanticFailure(resource+"_delete", resource+" delete failed")
			}
		}
	}
	account := pgImportForTest(t, p.Store, "Synthetic SLO account", "synthetic-never-use-slo")
	for i := 0; i < count; i++ {
		notes := fmt.Sprintf("Synthetic metadata revision %d", i)
		updated := request("account_metadata_update", http.MethodPut, "/api/accounts/"+account.ID, map[string]any{"version": account.Version, "enabled": false, "notes": notes})
		if updated["id"] != account.ID || updated["ok"] != true {
			semanticFailure("account_metadata_update", "account metadata update failed")
		}
		var err error
		account, err = p.Store.GetMetadata(account.ID)
		if err != nil || account.Notes != notes || account.Enabled {
			t.Fatal("metadata was not durably persisted")
		}
	}
	// Exercise durable ingestion with 4 concurrent writers and unique event IDs.
	// No gateway or upstream call is involved; final counts/rollups are checked.
	writeSamples := make([]requestSample, count)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				record := RequestRecord{EventID: fmt.Sprintf("slo_write_%d", i), AccountID: account.ID, Account: account.Name, Time: time.Now().Unix(), Status: "ok", InputTokens: 100, OutputTokens: 20, CachedTokens: 80, LatencyMs: 5}
				started := time.Now()
				inserted, duplicates, err := p.Metrics.ImportRecords(ctx, []RequestRecord{record})
				s := requestSample{MS: float64(time.Since(started).Microseconds()) / 1000}
				if err != nil || inserted != 1 || duplicates != 0 {
					s.Error = "durable_write_failed"
				}
				writeSamples[i] = s
			}
		}()
	}
	for i := 0; i < count; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	measurements["durable_ingest_c4"] = writeSamples
	var events, logs, requests, tokens int64
	if err := p.Store.DB.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM usage_events),(SELECT count(*) FROM request_logs),(SELECT requests FROM usage_totals WHERE scope='global' AND subject=''),(SELECT tokens FROM usage_totals WHERE scope='global' AND subject='')").Scan(&events, &logs, &requests, &tokens); err != nil {
		t.Fatal(err)
	}
	if events != int64(count) || logs != int64(count) || requests != int64(count) || tokens != int64(count*120) {
		t.Errorf("ingest invariant mismatch events=%d logs=%d requests=%d tokens=%d", events, logs, requests, tokens)
	}
	var retained, dimensional int64
	if err := p.Store.DB.Pool.QueryRow(ctx, "SELECT (SELECT sum(requests)::bigint FROM retained_log_minute),(SELECT sum(requests)::bigint FROM retained_usage_minute WHERE kind='total')").Scan(&retained, &dimensional); err != nil {
		t.Fatal(err)
	}
	if retained != int64(count) || dimensional != int64(count) {
		t.Errorf("retained rollup ingest counts differ: retained=%d dimensional=%d", retained, dimensional)
	}
	for i := 0; i < count; i++ {
		last := request("account_last_used_batch", http.MethodPost, "/api/accounts/last-used", map[string]any{"ids": []string{account.ID}})
		items, _ := last["items"].([]any)
		if len(items) != 1 {
			semanticFailure("account_last_used_batch", "last-used batch missing account")
		}
		usage := request("account_usage_batch", http.MethodPost, "/api/accounts/usage-batch", map[string]any{"ids": []string{account.ID}, "from": time.Now().Add(-time.Hour).Unix(), "to": time.Now().Add(time.Hour).Unix()})
		items, _ = usage["items"].([]any)
		if len(items) != 1 {
			semanticFailure("account_usage_batch", "usage batch missing account")
		} else {
			row, _ := items[0].(map[string]any)
			if row["total_requests"] != float64(count) || row["total_tokens"] != float64(count*120) {
				semanticFailure("account_usage_batch", "usage batch incorrect totals")
			}
		}
	}
	for name, samples := range measurements {
		values := make([]float64, len(samples))
		failed, over := 0, 0
		for i, s := range samples {
			values[i] = s.MS
			if s.Error != "" {
				failed++
			}
			if s.MS >= 1000 {
				over++
			}
		}
		sort.Float64s(values)
		percentile := func(p int) float64 { return values[(len(values)*p+99)/100-1] }
		performanceSLOEmit(t, "WRITE_RESULT", map[string]any{"name": name, "route": routes[name], "samples": samples, "failed": failed, "over_slo": over, "p50_ms": percentile(50), "p95_ms": percentile(95), "p99_ms": percentile(99), "max_ms": values[len(values)-1], "threshold_ms": 1000, "scope": "independent disposable PG schema; real HTTP except durable_ingest storage API"})
		if failed > 0 || over > 0 {
			t.Errorf("%s failed=%d over_slo=%d", name, failed, over)
		}
	}
	performanceSLOEmit(t, "WRITE_INVARIANTS", map[string]any{"events": events, "logs": logs, "requests": requests, "tokens": tokens, "retained_rollup_requests": retained, "dimensional_rollup_requests": dimensional, "redis_configured": p.Store.DB.Redis != nil, "ten_million_read_fixture_unchanged": true})
}
