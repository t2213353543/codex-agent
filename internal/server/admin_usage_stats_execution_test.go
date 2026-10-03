package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGManagedClassStatsGenericPlanIsScopedAndRestored(t *testing.T) {
	db := newMetricsTestDB(t)
	config := db.Pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.RuntimeParams["plan_cache_mode"] = "force_custom_plan"
	config.ConnConfig.RuntimeParams["max_parallel_workers_per_gather"] = "2"
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	m := &Metrics{db: &Database{Pool: pool}}
	for _, q := range []managedLogQuery{
		{SnapshotID: "usage_fresh", ErrorCategory: "rate_limit"},
		{SnapshotID: "usage_fresh", ErrorSeverity: "P1"},
		{ErrorCategory: "rate_limit"},
		{ErrorSeverity: "P1"},
		{ErrorCategory: "rate_limit", ErrorSeverity: "P1"},
		{SnapshotID: "usage_fresh"},
		{},
		{Model: "fixture", Status: "error"},
	} {
		rows, err := q.managedStatsQueryer(m).Query(t.Context(), "SELECT current_setting('plan_cache_mode'),current_setting('transaction_read_only'),current_setting('max_parallel_workers_per_gather')")
		if err != nil || !rows.Next() {
			t.Fatal("snapshot query unavailable", err)
		}
		var mode, readOnly, workers string
		if err := rows.Scan(&mode, &readOnly, &workers); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		selected := q.ErrorCategory != "" || q.ErrorSeverity != ""
		if selected && (mode != "force_generic_plan" || readOnly != "on" || workers != "2") || !selected && (mode != "force_custom_plan" || readOnly != "off" || workers != "2") {
			t.Fatalf("wrong classification-specific plan policy: mode=%s read_only=%s workers=%s query=%+v", mode, readOnly, workers, q)
		}
		if err := pool.QueryRow(t.Context(), "SELECT current_setting('plan_cache_mode'),current_setting('transaction_read_only'),current_setting('max_parallel_workers_per_gather')").Scan(&mode, &readOnly, &workers); err != nil || mode != "force_custom_plan" || readOnly != "off" || workers != "2" || pool.Stat().AcquiredConns() != 0 {
			t.Fatalf("aggregate policy leaked into next request: mode=%s readonly=%s workers=%s err=%v", mode, readOnly, workers, err)
		}
	}
	q := managedLogQuery{SnapshotID: "usage_fresh", ErrorCategory: "rate_limit"}
	if _, err := q.managedStatsQueryer(m).Query(t.Context(), "SELECT missing_snapshot_column"); err == nil {
		t.Fatal("injected query error unexpectedly succeeded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := q.managedStatsQueryer(m).Query(ctx, "SELECT 1"); err == nil {
		t.Fatal("canceled snapshot read succeeded")
	}
	var mode, workers string
	if err := pool.QueryRow(t.Context(), "SELECT current_setting('plan_cache_mode'),current_setting('max_parallel_workers_per_gather')").Scan(&mode, &workers); err != nil || mode != "force_custom_plan" || workers != "2" || pool.Stat().AcquiredConns() != 0 {
		t.Fatalf("failure/cancellation left an aggregate transaction open: mode=%s workers=%s %v", mode, workers, err)
	}
}
