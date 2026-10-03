package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestOnlineMigrationIndexParser(t *testing.T) {
	raw, err := migrationFiles.ReadFile("006_metrics_query_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	indexes, err := parseMigrationIndexes(raw)
	if err != nil || len(indexes) != 5 {
		t.Fatalf("migration index definitions: %d %v", len(indexes), err)
	}
	for _, index := range indexes {
		if !strings.HasPrefix(index.statement, "CREATE INDEX CONCURRENTLY ") || index.table != "request_logs" || len(index.checksum) != 64 {
			t.Fatalf("invalid online statement: %+v", index)
		}
	}
	raw, err = migrationFiles.ReadFile("020_request_log_error_class_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	indexes, err = parseMigrationIndexes(raw)
	if err != nil || len(indexes) != 1 || indexes[0].name != "request_logs_error_class_tags_idx" || indexes[0].table != "request_logs" {
		t.Fatalf("error-class migration index definitions: %+v %v", indexes, err)
	}
	raw, err = migrationFiles.ReadFile("023_request_log_error_class_scalar_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	indexes, err = parseMigrationIndexes(raw)
	if err != nil || len(indexes) != 2 {
		t.Fatalf("scalar class migration indexes: %+v %v", indexes, err)
	}
	for i, name := range []string{"request_logs_error_category_time_idx", "request_logs_error_severity_time_idx"} {
		if indexes[i].name != name || indexes[i].table != "request_logs" || !strings.HasPrefix(indexes[i].statement, "CREATE INDEX CONCURRENTLY ") {
			t.Fatalf("scalar class index must use concurrent migration: %+v", indexes[i])
		}
	}
	for _, raw := range []string{"", "DELETE FROM request_logs;", "CREATE INDEX a ON request_logs(id); SELECT 1;"} {
		if _, err := parseMigrationIndexes([]byte(raw)); err == nil {
			t.Fatalf("accepted non-index migration: %q", raw)
		}
	}
}

func TestPGOnlineIndexMigrationResumeAndTimeout(t *testing.T) {
	db := newMetricsTestDB(t)
	ctx := t.Context()
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "CREATE TABLE online_index_fixture(id bigint); SET statement_timeout='2345ms'; SET lock_timeout='1234ms'"); err != nil {
		t.Fatal(err)
	}
	raw := []byte("CREATE INDEX online_index_fixture_idx ON online_index_fixture(id);")
	for range 2 {
		if err := applyOnlineIndexMigration(ctx, conn, "test/online", raw); err != nil {
			t.Fatal(err)
		}
		var timeout, lockTimeout string
		if err := conn.QueryRow(ctx, "SELECT current_setting('statement_timeout'),current_setting('lock_timeout')").Scan(&timeout, &lockTimeout); err != nil || timeout != "2345ms" || lockTimeout != "1234ms" {
			t.Fatalf("maintenance timeout escaped migration: statement=%s lock=%s %v", timeout, lockTimeout, err)
		}
	}
	if err := applyOnlineIndexMigration(ctx, conn, "test/online", []byte("CREATE INDEX online_index_fixture_idx ON online_index_fixture((id+1));")); err == nil {
		t.Fatal("changed interrupted definition accepted")
	}
	if _, err := conn.Exec(ctx, "DROP INDEX online_index_fixture_idx; CREATE INDEX online_index_fixture_idx ON online_index_fixture((id+1))"); err != nil {
		t.Fatal(err)
	}
	if err := applyOnlineIndexMigration(ctx, conn, "test/online", raw); err == nil {
		t.Fatal("replaced same-name index definition was silently adopted")
	}
	if _, err := conn.Exec(ctx, "CREATE INDEX online_unclaimed_idx ON online_index_fixture(id)"); err != nil {
		t.Fatal(err)
	}
	if err := applyOnlineIndexMigration(ctx, conn, "test/unclaimed", []byte("CREATE INDEX online_unclaimed_idx ON online_index_fixture(id);")); err == nil {
		t.Fatal("unclaimed index was silently adopted")
	}
	// Hold an ordinary writer open: CREATE INDEX CONCURRENTLY must wait for
	// that snapshot. Cancellation can leave an invalid catalog entry; a retry
	// on a new connection must recover it using its durable migration claim.
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "INSERT INTO online_index_fixture VALUES(1)"); err != nil {
		t.Fatal(err)
	}
	retryRaw := []byte("CREATE INDEX online_retry_idx ON online_index_fixture(id);")
	deadline, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err = applyOnlineIndexMigration(deadline, conn, "test/retry", retryRaw)
	cancel()
	if err == nil {
		t.Fatal("online index unexpectedly ignored the writer snapshot")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	retry, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Release()
	if err := applyOnlineIndexMigration(ctx, retry, "test/retry", retryRaw); err != nil {
		t.Fatal("interrupted online build did not recover:", err)
	}
}
