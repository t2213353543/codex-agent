package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPGLogAnalyticsProjectionMutationAndFallback(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	db, m := p.Store.DB, p.Metrics
	for i := range 12 {
		r := managedPerformanceRecord(i)
		r.EventID, r.Status, r.HTTPCode, r.Error = fmt.Sprintf("analytics-%d", i), "error", 429, "rate limit exceeded"
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET record=record||jsonb_build_object('unrelated_payload',repeat('wide metadata ',10000))`); err != nil {
		t.Fatal(err)
	}
	assertProjection := func() {
		t.Helper()
		var invalid, missing, extra, leaked int
		if err := db.Pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM request_logs l JOIN request_log_analytics a ON a.log_id=l.id WHERE a.record IS DISTINCT FROM request_log_analytics_record(l.record)),
 (SELECT count(*) FROM request_logs l LEFT JOIN request_log_analytics a ON a.log_id=l.id WHERE a.log_id IS NULL),
 (SELECT count(*) FROM request_log_analytics a LEFT JOIN request_logs l ON l.id=a.log_id WHERE l.id IS NULL),
 (SELECT count(*) FROM request_log_analytics WHERE record ? 'unrelated_payload')`).Scan(&invalid, &missing, &extra, &leaked); err != nil || invalid+missing+extra+leaked != 0 {
			t.Fatalf("projection mismatch invalid=%d missing=%d extra=%d leaked=%d err=%v", invalid, missing, extra, leaked, err)
		}
	}
	assertParity := func() {
		t.Helper()
		for _, filter := range []string{"error_category=rate_limit", "error_severity=P1", "account_id=a1", "model=m2&status=error", "group_id=g1", "request_id=analytics-3"} {
			q := managedQuery(t, filter)
			got, err := m.managedLogStats(t.Context(), q)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(t.Context(), "DELETE FROM request_log_analytics"); err != nil {
				t.Fatal(err)
			}
			q.statsQueryer = tx
			want, err := m.managedLogStats(t.Context(), q)
			_ = tx.Rollback(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			got.CheckedAt, want.CheckedAt = 0, 0
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Fatalf("thin/fallback mismatch %s: thin=%s fallback=%s", filter, a, b)
			}
		}
	}
	assertProjection()
	assertParity()
	for _, sql := range []string{
		`UPDATE request_logs SET record=record||'{"http_code":403,"error":"authentication failure","input_tokens":993,"groups":["new","new"]}'::jsonb WHERE event_id='analytics-3'`,
		`UPDATE request_logs SET account_name='new label' WHERE event_id='analytics-4'`,
		`UPDATE request_logs SET id=id+10000 WHERE event_id='analytics-5'`,
		`DELETE FROM request_logs WHERE event_id='analytics-6'`,
	} {
		if _, err := db.Pool.Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
		assertProjection()
		assertParity()
	}
	tx, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE request_logs SET record=record||'{"input_tokens":888888}'::jsonb; DELETE FROM request_logs WHERE event_id='analytics-0'`); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(t.Context())
	assertProjection()
	assertParity()
	q := managedQuery(t, "error_category=rate_limit")
	before, err := m.managedUsageOptionsPGCurrent(t.Context(), q, "account", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_log_analytics"); err != nil {
		t.Fatal(err)
	}
	after, err := m.managedUsageOptionsPGCurrent(t.Context(), q, "account", "", 50)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("missing projection lost options: %v %v %v", before, after, err)
	}
	result, err := db.BackfillLogAnalytics(t.Context(), 3)
	if err != nil || !result.Complete || result.Inserted != 11 || result.Batches != 4 {
		t.Fatalf("bounded backfill: %+v %v", result, err)
	}
	assertProjection()
	result, err = db.BackfillLogAnalytics(t.Context(), 3)
	if err != nil || !result.Complete || result.Inserted != 0 {
		t.Fatalf("repeat backfill: %+v %v", result, err)
	}
	if _, err := db.Pool.Exec(t.Context(), "TRUNCATE request_logs"); err != nil {
		t.Fatalf("original truncate contract changed: %v", err)
	}
	assertProjection()
}

func TestPGLogAnalyticsProjectionFieldAndLegacyContract(t *testing.T) {
	db := newMetricsTestDB(t)
	values := map[string]any{"groups": []string{"g", "g"}, "secret_unsupported": strings.Repeat("private unrelated payload", 100)}
	for _, name := range managedUsageDecodedTextFields {
		values[name] = nil
	}
	raw, _ := json.Marshal(values)
	var compact []byte
	if err := db.Pool.QueryRow(t.Context(), "SELECT request_log_analytics_record($1::jsonb)", raw).Scan(&compact); err != nil {
		t.Fatal(err)
	}
	delete(values, "secret_unsupported")
	var got map[string]any
	if json.Unmarshal(compact, &got) != nil || len(got) != len(values) {
		t.Fatalf("projection whitelist drifted: %s", compact)
	}
	for name := range values {
		if _, ok := got[name]; !ok {
			t.Fatalf("projection omits %s", name)
		}
	}
	for _, raw := range []string{`null`, `[]`, `42`, `true`, `"legacy"`} {
		var equal bool
		if err := db.Pool.QueryRow(t.Context(), "SELECT request_log_analytics_record($1::jsonb)=$1::jsonb", raw).Scan(&equal); err != nil || !equal {
			t.Fatalf("projection changed nonobject %s: %v", raw, err)
		}
	}
}

func TestPGLogAnalyticsBackfillFullIDDomainAndMetadataOnlyMigration(t *testing.T) {
	db := newMetricsTestDB(t)
	if _, err := db.Pool.Exec(t.Context(), `DROP TRIGGER request_log_analytics_change ON request_logs;
 DROP TRIGGER request_log_analytics_delete ON request_logs;
 DROP TRIGGER request_log_analytics_truncate ON request_logs;
 DROP FUNCTION maintain_request_log_analytics(),delete_request_log_analytics(),truncate_request_log_analytics();
 DROP TABLE request_log_analytics; DROP FUNCTION request_log_analytics_record(jsonb);
 DELETE FROM schema_migrations WHERE name='migrations/024_request_log_analytics.sql'`); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int64{math.MinInt64, -1, 0, 1} {
		_, err := db.Pool.Exec(t.Context(), `WITH e AS (
 INSERT INTO usage_events(event_id,created_at,status) VALUES($2,to_timestamp(1790553600),'ok') RETURNING event_id
 ) INSERT INTO request_logs(id,event_id,created_at,status,record) SELECT $1,event_id,to_timestamp(1790553600),'ok','{"input_tokens":17,"unrelated_protocol_payload":"exclude"}'::jsonb FROM e`, id, fmt.Sprintf("analytics-domain-%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	var projected int
	if err := db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM request_log_analytics").Scan(&projected); err != nil || projected != 0 {
		t.Fatalf("DDL migration must not hold writer lock through historical backfill: %d %v", projected, err)
	}
	result, err := db.BackfillLogAnalytics(t.Context(), 1)
	if err != nil || !result.Complete || result.Inserted != 4 || result.Batches != 4 {
		t.Fatalf("backfill skipped nonpositive/bigint-boundary IDs: %+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := db.BackfillLogAnalytics(ctx, 1); err == nil {
		t.Fatal("canceled maintenance unexpectedly succeeded")
	}
}

func TestPGLogAnalyticsBackfillConcurrentUpdateDeleteAndLateInsert(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for i := range 20 {
		m.Record(RequestRecord{EventID: fmt.Sprintf("backfill-concurrent-%d", i), Time: 1790553600 + int64(i)*60, Status: "ok", InputTokens: int64(i)})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_log_analytics"); err != nil {
		t.Fatal(err)
	}
	writer, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err := writer.Exec(t.Context(), `UPDATE request_logs SET record=record||'{"input_tokens":777}'::jsonb WHERE event_id='backfill-concurrent-0'; DELETE FROM request_logs WHERE event_id='backfill-concurrent-1'`); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := db.BackfillLogAnalytics(t.Context(), 3)
		finished <- err
	}()
	select {
	case err := <-finished:
		t.Fatalf("backfill must wait for the concurrent row writer: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	if err := writer.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	// A late committed lower sequence ID must get its projection from the
	// trigger even when maintenance's fixed scan ceiling/cursor predates it.
	_, err = db.Pool.Exec(t.Context(), `WITH e AS(INSERT INTO usage_events(event_id,created_at,status) VALUES('late-analytics',to_timestamp(1790553610),'ok') RETURNING event_id)
 INSERT INTO request_logs(id,event_id,created_at,status,record) SELECT -100,event_id,to_timestamp(1790553610),'ok','{"input_tokens":555}'::jsonb FROM e`, pgx.QueryExecModeExec)
	if err != nil {
		t.Fatal(err)
	}
	var mismatch, orphans int
	if err := db.Pool.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM request_logs l LEFT JOIN request_log_analytics a ON a.log_id=l.id WHERE a.log_id IS NULL OR a.record IS DISTINCT FROM request_log_analytics_record(l.record)),
 (SELECT count(*) FROM request_log_analytics a LEFT JOIN request_logs l ON l.id=a.log_id WHERE l.id IS NULL)`).Scan(&mismatch, &orphans); err != nil || mismatch+orphans != 0 {
		t.Fatalf("concurrent backfill lost update/delete/late insert: mismatch=%d orphans=%d err=%v", mismatch, orphans, err)
	}
}
