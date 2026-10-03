package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func assertManagedUsageRollupParity(t *testing.T, m *Metrics, filters ...string) {
	t.Helper()
	for _, filter := range filters {
		q := managedQuery(t, filter)
		if !managedUsageRollupEligible(q) {
			t.Fatalf("rollup did not cover %q", filter)
		}
		got, err := m.managedLogStats(t.Context(), q)
		if err != nil {
			t.Fatalf("rollup %q: %v", filter, err)
		}
		q.statsQueryer = m.db.Pool
		want, err := m.managedLogStats(t.Context(), q)
		if err != nil {
			t.Fatalf("raw %q: %v", filter, err)
		}
		got.CheckedAt, want.CheckedAt = 0, 0
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("filter %q\ngot %s\nwant %s", filter, gotJSON, wantJSON)
		}
	}
}

func TestManagedUsageRollupEligibility(t *testing.T) {
	for _, filter := range []string{"", "status=ok", "status=failed", "from=1699999980&to=1700000100&step=60&timezone=UTC"} {
		if !managedUsageRollupEligible(managedQuery(t, filter)) {
			t.Fatalf("basic query rejected: %s", filter)
		}
	}
	for _, filter := range []string{"model=m", "account_id=a", "key_id=k", "error_category=timeout", "error_severity=P1", "stream=false", "websocket_warmup=false", "group_id=g", "client_request_id=test", "request_id=request", "sent_model=x", "error_truncated=false", "as_of=usage_snapshot"} {
		if managedUsageRollupEligible(managedQuery(t, filter)) {
			t.Fatalf("unsupported filter used summary: %s", filter)
		}
	}
}

func TestPGManagedUsageRollupMutationParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1699999980
	filters := []string{"", "timezone=UTC", "status=ok", "status=failed", "status=error", "status=aborted", "from=1699999981&to=1700000161&step=60&timezone=UTC", "from=1699999991&to=1699999999&step=60", "from=1699999980&to=1700000220&step=300", "from=1701000000&to=1701000060&step=60"}
	for i := 0; i < 8; i++ {
		v := int64(i - 1)
		known, cacheKnown := i%2 == 0, i%3 == 0
		image := int64(12)
		if i == 2 || i == 3 {
			image = math.MaxInt64
		}
		name := "A"
		if i == 3 {
			name = "Z"
		}
		r := RequestRecord{EventID: fmt.Sprintf("rollup-%d", i), Time: base + int64(i*23), AccountID: "account", Account: name, KeyID: "key", KeyName: name, Groups: []string{"g1", "g1", "g2"}, Model: "m", UpstreamModel: "up", SentModel: "sent", ReturnedModel: "returned", InboundPath: "/v1/responses", UpstreamPath: "/responses", Endpoint: "responses", Status: []string{"ok", "error", "aborted"}[i%3], HTTPCode: 429, Error: "rate limit", InputTokens: 20, OutputTokens: 3, CachedTokens: 8, UsageKnown: &known, CachedTokensKnown: &cacheKnown, LatencyMs: v, LatencyKnown: &known, TTFTMs: &v, ReasoningTokens: &v, CacheWriteTokens: &v, RequestPreparationMs: &v, AccountSelectionMs: &v, ProxySelectionMs: &v, AuthenticationMs: &v, ResponseTransferMs: &v, ImageInputTokens: &image, ImageOutputTokens: &v}
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertManagedUsageRollupParity(t, m, filters...)
	// Bulk deletion removes the largest observed label and reverses an image
	// sum overflow; neither may remain sticky in the retained summary.
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs WHERE event_id=ANY($1::text[])", []string{"rollup-2", "rollup-3"}); err != nil {
		t.Fatal(err)
	}
	assertManagedUsageRollupParity(t, m, filters...)
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET created_at=created_at+interval '2 minutes',status='error',account_name='renamed',record=record||'{"account":"renamed","groups":["new","new"],"input_tokens":-2,"status":"error"}'::jsonb WHERE event_id='rollup-4'`); err != nil {
		t.Fatal(err)
	}
	assertManagedUsageRollupParity(t, m, filters...)
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "TRUNCATE request_logs"); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM retained_usage_minute").Scan(&count); err != nil || count != 0 {
			t.Fatalf("truncate derived state: %d %v", count, err)
		}
		return errInvalid
	}); err != errInvalid {
		t.Fatal(err)
	}
	assertManagedUsageRollupParity(t, m, filters...)
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs"); err != nil {
		t.Fatal(err)
	}
	assertManagedUsageRollupParity(t, m, filters...)
}

func TestPGManagedUsageRollupUnsafeAndCalendarParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	stamps := []int64{1699999998, 1699999999, 1700000000, 1700000001, 1710046800, 1710053999, 1710054000, 1730613599, 1730613600, 1730617199}
	for i, stamp := range stamps {
		r := RequestRecord{EventID: fmt.Sprintf("calendar-%d", i), Time: stamp, AccountID: "name:real-id", Account: "real", KeyID: "name:real-key", KeyName: "real", Model: "m", Status: "ok", InputTokens: 3}
		if i == 2 {
			r.Groups = []string{strings.Repeat("long-group-", 300)}
		}
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), "UPDATE request_logs SET created_at=to_timestamp(1699999999.75) WHERE event_id='calendar-1'"); err != nil {
		t.Fatal(err)
	}
	assertManagedUsageRollupParity(t, m,
		"from=1699999980&to=1700000100&step=60&timezone=UTC",
		"from=1699999999&to=1700000001&step=60&timezone=UTC",
		"from=1699999800&to=1700000300&step=300&timezone=UTC",
		"from=1710000000&to=1730700000&step=86400&timezone=America%2FNew_York",
		"from=1710000000&to=1730700000&step=604800&timezone=America%2FNew_York",
		"from=1710000000&to=1730700000&step=2592000&timezone=Asia%2FKathmandu")
	if _, err := db.Pool.Exec(t.Context(), "DELETE FROM request_logs WHERE event_id IN ('calendar-1','calendar-2')"); err != nil {
		t.Fatal(err)
	}
	var unsafe int
	if err := db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM retained_usage_unsafe_minute WHERE requests>0").Scan(&unsafe); err != nil || unsafe != 0 {
		t.Fatalf("unsafe minute marker did not clear: %d %v", unsafe, err)
	}
	assertManagedUsageRollupParity(t, m, "from=1699999980&to=1700000100&step=60&timezone=UTC")
}

func TestPGManagedUsageRollupConcurrentCommitParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1699999980
	errors := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		go func(worker int) {
			for i := 0; i < 4; i++ {
				record := RequestRecord{EventID: fmt.Sprintf("parallel-%d-%d", worker, i), Time: base + int64(i), Account: fmt.Sprint(worker), Model: "m", Status: "ok", InputTokens: 3}
				if _, err := m.persistRecord(context.Background(), record); err != nil {
					errors <- err
					return
				}
			}
			errors <- nil
		}(worker)
	}
	for range 8 {
		select {
		case err := <-errors:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent rollup writes stalled")
		}
	}
	q := managedQuery(t, "timezone=UTC")
	stats, err := m.managedLogStats(t.Context(), q)
	if err != nil || stats.Requests != 32 || stats.Input != 96 {
		t.Fatalf("lost concurrent contribution: %d %d %v", stats.Requests, stats.Input, err)
	}
	assertManagedUsageRollupParity(t, m, "timezone=UTC")
}

func TestPGManagedUsageRollupBackfillAtomic(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	ctx := t.Context()
	for i := 0; i < 20; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("backfill-%d", i), Time: 1699999980 + int64(i*17), Status: "ok", Model: "m", Account: "account", InputTokens: 10, Groups: []string{"g", "g"}})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	dropRetainedUsageDailyForTest(t, db)
	if _, err := db.Pool.Exec(ctx, `DROP TRIGGER request_logs_usage_insert ON request_logs;
DROP TRIGGER request_logs_usage_update ON request_logs; DROP TRIGGER request_logs_usage_delete ON request_logs;
DROP TRIGGER request_logs_usage_truncate ON request_logs;
DROP FUNCTION maintain_retained_usage_insert(); DROP FUNCTION maintain_retained_usage_update(); DROP FUNCTION maintain_retained_usage_delete(); DROP FUNCTION truncate_retained_usage();
DROP FUNCTION retained_usage_counters(jsonb,text); DROP FUNCTION retained_usage_dimensions(jsonb,text,text,text,text); DROP FUNCTION retained_usage_safe(timestamptz,text,jsonb,text,text,text);
DROP TABLE retained_usage_minute,retained_usage_unsafe_minute`); err != nil {
		t.Fatal(err)
	}
	writer, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err := writer.Exec(ctx, "UPDATE request_logs SET model='committing' WHERE event_id='backfill-0'"); err != nil {
		t.Fatal(err)
	}
	raw, err := migrationFiles.ReadFile("009_retained_usage_dimensions.sql")
	if err != nil {
		t.Fatal(err)
	}
	daily, err := migrationFiles.ReadFile("010_retained_usage_daily.sql")
	if err != nil {
		t.Fatal(err)
	}
	model, err := migrationFiles.ReadFile("017_retained_usage_model.sql")
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Release()
	pid := migrator.Conn().PgConn().PID()
	done := make(chan error, 1)
	go func() {
		done <- pgx.BeginFunc(ctx, migrator, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, string(raw)+"\n"+string(daily)+"\n"+string(model))
			return err
		})
	}()
	// Wait for the real table lock boundary, not an arbitrary scheduling delay.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		if err := db.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='request_logs'::regclass AND NOT granted)", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backfill did not wait for its DML boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// An ordinary detail reader can still observe the previous committed state.
	var before string
	if err := db.Pool.QueryRow(ctx, "SELECT model FROM request_logs WHERE event_id='backfill-0'").Scan(&before); err != nil || before != "m" {
		t.Fatalf("backfill lock blocked or changed ordinary reads: %q %v", before, err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertManagedUsageRollupParity(t, m, "timezone=UTC", "from=1699999981&to=1700000500&step=60", "status=failed")
	m.Record(RequestRecord{EventID: "after-backfill", Time: 1699999980, Status: "ok", InputTokens: 17, Model: "new"})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertManagedUsageRollupParity(t, m, "timezone=UTC")
}
