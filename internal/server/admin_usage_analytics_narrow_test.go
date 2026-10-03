package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Restore only the three SQL fragments changed from release 9042. Keeping the
// old and new query on the same connection/data makes this a paired comparison,
// rather than attributing a later fixture or resource change to the patch.
func analyticsWideBaselineSQL(statement string) string {
	statement = analyticsScalarBaselineSQL(statement)
	statement = strings.ReplaceAll(statement, `CASE WHEN analytics.log_id IS NOT NULL THEN analytics.record ELSE
 (SELECT original.record FROM request_logs original WHERE original.id=selected.id) END AS record`,
		`CASE WHEN analytics.log_id IS NOT NULL THEN analytics.record ELSE selected.record END AS record`)
	statement = strings.ReplaceAll(statement, `SELECT id,event_id,account_ref,account_name,created_at,status,model,key_name FROM `,
		`SELECT id,event_id,account_ref,account_name,created_at,status,model,key_name,record FROM `)
	statement = strings.ReplaceAll(statement, `SELECT id,created_at,status,model,account_name,key_name FROM request_logs`,
		`SELECT id,created_at,status,model,account_name,key_name,record FROM request_logs`)
	return strings.ReplaceAll(statement, `FROM request_logs original
 CROSS JOIN LATERAL jsonb_each(CASE WHEN jsonb_typeof(original.record)='object' THEN original.record ELSE '{}'::jsonb END)
 WHERE original.id=r.id AND key IN (`, `FROM jsonb_each(CASE WHEN jsonb_typeof(r.record)='object' THEN r.record ELSE '{}'::jsonb END)
 WHERE key IN (`)
}

func analyticsScalarBaselineSQL(statement string) string {
	statement = analyticsDecodeOutsideBaselineSQL(statement)
	start := `(SELECT selected.id,selected.event_id,selected.account_ref,selected.account_name,
 selected.created_at,selected.status,selected.model,selected.key_name,
 analytics.record AS record FROM (`
	boundary := `) selected
 JOIN request_log_analytics analytics ON analytics.log_id=selected.id`
	end := ` WHERE original.id=missing.id OFFSET 0) original) analytical_rows`
	for {
		position := strings.Index(statement, start)
		if position < 0 {
			return statement
		}
		remaining := statement[position+len(start):]
		selectedEnd := strings.Index(remaining, boundary)
		unionEnd := strings.Index(remaining, end)
		if selectedEnd < 0 || unionEnd < selectedEnd {
			panic("analytics UNION baseline boundaries drifted")
		}
		selection := remaining[:selectedEnd]
		replacement := `(SELECT selected.id,selected.event_id,selected.account_ref,selected.account_name,
 selected.created_at,selected.status,selected.model,selected.key_name,
 CASE WHEN analytics.log_id IS NOT NULL THEN analytics.record ELSE
 (SELECT original.record FROM request_logs original WHERE original.id=selected.id) END AS record
 FROM (` + selection + `) selected
 LEFT JOIN request_log_analytics analytics ON analytics.log_id=selected.id) analytical_rows`
		statement = statement[:position] + replacement + remaining[unionEnd+len(end):]
	}
}

// Restore the original single decode above the record UNION before applying
// scalar/wide baselines. This does not duplicate any classification/counter
// formulas: each variant retains exactly the query's generated typed fields.
func analyticsDecodeOutsideBaselineSQL(statement string) string {
	start := "WITH decoded AS MATERIALIZED (SELECT created_at,status,model,account_name,key_name,"
	branch := " UNION ALL SELECT created_at,status,model,account_name,key_name,"
	boundary := ") analytical_rows CROSS JOIN LATERAL "
	for offset := 0; ; {
		position := strings.Index(statement[offset:], start)
		if position < 0 {
			return statement
		}
		position += offset
		remaining := statement[position:]
		end := strings.Index(remaining, " WHERE TRUE), ")
		if end < 0 {
			return statement
		}
		middle := strings.Index(remaining[:end], branch)
		if middle < 0 {
			offset = position + end
			continue
		}
		first, second := remaining[:middle], remaining[middle+len(" UNION ALL "):end+len(" WHERE TRUE")]
		firstFrom, secondFrom := strings.Index(first, " FROM ("), strings.Index(second, " FROM (")
		firstEnd, secondEnd := strings.LastIndex(first, boundary), strings.LastIndex(second, boundary)
		if firstFrom < 0 || secondFrom < 0 || firstEnd < firstFrom || secondEnd < secondFrom {
			panic("typed analytics union baseline boundaries drifted")
		}
		present := first[firstFrom+len(" FROM (") : firstEnd]
		missing := second[secondFrom+len(" FROM (") : secondEnd]
		replacement := first[:firstFrom] + " FROM (" + present + " UNION ALL " + missing + first[firstEnd:] + "), "
		statement = statement[:position] + replacement + remaining[end+len(" WHERE TRUE), "):]
		offset = position + len(replacement)
	}
}

func analyticsQueryRows(t *testing.T, conn managedPGStatsQueryer, statement string, args ...any) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, string(encoded))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}

func analyticsAssertPaired(t *testing.T, conn managedPGStatsQueryer, statement string, args ...any) {
	t.Helper()
	baseline := analyticsWideBaselineSQL(statement)
	if baseline == statement {
		t.Fatal("paired query did not restore the 9042 wide-record fragments")
	}
	got := analyticsQueryRows(t, conn, statement, args...)
	want := analyticsQueryRows(t, conn, baseline, args...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("narrow/wide SQL result mismatch: narrow=%v wide=%v", got, want)
	}
}

// Plans are kept in full in the isolated run log; summaries intentionally do
// not add inclusive node timings or multiply parallel timings into wall time.
func analyticsNarrowPlan(t *testing.T, conn *pgxpool.Conn, statement string, args ...any) (json.RawMessage, map[string]any) {
	t.Helper()
	var raw json.RawMessage
	if err := conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON) "+statement, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan        map[string]any
		ExecutionMS float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode actual plan: %v", err)
	}
	fallbackLoops := float64(0)
	joins := []map[string]any{}
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if node["Alias"] == "original" && node["Relation Name"] == "request_logs" {
			loops, _ := node["Actual Loops"].(float64)
			fallbackLoops += loops
		}
		if node["Node Type"] == "Hash Join" || node["Node Type"] == "Hash" {
			joins = append(joins, map[string]any{"node": node["Node Type"], "width": node["Plan Width"], "rows": node["Actual Rows"], "loops": node["Actual Loops"], "batches": node["Hash Batches"], "temp_written_blocks": node["Temp Written Blocks"], "peak_memory_kib": node["Peak Memory Usage"]})
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
	}
	walk(plans[0].Plan)
	return raw, map[string]any{"execution_ms": plans[0].ExecutionMS, "temp_written_blocks": plans[0].Plan["Temp Written Blocks"], "temp_read_blocks": plans[0].Plan["Temp Read Blocks"], "fallback_loops": fallbackLoops, "hash_nodes": joins}
}

func TestPGLogAnalyticsNarrowFallbackAndDirtySnapshot(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	db := p.Store.DB
	const base int64 = 1790553600
	for i := range 24 {
		r := managedPerformanceRecord(i)
		r.EventID, r.Time, r.Status, r.HTTPCode, r.Error = fmt.Sprintf("narrow-%d", i), base+int64(i/4)*60+10+int64(i%4), "error", 429, "rate limit exceeded"
		if i%2 != 0 {
			r.HTTPCode, r.Error = 403, "authentication failed"
		}
		p.Metrics.Record(r)
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET record=record||jsonb_build_object('unrelated_payload',repeat('metadata ',10000));
 UPDATE request_logs SET id=0 WHERE event_id='narrow-0'; UPDATE request_logs SET id=-100 WHERE event_id='narrow-1';
 INSERT INTO management_cleanup_targets(preview_id,log_id) SELECT 'usage-narrow',id FROM request_logs`); err != nil {
		t.Fatal(err)
	}
	// A nonmember in an existing minute makes that minute dirty; its lower ID
	// must not sneak into the fixed membership through the fallback lookup.
	if _, err := db.Pool.Exec(t.Context(), `WITH e AS (
 INSERT INTO usage_events(event_id,created_at,status) VALUES('narrow-late',to_timestamp($1::bigint),'error') RETURNING event_id)
 INSERT INTO request_logs(id,event_id,created_at,status,record)
 SELECT -200,event_id,to_timestamp($1::bigint),'error','{"http_code":429,"input_tokens":999999}'::jsonb FROM e`, base+12); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), "SET jit=off; SET plan_cache_mode=force_generic_plan"); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{"none", "some", "all"} {
		switch missing {
		case "some":
			_, err = conn.Exec(t.Context(), `DELETE FROM request_log_analytics WHERE log_id IN (0,-100)`)
		case "all":
			_, err = conn.Exec(t.Context(), `DELETE FROM request_log_analytics`)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, filter := range []string{"", "error_category=rate_limit", "error_severity=P1", "account_id=a1", "request_id=narrow-0", "group_id=g1&model=m2"} {
			q := managedQuery(t, filter)
			q.AsOf, q.SnapshotID = "usage-narrow", "usage-narrow"
			source, where, args := q.readSourceSQL()
			statement := "SELECT id,record FROM " + managedUsageAnalyticsSource(source, where)
			analyticsAssertPaired(t, conn, statement, args...)
			_, summary := analyticsNarrowPlan(t, conn, statement, args...)
			var expected int
			if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM (SELECT id FROM "+source+" WHERE "+where+") selected LEFT JOIN request_log_analytics a ON a.log_id=selected.id WHERE a.log_id IS NULL", args...).Scan(&expected); err != nil {
				t.Fatal(err)
			}
			if summary["fallback_loops"] != float64(expected) {
				t.Fatalf("%s %s fallback ran for present/nonmatching rows: %v expected=%d", missing, filter, summary, expected)
			}
		}
		for _, edge := range []bool{false, true} {
			q := managedQuery(t, "timezone=UTC&step=60")
			q.AsOf, q.SnapshotID = "usage-narrow", "usage-narrow"
			if edge {
				q.From, q.To = base+11, base+251
			}
			statement, args := managedUsageRollupQuery(q, managedUsageStats{Step: 60})
			analyticsAssertPaired(t, conn, statement, args...)
		}
	}
	// Under a stable reader snapshot, both the selected original row and its
	// missing-projection fallback must keep the same version after another
	// writer updates/deletes it. No new statement may fetch a newer record.
	tx, err := conn.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	statement := "SELECT id,record FROM " + managedUsageAnalyticsSource("request_logs", "id IN(0,-100)")
	before := analyticsQueryRows(t, tx, statement)
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET record=record||'{"input_tokens":7777}'::jsonb WHERE id=0; DELETE FROM request_logs WHERE id=-100`); err != nil {
		t.Fatal(err)
	}
	if after := analyticsQueryRows(t, tx, statement); !reflect.DeepEqual(before, after) {
		t.Fatalf("fallback escaped the reader snapshot: before=%v after=%v", before, after)
	}
}

type analyticsNarrowCapture struct {
	pool *pgxpool.Pool
	sql  string
	args []any
}

func (c *analyticsNarrowCapture) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	c.sql, c.args = statement, append([]any(nil), args...)
	return c.pool.Query(ctx, "SELECT 1 WHERE false")
}

func TestPerformanceLogAnalyticsNarrowJoin(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("opt-in isolated wide-record join comparison")
	}
	count := envInt("PERFORMANCE_SLO_ROWS", 100000, 1000, 100000)
	db := newMetricsTestDB(t)
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), "SET statement_timeout='120s'; SET jit=off; SET work_mem='4MB'; SET plan_cache_mode=force_generic_plan"); err != nil {
		t.Fatal(err)
	}
	seedStart := time.Now()
	for first := 0; first < count; first += 5000 {
		last := min(first+4999, count-1)
		_, err := conn.Exec(t.Context(), `WITH input AS MATERIALIZED (
 SELECT i,'narrow-plan-'||i AS event_id,to_timestamp(1790553600+(i/1000)*60+i%50) AS created_at,
 'error'::text AS status,'model-'||(i%3) AS model FROM generate_series($1::bigint,$2::bigint)i
 ), e AS (INSERT INTO usage_events(event_id,created_at,status) SELECT event_id,created_at,status FROM input RETURNING event_id),
 payload AS (SELECT repeat(string_agg(md5(j::text),''),8) AS value FROM generate_series(1,32)j)
 INSERT INTO request_logs(event_id,created_at,status,model,account_name,key_name,record)
 SELECT input.event_id,created_at,status,model,'account-'||(i%10),'key-'||(i%5),
 jsonb_build_object('event_id',input.event_id,'time',extract(epoch FROM created_at)::bigint,'status',status,'model',model,
 'account_id','a'||(i%10),'key_id','k'||(i%5),'groups',jsonb_build_array('g'||(i%3),'g'||(i%3)),'endpoint','responses',
 'input_tokens',101,'output_tokens',13,'cached_tokens',37,'usage_known',true,'latency_ms',21,'latency_known',true,'ttft_ms',3,
 'http_code',CASE WHEN i%2=0 THEN 429 ELSE 403 END,'error',CASE WHEN i%2=0 THEN 'rate limit exceeded' ELSE 'authentication failed' END,
 'unrelated_protocol_payload',payload.value) FROM input JOIN e USING(event_id) CROSS JOIN payload`, first, last)
		if err != nil {
			t.Fatal(err)
		}
		performanceSLOEmit(t, "ANALYTICS_NARROW_SEED", map[string]any{"rows": last + 1, "elapsed_s": time.Since(seedStart).Seconds()})
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO management_cleanup_targets(preview_id,log_id) SELECT 'usage-narrow-plan',id FROM request_logs;
 ANALYZE request_logs; ANALYZE request_log_analytics; ANALYZE management_cleanup_targets; ANALYZE retained_usage_minute; ANALYZE retained_usage_unsafe_minute`); err != nil {
		t.Fatal(err)
	}
	var originalBytes, thinBytes float64
	if err := conn.QueryRow(t.Context(), `SELECT avg(pg_column_size(l.record)),avg(pg_column_size(a.record)) FROM request_logs l JOIN request_log_analytics a ON a.log_id=l.id`).Scan(&originalBytes, &thinBytes); err != nil {
		t.Fatal(err)
	}
	performanceSLOEmit(t, "ANALYTICS_NARROW_FIXTURE", map[string]any{"rows": count, "matching_class": count / 2, "original_record_avg_bytes": originalBytes, "thin_record_avg_bytes": thinBytes, "scope": "disposable isolated PG schema; same data/settings/connection for both SQL versions; no production reads"})
	for _, snapshot := range []bool{false, true} {
		q := managedQuery(t, "error_category=rate_limit&timezone=Asia%2FShanghai&step=2592000")
		if snapshot {
			q.AsOf, q.SnapshotID = "usage-narrow-plan", "usage-narrow-plan"
		}
		capture := &analyticsNarrowCapture{pool: db.Pool}
		rawQ := q
		rawQ.statsQueryer = capture
		out, err := (&Metrics{db: db}).managedLogStats(t.Context(), rawQ)
		if err != nil {
			t.Fatal(err)
		}
		fast, fastArgs := managedUsageRollupQuery(q, out)
		statement, args := managedUsageAdaptiveClassSQL(q, fast, fastArgs, capture.sql, capture.args)
		analyticsAssertPaired(t, conn, statement, args...)
		// Alternate order across two samples so a single first-read cache fill
		// cannot masquerade as an improvement from the SQL change.
		for sample, order := range [][]string{{"wide", "narrow"}, {"narrow", "wide"}} {
			for _, version := range order {
				sql := statement
				if version == "wide" {
					sql = analyticsWideBaselineSQL(statement)
				}
				plan, summary := analyticsNarrowPlan(t, conn, sql, args...)
				if version == "narrow" && summary["fallback_loops"] != float64(0) {
					t.Fatalf("complete projection performed original-record fallback: %v", summary)
				}
				performanceSLOEmit(t, "ANALYTICS_NARROW_PLAN", map[string]any{"case": fmt.Sprintf("class_snapshot_%t", snapshot), "version": version, "sample": sample, "summary": summary, "plan": plan, "sql": sql, "args": args, "slo_proven": false})
			}
		}
	}
	// Force dirty-minute raw reads through actual late nonmembers. Keep this
	// diagnostic focused on raw row/projection transport; the class cases above
	// already execute the full production aggregate and adaptive guard.
	if _, err := conn.Exec(t.Context(), `WITH e AS (INSERT INTO usage_events(event_id,created_at,status)
 SELECT 'narrow-late-plan-'||i,to_timestamp(1790553615+i*60),'error' FROM generate_series(0,$1::int/1000-1)i RETURNING *)
 INSERT INTO request_logs(event_id,created_at,status,record) SELECT event_id,created_at,status,'{"http_code":429,"input_tokens":999999}'::jsonb FROM e`, count); err != nil {
		t.Fatal(err)
	}
	q := managedQuery(t, "timezone=UTC&step=60")
	q.AsOf, q.SnapshotID, q.From, q.To = "usage-narrow-plan", "usage-narrow-plan", 1790553601, 1790553600+int64(count/1000)*60-1
	ctes, args := managedUsageRollupSourceWithMaterialization(q, false)
	statement := ctes + ` SELECT count(*),sum((record->>'input_tokens')::bigint) FROM raw_projected`
	analyticsAssertPaired(t, conn, statement, args...)
	for _, version := range []string{"wide", "narrow"} {
		sql := statement
		if version == "wide" {
			sql = analyticsWideBaselineSQL(statement)
		}
		plan, summary := analyticsNarrowPlan(t, conn, sql, args...)
		if version == "narrow" && summary["fallback_loops"] != float64(0) {
			t.Fatal("dirty minute read a present projection's original payload")
		}
		performanceSLOEmit(t, "ANALYTICS_NARROW_PLAN", map[string]any{"case": "dirty_snapshot_raw_transport", "version": version, "summary": summary, "plan": plan, "sql": sql, "args": args, "slo_proven": false})
	}
}
