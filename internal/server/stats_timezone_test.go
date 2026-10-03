package server

import (
	"testing"
	"time"
)

func TestStatsDefaultShanghaiAndExplicitTimezone(t *testing.T) {
	for _, zone := range []string{"", "UTC", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			t.Setenv("STATS_TIMEZONE", zone)
			m, err := NewMetrics(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			want := zone
			if want == "" {
				want = "Asia/Shanghai"
			}
			if m.Timezone() != want {
				t.Fatalf("timezone = %q, want %q", m.Timezone(), want)
			}
			start := time.Unix(m.TodayStart(), 0).In(m.location)
			if start.Hour() != 0 || start.Minute() != 0 || start.Second() != 0 {
				t.Fatalf("not calendar midnight: %v", start)
			}
		})
	}
}
