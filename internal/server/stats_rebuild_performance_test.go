package server

import "testing"

func TestPGMetricsRebuildHourlyBulkRollbackAndLiveWrites(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	m.Record(RequestRecord{EventID: "bulk-rebuild-before", Time: 1800000000, Status: "ok", Account: "one", InputTokens: 9, LatencyMs: 4})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	want := m.GrandTotals()
	_, err := db.Pool.Exec(t.Context(), `CREATE FUNCTION reject_bulk_rebuild() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected bulk rebuild failure'; END $$;
 CREATE TRIGGER reject_bulk_rebuild BEFORE INSERT ON usage_daily FOR EACH STATEMENT EXECUTE FUNCTION reject_bulk_rebuild()`)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RebuildAggregates(t.Context()); err == nil {
		t.Fatal("injected rebuild unexpectedly succeeded")
	}
	if got := m.GrandTotals(); got != want {
		t.Fatalf("failed rebuild changed committed totals: got=%+v want=%+v", got, want)
	}
	assertHourlyGlobalMatchesMinutes(t, db)
	var state string
	if err := db.Pool.QueryRow(t.Context(), "SELECT tgenabled::text FROM pg_trigger WHERE tgrelid='usage_minute'::regclass AND tgname='usage_minute_hourly_change'").Scan(&state); err != nil || state != "O" {
		t.Fatalf("rollback did not restore hourly writer trigger: %q %v", state, err)
	}
	if _, err := db.Pool.Exec(t.Context(), "DROP TRIGGER reject_bulk_rebuild ON usage_daily"); err != nil {
		t.Fatal(err)
	}
	m.Record(RequestRecord{EventID: "bulk-rebuild-after", Time: 1800000127, Status: "error", Account: "two", InputTokens: 11, LatencyMs: 7})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	want = m.GrandTotals()
	for range 2 {
		if err := m.RebuildAggregates(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := m.GrandTotals(); got != want {
			t.Fatalf("bulk rebuild changed totals: got=%+v want=%+v", got, want)
		}
		assertHourlyGlobalMatchesMinutes(t, db)
	}
	m.Record(RequestRecord{EventID: "bulk-rebuild-live", Time: 1800007200, Status: "ok", InputTokens: 5, LatencyMs: 6})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertHourlyGlobalMatchesMinutes(t, db)
}
