package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A/B only the retained dimensional trigger bodies on a disposable schema.
// Existing ingestion/durable summaries stay enabled in both configurations.
func TestPerformanceManagedUsageModelWriteCost(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("opt-in isolated model maintenance cost experiment")
	}
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	for repetition := 0; repetition < 3; repetition++ {
		// Reverse the order every repetition to expose warm/order sensitivity.
		for _, modelEnabled := range []bool{repetition%2 == 1, repetition%2 == 0} {
			if _, err := db.Pool.Exec(t.Context(), `TRUNCATE request_logs,usage_events,usage_minute,usage_daily,usage_totals,account_request_state,account_credential_request_state RESTART IDENTITY`); err != nil {
				t.Fatal(err)
			}
			migration := "010_retained_usage_daily.sql"
			if modelEnabled {
				migration = "017_retained_usage_model.sql"
			}
			raw, err := migrationFiles.ReadFile(migration)
			if err != nil {
				t.Fatal(err)
			}
			for _, operation := range []string{"insert", "update", "delete"} {
				start := strings.Index(string(raw), "FUNCTION maintain_retained_usage_"+operation+"()")
				if start < 0 {
					t.Fatal("missing maintenance function", migration, operation)
				}
				body := string(raw)[start:]
				end := strings.Index(body, "\n$$;")
				if end < 0 {
					t.Fatal("missing maintenance function terminator")
				}
				if _, err := db.Pool.Exec(t.Context(), "CREATE OR REPLACE "+body[:end+4]); err != nil {
					t.Fatal(err)
				}
			}
			for _, concurrency := range []int{1, 4} {
				const count = 100
				samples := make([]float64, count)
				failures := make([]error, count)
				var workers sync.WaitGroup
				started := time.Now()
				for worker := 0; worker < concurrency; worker++ {
					workers.Add(1)
					go func(worker int) {
						defer workers.Done()
						for i := worker; i < count; i += concurrency {
							r := managedPerformanceRecord(i)
							r.EventID = fmt.Sprintf("model-write-cost-%d-%d", concurrency, i)
							r.Model = []string{"cost-a", "cost-b"}[i%2]
							r.AccountID = ""
							start := time.Now()
							_, failures[i] = m.persistRecord(context.Background(), r)
							samples[i] = float64(time.Since(start).Microseconds()) / 1000
						}
					}(worker)
				}
				workers.Wait()
				for _, err := range failures {
					if err != nil {
						t.Fatal(err)
					}
				}
				performanceSLOEmit(t, "MODEL_WRITE_COST", map[string]any{
					"model_enabled": modelEnabled, "repetition": repetition, "concurrency": concurrency,
					"samples_ms": samples, "wall_ms": float64(time.Since(started).Microseconds()) / 1000,
					"scope": "100 persistRecord transactions per case; baseline daily+minute vs model-conditioned additional summaries; all durable ingestion enabled; no HTTP",
				})
			}
			assertRetainedUsageDailyCanonical(t, db)
			if modelEnabled {
				assertFilteredRollupParity(t, m, "model=cost-a", "model=cost-b")
			}
		}
	}
}
