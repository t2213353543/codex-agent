package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func dropRetainedUsageDailyForTest(t *testing.T, db *Database) {
	t.Helper()
	dropRetainedUsageModelForTest(t, db)
	_, err := db.Pool.Exec(t.Context(), `DROP TRIGGER retained_usage_minute_daily_truncate ON retained_usage_minute;
DROP FUNCTION truncate_retained_usage_daily();
DROP TABLE retained_usage_daily;
DROP INDEX retained_usage_minute_total_time_idx;`)
	if err != nil {
		t.Fatal(err)
	}
}

func assertRetainedUsageDailyCanonical(t *testing.T, db *Database) {
	t.Helper()
	var sums []string
	for _, column := range strings.Split(managedUsageRollupColumns, ",") {
		sums = append(sums, "sum("+column+") AS "+column)
	}
	var count int
	err := db.Pool.QueryRow(t.Context(), `WITH expected AS (
 SELECT (bucket/86400)*86400 AS bucket,status,kind,resource_id,name,`+strings.Join(sums, ",")+`
 FROM retained_usage_minute WHERE kind<>'total' GROUP BY 1,2,3,4,5
 ), a AS (SELECT * FROM expected WHERE requests>0),b AS (SELECT * FROM retained_usage_daily WHERE requests>0)
 SELECT count(*) FROM ((SELECT * FROM a EXCEPT SELECT * FROM b) UNION ALL (SELECT * FROM b EXCEPT SELECT * FROM a)) differences`).Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("daily summary differs from canonical minute counters: %d %v", count, err)
	}
}

func TestPGManagedUsageDailyExactRangesAndMutations(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	base := time.Date(2024, 3, 8, 0, 0, 0, 0, time.UTC).Unix()
	for day := 0; day < 6; day++ {
		for i := 0; i < 12; i++ {
			name := "old"
			if day == 2 {
				name = "z-new"
			}
			r := managedPerformanceRecord(i)
			r.EventID, r.Time = fmt.Sprintf("daily-%d-%d", day, i), base+int64(day)*86400+int64(i)*7211
			r.Account, r.KeyName = name, name
			r.Status = []string{"ok", "error", "aborted"}[i%3]
			m.Record(r)
		}
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	filters := []string{"", "status=ok", "status=failed", "status=aborted"}
	for _, zone := range []string{"UTC", "Asia%2FShanghai", "America%2FNew_York", "Asia%2FKathmandu"} {
		for _, step := range []int64{60, 3600, 86400, 604800, 2592000} {
			end := base + 6*86400
			if step == 60 {
				end = base + 2*86400
			}
			filters = append(filters, fmt.Sprintf("from=%d&to=%d&step=%d&timezone=%s", base, end, step, zone))
			filters = append(filters, fmt.Sprintf("from=%d&to=%d&step=%d&timezone=%s&status=failed", base+61, end-37, step, zone))
		}
	}
	assertRetainedUsageDailyCanonical(t, db)
	assertManagedUsageRollupParity(t, m, filters...)
	// A single unsafe observation invalidates only its whole daily parent;
	// regular minutes from that day still contribute exactly once via minute.
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET record=record||jsonb_build_object('upstream_model',$1::text),created_at=created_at+interval '0.75 seconds' WHERE event_id='daily-1-3'`, strings.Repeat("long", 500)); err != nil {
		t.Fatal(err)
	}
	assertRetainedUsageDailyCanonical(t, db)
	assertManagedUsageRollupParity(t, m, filters...)
	if _, err := db.Pool.Exec(t.Context(), `DELETE FROM request_logs WHERE event_id LIKE 'daily-2-%'; UPDATE request_logs SET created_at=created_at+interval '2 days',account_name='moved' WHERE event_id='daily-0-1'`); err != nil {
		t.Fatal(err)
	}
	assertRetainedUsageDailyCanonical(t, db)
	assertManagedUsageRollupParity(t, m, filters...)
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "TRUNCATE request_logs"); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM retained_usage_daily").Scan(&count); err != nil || count != 0 {
			t.Fatalf("truncate left daily state: %d %v", count, err)
		}
		return errInvalid
	}); err != errInvalid {
		t.Fatal(err)
	}
	assertRetainedUsageDailyCanonical(t, db)
	assertManagedUsageRollupParity(t, m, filters...)
}

func TestPGManagedUsageDailyConcurrentBackfillAndProjection(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1699920000
	var workers sync.WaitGroup
	failures := make(chan error, 6)
	for worker := 0; worker < 6; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 4; i++ {
				r := managedPerformanceRecord(i)
				r.EventID, r.Time = fmt.Sprintf("coarse-parallel-%d-%d", worker, i), base+int64(i)*86400
				if _, err := m.persistRecord(context.Background(), r); err != nil {
					failures <- err
					return
				}
			}
		}(worker)
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	assertRetainedUsageDailyCanonical(t, db)
	dropRetainedUsageDailyForTest(t, db)
	raw, err := migrationFiles.ReadFile("010_retained_usage_daily.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), string(raw))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	replayRetainedModelMigration(t, db)
	assertRetainedUsageDailyCanonical(t, db)
	assertManagedUsageRollupParity(t, m, fmt.Sprintf("from=%d&to=%d&step=86400&timezone=Asia%%2FShanghai", base, base+4*86400))
	q := managedQuery(t, fmt.Sprintf("from=%d&to=%d&step=86400&timezone=America%%2FNew_York", base, base+4*86400))
	statement, args := managedUsageRollupQuery(q, managedUsageStats{Step: 86400})
	var plan json.RawMessage
	if err := db.Pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement, args...).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan), `"Relation Name": "retained_usage_daily"`) {
		t.Fatal("complete days did not use the daily table")
	}
	t.Logf("DAILY_ROLLUP_PLAN %s", plan)
}

func TestPGManagedUsageDailyOpposingMutations(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for worker := 0; worker < 2; worker++ {
		// Different minute locks, but the same daily model/key/total targets.
		m.Record(RequestRecord{EventID: fmt.Sprintf("opposing-%d", worker), Time: 1699920000 + int64(worker)*120, Status: []string{"ok", "error"}[worker], Model: "same", KeyID: "same", InputTokens: 3})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 20; iteration++ {
		start := make(chan struct{})
		done := make(chan error, 2)
		for worker := 0; worker < 2; worker++ {
			go func(worker int) {
				<-start
				_, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET status=CASE WHEN status='ok' THEN 'error' ELSE 'ok' END WHERE event_id=$1`, fmt.Sprintf("opposing-%d", worker))
				done <- err
			}(worker)
		}
		close(start)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatalf("opposite status move failed without application retries: %v", err)
			}
		}
	}
	assertRetainedUsageDailyCanonical(t, db)
	assertManagedUsageRollupParity(t, m, "timezone=UTC", "status=ok", "status=failed")
}

func TestPerformanceManagedUsageDailyWriteCost(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("opt-in isolated daily-maintenance cost experiment")
	}
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for repetition := 0; repetition < 3; repetition++ {
		for _, enabled := range []bool{false, true} {
			// Reset both detail and durable counters in this disposable schema.
			// The truncate trigger remains enabled to keep the daily state empty
			// even while its DML maintenance is disabled for the baseline.
			if _, err := db.Pool.Exec(t.Context(), `TRUNCATE request_logs,usage_events,usage_minute,usage_daily,usage_totals,account_request_state,account_credential_request_state RESTART IDENTITY`); err != nil {
				t.Fatal(err)
			}
			// Install the original minute-only or combined minute/daily trigger
			// bodies, leaving every other schema/summary unchanged.
			migration := "009_retained_usage_dimensions.sql"
			if enabled {
				migration = "010_retained_usage_daily.sql"
			}
			raw, err := migrationFiles.ReadFile(migration)
			if err != nil {
				t.Fatal(err)
			}
			for _, operation := range []string{"insert", "update", "delete"} {
				start := strings.Index(string(raw), "FUNCTION maintain_retained_usage_"+operation+"()")
				if start < 0 {
					t.Fatal("maintenance function missing", migration, operation)
				}
				body := string(raw)[start:]
				end := strings.Index(body, "\n$$;")
				if end < 0 {
					t.Fatal("maintenance function delimiter missing")
				}
				if _, err := db.Pool.Exec(t.Context(), "CREATE OR REPLACE "+body[:end+4]); err != nil {
					t.Fatal(err)
				}
			}
			var samples []float64
			for i := 0; i < 100; i++ {
				r := managedPerformanceRecord(i)
				r.EventID, r.AccountID = fmt.Sprintf("daily-cost-%d", i), ""
				start := time.Now()
				if _, err := m.persistRecord(t.Context(), r); err != nil {
					t.Fatal(err)
				}
				samples = append(samples, float64(time.Since(start).Microseconds())/1000)
			}
			if enabled {
				assertRetainedUsageDailyCanonical(t, db)
			}
			evidence, _ := json.Marshal(map[string]any{"daily_enabled": enabled, "repetition": repetition, "samples_ms": samples, "scope": "persistRecord 100 serial transactions; other summaries unchanged; disposable schema reset per case"})
			t.Logf("DAILY_WRITE_COST %s", evidence)
		}
	}
}
