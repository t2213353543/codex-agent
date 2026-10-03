package server

import (
	"context"
	"testing"
	"time"
)

// RebuildRetainedRollups must reproduce, from the committed details alone, the
// exact counters the statement triggers maintain incrementally. The snapshot
// hashes the sums of every numeric column of every retained family before and
// after, then repeats the rebuild to prove it is a fixed point.
func TestPGRetainedRollupRebuildParity(t *testing.T) {
	db := newMetricsTestDB(t)
	insertMetricsAccount(t, db)
	m := newTestPGMetrics(t, db)
	ctx := context.Background()
	base := time.Now().Unix()/60*60 - 900
	known, ttft := true, int64(900)
	records := []RequestRecord{
		{EventID: "rebuild_ok", Time: base, Account: "Metrics account", Model: "g-small", KeyID: "k1", KeyName: "k1", Status: "ok", InputTokens: 100, OutputTokens: 40, CachedTokens: 10, LatencyMs: 120, LatencyKnown: &known, Endpoint: "responses", Groups: []string{"g1", "g2"}},
		{EventID: "rebuild_rate_limited", Time: base + 61, Account: "Metrics account", Model: "g-small", Status: "error", HTTPCode: 429, Error: "rate limit exceeded", LatencyMs: 30, Endpoint: "responses"},
		{EventID: "rebuild_auth", Time: base + 121, Account: "Metrics account", Model: "g-other", Status: "error", HTTPCode: 401, Error: "unauthorized", Endpoint: "responses"},
		{EventID: "rebuild_aborted", Time: base + 122, Account: "Metrics account", Model: "g-other", Status: "aborted", Error: "context canceled", Endpoint: "responses"},
		{EventID: "rebuild_timeout", Time: base + 185, Account: "Metrics account", Model: "g-other", Status: "error", HTTPCode: 504, Error: "upstream request timed out", TTFTMs: &ttft, Endpoint: "responses"},
		{EventID: "rebuild_second_edge", Time: base + 245, Account: "Metrics account", Model: "g-small", Status: "ok", InputTokens: 7, OutputTokens: 3, LatencyMs: 2000, Endpoint: "responses"},
	}
	inserted, _, err := m.ImportRecords(ctx, records)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != len(records) {
		t.Fatalf("imported %d of %d records", inserted, len(records))
	}
	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, table := range []string{"retained_log_minute", "retained_usage_minute", "retained_usage_daily", "retained_usage_model_minute", "retained_usage_model_daily", "retained_operations", "retained_usage_unsafe_minute", "retained_usage_model_unsafe_minute"} {
			var sums string
			if err := db.Pool.QueryRow(ctx, `SELECT string_agg('COALESCE(sum('||quote_ident(column_name)||'),0)::text',',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND data_type='numeric'`, table).Scan(&sums); err != nil {
				t.Fatal(err)
			}
			if sums == "" {
				out[table] = "<no numeric columns>"
				continue
			}
			var digest string
			if err := db.Pool.QueryRow(ctx, "SELECT md5(concat_ws(',',"+sums+")) FROM "+table).Scan(&digest); err != nil {
				t.Fatal(err)
			}
			out[table] = digest
		}
		return out
	}
	before := snapshot()
	if err := m.RebuildRetainedRollups(ctx); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	for table, digest := range before {
		if after[table] != digest {
			t.Fatalf("%s digest changed after rebuild: trigger path %s, rebuilt %s", table, digest, after[table])
		}
	}
	if err := m.RebuildRetainedRollups(ctx); err != nil {
		t.Fatal(err)
	}
	fixed := snapshot()
	for table, digest := range after {
		if fixed[table] != digest {
			t.Fatalf("%s digest drifted on idempotent rebuild: %s then %s", table, digest, fixed[table])
		}
	}
}
