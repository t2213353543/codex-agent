package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func assertAccountTopMinuteParity(t *testing.T, m *Metrics, bounds ...[2]int64) {
	t.Helper()
	for _, bound := range bounds {
		for _, limit := range []int{1, 10, 100} {
			before := m.ReadFailureVersion()
			got := m.accountTopPG(bound[0], bound[1], limit)
			if m.ReadFailureVersion() != before {
				t.Fatalf("derived ranking failed for %v", bound)
			}
			want := got[:0:0]
			rows, err := m.db.Pool.Query(t.Context(), `SELECT max(account_name),sum(tokens)::bigint,sum(requests)::bigint,sum(errors)::bigint
 FROM usage_minute WHERE bucket >= $1 AND bucket < $2 AND account_name<>'' GROUP BY account_ref ORDER BY sum(tokens) DESC,account_ref LIMIT $3`, bound[0], bound[1], limit)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var value struct {
					Name     string `json:"name"`
					Tokens   int64  `json:"tokens"`
					Requests int64  `json:"requests"`
					Errors   int64  `json:"errors"`
				}
				if err := rows.Scan(&value.Name, &value.Tokens, &value.Requests, &value.Errors); err != nil {
					t.Fatal(err)
				}
				want = append(want, value)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ranking changed range=%v limit=%d got=%+v want=%+v", bound, limit, got, want)
			}
		}
	}
}

func seedAccountTopMinuteFixture(t *testing.T, m *Metrics) {
	t.Helper()
	_, err := m.db.Pool.Exec(t.Context(), `INSERT INTO usage_minute(bucket,account_ref,account_name,requests,errors,tokens) VALUES
 (0,'same','Alpha',1,0,10),(60,'same','Zulu',3,1,40),(120,'same','',1000,1000,999999),
 (86400,'same','Bravo',2,1,20),(86460,'tie-a','名字',4,1,70),(86460,'tie-b','é',4,1,70),
 (172800,'zero','Zed zero',0,0,0),(172860,'zero','Alpha zero',0,0,0),
 (172860,'negative','signed',-3,-1,-20),(-60,'before-epoch','Before',1,0,5)`)
	if err != nil {
		t.Fatal(err)
	}
	// Independent daily-only legacy usage and global empty-name minute deltas
	// are intentionally excluded from ranking, exactly as the old query did.
	if _, err := m.db.Pool.Exec(t.Context(), `INSERT INTO usage_daily(bucket,account_ref,account_name,requests,tokens) VALUES(0,'daily-only','legacy day',999,9999999)`); err != nil {
		t.Fatal(err)
	}
}

func TestPGAccountTopDayParityAndMutations(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	seedAccountTopMinuteFixture(t, m)
	bounds := [][2]int64{{0, 259200}, {-86400, 259200}, {1, 259199}, {86401, 172860}, {60, 120}, {0, 0}, {259200, 259260}, {math.MinInt64, math.MaxInt64}}
	assertAccountTopMinuteParity(t, m, bounds...)
	for _, statement := range []string{
		`DELETE FROM usage_minute WHERE account_ref='same' AND account_name='Zulu'`,
		`UPDATE usage_minute SET account_name='Renamed',bucket=bucket+172800 WHERE account_ref='same' AND account_name='Alpha'`,
		`UPDATE usage_minute SET account_name='' WHERE account_ref='tie-a'`,
		`DELETE FROM usage_minute WHERE account_ref='zero' AND account_name='Zed zero'`,
	} {
		if _, err := m.db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		assertAccountTopMinuteParity(t, m, bounds...)
	}
	if err := pgx.BeginFunc(t.Context(), m.db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "TRUNCATE usage_minute"); err != nil {
			return err
		}
		var rows int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM usage_account_top_day").Scan(&rows); err != nil {
			return err
		}
		if rows != 0 {
			t.Fatalf("truncate left %d derived rows", rows)
		}
		return errInvalid
	}); err != errInvalid {
		t.Fatal(err)
	}
	assertAccountTopMinuteParity(t, m, bounds...)
	if _, err := m.db.Pool.Exec(t.Context(), "DELETE FROM usage_minute"); err != nil {
		t.Fatal(err)
	}
	assertAccountTopMinuteParity(t, m, bounds...)
}

func TestPGAccountTopLongNamesAndSignedDeltas(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	seedAccountTopMinuteFixture(t, m)
	long := strings.Repeat("LongLegalName/", 1000)
	if _, err := m.db.Pool.Exec(t.Context(), "INSERT INTO usage_minute(bucket,account_ref,account_name,requests,tokens) VALUES(300,'long',$1,1,100)", long); err != nil {
		t.Fatal(err)
	}
	assertAccountTopMinuteParity(t, m, [2]int64{0, 259200}, [2]int64{1, 259199})
	var guards int
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM usage_account_top_unsafe_day WHERE minute_rows>0").Scan(&guards); err != nil || guards != 1 {
		t.Fatalf("guard=%d err=%v", guards, err)
	}
	if _, err := m.db.Pool.Exec(t.Context(), "DELETE FROM usage_minute WHERE account_ref='long'"); err != nil {
		t.Fatal(err)
	}
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT count(*) FROM usage_account_top_unsafe_day WHERE minute_rows>0").Scan(&guards); err != nil || guards != 0 {
		t.Fatalf("guard did not clear=%d err=%v", guards, err)
	}
	if _, err := m.db.Pool.Exec(t.Context(), "INSERT INTO usage_minute(bucket,account_ref,account_name,tokens) VALUES($1,'extreme','Min bucket',0),(400,'signed','Min token',$2)", int64(math.MinInt64), int64(math.MinInt64)); err != nil {
		t.Fatal(err)
	}
	assertAccountTopMinuteParity(t, m, [2]int64{math.MinInt64, math.MaxInt64})
	if _, err := m.db.Pool.Exec(t.Context(), "DELETE FROM usage_minute WHERE account_ref='signed'"); err != nil {
		t.Fatal(err)
	}
	assertAccountTopMinuteParity(t, m, [2]int64{math.MinInt64, math.MaxInt64})
}

func TestPGAccountTopBackfillAndRebuild(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	seedAccountTopMinuteFixture(t, m)
	for _, op := range []string{"insert", "update", "delete", "truncate"} {
		if _, err := m.db.Pool.Exec(t.Context(), "DROP TRIGGER usage_minute_account_top_"+op+" ON usage_minute"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.db.Pool.Exec(t.Context(), `DROP TABLE usage_account_top_day,usage_account_top_unsafe_day;
 DROP FUNCTION maintain_usage_account_top_insert(),maintain_usage_account_top_update(),maintain_usage_account_top_delete(),truncate_usage_account_top()`); err != nil {
		t.Fatal(err)
	}
	raw, err := migrationFiles.ReadFile("012_account_top_day.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(t.Context(), m.db.Pool, func(tx pgx.Tx) error { _, err := tx.Exec(t.Context(), string(raw)); return err }); err != nil {
		t.Fatal(err)
	}
	assertAccountTopMinuteParity(t, m, [2]int64{-86400, 259200})
	for i, name := range []string{"Old", "New", "Zulu", "Alpha"} {
		if _, err := m.persistRecord(context.Background(), RequestRecord{EventID: "account-top-" + name, Time: 1700000000 + int64(i*86400), AccountID: "ranking-id", Account: name, Status: "ok", InputTokens: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RebuildAggregates(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertAccountTopMinuteParity(t, m, [2]int64{1699920000, 1700438400}, [2]int64{1700000001, 1700172801})
}

func TestPGAccountTopExternalMixedUpsertRetry(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	ctx := t.Context()
	if _, err := m.db.Pool.Exec(ctx, `INSERT INTO usage_minute(bucket,account_ref,account_name,tokens) VALUES(0,'race','A',1),(3600,'race','B',1)`); err != nil {
		t.Fatal(err)
	}
	gate, err := m.db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Release()
	const lock int64 = 78129291012
	if _, err := gate.Exec(ctx, "SELECT pg_advisory_lock($1)", lock); err != nil {
		t.Fatal(err)
	}
	defer gate.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lock)
	if _, err := m.db.Pool.Exec(ctx, `CREATE FUNCTION account_top_test_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM pg_advisory_lock(78129291012);PERFORM pg_advisory_unlock(78129291012);RETURN NULL;END $$;
 CREATE TRIGGER z_account_top_test_barrier AFTER UPDATE ON usage_minute FOR EACH STATEMENT EXECUTE FUNCTION account_top_test_barrier()`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		worker int
		err    error
	}
	results := make(chan result, 2)
	write := func(worker int) error {
		first, second := "A", "B"
		if worker == 1 {
			first, second = second, first
		}
		_, err := m.db.Pool.Exec(ctx, `INSERT INTO usage_minute(bucket,account_ref,account_name,tokens)
 VALUES($1,'race',$2,1),($3,'race',$4,1)
 ON CONFLICT(bucket,account_ref) DO UPDATE SET tokens=usage_minute.tokens+EXCLUDED.tokens`, int64(worker*3600), first, int64((worker+2)*3600), second)
		return err
	}
	for worker := range 2 {
		go func(worker int) {
			results <- result{worker, write(worker)}
		}(worker)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := m.db.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid=18 AND objid=819879684 AND NOT granted").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workers did not reach mixed-upsert boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := gate.Exec(ctx, "SELECT pg_advisory_unlock($1)", lock); err != nil {
		t.Fatal(err)
	}
	var retries []int
	for range 2 {
		out := <-results
		if out.err != nil {
			var pgerr *pgconn.PgError
			if !errors.As(out.err, &pgerr) || pgerr.Code != "40P01" {
				t.Fatal(out.err)
			}
			retries = append(retries, out.worker)
		}
	}
	// This deliberately adversarial external multirow UPSERT is not emitted by
	// the application writer. PostgreSQL transaction retry must restore every
	// failed contribution, not silently accept a partially maintained summary.
	if _, err := m.db.Pool.Exec(ctx, "DROP TRIGGER z_account_top_test_barrier ON usage_minute"); err != nil {
		t.Fatal(err)
	}
	for _, worker := range retries {
		if err := write(worker); err != nil {
			t.Fatal("whole-transaction retry:", err)
		}
	}
	var tokens int64
	if err := m.db.Pool.QueryRow(ctx, "SELECT sum(tokens)::bigint FROM usage_minute").Scan(&tokens); err != nil || tokens != 6 {
		t.Fatalf("failed contribution lost: tokens=%d err=%v", tokens, err)
	}
	assertAccountTopMinuteParity(t, m, [2]int64{0, 86400})
}

func TestPGAccountTopConcurrentSingleRowWriters(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	const base int64 = 1699920000
	var workers sync.WaitGroup
	failures := make(chan error, 4)
	for worker := range 4 {
		workers.Go(func() {
			for i := range 12 {
				name := []string{"Alpha", "Zulu"}[(i+worker)%2]
				_, err := m.db.Pool.Exec(t.Context(), `INSERT INTO usage_minute(bucket,account_ref,account_name,tokens,requests)
 VALUES($1,'single-row',$2,1,1) ON CONFLICT(bucket,account_ref) DO UPDATE
 SET account_name=EXCLUDED.account_name,tokens=usage_minute.tokens+1,requests=usage_minute.requests+1`, base+int64(worker*3600), name)
				if err != nil {
					failures <- err
					return
				}
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal("ordinary single-row writer failed without retry:", err)
	}
	assertAccountTopMinuteParity(t, m, [2]int64{base, base + 86400}, [2]int64{base + 1, base + 86401})
}

func TestPGAccountTopConcurrentBusinessWriters(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	const base int64 = 1699920000
	var workers sync.WaitGroup
	failures := make(chan error, 4)
	for worker := range 4 {
		workers.Go(func() {
			for i := range 8 {
				record := RequestRecord{EventID: fmt.Sprintf("business-top-%d-%d", worker, i), Time: base + int64(worker*3600+i*60), AccountID: "business-shared", Account: []string{"Alpha", "Zulu"}[(worker+i)%2], Status: "ok", InputTokens: 3}
				if _, err := m.persistRecordOnce(t.Context(), record); err != nil {
					failures <- err
					return
				}
			}
		})
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal("business transaction failed without retry:", err)
	}
	assertAccountTopMinuteParity(t, m, [2]int64{base, base + 86400})
	var count int64
	if err := m.db.Pool.QueryRow(t.Context(), "SELECT sum(requests)::bigint FROM usage_account_top_day WHERE minute_rows>0").Scan(&count); err != nil || count != 32 {
		t.Fatalf("business contributions lost: %d %v", count, err)
	}
}
