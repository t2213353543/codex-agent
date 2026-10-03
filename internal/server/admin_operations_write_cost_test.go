package server

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPerformanceOperationsWriteCost(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("opt-in isolated operations maintenance comparison")
	}
	db := newMetricsTestDB(t)
	for repetition := 0; repetition < 3; repetition++ {
		for _, enabled := range []bool{false, true} {
			if _, err := db.Pool.Exec(t.Context(), "TRUNCATE request_logs,usage_events,usage_minute,usage_daily,usage_totals,account_request_state,account_credential_request_state RESTART IDENTITY"); err != nil {
				t.Fatal(err)
			}
			action := "DISABLE"
			if enabled {
				action = "ENABLE"
			}
			if _, err := db.Pool.Exec(t.Context(), "ALTER TABLE request_logs "+action+" TRIGGER request_logs_operations_insert"); err != nil {
				t.Fatal(err)
			}
			m := newTestPGMetrics(t, db)
			samples := make([]float64, 0, 100)
			for i := 0; i < 100; i++ {
				r := managedPerformanceRecord(i)
				r.EventID = fmt.Sprintf("operations-cost-%d", i)
				r.AccountID = ""
				start := time.Now()
				if _, err := m.persistRecord(t.Context(), r); err != nil {
					t.Fatal(err)
				}
				samples = append(samples, float64(time.Since(start).Microseconds())/1000)
			}
			var requests int64
			if enabled {
				if err := db.Pool.QueryRow(t.Context(), "SELECT sum(requests)::bigint FROM retained_operations WHERE resolution=1").Scan(&requests); err != nil || requests != 100 {
					t.Fatalf("operations write summary: requests=%d err=%v", requests, err)
				}
			}
			performanceSLOEmit(t, "OPERATIONS_WRITE_COST", map[string]any{"enabled": enabled, "repetition": repetition, "samples_ms": samples, "scope": "100 serial persistRecord transactions; identical other summaries; reset disposable schema before each case"})
		}
	}
}
