package server

import (
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

	"github.com/jackc/pgx/v5/pgxpool"
)

// Opt-in next-stage baseline. Each account has both model values, so neither
// a constant-dimension proof nor account narrowing can answer the joint query.
// It creates only a disposable synthetic schema and uses the real stats method.
// Preparing/compiling this test does not create data or imply an SLO result.
func TestPerformanceMixedModelStatsBaseline(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("opt-in isolated mixed-model baseline")
	}
	count := envInt("PERFORMANCE_SLO_ROWS", 100000, 2000, 1000000)
	if count%2000 != 0 {
		t.Fatal("mixed-model fixture size must be divisible by 2000")
	}
	db := accountPGTestStore(t).DB
	ctx := t.Context()
	const from int64 = 1790553600 - 30*86400
	const to int64 = 1790553600
	seedStart := time.Now()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO accounts(id,display_name,enabled,created_at,updated_at) SELECT 'mixed-account-'||i,'Synthetic account '||i,false,$1::bigint,$1::bigint FROM generate_series(0,999)i`, to); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, "INSERT INTO groups(id,name) VALUES('shared','Synthetic shared group')"); err != nil {
		t.Fatal(err)
	}
	for first := 0; first < count; first += 25000 {
		last := min(first+24999, count-1)
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout='5min'"); err == nil {
			_, err = tx.Exec(ctx, `WITH input AS (
 SELECT i,'mixed-'||i AS event_id,to_timestamp($3::bigint+((i*7919)%43200)*60+(i*17)%60) AS created_at,
 'mixed-account-'||(i%1000) AS account_ref,'Synthetic account '||(i%1000) AS account_name,
 CASE WHEN (i/1000)%2=0 THEN 'mix-a' ELSE 'mix-b' END AS model,
 CASE WHEN i%10=0 THEN 'error' ELSE 'ok' END AS status
 FROM generate_series($1::bigint,$2::bigint)i
 ), events AS (
 INSERT INTO usage_events(event_id,created_at,account_ref,account_name,key_id,status,input_tokens,output_tokens,cached_tokens,latency_ms,latency_known)
 SELECT event_id,created_at,account_ref,account_name,'mixed-key',status,101,13,37,21,true FROM input RETURNING *
 ) INSERT INTO request_logs(event_id,account_ref,account_name,created_at,status,model,key_name,record)
 SELECT e.event_id,e.account_ref,e.account_name,e.created_at,e.status,i.model,'Synthetic key',
 jsonb_build_object('event_id',e.event_id,'account_id',e.account_ref,'account',e.account_name,'time',extract(epoch FROM e.created_at)::bigint,
 'status',e.status,'model',i.model,'key_id','mixed-key','key_name','Synthetic key','groups',jsonb_build_array('shared','shared'),
 'endpoint','responses','input_tokens',101,'output_tokens',13,'cached_tokens',37,'latency_ms',21,'latency_known',true,'usage_known',true,
 'cached_tokens_known',true,'http_code',CASE WHEN e.status='ok' THEN 200 ELSE 429 END)
 FROM events e JOIN input i USING(event_id)`, first, last, from)
		}
		if err != nil {
			tx.Rollback(context.Background())
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		performanceSLOEmit(t, "MIXED_SEED_PROGRESS", map[string]any{"rows": last + 1, "target": count, "elapsed_s": time.Since(seedStart).Seconds()})
	}
	if _, err := db.Pool.Exec(ctx, "ANALYZE request_logs; ANALYZE retained_usage_minute; ANALYZE retained_usage_daily"); err != nil {
		t.Fatal(err)
	}
	var matching, accountsWithBoth int64
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE model='mix-a'),
 (SELECT count(*) FROM (SELECT account_ref FROM request_logs GROUP BY account_ref HAVING count(DISTINCT model)=2)a)
 FROM request_logs`).Scan(&matching, &accountsWithBoth); err != nil || matching != int64(count/2) || accountsWithBoth != 1000 {
		t.Fatalf("fixture did not isolate the intended mixed case: matching=%d accounts_with_both=%d err=%v", matching, accountsWithBoth, err)
	}
	t.Logf("MIXED_STATS_FIXTURE rows=%d matching=%d accounts_with_both=%d seed_seconds=%.3f", count, matching, accountsWithBoth, time.Since(seedStart).Seconds())
	performanceSLOEmit(t, "MIXED_FIXTURE", map[string]any{"rows": count, "matching": matching, "accounts_with_both_models": accountsWithBoth, "seed_elapsed_s": time.Since(seedStart).Seconds(), "scope": "independent synthetic schema, 1000 accounts each has both models; no upstream calls"})
	trace := &managedFilteredStatsPlanTrace{}
	config := db.Pool.Config()
	config.MaxConns = 1
	config.ConnConfig.Tracer = trace
	config.ConnConfig.RuntimeParams["jit"] = "off"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "120000"
	config.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_generic_plan"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	m := &Metrics{db: &Database{Pool: pool}}
	for _, selection := range []string{"model=mix-a", "model=mix-a&key_id=mixed-key&group_id=shared"} {
		q := managedQuery(t, fmt.Sprintf("%s&from=%d&to=%d&step=86400&timezone=UTC", selection, from, to))
		gate, args, eligible := q.rollupFilterGateSQL()
		if !eligible {
			t.Fatal("mixed baseline gate unexpectedly unavailable")
		}
		var matched, different bool
		if err := pool.QueryRow(ctx, gate, args...).Scan(&matched, &different); err != nil || !matched || !different {
			t.Fatalf("mixed fixture incorrectly proves full coverage: %v %v %v", matched, different, err)
		}
		performanceSLOEmit(t, "MIXED_GLOBAL_GATE", map[string]any{"selection": selection, "matched": matched, "different": different, "interpretation": "global model is mixed; this gate alone cannot prove the whole dataset matches; inspect actual SQL source and plan for model-conditioned summaries"})
		start := time.Now()
		stats, err := m.managedLogStats(ctx, q)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("mixed stats %s after %s: %v", selection, elapsed, err)
		}
		want := int64(count / 2)
		if stats.Requests != want || stats.Input != want*101 || stats.Output != want*13 || stats.Cached != want*37 || stats.Errors != want/10 || stats.UsageKnownRequests != want || stats.LatencySamples != want || stats.AverageLatency == nil || *stats.AverageLatency != 21 || stats.CacheReadSamples != want || stats.CacheReadInputTokens != want*101 || stats.CacheReadTokens != want*37 || len(stats.Models) != 1 || stats.Models[0].Name != "mix-a" || len(stats.Keys) != 1 || len(stats.Groups) != 1 || stats.Groups[0].Requests != want || len(stats.Accounts) != 1000 {
			t.Fatalf("mixed aggregate lost exact counters/dimensions: requests=%d errors=%d accounts=%d", stats.Requests, stats.Errors, len(stats.Accounts))
		}
		var accountRequests, seriesRequests int64
		for _, a := range stats.Accounts {
			if a.Requests != want/1000 {
				t.Fatalf("cross-account model selection incorrect: %s=%d", a.ID, a.Requests)
			}
			accountRequests += a.Requests
		}
		for _, point := range stats.Series {
			seriesRequests += point.Requests
		}
		if accountRequests != want || seriesRequests != want {
			t.Fatal("marginal output does not sum to exact filtered request count")
		}
		statement, parameters := trace.statement, append([]any(nil), trace.args...)
		if statement == "" {
			t.Fatal("actual mixed stats statement was not captured")
		}
		var plan json.RawMessage
		if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) "+statement, parameters...).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		evidence, _ := json.Marshal(map[string]any{"selection": selection, "rows": count, "matching": want, "storage_method_ms": float64(elapsed.Microseconds()) / 1000, "plan": plan, "sql": statement, "args": parameters, "scope": "actual filtered stats storage method; excludes HTTP/auth/encoding; read timeout raised for baseline evidence", "slo_proven": false})
		t.Logf("MIXED_STATS_BASELINE %s", evidence)
		performanceSLOEmit(t, "MIXED_STORAGE_PLAN", json.RawMessage(evidence))
	}
	// Actual routing, bearer authentication, JSON encoding/decoding and loopback
	// sockets use the ordinary application pool/deadlines. The raised timeout
	// and forced plan above are diagnostic only, never an HTTP SLO shortcut.
	metrics, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.initializeAdminKey(ctx, webTestKey); err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Store: Store{DB: db}, Groups: NewPGGroupStore(db), Settings: NewPGSettingsStore(db), Metrics: metrics, AdminKey: webTestKey}
	server := httptest.NewServer(p.adminRoutes())
	defer server.Close()
	client := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 8}}
	defer client.CloseIdleConnections()
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 20, 1, 100)
	functional := os.Getenv("PERFORMANCE_LOCAL_WRITE_FUNCTIONAL") == "1"
	type sample struct {
		MS     float64 `json:"ms"`
		Status int     `json:"status"`
		Bytes  int     `json:"bytes"`
		Error  string  `json:"error,omitempty"`
	}
	measure := func(path string) sample {
		request, _ := http.NewRequest("GET", server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer "+webTestKey)
		start := time.Now()
		response, err := client.Do(request)
		if err != nil {
			return sample{MS: float64(time.Since(start).Microseconds()) / 1000, Error: err.Error()}
		}
		body, e := io.ReadAll(response.Body)
		response.Body.Close()
		result := sample{MS: float64(time.Since(start).Microseconds()) / 1000, Status: response.StatusCode, Bytes: len(body)}
		var stats managedUsageStats
		if e != nil || response.StatusCode != 200 || json.Unmarshal(body, &stats) != nil {
			result.Error = "http_or_json_failure"
			return result
		}
		want := int64(count / 2)
		if stats.Requests != want || stats.Input != want*101 || stats.Output != want*13 || stats.Cached != want*37 || stats.Errors != want/10 || stats.UsageKnownRequests != want || stats.LatencySamples != want || stats.AverageLatency == nil || *stats.AverageLatency != 21 || len(stats.Accounts) != 1000 || len(stats.Models) != 1 || stats.Models[0].Name != "mix-a" || len(stats.Keys) != 1 || len(stats.Groups) != 1 || stats.Groups[0].Requests != want {
			result.Error = "mixed_http_counters_or_dimensions_incorrect"
			return result
		}
		var accountsTotal, seriesTotal int64
		for _, a := range stats.Accounts {
			if a.Requests != want/1000 {
				result.Error = "mixed_http_account_total_incorrect"
			}
			accountsTotal += a.Requests
		}
		for _, point := range stats.Series {
			seriesTotal += point.Requests
		}
		if accountsTotal != want || seriesTotal != want {
			result.Error = "mixed_http_marginal_sum_incorrect"
		}
		return result
	}
	for i, selection := range []string{"model=mix-a", "model=mix-a&key_id=mixed-key&group_id=shared"} {
		name := []string{"mixed_model_half", "mixed_model_key_group_half"}[i]
		path := fmt.Sprintf("/api/logs/stats?%s&from=%d&to=%d&step=86400&timezone=UTC", selection, from, to)
		t.Run(name, func(t *testing.T) {
			first := measure(path)
			performanceSLOEmit(t, "FIRST", map[string]any{"name": name, "path": path, "sample": first, "threshold_ms": 1000, "functional_only": functional, "slo_pass": !functional && first.Error == "" && first.MS < 1000})
			if first.Error != "" || !functional && first.MS >= 1000 {
				t.Errorf("mixed first failed %+v", first)
				return
			}
			for _, concurrency := range []int{1, 4} {
				values := make([]sample, samples)
				jobs := make(chan int)
				var wg sync.WaitGroup
				for range concurrency {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := range jobs {
							values[i] = measure(path)
						}
					}()
				}
				for i := range samples {
					jobs <- i
				}
				close(jobs)
				wg.Wait()
				latencies := make([]float64, samples)
				failed, over := 0, 0
				statuses := map[int]int{}
				for i, v := range values {
					latencies[i] = v.MS
					statuses[v.Status]++
					if v.Error != "" {
						failed++
					}
					if v.MS >= 1000 {
						over++
					}
				}
				sort.Float64s(latencies)
				percentile := func(p int) float64 { return latencies[(samples*p+99)/100-1] }
				performanceSLOEmit(t, "RESULT", map[string]any{"name": name, "path": path, "concurrency": concurrency, "samples": values, "failed": failed, "over_slo": over, "threshold_ms": 1000, "functional_only": functional, "statuses": statuses, "p50_ms": percentile(50), "p95_ms": percentile(95), "p99_ms": percentile(99), "max_ms": latencies[samples-1]})
				if failed > 0 || !functional && over > 0 {
					t.Errorf("mixed warm failed=%d over=%d", failed, over)
					return
				}
			}
		})
	}
}
