package server

import (
	"fmt"
	"strings"
	"testing"
)

func TestManagedUsageUTCDailySelection(t *testing.T) {
	const base int64 = 1710028800
	for _, spec := range []struct {
		zone string
		step int64
		from int64
		want bool
	}{
		{"UTC", 86400, base, true}, {"UTC", 604800, base, true}, {"UTC", 2592000, base, true},
		{"UTC", 3600, base, false}, {"Asia/Shanghai", 86400, base, false},
		{"America/New_York", 86400, base, false}, {"UTC", 86400, 0, false},
	} {
		q := managedLogQuery{Timezone: spec.zone, Step: spec.step, From: spec.from, To: base + 6*86400}
		if got := managedUsageDailyTotalEligible(q); got != spec.want {
			t.Fatalf("%+v: eligible=%v", spec, got)
		}
		statement, _ := managedUsageRollupQuery(q, managedUsageStats{Step: spec.step})
		if strings.Contains(statement, "to_timestamp(bucket+59)") && spec.zone == "UTC" {
			t.Fatal("UTC must not scan total minutes for historical-second calendar offsets")
		}
		wantEdge := "max(total_source.bucket)+60)"
		if spec.want {
			wantEdge = "max(total_source.bucket)+86400)"
		}
		if !strings.Contains(statement, wantEdge) {
			t.Fatalf("%+v: wrong exact detail edge", spec)
		}
	}
}

func TestPGManagedUsageUTCDailyTotalsExactEdges(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1711670400 // 2024-03-29 UTC, spans a month and week boundary.
	// No record at midnight: first/last cannot be inferred from a daily key.
	for day := 0; day < 8; day++ {
		for i, second := range []int64{61, 3601, 36011, 86398} {
			r := managedPerformanceRecord(i)
			r.EventID = fmt.Sprintf("utc-day-%d-%d", day, i)
			r.Time = base + int64(day)*86400 + second
			r.Model = []string{"utc-a", "utc-b"}[i%2]
			r.Status = []string{"ok", "error", "aborted", "ok"}[i]
			m.Record(r)
		}
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for _, step := range []int64{86400, 604800, 2592000} {
			for _, bounds := range [][2]int64{
				{base, base + 8*86400}, {base + 62, base + 7*86400 + 86398},
				{base + 300, base + 3700}, {base, base + 60},
				{base + 2*86400, 0},
			} {
				filter := fmt.Sprintf("from=%d&step=%d&timezone=UTC", bounds[0], step)
				if bounds[1] > 0 {
					filter += fmt.Sprintf("&to=%d", bounds[1])
				}
				assertManagedUsageRollupParity(t, m, filter, filter+"&status=failed", filter+"&status=ok")
				assertFilteredRollupParity(t, m, filter+"&model=utc-a", filter+"&model=utc-b", filter+"&model=missing")
			}
		}
	}
	check()
	// Fractional/unsafe observations force only their day back to exact minute
	// sources. Deleting them must restore the coarse path without double count.
	if _, err := db.Pool.Exec(t.Context(), `UPDATE request_logs SET created_at=created_at+interval '0.75 second',record=record||jsonb_build_object('upstream_model',$1::text) WHERE event_id='utc-day-2-0'`, strings.Repeat("long", 500)); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := db.Pool.Exec(t.Context(), `DELETE FROM request_logs WHERE event_id IN ('utc-day-2-0','utc-day-0-0','utc-day-7-3')`); err != nil {
		t.Fatal(err)
	}
	check()
}
