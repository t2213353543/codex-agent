package server

import (
	"bytes"
	"context"
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
	"sync"
	"testing"
	"time"
)

// Extends only a previously verified uniform-distribution synthetic schema.
// Migrations/rebuilds require explicit maintenance flags; captured logs/events
// remain unchanged while account documents and aggregates are prepared.
func TestPerformanceUniformHTTP(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("requires explicit isolated uniform fixture")
	}
	ctx := context.Background()
	schema := os.Getenv("PERFORMANCE_SLO_SCHEMA")
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || u.User == nil || u.User.Username() != "codex_agent_test" || u.Path != "/codex_agent_integration" || u.Port() != "25432" || !stringIn(u.Hostname(), "localhost", "127.0.0.1", "::1") || !regexp.MustCompile(`^perf_uniform_[a-z0-9_]+$`).MatchString(schema) {
		t.Fatal("uniform synthetic schema required")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := OpenDatabase(ctx, u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32)), "uniform-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := envInt("PERFORMANCE_SLO_ROWS", 100000, 100000, 10000000)
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 5, 1, 100)
	threshold := 1000.0
	if rows >= 10000000 {
		threshold = 3000
	}
	if os.Getenv("PERFORMANCE_UNIFORM_MIGRATE") == "1" {
		started := time.Now()
		if err = db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		performanceSLOEmit(t, "UNIFORM_MIGRATION", map[string]any{"elapsed_s": time.Since(started).Seconds(), "schema": schema})
	}
	var events, logs, from, to int64
	if err = db.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_events),count(*),floor(extract(epoch FROM min(created_at))/86400)::bigint*86400,(floor(extract(epoch FROM max(created_at))/86400)::bigint+1)*86400 FROM request_logs`).Scan(&events, &logs, &from, &to); err != nil {
		t.Fatal(err)
	}
	if events != int64(rows) || logs != int64(rows) || to-from != 30*86400 {
		t.Fatalf("unexpected uniform fixture rows=%d/%d range=%d", events, logs, to-from)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	maintenance, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = maintenance.Exec(ctx, "SET statement_timeout='30min'"); err != nil {
		t.Fatal(err)
	}
	maintenance.Release()
	var extensionExists bool
	if err = db.Pool.QueryRow(ctx, "SELECT to_regclass('performance_uniform_extend_checkpoint') IS NOT NULL").Scan(&extensionExists); err != nil {
		t.Fatal(err)
	}
	if extensionExists {
		var checkpoint, target int64
		if err = db.Pool.QueryRow(ctx, "SELECT next_i,target_rows FROM performance_uniform_extend_checkpoint").Scan(&checkpoint, &target); err != nil || checkpoint != int64(rows) || target != int64(rows) {
			t.Fatal("uniform append is not complete; no HTTP measurement permitted")
		}
		if os.Getenv("PERFORMANCE_UNIFORM_REBUILD") != "1" {
			var aggregateRows int64
			if err = db.Pool.QueryRow(ctx, "SELECT requests FROM usage_totals WHERE scope='global' AND subject=''").Scan(&aggregateRows); err != nil || aggregateRows != int64(rows) {
				t.Fatal("appended fixture requires real durable aggregate rebuild before HTTP")
			}
		}
	}
	var ready bool
	if err = db.Pool.QueryRow(ctx, "SELECT to_regclass('performance_uniform_http_fixture') IS NOT NULL").Scan(&ready); err != nil {
		t.Fatal(err)
	}
	if !ready {
		started := time.Now()
		performanceSLOEmit(t, "UNIFORM_HTTP_STAGE", map[string]any{"stage": "account_documents", "rows": rows})
		exec(`INSERT INTO accounts(id,display_name,enabled,created_at,updated_at,window_start) SELECT 'uniform_a_'||i,'Synthetic account '||i,false,$1::bigint,$1::bigint,$2::bigint FROM generate_series(1,1000)i`, to, from)
		encrypted, e := db.Encrypt([]byte(`{"access_token":"uniform-synthetic-unusable"}`))
		if e != nil {
			t.Fatal(e)
		}
		exec(`INSERT INTO account_credentials(account_id,credentials_cipher,token_fingerprint,encryption_key_id,updated_at) SELECT id,$1,$2,'uniform-synthetic',$3 FROM accounts`, encrypted, []byte("uniform-synthetic"), to)
		m, e := NewPGMetrics(db)
		if e != nil {
			t.Fatal(e)
		}
		performanceSLOEmit(t, "UNIFORM_HTTP_STAGE", map[string]any{"stage": "rebuild_durable_aggregates", "elapsed_s": time.Since(started).Seconds()})
		if err = m.RebuildAggregates(ctx); err != nil {
			t.Fatal(err)
		}
		performanceSLOEmit(t, "UNIFORM_HTTP_STAGE", map[string]any{"stage": "account_last_used_snapshot", "elapsed_s": time.Since(started).Seconds()})
		exec(`INSERT INTO account_request_state(account_ref,account_name,last_at,last_log_id,record) SELECT a.id,a.display_name,l.created_at,l.id,l.record FROM accounts a CROSS JOIN LATERAL(SELECT created_at,id,record FROM request_logs WHERE account_ref=a.id ORDER BY created_at DESC,id DESC LIMIT 1)l`)
		exec("ANALYZE accounts,account_credentials,account_request_state,usage_minute,usage_daily,usage_totals,usage_hourly_global")
		exec("CREATE TABLE performance_uniform_http_fixture(prepared_at timestamptz NOT NULL DEFAULT now())")
		exec("INSERT INTO performance_uniform_http_fixture DEFAULT VALUES")
		performanceSLOEmit(t, "UNIFORM_HTTP_PREPARE", map[string]any{"elapsed_s": time.Since(started).Seconds(), "accounts": 1000, "durable_aggregates": "actual RebuildAggregates", "logs_events_unchanged": true})
	}
	if os.Getenv("PERFORMANCE_UNIFORM_REBUILD") == "1" {
		var beforeRequests, beforeTokens, legacyBefore int64
		if err = db.Pool.QueryRow(ctx, "SELECT requests,tokens,(SELECT count(*) FROM usage_legacy_baselines) FROM usage_totals WHERE scope='global' AND subject=''").Scan(&beforeRequests, &beforeTokens, &legacyBefore); err != nil {
			t.Fatal(err)
		}
		appended := false
		if beforeRequests != int64(rows) {
			var checkpointExists bool
			if err = db.Pool.QueryRow(ctx, "SELECT to_regclass('performance_uniform_extend_checkpoint') IS NOT NULL").Scan(&checkpointExists); err != nil || !checkpointExists {
				t.Fatal("missing extension checkpoint for changed aggregate base")
			}
			var next, target, original int64
			if err = db.Pool.QueryRow(ctx, "SELECT next_i,target_rows,original_rows FROM performance_uniform_extend_checkpoint").Scan(&next, &target, &original); err != nil || next != int64(rows) || target != int64(rows) || original != beforeRequests {
				t.Fatal("extension checkpoint does not prove intended aggregate advance")
			}
			appended = true
		}
		m, e := NewPGMetrics(db)
		if e != nil {
			t.Fatal(e)
		}
		started := time.Now()
		performanceSLOEmit(t, "UNIFORM_HTTP_STAGE", map[string]any{"stage": "repeat_actual_rebuild", "rows": rows})
		if err = m.RebuildAggregates(ctx); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO account_request_state(account_ref,account_name,last_at,last_log_id,record) SELECT a.id,a.display_name,l.created_at,l.id,l.record FROM accounts a CROSS JOIN LATERAL(SELECT created_at,id,record FROM request_logs WHERE account_ref=a.id ORDER BY created_at DESC,id DESC LIMIT 1)l ON CONFLICT(account_ref) DO UPDATE SET account_name=EXCLUDED.account_name,last_at=EXCLUDED.last_at,last_log_id=EXCLUDED.last_log_id,record=EXCLUDED.record`)
		var afterRequests, afterTokens, hourlyRequests, hourlyTokens, legacyAfter, minuteRequests, minuteTokens int64
		if err = db.Pool.QueryRow(ctx, "SELECT requests,tokens,(SELECT sum(requests)::bigint FROM usage_hourly_global),(SELECT sum(tokens)::bigint FROM usage_hourly_global),(SELECT count(*) FROM usage_legacy_baselines),(SELECT sum(requests)::bigint FROM usage_minute),(SELECT sum(tokens)::bigint FROM usage_minute) FROM usage_totals WHERE scope='global' AND subject=''").Scan(&afterRequests, &afterTokens, &hourlyRequests, &hourlyTokens, &legacyAfter, &minuteRequests, &minuteTokens); err != nil {
			t.Fatal(err)
		}
		performanceSLOEmit(t, "UNIFORM_REBUILD", map[string]any{"elapsed_s": time.Since(started).Seconds(), "appended_fixture": appended, "before_requests": beforeRequests, "after_requests": afterRequests, "before_tokens": beforeTokens, "after_tokens": afterTokens, "hourly_requests": hourlyRequests, "hourly_tokens": hourlyTokens, "minute_requests": minuteRequests, "minute_tokens": minuteTokens, "legacy_rows_before": legacyBefore, "legacy_rows_after": legacyAfter})
		if !appended && (beforeRequests != afterRequests || beforeTokens != afterTokens) || hourlyRequests != minuteRequests || hourlyTokens != minuteTokens || legacyAfter != legacyBefore || afterRequests != int64(rows) || afterTokens != int64(rows)*1399 {
			t.Fatal("rebuild changed durable totals/hourly/legacy invariants")
		}
	}
	db.Pool.Reset()
	if os.Getenv("PERFORMANCE_UNIFORM_MAINTENANCE_ONLY") == "1" {
		performanceSLOEmit(t, "UNIFORM_MAINTENANCE_COMPLETE", map[string]any{"schema": schema, "rows": rows, "http_samples": 0})
		return
	}
	metrics, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewPGKeyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = keys.store.Modify(func(state *keyState) error {
		if len(state.Keys) == 0 {
			state.Keys = []APIKey{{ID: "uniform_key", Name: "Synthetic key", Key: "uniform-synthetic-never-use", Version: 1}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	proxies, err := NewPGProxyStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.initializeAdminKey(ctx, webTestKey); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Proxies: proxies, Keys: keys, Settings: NewPGSettingsStore(db), Metrics: metrics, AdminKey: webTestKey}
	server := httptest.NewServer(p.adminRoutes())
	defer server.Close()
	client := &http.Client{Timeout: 45 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 8}}
	defer client.CloseIdleConnections()
	performanceSLOEmit(t, "FIXTURE", map[string]any{"schema": schema, "rows": map[string]int64{"accounts": 1000, "usage_events": events, "request_logs": logs}, "distribution": "uniform across 43200 minutes; each record has distinct account-minute"})
	relations, e := db.Pool.Query(ctx, `SELECT c.relname,pg_total_relation_size(c.oid),pg_relation_size(c.oid),c.reltuples::bigint FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind='r' ORDER BY c.relname`)
	if e != nil {
		t.Fatal(e)
	}
	storage := []map[string]any{}
	for relations.Next() {
		var name string
		var total, heap, estimate int64
		if e = relations.Scan(&name, &total, &heap, &estimate); e != nil {
			relations.Close()
			t.Fatal(e)
		}
		storage = append(storage, map[string]any{"table": name, "total_bytes": total, "heap_bytes": heap, "estimated_rows": estimate})
	}
	if e = relations.Err(); e != nil {
		relations.Close()
		t.Fatal(e)
	}
	relations.Close()
	performanceSLOEmit(t, "STORAGE", map[string]any{"relations": storage})
	// Independent deterministic event-time oracle. Build once outside sampling;
	// binary searches validate rolling operations totals without extra SQL or
	// assuming the wall clock is frozen between successive requests.
	times := make([]int64, rows)
	errorTimes := make([]int64, 0, rows/10)
	for i := 0; i < rows; i++ {
		value := int64(i)
		at := from + ((value*7919)%43200)*60 + (value*17)%60
		times[i] = at
		if i%10 == 0 {
			errorTimes = append(errorTimes, at)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	sort.Slice(errorTimes, func(i, j int) bool { return errorTimes[i] < errorTimes[j] })
	countRange := func(values []int64, low, high int64) int {
		return sort.Search(len(values), func(i int) bool { return values[i] >= high }) - sort.Search(len(values), func(i int) bool { return values[i] >= low })
	}
	interval := fmt.Sprintf("from=%d&to=%d&step=86400&timezone=UTC", from, to)
	type endpoint struct{ name, path, kind string }
	cases := []endpoint{{"uniform_accounts_50", "/api/accounts/manage?page_size=50", "accounts"}, {"uniform_options_account", "/api/logs/options?field=account", "items"}, {"uniform_options_model", "/api/logs/options?field=model", "model"}, {"uniform_log_stats_30d", "/api/logs/stats?" + interval, "stats"}, {"uniform_overview_30d", "/api/overview?" + interval, "overview"}, {"uniform_operations_24h", "/api/operations?range=24h", "operations"}}
	type sample struct {
		MS                float64 `json:"ms"`
		Status            int     `json:"status"`
		Bytes             int     `json:"bytes"`
		Error             string  `json:"error,omitempty"`
		RollingWindowRows *int    `json:"rolling_window_rows,omitempty"`
	}
	measure := func(c endpoint) sample {
		start := time.Now()
		req, _ := http.NewRequest(http.MethodGet, server.URL+c.path, nil)
		req.Header.Set("Authorization", "Bearer "+webTestKey)
		response, err := client.Do(req)
		if err != nil {
			return sample{MS: float64(time.Since(start).Microseconds()) / 1000, Error: err.Error()}
		}
		body, e := io.ReadAll(response.Body)
		response.Body.Close()
		s := sample{MS: float64(time.Since(start).Microseconds()) / 1000, Status: response.StatusCode, Bytes: len(body)}
		var data map[string]any
		if e != nil || response.StatusCode != 200 || json.Unmarshal(body, &data) != nil {
			s.Error = "response_failure"
			return s
		}
		switch c.kind {
		case "accounts", "items":
			items, _ := data[c.kind].([]any)
			if len(items) != 50 {
				s.Error = "incorrect_list_size"
			}
		case "stats":
			if data["total_requests"] != float64(rows) || data["errors"] != float64(rows/10) || data["total_tokens"] != float64(rows*1399) {
				s.Error = "incorrect_stats_totals"
			}
		case "overview":
			cards, _ := data["cards"].(map[string]any)
			if cards["requests"] != float64(rows) || cards["accounts"] != float64(1000) || cards["tokens"] != float64(rows*1399) {
				s.Error = "incorrect_overview_totals"
			}
		case "model":
			items, _ := data["items"].([]any)
			if len(items) != 1 {
				s.Error = "incorrect_model_options"
			}
		case "operations":
			traffic, _ := data["traffic"].(map[string]any)
			low, lowOK := data["from"].(float64)
			high, highOK := data["to"].(float64)
			if lowOK && highOK {
				expected := countRange(times, int64(low), int64(high))
				s.RollingWindowRows = &expected
			}
			if !lowOK || !highOK || traffic["requests"] != float64(countRange(times, int64(low), int64(high))) || traffic["errors"] != float64(countRange(errorTimes, int64(low), int64(high))) {
				s.Error = "incorrect_operations_traffic"
			}
		}
		return s
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first := measure(c)
			performanceSLOEmit(t, "FIRST", map[string]any{"name": c.name, "path": c.path, "sample": first, "threshold_ms": threshold, "slo_pass": first.Error == "" && first.MS < threshold, "phase": "first endpoint request, OS/PG buffers uncontrolled"})
			if first.Error != "" || first.MS >= threshold {
				t.Errorf("first request failed/over %.0fms: %+v", threshold, first)
				return
			}
			for _, concurrency := range []int{1, 4} {
				measured := make([]sample, samples)
				jobs := make(chan int)
				var wg sync.WaitGroup
				for range concurrency {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := range jobs {
							measured[i] = measure(c)
						}
					}()
				}
				for i := range samples {
					jobs <- i
				}
				close(jobs)
				wg.Wait()
				ordered := make([]float64, samples)
				failed, over := 0, 0
				statuses := map[int]int{}
				for i, s := range measured {
					ordered[i] = s.MS
					statuses[s.Status]++
					if s.Error != "" {
						failed++
					}
					if s.MS >= threshold {
						over++
					}
				}
				sort.Float64s(ordered)
				percentile := func(p int) float64 { return ordered[(samples*p+99)/100-1] }
				performanceSLOEmit(t, "RESULT", map[string]any{"name": c.name, "path": c.path, "samples": measured, "concurrency": concurrency, "failed": failed, "over_slo": over, "statuses": statuses, "p50_ms": percentile(50), "p95_ms": percentile(95), "p99_ms": percentile(99), "max_ms": ordered[samples-1], "threshold_ms": threshold})
				if failed > 0 || over > 0 {
					t.Errorf("warm failed=%d over_slo=%d", failed, over)
					return
				}
			}
		})
	}
}
