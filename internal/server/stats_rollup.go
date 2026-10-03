package server

import (
	"fmt"
	"strings"
	"time"
)

// Rollups cover time/status only. All other predicates retain the original
// detail query, including snapshots and review membership. Internal queryers
// deliberately use raw SQL so parity tests and diagnostics can compare paths.
func managedUsageRollupEligible(q managedLogQuery) bool {
	if q.accountsOnly || q.statsQueryer != nil || q.AsOf != "" && q.SnapshotID == "" || q.From < 0 || q.To < 0 {
		return false
	}
	q.From, q.To, q.Status, q.AsOf, q.SnapshotID = 0, 0, "", "", ""
	where, args := q.whereSQL()
	return where == "TRUE" && len(args) == 0
}

const managedUsageRollupColumns = "requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens"

// managedUsageRollupSourceSQL returns CTEs ending in source(bucket,series_at,status,kind,
// resource_id,name,<counter columns>). Names are not collapsed here: callers
// can recover the lexically greatest retained label after deletions. Unsafe
// minutes and sub-minute boundaries are read from details in this same snapshot.
func managedUsageRollupSourceSQL(q managedLogQuery) (string, []any) {
	return managedUsageRollupSourceWithMaterialization(q, true)
}

// Options and the separated stats branches let PostgreSQL push kind/column
// projections into each source, instead of writing unused wide numeric rows.
func managedUsageRollupSourceWithMaterialization(q managedLogQuery, materialized bool) (string, []any) {
	rawWhere, args := q.whereSQL()
	minuteTable, _ := managedUsageRollupTables(q)
	addTime := func(stamp int64) string {
		args = append(args, time.Unix(stamp, 0).UTC())
		return fmt.Sprintf("$%d", len(args))
	}
	fullStart, fullEnd := q.From, q.To
	if fullStart%60 != 0 {
		fullStart = (fullStart/60 + 1) * 60
	}
	if fullEnd != 0 {
		fullEnd = fullEnd / 60 * 60
	}
	minuteWhere := []string{"requests>0"}
	guardWhere := []string{"requests>0"}
	if q.From != 0 {
		minuteWhere = append(minuteWhere, fmt.Sprintf("bucket>=%d", fullStart))
		guardWhere = append(guardWhere, fmt.Sprintf("bucket>=%d", fullStart))
	}
	if q.To != 0 {
		minuteWhere = append(minuteWhere, fmt.Sprintf("bucket<%d", fullEnd))
		guardWhere = append(guardWhere, fmt.Sprintf("bucket<%d", fullEnd))
	}
	if q.Status == "failed" {
		minuteWhere = append(minuteWhere, "status<>'ok'")
	} else if q.Status != "" {
		args = append(args, q.Status)
		minuteWhere = append(minuteWhere, fmt.Sprintf("status=$%d", len(args)))
	}
	if q.Model != "" {
		args = append(args, q.Model)
		minuteWhere = append(minuteWhere, fmt.Sprintf("selected_model=$%d", len(args)))
	}
	projection := "id,created_at,status,model,account_name,key_name"
	rawParts := []string{}
	if q.From != 0 && q.To != 0 && fullStart >= fullEnd {
		rawParts = append(rawParts, "SELECT "+projection+" FROM request_logs WHERE "+rawWhere)
		minuteWhere = append(minuteWhere, "FALSE")
		guardWhere = append(guardWhere, "FALSE")
	} else {
		if fullStart != q.From {
			rawParts = append(rawParts, "SELECT "+projection+" FROM request_logs WHERE "+rawWhere+" AND created_at<"+addTime(fullStart))
		}
		if fullEnd != q.To {
			rawParts = append(rawParts, "SELECT "+projection+" FROM request_logs WHERE "+rawWhere+" AND created_at>="+addTime(fullEnd))
		}
	}
	// OFFSET 0 preserves the parameterized minute probes. A flat OR predicate
	// involving floor(created_at) can otherwise cause a full request_logs scan.
	rawParts = append(rawParts, "SELECT l.* FROM unsafe u CROSS JOIN LATERAL (SELECT "+projection+" FROM request_logs WHERE "+rawWhere+" AND created_at>=to_timestamp(u.bucket) AND created_at<to_timestamp(u.bucket+60) OFFSET 0) l")
	rawSums := []string{}
	for _, column := range strings.Split(managedUsageRollupColumns, ",") {
		rawSums = append(rawSums, "sum("+column+") AS "+column)
	}
	unsafeSQL := "SELECT bucket FROM retained_usage_unsafe_minute WHERE " + strings.Join(guardWhere, " AND ")
	classCTE := ""
	if kind, value, eligible := managedUsageClassRollup(q); eligible {
		args = append(args, value)
		classCTE = managedUsageClassMinutesSQL(minuteTable, minuteWhere, kind, fmt.Sprintf("$%d", len(args)))
		unsafeSQL += " UNION SELECT bucket FROM class_minutes WHERE matched>0 AND matched<total"
		// Daily summaries may combine minutes with different categories. Class
		// reads use minute summaries, only when the entire selected minute is
		// proven to match; mixed/unsafe minutes use the exact original filter.
		minuteWhere = append(minuteWhere, "bucket IN (SELECT bucket FROM class_minutes WHERE matched>0 AND matched=total)")
	}
	if q.SnapshotID != "" {
		// The UI always asks for a fixed-ID snapshot. Summaries are still exact
		// for minutes whose current matching rows all belong to that snapshot.
		// Newly committed rows (including a late commit with a lower ID) make
		// only their minute unsafe; the existing raw branch then reads just the
		// snapshot members of that minute. Deletions and summary updates share
		// this statement's MVCC snapshot, so no removed record is resurrected.
		current := q
		current.AsOf, current.SnapshotID = "", ""
		currentWhere, currentArgs := current.whereSQL()
		// Partial boundary minutes already belong to rawParts above. Only
		// complete selected minutes may become additional unsafe probes.
		if q.From != 0 {
			currentWhere += fmt.Sprintf(" AND created_at>=to_timestamp(%d)", fullStart)
		}
		if q.To != 0 {
			currentWhere += fmt.Sprintf(" AND created_at<to_timestamp(%d)", fullEnd)
		}
		currentWhere = managedSQLShiftParameters(currentWhere, len(args))
		args = append(args, currentArgs...)
		args = append(args, q.SnapshotID)
		unsafeSQL += fmt.Sprintf(" UNION SELECT DISTINCT floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket FROM request_logs WHERE %s AND NOT EXISTS(SELECT 1 FROM management_cleanup_targets snapshot WHERE snapshot.preview_id=$%d AND snapshot.log_id=request_logs.id)", currentWhere, len(args))
	}
	if q.Model != "" {
		// A second model string in the dimensional key needs a smaller byte
		// budget. Such records remain legal and use exact detail rows for the
		// affected minute; the original, unconditioned summaries stay usable.
		unsafeSQL += " UNION SELECT bucket FROM retained_usage_model_unsafe_minute WHERE " + strings.Join(guardWhere, " AND ")
	}
	if q.Step >= 86400 && q.Timezone != "UTC" {
		unit := "day"
		if q.Step == 604800 {
			unit = "week"
		}
		if q.Step == 2592000 {
			unit = "month"
		}
		args = append(args, q.Timezone, unit)
		// Historical zones can change calendar dates inside a UTC minute (offsets
		// containing seconds). Such minutes use exact record timestamps as well.
		boundary := fmt.Sprintf("date_trunc($%d::text,to_timestamp(bucket) AT TIME ZONE $%d::text)<>date_trunc($%d::text,to_timestamp(bucket+59) AT TIME ZONE $%d::text)", len(args), len(args)-1, len(args), len(args)-1)
		unsafeSQL += " UNION SELECT bucket FROM " + minuteTable + " WHERE kind='total' AND " + strings.Join(minuteWhere, " AND ") + " AND " + boundary
	}
	mode := "NOT MATERIALIZED"
	if materialized {
		mode = "MATERIALIZED"
	}
	ctes := `WITH ` + classCTE + `unsafe AS MATERIALIZED (` + unsafeSQL + `),
 unsafe_days AS MATERIALIZED (SELECT DISTINCT floor(bucket::numeric/86400)::bigint*86400 AS bucket FROM unsafe),
 raw_rows AS MATERIALIZED (` + strings.Join(rawParts, " UNION ALL ") + `),
 ` + managedUsageRawRollupProjectionSQL() + `raw_counted AS MATERIALIZED (SELECT floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,r.created_at AS series_at,r.status,r.model,r.account_name,r.key_name,r.record,c.*
 FROM raw_projected r CROSS JOIN LATERAL retained_usage_counters(r.record,r.status)c),
 source AS ` + mode + ` (
 ` + managedUsageCoarseSelection(q, minuteWhere) + `
 UNION ALL SELECT bucket,series_at,status,d.kind,d.resource_id,d.name,` + strings.Join(rawSums, ",") + `
 FROM raw_counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
 GROUP BY bucket,series_at,status,d.kind,d.resource_id,d.name
 ) `
	return ctes, args
}

func managedUsageRollupQuery(q managedLogQuery, out managedUsageStats) (string, []any) {
	q.Step = out.Step
	source, args := managedUsageRollupSourceWithMaterialization(q, false)
	unit := "day"
	if out.Step == 604800 {
		unit = "week"
	}
	if out.Step == 2592000 {
		unit = "month"
	}
	args = append(args, q.Timezone, unit, out.Step)
	zone, unitIndex, step := len(args)-2, len(args)-1, len(args)
	point := fmt.Sprintf("CASE WHEN $%d::bigint<86400 THEN (extract(epoch FROM series_at)::bigint/$%d::bigint)*$%d::bigint ELSE extract(epoch FROM (date_trunc($%d::text,series_at AT TIME ZONE $%d::text) AT TIME ZONE $%d::text))::bigint END", step, step, step, unitIndex, zone, zone)
	rawWhere, rawArgs := q.whereSQL()
	_ = rawArgs // All original filter arguments lead the source argument list.
	edgeWidth := "60"
	if managedUsageDailyTotalEligible(q) {
		// A daily counter's bucket is the day start, not the first observation.
		// Seek the original filtered detail within that day for exact first/last.
		// For partial-day minute rows the original time predicates still clamp
		// the range, and ordering chooses the exact earliest/latest observation.
		edgeWidth = "86400"
	}
	first := "CASE WHEN GROUPING(point)=1 THEN COALESCE((SELECT extract(epoch FROM created_at)::bigint FROM request_logs WHERE " + rawWhere + " AND created_at>=to_timestamp(min(total_source.bucket)) AND created_at<to_timestamp(min(total_source.bucket)+" + edgeWidth + ") ORDER BY created_at,id LIMIT 1),0) ELSE 0 END"
	last := "CASE WHEN GROUPING(point)=1 THEN COALESCE((SELECT extract(epoch FROM created_at)::bigint FROM request_logs WHERE " + rawWhere + " AND created_at>=to_timestamp(max(total_source.bucket)) AND created_at<to_timestamp(max(total_source.bucket)+" + edgeWidth + ") ORDER BY created_at DESC,id DESC LIMIT 1),0) ELSE 0 END"
	// Aggregate totals and chart points directly from the compact total-minute
	// source. Expanding them alongside all dimensions duplicated wide numeric
	// rows and forced a disk sort on otherwise bounded daily summaries.
	return source + `, dimensions AS (
 SELECT CASE kind WHEN 'account_legacy' THEN 'account' WHEN 'key_legacy' THEN 'key' ELSE kind END AS kind,resource_id,name,` + managedUsageRollupColumns + ` FROM source WHERE kind<>'total'
 ), total_source AS (
 SELECT bucket,` + point + ` AS point,` + managedUsageRollupColumns + ` FROM source WHERE kind='total'
 ) SELECT kind,max(name COLLATE "C"),resource_id,0::bigint,` + managedUsageRollupAggregateSQL("0", "0") + ` FROM dimensions
 GROUP BY kind,CASE WHEN kind IN ('account','key') THEN '' ELSE name END,resource_id
 UNION ALL SELECT CASE WHEN GROUPING(point)=1 THEN 'total' ELSE 'series' END,'','',CASE WHEN GROUPING(point)=1 THEN 0::bigint ELSE point END,` + managedUsageRollupAggregateSQL(first, last) + `
 FROM total_source GROUP BY GROUPING SETS((),(point))`, args
}

func managedUsageRollupAggregateSQL(first, last string) string {
	sum := func(column string) string { return "COALESCE(sum(" + column + "),0)::bigint" }
	avg := func(name string) string { return "(sum(" + name + "_sum)/NULLIF(sum(" + name + "_samples),0))::float8" }
	nullable := func(value, samples string) string {
		return "CASE WHEN sum(" + samples + ")>0 THEN sum(" + value + ")::bigint END"
	}
	image := func(name string) string {
		return "CASE WHEN sum(" + name + "_samples)>0 AND sum(" + name + "_sum)<=9223372036854775807::numeric THEN sum(" + name + "_sum)::bigint END"
	}
	fields := []string{sum("requests"), sum("success"), sum("errors"), sum("aborted"), sum("input_tokens"), sum("output_tokens"), sum("cached_tokens"), avg("latency"), sum("latency_samples"), first, last, sum("usage_known"), "COALESCE(sum(requests)-sum(usage_known),0)::bigint",
		sum("ttft_samples"), avg("ttft"), sum("reasoning_samples"), nullable("reasoning_sum", "reasoning_samples"), sum("cache_write_samples"), nullable("cache_write_tokens", "cache_write_samples")}
	for _, name := range []string{"request_preparation", "account_selection", "proxy_selection", "authentication", "response_transfer"} {
		fields = append(fields, sum(name+"_samples"), avg(name))
	}
	fields = append(fields, sum("image_input_samples"), image("image_input"), sum("image_output_samples"), image("image_output"), "COALESCE(sum(image_input_sum)>9223372036854775807::numeric,false)", "COALESCE(sum(image_output_sum)>9223372036854775807::numeric,false)", sum("cache_read_samples"), sum("cache_read_unknown_requests"), sum("cache_read_input_tokens"), sum("cache_read_tokens"))
	return strings.Join(fields, ",")
}
