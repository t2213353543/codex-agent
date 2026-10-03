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
)

// An independent recent-traffic fixture avoids comparing an aged empty rolling
// window against the previously active burst/uniform history. No upstream I/O.
func TestPerformanceOperationsRecentScale(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("explicit recent operations performance run required")
	}
	rows := envInt("PERFORMANCE_SLO_ROWS", 100000, 100000, 1000000)
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 20, 1, 100)
	p, handler := adminPGTestProxy(t)
	ctx := context.Background()
	db := p.Store.DB
	anchor := time.Now().Unix() - 1
	started := time.Now()
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, "SET statement_timeout='30min'"); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	for low := 0; low < rows; low += 100000 {
		high := min(low+99999, rows-1)
		_, err = conn.Exec(ctx, `WITH input AS (
 SELECT i,$3::bigint-CASE WHEN i%2=0 THEN (i*17)%300 ELSE (i*7919)%86400 END AS event_epoch,
 CASE WHEN i%10=0 THEN 'error' WHEN i%17=0 THEN 'aborted' ELSE 'ok' END status
 FROM generate_series($1::bigint,$2::bigint)i
),events AS (
 INSERT INTO usage_events(event_id,created_at,status,input_tokens,output_tokens,cached_tokens,latency_ms,latency_known)
 SELECT 'recent_e_'||i,to_timestamp(event_epoch),status,1000+i%400,100+i%200,800,50+i%1000,true FROM input RETURNING *
) INSERT INTO request_logs(event_id,created_at,status,model,key_name,record)
 SELECT event_id,created_at,status,'synthetic-recent','Synthetic recent key',jsonb_build_object(
 'event_id',event_id,'time',extract(epoch FROM created_at)::bigint,'status',status,'model','synthetic-recent',
 'input_tokens',input_tokens,'output_tokens',output_tokens,'cached_tokens',cached_tokens,'latency_ms',latency_ms,
 'usage_known',true,'http_code',CASE WHEN status='ok' THEN 200 WHEN status='aborted' THEN 499 ELSE 429 END,
 'error',CASE WHEN status='ok' THEN '' WHEN status='aborted' THEN 'Synthetic client aborted' ELSE 'Synthetic rate limit' END) FROM events`, low, high, anchor)
		if err != nil {
			conn.Release()
			t.Fatal(err)
		}
		performanceSLOEmit(t, "RECENT_SEED", map[string]any{"rows": high + 1, "target": rows, "elapsed_s": time.Since(started).Seconds()})
	}
	if _, err = conn.Exec(ctx, "ANALYZE request_logs,retained_operations"); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	conn.Release()
	db.Pool.Reset()
	type point struct{ at, requests, errors, success, tokens int64 }
	points := make([]point, rows)
	for i := range rows {
		n := int64(i)
		age := (n * 7919) % 86400
		if i%2 == 0 {
			age = n * 17 % 300
		}
		failed := n%10 == 0 || n%17 == 0
		point := point{at: anchor - age, requests: 1, tokens: 1100 + n%400 + n%200}
		if failed {
			point.errors = 1
		} else {
			point.success = 1
		}
		points[i] = point
	}
	sort.Slice(points, func(i, j int) bool { return points[i].at < points[j].at })
	prefix := make([]point, rows+1)
	for i, p := range points {
		prefix[i+1] = point{requests: prefix[i].requests + p.requests, errors: prefix[i].errors + p.errors, success: prefix[i].success + p.success, tokens: prefix[i].tokens + p.tokens}
	}
	expected := func(from, to int64) point {
		a := sort.Search(len(points), func(i int) bool { return points[i].at >= from })
		b := sort.Search(len(points), func(i int) bool { return points[i].at >= to })
		return point{requests: prefix[b].requests - prefix[a].requests, errors: prefix[b].errors - prefix[a].errors, success: prefix[b].success - prefix[a].success, tokens: prefix[b].tokens - prefix[a].tokens}
	}
	var count int64
	if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM request_logs").Scan(&count); err != nil || count != int64(rows) {
		t.Fatal("recent fixture count mismatch", err)
	}
	performanceSLOEmit(t, "RECENT_FIXTURE", map[string]any{"rows": rows, "anchor": anchor, "seed_elapsed_s": time.Since(started).Seconds(), "distribution": "half the events span the last 5 minutes; half span 24h, deterministic mixed status, latency and tokens", "oracle": "independently generated sorted timestamp prefix sums; each response's rolling from/to", "upstream_calls": 0})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	type sample struct {
		MS     float64 `json:"ms"`
		Status int     `json:"status"`
		Bytes  int     `json:"bytes"`
		Rows   int64   `json:"rolling_window_rows"`
		Error  string  `json:"error,omitempty"`
	}
	measure := func(period string) sample {
		start := time.Now()
		r, _ := http.NewRequest("GET", server.URL+"/api/operations?range="+period, nil)
		r.Header.Set("Authorization", "Bearer "+webTestKey)
		response, err := client.Do(r)
		if err != nil {
			return sample{MS: float64(time.Since(start).Microseconds()) / 1000, Error: err.Error()}
		}
		body, e := io.ReadAll(response.Body)
		response.Body.Close()
		s := sample{MS: float64(time.Since(start).Microseconds()) / 1000, Status: response.StatusCode, Bytes: len(body)}
		var data operationsSnapshot
		if e != nil || response.StatusCode != 200 || json.Unmarshal(body, &data) != nil {
			s.Error = "response_failure"
			return s
		}
		want := expected(data.From, data.To)
		s.Rows = want.requests
		if want.requests < 1 || data.Traffic.Requests != want.requests || data.Traffic.Errors != want.errors || data.Traffic.Success != want.success || data.Traffic.Tokens != want.tokens {
			s.Error = "incorrect_nonempty_rolling_totals"
		}
		return s
	}
	for _, period := range []string{"15m", "1h", "6h", "24h"} {
		t.Run(period, func(t *testing.T) {
			name := fmt.Sprintf("recent_%d_%s", rows, period)
			path := "/api/operations?range=" + period
			first := measure(period)
			performanceSLOEmit(t, "FIRST", map[string]any{"name": name, "path": path, "sample": first, "threshold_ms": 1000, "slo_pass": first.Error == "" && first.MS < 1000})
			if first.Error != "" || first.MS >= 1000 {
				t.Errorf("recent first failed %+v", first)
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
							values[i] = measure(period)
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
				performanceSLOEmit(t, "RESULT", map[string]any{"name": name, "path": path, "samples": values, "concurrency": concurrency, "threshold_ms": 1000, "failed": failed, "over_slo": over, "statuses": statuses, "p50_ms": percentile(50), "p95_ms": percentile(95), "p99_ms": percentile(99), "max_ms": latencies[samples-1]})
				if failed > 0 || over > 0 {
					t.Errorf("recent failed=%d over=%d", failed, over)
					return
				}
			}
		})
	}
}
