package server

import (
	"fmt"
	"strings"
)

// Table names are a fixed server choice. All model names remain bind values.
func managedUsageRollupTables(q managedLogQuery) (string, string) {
	if q.Model != "" {
		return "retained_usage_model_minute", "retained_usage_model_daily"
	}
	return "retained_usage_minute", "retained_usage_daily"
}

// A full UTC day belongs to exactly one UTC day/week/month chart point. Other
// zones and sub-day points retain minute totals, including historical offsets
// containing seconds. An explicit positive lower bound keeps pre-epoch rows
// out of this optimization when an unbounded query can include them.
func managedUsageDailyTotalEligible(q managedLogQuery) bool {
	return q.From > 0 && q.Timezone == "UTC" && q.Step >= 86400
}

// Every selected dimension is read from exactly one daily or minute source.
// Full UTC days can also serve totals and calendar points; edges and unsafe
// days continue to use exact minute/detail data.
func managedUsageCoarseSelection(q managedLogQuery, minuteWhere []string) string {
	minuteTable, dailyTable := managedUsageRollupTables(q)
	projection := "bucket,to_timestamp(bucket) AS series_at,status,kind,resource_id,name," + managedUsageRollupColumns
	minutes := strings.Join(minuteWhere, " AND ")
	base := "SELECT " + projection + " FROM " + minuteTable + " WHERE " + minutes + " AND NOT EXISTS(SELECT 1 FROM unsafe WHERE unsafe.bucket=" + minuteTable + ".bucket)"
	if _, _, class := managedUsageClassRollup(q); class {
		return base
	}
	dayStart, dayEnd := q.From, q.To
	if dayStart%86400 != 0 {
		dayStart = (dayStart/86400 + 1) * 86400
	}
	if dayEnd != 0 {
		dayEnd = dayEnd / 86400 * 86400
	}
	if q.To != 0 && dayStart >= dayEnd {
		return base
	}
	// retained_usage_daily stores only dimension rows (kind<>'total'); totals
	// always come from the minute source, including aligned daily windows.
	// Reading totals from daily would be empty: migration 010 never inserts
	// kind='total' into the daily table (minute total rows stay the compact
	// chart/first-last source). Keep the same time/status placeholders as the
	// minute source; the extra day bounds ensure complete selected UTC days.
	dimensionOnly := " AND kind<>'total'"
	dailyWhere := append(append([]string{}, minuteWhere...), "kind<>'total'", fmt.Sprintf("bucket>=%d", dayStart))
	if q.To != 0 {
		dailyWhere = append(dailyWhere, fmt.Sprintf("bucket<%d", dayEnd))
	}
	daily := "SELECT " + projection + " FROM " + dailyTable + " WHERE " + strings.Join(dailyWhere, " AND ") + " AND NOT EXISTS(SELECT 1 FROM unsafe_days WHERE unsafe_days.bucket=" + dailyTable + ".bucket)"
	parts := []string{daily, base + " AND kind='total'"}
	if dayStart != 0 {
		parts = append(parts, base+dimensionOnly+fmt.Sprintf(" AND bucket<%d", dayStart))
	}
	if q.To != 0 {
		parts = append(parts, base+dimensionOnly+fmt.Sprintf(" AND bucket>=%d", dayEnd))
	}
	// Probe only dirty full days; a broad OR exclusion could otherwise scan all
	// account-minute rows before discarding the days already served by daily.
	dirty := fmt.Sprintf("SELECT bucket FROM unsafe_days WHERE bucket>=%d", dayStart)
	if q.To != 0 {
		dirty += fmt.Sprintf(" AND bucket<%d", dayEnd)
	}
	parts = append(parts, "SELECT selected.* FROM ("+dirty+") dirty CROSS JOIN LATERAL ("+base+dimensionOnly+" AND bucket>=dirty.bucket AND bucket<dirty.bucket+86400 OFFSET 0) selected")
	return strings.Join(parts, " UNION ALL ")
}
