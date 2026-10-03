package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newMetricsTestDB(t *testing.T) *Database {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	bootstrap, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("connect isolated test database:", err)
	}
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "metrics_test_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = bootstrap.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		bootstrap.Close(ctx)
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 6
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["timezone"] = "UTC"
	config.ConnConfig.RuntimeParams["statement_timeout"] = "10000"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := bootstrap.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
		bootstrap.Close(ctx)
	})
	db := &Database{Pool: pool}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal("repeat migration:", err)
	}
	return db
}

func newTestPGMetrics(t *testing.T, db *Database) *Metrics {
	t.Helper()
	m, err := NewPGMetrics(db)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func insertMetricsAccount(t *testing.T, db *Database) {
	t.Helper()
	_, err := db.Pool.Exec(context.Background(), "INSERT INTO accounts(id,display_name,created_at,updated_at) VALUES('a_metrics','Metrics account',1,1)")
	if err != nil {
		t.Fatal(err)
	}
}

func TestPGMetricsIdempotentConcurrentEventsRestartAndDelete(t *testing.T) {
	db := newMetricsTestDB(t)
	insertMetricsAccount(t, db)
	m := newTestPGMetrics(t, db)
	base := time.Now().Unix() / 60 * 60
	rec := RequestRecord{EventID: "identical", Time: base, Account: "Metrics account", Status: "error", Error: "upstream failed", KeyID: "key1", InputTokens: 100, OutputTokens: 20, CachedTokens: 80, LatencyMs: 30}
	var wg sync.WaitGroup
	var mu sync.Mutex
	inserted := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, _, err := m.ImportRecords(context.Background(), []RequestRecord{rec})
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			inserted += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if inserted != 1 {
		t.Fatalf("inserted %d copies of one event", inserted)
	}
	if total := m.GrandTotals(); total.Requests != 1 || total.Tokens != 120 {
		t.Fatalf("incorrect totals: %+v", total)
	}
	other := newTestPGMetrics(t, db)
	last, found := other.LastFor("a_metrics")
	if !found || last.Status != "error" || last.AccountID != "a_metrics" {
		t.Fatalf("restart lost recent state: %+v %v", last, found)
	}
	batch, err := other.BatchAccountMetrics(context.Background(), map[string]int64{"a_metrics": base - 86400})
	if err != nil {
		t.Fatal(err)
	}
	if batch["a_metrics"].Usage.Requests != 1 || !batch["a_metrics"].HasLast {
		t.Fatalf("wrong batch: %+v", batch)
	}
	if _, err = db.Pool.Exec(context.Background(), "DELETE FROM accounts WHERE id='a_metrics'"); err != nil {
		t.Fatal(err)
	}
	page, err := other.Logs(LogQuery{AccountID: "a_metrics"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Account != "Metrics account" {
		t.Fatal("deleting account removed historical log")
	}
	if other.KeyUsage("key1").Requests != 1 {
		t.Fatal("restart lost key usage")
	}
}

func TestPGMetricsTransactionFailureRetryAndHealth(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	ctx := context.Background()
	_, err := db.Pool.Exec(ctx, `CREATE FUNCTION reject_usage() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected aggregate failure'; END $$;
	CREATE TRIGGER reject_usage BEFORE INSERT ON usage_totals FOR EACH ROW EXECUTE FUNCTION reject_usage()`)
	if err != nil {
		t.Fatal(err)
	}
	m.Record(RequestRecord{EventID: "failure-retry", Time: time.Now().Unix(), Account: "legacy", Status: "ok", InputTokens: 7})
	if m.HealthError() == nil {
		t.Fatal("failed persistence reported healthy")
	}
	if m.Save() == nil {
		t.Fatal("failed retry reported success")
	}
	for _, table := range []string{"request_logs", "usage_events", "usage_minute", "usage_daily", "account_request_state"} {
		var count int
		if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("transaction leaked %s rows: %d", table, count)
		}
	}
	if _, err = db.Pool.Exec(ctx, "DROP TRIGGER reject_usage ON usage_totals"); err != nil {
		t.Fatal(err)
	}
	if err = m.Save(); err != nil {
		t.Fatal(err)
	}
	if m.HealthError() != nil {
		t.Fatal("recovered persistence remained unhealthy")
	}
	if err = m.Save(); err != nil {
		t.Fatal(err)
	}
	if total := m.GrandTotals(); total.Requests != 1 || total.Tokens != 7 {
		t.Fatalf("retry counted twice: %+v", total)
	}
}

func TestPGMetricsCursorStableDescendingAndNoCount(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	base := time.Now().Unix()
	records := []RequestRecord{{EventID: "first", Time: base, Status: "ok", Model: "one"}, {EventID: "second", Time: base, Status: "ok", Model: "one"}, {EventID: "older", Time: base - 60, Status: "error", Model: "two"}}
	if _, _, err := m.ImportRecords(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	first, err := m.Logs(LogQuery{Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != -1 || first.TotalKind != "not_computed" || !first.HasMore || first.Items[0].EventID != "second" {
		t.Fatalf("bad first page: %+v", first)
	}
	second, err := m.Logs(LogQuery{Size: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if second.Items[0].EventID != "first" {
		t.Fatalf("tie order unstable: %+v", second)
	}
	third, err := m.Logs(LogQuery{Size: 1, Cursor: second.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if third.HasMore || third.Items[0].EventID != "older" {
		t.Fatalf("bad final page: %+v", third)
	}
	compat, err := m.Logs(LogQuery{Page: 2, Size: 1})
	if err != nil || compat.Items[0].EventID != "first" {
		t.Fatalf("legacy page: %+v %v", compat, err)
	}
	filtered, err := m.Logs(LogQuery{Model: "two"})
	if err != nil || len(filtered.Items) != 1 {
		t.Fatalf("model filter: %+v %v", filtered, err)
	}
	if _, err = m.Logs(LogQuery{Cursor: "malformed"}); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	if _, err = m.Logs(LogQuery{Page: 101}); err == nil {
		t.Fatal("unbounded legacy pagination accepted")
	}
}

func TestPGMetricsRebuildPruneAndLegacyExport(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	ctx := context.Background()
	records := []RequestRecord{{EventID: "old", Time: time.Now().Add(-100 * 24 * time.Hour).Unix(), Account: "archived", Status: "error", InputTokens: 11, Error: "old detail"}, {EventID: "new", Time: time.Now().Unix(), Account: "current", Status: "ok", InputTokens: 10, OutputTokens: 2, CachedTokens: 8}}
	if _, _, err := m.ImportRecords(ctx, records); err != nil {
		t.Fatal(err)
	}
	if m.GrandTotals().Requests != 2 || m.RetainedTotals().Requests != 1 {
		t.Fatal("lifetime and retained totals are not separated")
	}
	if _, err := m.Prune(ctx, time.Now().Add(-92*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.RebuildAggregates(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if m.GrandTotals().Requests != 2 || m.RetainedTotals().Requests != 1 {
		t.Fatal("rebuild lost or double-counted events")
	}
	last, found := m.LastFor("archived")
	if !found || last.Status != "error" {
		t.Fatal("prune lost recent account state")
	}
	var output bytes.Buffer
	if err := m.ExportRecords(ctx, &output); err != nil {
		t.Fatal(err)
	}
	var exported []RequestRecord
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var rec RequestRecord
		if err := decoder.Decode(&rec); err != nil {
			t.Fatal(err)
		}
		exported = append(exported, rec)
	}
	if len(exported) != 2 || exported[0].InputTokens != 11 || exported[0].Error != "" {
		t.Fatalf("expired event export: %+v", exported)
	}
	dir := t.TempDir()
	var legacy bytes.Buffer
	for _, rec := range exported {
		if err := json.NewEncoder(&legacy).Encode(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(dir+"/logs.jsonl", legacy.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := NewMetrics(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.GrandTotals().Requests != 2 {
		t.Fatal("legacy export could not rebuild stats snapshot")
	}
}

func TestMetricsErrorRedactionAndTimezone(t *testing.T) {
	value := redactMetricError(`Authorization: Bearer supersecret access_token="secret-access" refresh_token=secret-refresh password: secret-password eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature`)
	for _, secret := range []string{"supersecret", "secret-access", "secret-refresh", "secret-password", "eyJhbGci"} {
		if strings.Contains(value, secret) {
			t.Fatalf("secret survived redaction: %s", secret)
		}
	}
	m := &Metrics{location: time.FixedZone("UTC+8", 8*3600)}
	start := time.Unix(m.TodayStart(), 0).In(m.location)
	if start.Hour() != 0 || start.Minute() != 0 {
		t.Fatalf("today boundary not in configured timezone: %v", start)
	}
}

func TestFileMetricsNewestFirstAndRestartWithoutSnapshot(t *testing.T) {
	dir := t.TempDir()
	m, err := NewMetrics(dir)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Unix()
	m.Record(RequestRecord{Time: base, Account: "a", Status: "ok"})
	m.Record(RequestRecord{Time: base + 1, Account: "a", Status: "error"})
	m.Close()
	m, err = NewMetrics(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	page, err := m.Logs(LogQuery{Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Status != "error" || page.TotalKind != "exact" || !page.HasMore {
		t.Fatalf("oldest-first logs: %+v", page)
	}
	last, found := m.LastFor("a")
	if !found || last.Status != "error" {
		t.Fatal("restart lost latest failure")
	}
	if m.GrandTotals().Requests != 2 {
		t.Fatal("restart lost unsnapshotted events")
	}
}

func TestPGMetricsLegacyBaselinePreservesMissingLogsAndRebuild(t *testing.T) {
	db := newMetricsTestDB(t)
	insertMetricsAccount(t, db)
	m := newTestPGMetrics(t, db)
	ctx := context.Background()
	minute := time.Now().Unix() / 60 * 60
	day := minute / 86400 * 86400
	rec := RequestRecord{EventID: "in-original-log", Time: minute, Account: "Metrics account", Status: "ok", InputTokens: 10, OutputTokens: 2, CachedTokens: 8, LatencyMs: 5}
	if _, _, err := m.ImportRecords(ctx, []RequestRecord{rec}); err != nil {
		t.Fatal(err)
	}
	state := statsState{Minutes: map[int64]*minuteCounts{minute: {Requests: 3, OK: 2, Errors: 1, Tokens: 36, Input: 30, Output: 6, Cached: 24, Latency: 15, LatencyN: 3}}, Accounts: map[string]map[int64]*dayCounts{"Metrics account": {day: {Requests: 3, OK: 2, Errors: 1, Tokens: 36, Input: 30, Output: 6, Cached: 24, Latency: 15, LatencyN: 3}}}}
	for i := 0; i < 2; i++ {
		if err := m.ImportLegacyStats(ctx, state, "manifest-one"); err != nil {
			t.Fatal(err)
		}
	}
	if m.GrandTotals().Requests != 3 || m.AccountWindow("a_metrics", day).Requests != 3 {
		t.Fatal("baseline lost or double-counted missing events")
	}
	if err := m.RebuildAggregates(ctx); err != nil {
		t.Fatal(err)
	}
	if m.GrandTotals().Requests != 3 || m.AccountWindow("a_metrics", day).Requests != 3 {
		t.Fatal("rebuild lost baseline")
	}
	page, err := m.Logs(LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatal("baseline fabricated request details")
	}
	if _, err = db.Pool.Exec(ctx, "UPDATE accounts SET display_name='Renamed account' WHERE id='a_metrics'"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = m.ExportLegacyStats(ctx, &output); err != nil {
		t.Fatal(err)
	}
	var exported statsState
	if err = json.Unmarshal(output.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Minutes[minute].Requests != 3 || exported.Accounts["Renamed account"][day].Requests != 3 {
		t.Fatal("rollback export omitted baseline")
	}
	var exportedRecords bytes.Buffer
	if err = m.ExportRecords(ctx, &exportedRecords); err != nil {
		t.Fatal(err)
	}
	var exportedRec RequestRecord
	if err = json.NewDecoder(&exportedRecords).Decode(&exportedRec); err != nil {
		t.Fatal(err)
	}
	if exportedRec.Account != "Renamed account" || exportedRec.AccountNameSnapshot != "Metrics account" {
		t.Fatal("renamed rollback record lost identity or history")
	}
	stale := statsState{Minutes: map[int64]*minuteCounts{minute: {Requests: 1, OK: 1, Tokens: 12, Input: 10, Output: 2, Cached: 8, Latency: 5, LatencyN: 1}}}
	if err = m.ImportLegacyStats(ctx, stale, "stale-snapshot"); err != nil {
		t.Fatal("newer logs must supersede stale snapshot:", err)
	}
	if m.GrandTotals().Requests != 3 {
		t.Fatal("stale snapshot changed durable totals")
	}
	bad := statsState{Minutes: map[int64]*minuteCounts{minute: {Requests: 4, OK: 4, Tokens: 12, Input: 10, Output: 2, Cached: 8, Latency: 5, LatencyN: 1}}}
	if err = m.ImportLegacyStats(ctx, bad, "inconsistent-snapshot"); err == nil {
		t.Fatal("inconsistent snapshot was accepted")
	}
	var manifests int
	if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM usage_legacy_imports").Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if manifests != 2 {
		t.Fatal("rejected baseline left partial import")
	}
}
