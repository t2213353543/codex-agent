package server

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

func assertHourlyGlobalMatchesMinutes(t *testing.T, db *Database) {
	t.Helper()
	var differences int
	err := db.Pool.QueryRow(context.Background(), `WITH expected AS (
 SELECT (bucket/3600)*3600 AS bucket,`+metricsSums+` FROM usage_minute GROUP BY 1
 ), actual AS (SELECT bucket,`+metricsColumns+` FROM usage_hourly_global WHERE requests<>0 OR tokens<>0 OR latency<>0)
 SELECT count(*) FROM ((SELECT * FROM expected EXCEPT SELECT * FROM actual)
 UNION ALL (SELECT * FROM actual EXCEPT SELECT * FROM expected)) differences`).Scan(&differences)
	if err != nil || differences != 0 {
		t.Fatalf("hourly rollup differs from canonical minutes: differences=%d err=%v", differences, err)
	}
}

func TestPGMetricsHourlyRangeParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	ctx := context.Background()
	const base int64 = 1699920000 // UTC midnight
	_, err := db.Pool.Exec(ctx, `INSERT INTO usage_minute(bucket,account_ref,requests,ok,errors,tokens,input,output,cached,latency,latency_n)
 SELECT $1::bigint+i*60,'account-'||(i%7),1,(i%2),1-(i%2),10+i,8+i,2,3,i,1 FROM generate_series(0,5999)i`, base)
	if err != nil {
		t.Fatal(err)
	}
	assertHourlyGlobalMatchesMinutes(t, db)
	for _, bounds := range [][2]int64{
		{base, base + 3600}, {base, base + 86400*4}, {base + 1, base + 7201},
		{base + 3599, base + 7200}, {base + 61, base + 86400*3 + 1},
		{base + 60, base + 120}, {base + 3600, base + 3600}, {base + 7200, base},
		{0, base + 86400*5}, {-60, base + 120}, {base + 86400*6, base + 86400*7},
	} {
		var want rangeTotals
		if err = db.Pool.QueryRow(ctx, "SELECT "+metricsSums+" FROM usage_minute WHERE bucket >= $1 AND bucket < $2", bounds[0], bounds[1]).Scan(totalsTargets(&want)...); err != nil {
			t.Fatal(err)
		}
		if got := m.totalsPG(bounds[0], bounds[1]); got != want {
			t.Fatalf("range %v: got=%+v want=%+v", bounds, got, want)
		}
	}
	for _, spec := range [][3]int64{
		{base, base + 86400*4, 3600}, {base, base + 86400*4, 7200},
		{base, base + 86400*4, 86400}, {base + 60, base + 7200, 3600},
		{base, base + 7201, 3600}, {base, base + 7200, 3601},
		{base, base + 86400*4 + 1, 86400}, {base, base + 119, 3600},
		// Unaligned daily slots take the hourly-rollup plus straddled-hour
		// branch; parity with a plain minute scan must hold for both ends.
		{base + 60, base + 86400*4, 86400}, {base + 61, base + 86400*3 + 59, 86400},
		{base + 3599, base + 86400*2 + 3600, 86400}, {base + 60, base + 7200, 86400},
	} {
		got := m.seriesPG(spec[0], spec[1], spec[2])
		want := make([]SeriesPoint, len(got))
		for i := range want {
			want[i].Ts = spec[0] + int64(i)*spec[2]
		}
		rows, err := db.Pool.Query(ctx, "SELECT (bucket-$1)/$3 AS slot,"+metricsSums+" FROM usage_minute WHERE bucket >= $1 AND bucket < $2 GROUP BY slot", spec[0], spec[1], spec[2])
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var slot int
			var value rangeTotals
			if err = rows.Scan(append([]any{&slot}, totalsTargets(&value)...)...); err != nil {
				t.Fatal(err)
			}
			want[slot] = SeriesPoint{Ts: want[slot].Ts, Requests: value.Requests, OK: value.OK, Errors: value.Errors, Tokens: value.Tokens, Input: value.Input, Output: value.Output, Cached: value.Cached, LatencyMs: value.Latency}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("series %v differs: err=%v", spec, err)
		}
	}
	if m.ReadFailureVersion() != 0 {
		t.Fatal("rollup queries recorded a read failure")
	}
}

func TestPGMetricsHourlyMutationRebuildAndLegacyParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	ctx := context.Background()
	const base int64 = 1699920000
	for _, r := range []RequestRecord{
		{EventID: "hourly-a", Time: base + 61, Account: "same-hour-a", Status: "ok", InputTokens: 5, OutputTokens: 3, LatencyMs: 11},
		{EventID: "hourly-b", Time: base + 119, Account: "same-hour-a", Status: "error", InputTokens: 5, OutputTokens: 3, LatencyMs: 12},
		{EventID: "hourly-c", Time: base + 3601, Account: "next-hour", Status: "ok", InputTokens: 5, OutputTokens: 3, LatencyMs: 13},
	} {
		m.Record(r)
		m.Record(r) // An idempotent retry must not apply a second hourly delta.
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertHourlyGlobalMatchesMinutes(t, db)
	state := statsState{Minutes: map[int64]*minuteCounts{base + 60: {Requests: 5, OK: 3, Errors: 2, Tokens: 40, Input: 25, Output: 15, Latency: 50, LatencyN: 5}}, Accounts: map[string]map[int64]*dayCounts{"snapshot-only": {base: {Requests: 19, OK: 19, Tokens: 190, Input: 190}}}}
	if err := m.ImportLegacyStats(ctx, state, "hourly-baseline"); err != nil {
		t.Fatal(err)
	}
	assertHourlyGlobalMatchesMinutes(t, db)
	if got := m.totalsPG(base, base+86400); got.Requests != 6 || got.Tokens != 48 {
		t.Fatalf("independent account-day baseline leaked into global totals: %+v", got)
	}
	if err := m.RebuildAggregates(ctx); err != nil {
		t.Fatal(err)
	}
	assertHourlyGlobalMatchesMinutes(t, db)
	if got := m.totalsPG(base, base+86400); got.Requests != 6 || got.Tokens != 48 {
		t.Fatalf("rebuild lost global baseline: %+v", got)
	}
	for _, statement := range []string{
		"UPDATE usage_minute SET bucket=bucket+7200,requests=requests+1,ok=ok+1 WHERE account_ref='legacy:same-hour-a'",
		"DELETE FROM usage_minute WHERE account_ref='legacy:next-hour'",
	} {
		if _, err := db.Pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
		assertHourlyGlobalMatchesMinutes(t, db)
	}
	before := m.totalsPG(base, base+86400)
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "TRUNCATE usage_minute"); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM usage_hourly_global").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("truncate left hourly rows")
		}
		return errInvalid // Roll the truncate and its derived state back together.
	}); err != errInvalid {
		t.Fatal(err)
	}
	if got := m.totalsPG(base, base+86400); got != before {
		t.Fatalf("rolled back truncate changed totals: got=%+v want=%+v", got, before)
	}
	assertHourlyGlobalMatchesMinutes(t, db)
}

func TestPGMetricsHourlyBackfillAndConcurrentWrites(t *testing.T) {
	db := newMetricsTestDB(t)
	ctx := context.Background()
	const base int64 = 1699920000
	// Exercise upgrading existing minute rows rather than only an empty schema.
	_, err := db.Pool.Exec(ctx, `DROP TRIGGER usage_minute_hourly_change ON usage_minute;
 DROP TRIGGER usage_minute_hourly_truncate ON usage_minute;
 DROP FUNCTION maintain_usage_hourly_global(); DROP FUNCTION truncate_usage_hourly_global(); DROP TABLE usage_hourly_global;
 INSERT INTO usage_minute(bucket,account_ref,requests,ok,tokens,input) SELECT 1699920000+i*60,'existing-'||(i%10),1,1,3,3 FROM generate_series(0,359)i`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := migrationFiles.ReadFile("007_metrics_hourly_global.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	assertHourlyGlobalMatchesMinutes(t, db)
	m := newTestPGMetrics(t, db)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 8; i++ {
				r := RequestRecord{EventID: fmt.Sprintf("hourly-concurrent-%d-%d", worker, i), Account: fmt.Sprintf("writer-%d", worker), Time: base + 86400 + int64(i%2)*3600, Status: "ok", InputTokens: 3}
				if _, _, err := m.ImportRecords(ctx, []RequestRecord{r}); err != nil {
					t.Error(err)
					return
				}
			}
		}(worker)
	}
	workers.Wait()
	assertHourlyGlobalMatchesMinutes(t, db)
	if got := m.totalsPG(base+86400, base+86400+7200); got.Requests != 64 || got.Tokens != 192 {
		t.Fatalf("concurrent hour creation lost writes: %+v", got)
	}
	// Updating a signed boundary value must check the final total, not overflow
	// while negating OLD or adding NEW before subtracting the old contribution.
	for _, statement := range []string{
		"INSERT INTO usage_minute(bucket,account_ref,tokens) VALUES(1699920000+172800,'signed-boundary',-9223372036854775808)",
		"UPDATE usage_minute SET tokens=0 WHERE account_ref='signed-boundary'",
		"UPDATE usage_minute SET tokens=9223372036854775807 WHERE account_ref='signed-boundary'",
		"UPDATE usage_minute SET tokens=1 WHERE account_ref='signed-boundary'",
		"DELETE FROM usage_minute WHERE account_ref='signed-boundary'",
	} {
		if _, err := db.Pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	assertHourlyGlobalMatchesMinutes(t, db)
}
