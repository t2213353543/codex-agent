package server

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

func dropRetainedUsageModelForTest(t *testing.T, db *Database) {
	t.Helper()
	if _, err := db.Pool.Exec(t.Context(), `DROP TRIGGER IF EXISTS request_logs_usage_model_truncate ON request_logs;
DROP FUNCTION IF EXISTS truncate_retained_usage_model();
DROP TABLE IF EXISTS retained_usage_model_minute,retained_usage_model_daily,retained_usage_model_unsafe_minute`); err != nil {
		t.Fatal(err)
	}
}

func replayRetainedModelMigration(t *testing.T, db *Database) {
	t.Helper()
	raw, err := migrationFiles.ReadFile("017_retained_usage_model.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), string(raw))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRetainedModelMigrationExact(t *testing.T, db *Database) {
	t.Helper()
	columns := strings.Split(managedUsageRollupColumns, ",")
	sums := make([]string, len(columns))
	for i, column := range columns {
		sums[i] = "sum(" + column + ") AS " + column
	}
	for _, layer := range []struct {
		table, bucket, condition string
	}{
		{"retained_usage_model_minute", "floor(extract(epoch FROM created_at)/60)::bigint*60", "TRUE"},
		{"retained_usage_model_daily", "floor(extract(epoch FROM created_at)/86400)::bigint*86400", "kind<>'total'"},
	} {
		var differences int64
		statement := `WITH expected AS (
 SELECT model AS selected_model,` + layer.bucket + ` AS bucket,status,d.kind,d.resource_id,d.name,` + strings.Join(sums, ",") + `
 FROM request_logs CROSS JOIN LATERAL retained_usage_counters(record,status)c
 CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
 WHERE retained_usage_safe(created_at,status,record,model,account_name,key_name) AND octet_length(model)<=512 AND ` + layer.condition + `
 GROUP BY 1,2,3,4,5,6
 ), actual AS (SELECT * FROM ` + layer.table + ` WHERE requests>0)
 SELECT count(*) FROM ((SELECT * FROM expected EXCEPT SELECT * FROM actual) UNION ALL (SELECT * FROM actual EXCEPT SELECT * FROM expected)) differences`
		if err := db.Pool.QueryRow(t.Context(), statement).Scan(&differences); err != nil || differences != 0 {
			t.Fatalf("%s differs from raw model-conditioned contributions: %d %v", layer.table, differences, err)
		}
	}
	var differences int64
	err := db.Pool.QueryRow(t.Context(), `WITH expected AS (
 SELECT floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,count(*)::numeric AS requests
 FROM request_logs WHERE retained_usage_safe(created_at,status,record,model,account_name,key_name) AND octet_length(model)>512 GROUP BY 1
 ), actual AS (SELECT * FROM retained_usage_model_unsafe_minute WHERE requests>0)
 SELECT count(*) FROM ((SELECT * FROM expected EXCEPT SELECT * FROM actual) UNION ALL (SELECT * FROM actual EXCEPT SELECT * FROM expected)) differences`).Scan(&differences)
	if err != nil || differences != 0 {
		t.Fatalf("model unsafe guard mismatch: %d %v", differences, err)
	}
	assertRetainedUsageDailyCanonical(t, db)
}

func modelMigrationRecords() []RequestRecord {
	const base int64 = 1710028800
	result := []RequestRecord{}
	for i := 0; i < 18; i++ {
		r := managedPerformanceRecord(i)
		r.EventID = fmt.Sprintf("model-migration-%d", i)
		r.Time = base + int64(i/3)*60 + int64(i%3)
		r.Model = []string{"alpha", "alpha", "beta"}[i%3]
		if i < 3 {
			r.Model = "single"
		}
		if i == 5 {
			r.Status = "error" // One model for this minute/status, though its minute is mixed.
		}
		r.Account, r.KeyName = []string{"Old", "Z-new"}[i%2], []string{"Old", "Z-new"}[i%2]
		result = append(result, r)
	}
	result[6].Model = strings.Repeat("m", 512)
	result[6].AccountID, result[6].Account = strings.Repeat("a", 800), strings.Repeat("A", 800)
	result[6].KeyID, result[6].KeyName = strings.Repeat("k", 800), strings.Repeat("K", 800)
	result[7].Model = strings.Repeat("n", 513)
	result[8].Model = strings.Repeat("界", 180) // UTF-8 bytes, not rune count, set key safety.
	result[9].Model = strings.Repeat("huge", 500)
	result[10].Groups = []string{strings.Repeat("group", 300)}
	image := int64(math.MaxInt64)
	result[11].ImageInputTokens, result[12].ImageInputTokens = &image, &image
	return result
}

func TestPGModelRollupMigrationMutationSafety(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for _, record := range modelMigrationRecords() {
		m.Record(record)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertRetainedModelMigrationExact(t, db)
	for _, statement := range []string{
		`UPDATE request_logs SET model='moved',status='error',created_at=created_at+interval '2 days',record=record||'{"groups":["new","new"],"input_tokens":-4}'::jsonb WHERE event_id IN ('model-migration-1','model-migration-2','model-migration-4')`,
		`UPDATE request_logs SET model=repeat('m',513) WHERE event_id='model-migration-6'`,
		`UPDATE request_logs SET model='back-to-short' WHERE event_id='model-migration-7'`,
		`UPDATE request_logs SET created_at=created_at+interval '0.75 seconds' WHERE event_id='model-migration-13'`,
		`DELETE FROM request_logs WHERE event_id IN ('model-migration-3','model-migration-11')`,
	} {
		if _, err := db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		assertRetainedModelMigrationExact(t, db)
	}
	for _, statement := range []string{"DELETE FROM request_logs", "UPDATE request_logs SET model='rolled-back'", "TRUNCATE request_logs"} {
		if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), statement); err != nil {
				return err
			}
			return errInvalid
		}); err != errInvalid {
			t.Fatal(err)
		}
		assertRetainedModelMigrationExact(t, db)
	}
	if _, err := db.Pool.Exec(t.Context(), "TRUNCATE request_logs"); err != nil {
		t.Fatal(err)
	}
	assertRetainedModelMigrationExact(t, db)
}

func TestPGModelRollupMigrationBackfillSingleAndMixed(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for _, record := range modelMigrationRecords() {
		m.Record(record)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	dropRetainedUsageModelForTest(t, db)
	replayRetainedModelMigration(t, db)
	assertRetainedModelMigrationExact(t, db)
	m.Record(RequestRecord{EventID: "after-model-backfill", Time: 1710028801, Model: "third-model", Account: "Third", Status: "ok", InputTokens: 999})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertRetainedModelMigrationExact(t, db)
}

func TestPGModelRollupMigrationSingleModelSkipsJSON(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for i := 0; i < 12; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("single-copy-%d", i), Time: 1710028800 + int64(i), Model: "single", Status: "ok", InputTokens: 3, Groups: []string{"g", "g"}})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	dropRetainedUsageModelForTest(t, db)
	raw, err := migrationFiles.ReadFile("009_retained_usage_dimensions.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	start := strings.Index(text, "CREATE FUNCTION retained_usage_counters(")
	function := text[start:]
	end := strings.Index(function, "\n$$;") + 4
	function = function[:end]
	declaration := function[:strings.Index(function, " LANGUAGE sql")]
	injected := strings.Replace(declaration, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1) + " LANGUAGE plpgsql IMMUTABLE AS $$ BEGIN RAISE EXCEPTION 'single-model backfill must not parse JSON counters'; END $$;"
	if _, err := db.Pool.Exec(t.Context(), injected); err != nil {
		t.Fatal(err)
	}
	replayRetainedModelMigration(t, db)
	if _, err := db.Pool.Exec(t.Context(), strings.Replace(function, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)); err != nil {
		t.Fatal(err)
	}
	assertRetainedModelMigrationExact(t, db)
}

func TestPGModelRollupMigrationConcurrentModelMoves(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for i := 0; i < 8; i++ {
		m.Record(RequestRecord{EventID: fmt.Sprintf("model-worker-%d", i), Time: 1710028800 + int64(i)*120, Model: []string{"alpha", "beta"}[i%2], Status: "ok", InputTokens: 3})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 8; iteration++ {
		var workers sync.WaitGroup
		failures := make(chan error, 8)
		for worker := 0; worker < 8; worker++ {
			workers.Add(1)
			go func(worker int) {
				defer workers.Done()
				_, err := db.Pool.Exec(t.Context(), "UPDATE request_logs SET model=CASE WHEN model='alpha' THEN 'beta' ELSE 'alpha' END WHERE event_id=$1", fmt.Sprintf("model-worker-%d", worker))
				if err != nil {
					failures <- err
				}
			}(worker)
		}
		workers.Wait()
		close(failures)
		for err := range failures {
			t.Fatal(err)
		}
	}
	assertRetainedModelMigrationExact(t, db)
}

func TestPGModelRollupMigrationRollbackBoundary(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	m.Record(RequestRecord{EventID: "model-rollback", Time: 1710028801, Model: "alpha", Status: "ok", InputTokens: 7})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "UPDATE request_logs SET model='beta' WHERE event_id='model-rollback'"); err != nil {
			return err
		}
		return errInvalid
	}); err != errInvalid {
		t.Fatal(err)
	}
	assertRetainedModelMigrationExact(t, db)
}
