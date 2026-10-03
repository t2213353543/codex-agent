package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func retainedCacheDetailReference(t *testing.T, m *Metrics, from, to, step int64) (overviewCacheTotals, map[int64]overviewCacheTotals) {
	t.Helper()
	rows, err := m.db.Pool.Query(t.Context(), `WITH source AS MATERIALIZED (
 SELECT $1::bigint+((extract(epoch FROM created_at)::bigint-$1::bigint)/$3::bigint)*$3::bigint AS bucket,
 COALESCE((record->>'input_tokens')::bigint,0) AS input,COALESCE((record->>'cached_tokens')::bigint,0) AS cached,
 (record->>'cache_write_tokens')::bigint AS cache_write,(`+cacheReadObservedSQL+`) AS observed
 FROM request_logs WHERE created_at>=to_timestamp($1) AND created_at<to_timestamp($2) AND (`+legacyCacheObservationEligibleSQL+`)
 ) SELECT bucket,count(*) FILTER(WHERE observed)::bigint,count(*) FILTER(WHERE NOT observed)::bigint,
 COALESCE(sum(input) FILTER(WHERE observed),0)::bigint,COALESCE(sum(cached) FILTER(WHERE observed),0)::bigint,
 count(cache_write) FILTER(WHERE cache_write>=0)::bigint,(sum(cache_write) FILTER(WHERE cache_write>=0))::bigint
 FROM source GROUP BY GROUPING SETS((),(bucket))`, from, to, step)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var total overviewCacheTotals
	points := map[int64]overviewCacheTotals{}
	for rows.Next() {
		var bucket *int64
		var value overviewCacheTotals
		if err := rows.Scan(&bucket, &value.CacheReadSamples, &value.CacheReadUnknownRequests, &value.CacheReadInputTokens, &value.CacheReadTokens, &value.CacheWriteSamples, &value.CacheWriteTokens); err != nil {
			t.Fatal(err)
		}
		value.finishCacheRead()
		if bucket == nil {
			total = value
		} else {
			points[*bucket] = value
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return total, points
}

func assertRetainedRollupParity(t *testing.T, m *Metrics) {
	t.Helper()
	var expectedCount, expectedOK, expectedErrors, count, ok, errors int64
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT count(*),count(*) FILTER(WHERE status='ok'),count(*) FILTER(WHERE status<>'ok') FROM request_logs").Scan(&expectedCount, &expectedOK, &expectedErrors); err != nil {
		t.Fatal(err)
	}
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT COALESCE(sum(requests),0)::bigint,COALESCE(sum(ok),0)::bigint,COALESCE(sum(errors),0)::bigint FROM retained_log_minute").Scan(&count, &ok, &errors); err != nil {
		t.Fatal(err)
	}
	if count != expectedCount || ok != expectedOK || errors != expectedErrors {
		t.Fatalf("retained counters: got=%d/%d/%d want=%d/%d/%d", count, ok, errors, expectedCount, expectedOK, expectedErrors)
	}
	for _, span := range [][3]int64{
		{1699999920, 1700001000, 60}, {1700000001, 1700000497, 60}, {1700000001, 1700000497, 37},
		{1700000000, 1700000060, 1}, {1700000039, 1700000041, 60}, {1700000000, 1700000010, 20},
		{1700000000, 1700000600, 300}, {-121, 61, 37}, {1900000000, 1900000060, 60},
	} {
		want, wantPoints := retainedCacheDetailReference(t, m, span[0], span[1], span[2])
		got, points, err := m.OverviewCacheUsage(t.Context(), span[0], span[1], span[2])
		if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(points, wantPoints) {
			t.Fatalf("range %v changed cache observations: got=%+v want=%+v points=%+v wantPoints=%+v err=%v", span, got, want, points, wantPoints, err)
		}
	}
}

func seedRetainedRollupFixture(t *testing.T, m *Metrics) {
	t.Helper()
	_, err := m.db.Pool.Exec(t.Context(), `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status)
 SELECT 'retained-'||i,to_timestamp(1700000000+i*7+CASE WHEN i%7=0 THEN 0.75 ELSE 0 END),CASE WHEN i%3=0 THEN 'error' ELSE 'ok' END
 FROM generate_series(1,90)i RETURNING event_id,created_at,status
 ) INSERT INTO request_logs(event_id,created_at,status,record)
 SELECT event_id,created_at,status,jsonb_build_object('input_tokens',CASE WHEN n%11=0 THEN -1 ELSE 100 END,
 'cached_tokens',CASE WHEN n%13=0 THEN -1 WHEN n%17=0 THEN 101 ELSE n%90 END,
 'cache_write_tokens',CASE WHEN n%19=0 THEN -1 WHEN n%5=0 THEN NULL ELSE n%20 END,
 'usage_known',CASE WHEN n%5=0 THEN NULL ELSE n%5<>1 END,
 'cached_tokens_known',CASE WHEN n%3=0 THEN NULL ELSE n%3=1 END,
 'websocket_warmup',n%23=0,'endpoint',CASE WHEN n%29=0 THEN 'models' ELSE '/v1/responses' END)
 FROM events CROSS JOIN LATERAL (SELECT split_part(event_id,'-',2)::int AS n) number`)
	if err != nil {
		t.Fatal(err)
	}
	insertCacheObservationForTest(t, m, "negative-epoch", -61, map[string]any{"input_tokens": 100, "cached_tokens": 20})
}

func TestPGRetainedRollupStatementChangesAndRollback(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	seedRetainedRollupFixture(t, m)
	assertRetainedRollupParity(t, m)
	for _, statement := range []string{
		`UPDATE request_logs SET created_at=created_at+interval '3 minutes',status='aborted',record=record||'{"input_tokens":100,"cached_tokens":10,"cached_tokens_known":true,"usage_known":true,"cache_write_tokens":3}'::jsonb WHERE id%3=0`,
		`UPDATE request_logs SET record=record||'{"endpoint":"models"}'::jsonb WHERE id%5=0`,
		`DELETE FROM request_logs WHERE id%2=0`,
	} {
		if _, err := m.db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		assertRetainedRollupParity(t, m)
	}
	var before string
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT jsonb_agg(to_jsonb(s) ORDER BY bucket)::text FROM retained_log_minute s").Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := m.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), "DELETE FROM request_logs"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT jsonb_agg(to_jsonb(s) ORDER BY bucket)::text FROM retained_log_minute s").Scan(&after); err != nil || before != after {
		t.Fatalf("rollback changed retained summary: %v", err)
	}
	if _, err := m.db.Pool.Exec(t.Context(), "DELETE FROM request_logs"); err != nil {
		t.Fatal(err)
	}
	assertRetainedRollupParity(t, m)
	var durable int
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM usage_events").Scan(&durable); err != nil || durable != 91 {
		t.Fatalf("detail cleanup changed durable events: %d %v", durable, err)
	}
	if _, err := m.db.Pool.Exec(t.Context(), "TRUNCATE request_logs"); err != nil {
		t.Fatal(err)
	}
	assertRetainedRollupParity(t, m)
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM retained_log_minute").Scan(&durable); err != nil || durable != 0 {
		t.Fatalf("truncate left summary rows: %d %v", durable, err)
	}
}

func TestPGRetainedRollupBackfill(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	for _, operation := range []string{"insert", "update", "delete", "truncate"} {
		if _, err := m.db.Pool.Exec(t.Context(), "DROP TRIGGER request_logs_retained_"+operation+" ON request_logs"); err != nil {
			t.Fatal(err)
		}
	}
	seedRetainedRollupFixture(t, m)
	if _, err := m.db.Pool.Exec(t.Context(), `DROP TABLE retained_log_minute;
 DROP FUNCTION maintain_retained_log_insert(),maintain_retained_log_update(),maintain_retained_log_delete(),truncate_retained_log_minute(),retained_log_cache_observation(jsonb)`); err != nil {
		t.Fatal(err)
	}
	raw, err := migrationFiles.ReadFile("008_retained_log_rollup.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := m.db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertRetainedRollupParity(t, m)
	for i := 0; i < 3; i++ {
		insertCacheObservationForTest(t, m, fmt.Sprintf("post-backfill-%d", i), 1700000020+int64(i), map[string]any{"input_tokens": 100, "cached_tokens": 70})
	}
	assertRetainedRollupParity(t, m)
}

func TestPGRetainedRollupAlignedRangeDoesNotReadDetails(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	seedRetainedRollupFixture(t, m)
	if _, err := m.db.Pool.Exec(t.Context(), "UPDATE request_logs SET created_at=date_trunc('second',created_at)"); err != nil {
		t.Fatal(err)
	}
	conn, err := m.db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		if _, err := conn.Exec(t.Context(), "SET plan_cache_mode="+mode); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err := conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE, FORMAT JSON) "+overviewCacheRollupSQL(), int64(1699999920), int64(1700001000), int64(60)).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []map[string]any
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatal(err)
		}
		var walk func(map[string]any)
		walk = func(plan map[string]any) {
			if plan["Relation Name"] == "request_logs" && plan["Actual Loops"].(float64) > 0 {
				t.Fatalf("aligned cache range accessed raw details: %s", raw)
			}
			if children, ok := plan["Plans"].([]any); ok {
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
		}
		walk(plans[0]["Plan"].(map[string]any))
	}
	assertRetainedRollupParity(t, m)
}

func TestPGRetainedRollupConcurrentWritersAndRollback(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			_, err := m.db.Pool.Exec(t.Context(), `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status) SELECT 'concurrent-'||$1::int::text||'-'||j,to_timestamp(1700000000+j),'ok'
 FROM generate_series(1,20)j RETURNING event_id,created_at,status)
 INSERT INTO request_logs(event_id,created_at,status,record)
 SELECT event_id,created_at,status,'{"input_tokens":100,"cached_tokens":30}'::jsonb FROM events`, i)
			if err != nil {
				failures <- err
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	assertRetainedRollupParity(t, m)
	for _, operation := range []string{
		"TRUNCATE request_logs",
		`UPDATE request_logs SET created_at=created_at+interval '5 minutes'`,
		`WITH event AS (INSERT INTO usage_events(event_id,created_at,status) VALUES('rolled-back-insert',to_timestamp(1700000000),'ok') RETURNING *)
 INSERT INTO request_logs(event_id,created_at,status,record) SELECT event_id,created_at,status,'{}'::jsonb FROM event`,
	} {
		tx, err := m.db.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), operation); err != nil {
			tx.Rollback(context.Background())
			t.Fatal(err)
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		assertRetainedRollupParity(t, m)
	}
}
