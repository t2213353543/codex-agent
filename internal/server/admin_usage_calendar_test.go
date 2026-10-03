package server

import (
	"testing"
	"time"
)

func TestManagedUsageCalendarMissingDays(t *testing.T) {
	zone, _ := time.LoadLocation("America/New_York")
	start := time.Date(2026, 3, 7, 0, 0, 0, 0, zone)
	end := start.AddDate(0, 0, 3)
	stats := managedUsageStats{From: start.Unix(), To: end.Unix(), Step: 86400, Timezone: zone.String(), Series: []managedUsagePoint{
		{Time: start.Unix(), managedUsageTotals: managedUsageTotals{Requests: 1}},
		{Time: start.AddDate(0, 0, 2).Unix(), managedUsageTotals: managedUsageTotals{Requests: 1}},
	}}
	if err := stats.fillSeries(); err != nil {
		t.Fatal(err)
	}
	if len(stats.Series) != 3 || stats.Series[1].Requests != 0 || stats.Series[1].Tokens != 0 || stats.Series[2].Time-stats.Series[1].Time != 23*3600 {
		t.Fatalf("missing bucket incorrectly filled: %+v", stats.Series)
	}
	stats.From = time.Date(2026, 1, 15, 0, 0, 0, 0, zone).Unix()
	stats.To = time.Date(2026, 4, 1, 0, 0, 0, 0, zone).Unix()
	stats.Step = 2592000
	stats.Series = nil
	if err := stats.fillSeries(); err != nil || len(stats.Series) != 3 || time.Unix(stats.Series[1].Time, 0).In(zone).Day() != 1 {
		t.Fatal("calendar month fill", err, stats.Series)
	}
}
