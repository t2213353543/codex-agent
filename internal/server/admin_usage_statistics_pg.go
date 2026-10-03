package server

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

type managedPGStatsQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func managedUsageGroupsSQL(accountsOnly bool) string {
	if accountsOnly {
		return `SELECT 'account',max(account_name COLLATE "C"),account_bucket,0::bigint,` + managedUsageAggregateSQL + ` FROM source GROUP BY account_bucket`
	}
	// A request contributes once to each independent dimension. Group IDs are
	// multi-valued, so aggregate those in their own branch after deduplication.
	kinds, names, groups, present := []string{}, []string{}, []string{"()"}, []string{}
	for _, d := range []struct{ kind, column, label string }{
		{"model", "model", "model"}, {"endpoint", "endpoint", "endpoint"},
		{"account", "account_bucket", "account_name"}, {"key", "key_bucket", "key_name"},
		{"series", "bucket", ""}, {"category", "category", "category"}, {"severity", "severity", "severity"},
		{"upstream_model", "upstream_model", "upstream_model"}, {"sent_model", "sent_model", "sent_model"},
		{"returned_model", "returned_model", "returned_model"}, {"inbound_path", "inbound_path", "inbound_path"}, {"upstream_path", "upstream_path", "upstream_path"},
	} {
		grouped := "GROUPING(" + d.column + ")=0"
		kinds = append(kinds, "WHEN "+grouped+" THEN '"+d.kind+"'")
		if d.label != "" {
			label := d.label
			if d.label != d.column {
				label = "max(" + d.label + ` COLLATE "C")`
			}
			names = append(names, "WHEN "+grouped+" THEN "+label)
		}
		groups = append(groups, "("+d.column+")")
		if stringIn(d.kind, "category", "severity", "upstream_model", "sent_model", "returned_model", "inbound_path", "upstream_path") {
			present = append(present, "(GROUPING("+d.column+")=1 OR "+d.column+"<>'')")
		}
	}
	return "SELECT CASE " + strings.Join(kinds, " ") + " ELSE 'total' END,CASE " + strings.Join(names, " ") + ` ELSE '' END,
 CASE WHEN GROUPING(account_bucket)=0 THEN account_bucket WHEN GROUPING(key_bucket)=0 THEN key_bucket ELSE '' END,
 CASE WHEN GROUPING(bucket)=0 THEN bucket ELSE 0::bigint END,` + managedUsageAggregateSQL +
		" FROM source GROUP BY GROUPING SETS (" + strings.Join(groups, ",") + ") HAVING " + strings.Join(present, " AND ") +
		` UNION ALL SELECT 'group',group_id,group_id,0::bigint,` + managedUsageAggregateSQL + ` FROM source CROSS JOIN LATERAL
 (SELECT DISTINCT value AS group_id FROM jsonb_array_elements_text(groups) WHERE value<>'') g GROUP BY group_id`
}

func (q managedLogQuery) managedStatsQueryer(m *Metrics) managedPGStatsQueryer {
	if q.statsQueryer != nil {
		return q.statsQueryer
	}
	if q.ErrorCategory != "" || q.ErrorSeverity != "" {
		return managedClassStatsQueryer{pool: m.db.Pool}
	}
	return m.db.Pool
}

func (m *Metrics) managedLogStatsPG(ctx context.Context, q managedLogQuery, out managedUsageStats) (managedUsageStats, error) {
	prefix, source, where, args := managedUsageDecodedSource(q)
	category, severity, args := managedUsageSelectedClassExpressions(q, args)
	unit := "day"
	if out.Step == 604800 {
		unit = "week"
	}
	if out.Step == 2592000 {
		unit = "month"
	}
	args = append(args, q.Timezone, unit, out.Step)
	zone, unitIndex, step := len(args)-2, len(args)-1, len(args)
	bucket := fmt.Sprintf("CASE WHEN $%d::bigint<86400 THEN (extract(epoch FROM created_at)::bigint/$%d::bigint)*$%d::bigint ELSE extract(epoch FROM (date_trunc($%d::text,created_at AT TIME ZONE $%d::text) AT TIME ZONE $%d::text))::bigint END", step, step, step, unitIndex, zone, zone)
	materialization := "MATERIALIZED"
	if q.accountsOnly {
		// One grouping uses each counter once and needs no dimension expansion.
		// Inlining also lets the planner omit unused observation dimensions.
		materialization = "NOT MATERIALIZED"
	}
	// Keep only parsed counters and compact dimensions in the shared source.
	// GROUPING SETS aggregates each request directly instead of materializing
	// up to fourteen wide copies of it before aggregation.
	statement := prefix + managedUsageDecodedExpression(fmt.Sprintf(`source AS %s (
 SELECT model,status,account_name,key_name,
 COALESCE(record->>'upstream_model','') AS upstream_model,COALESCE(record->>'sent_model','') AS sent_model,
 COALESCE(record->>'returned_model','') AS returned_model,COALESCE(record->>'inbound_path','') AS inbound_path,
 COALESCE(record->>'upstream_path','') AS upstream_path,COALESCE(record->'groups','[]'::jsonb) AS groups,
 COALESCE(NULLIF(record->>'account_id',''),'name:'||account_name) AS account_bucket,COALESCE(NULLIF(record->>'key_id',''),'name:'||key_name) AS key_bucket,
 COALESCE(record->>'endpoint','') AS endpoint,(%s) AS category,(%s) AS severity,extract(epoch FROM created_at)::bigint AS stamp,%s AS bucket,
 COALESCE((record->>'input_tokens')::bigint,0) AS input,COALESCE((record->>'output_tokens')::bigint,0) AS output,COALESCE((record->>'cached_tokens')::bigint,0) AS cached,
 CASE WHEN COALESCE((record->>'latency_known')::boolean,COALESCE((record->>'latency_ms')::bigint,0)>0) AND COALESCE((record->>'latency_ms')::bigint,0)>=0 THEN COALESCE((record->>'latency_ms')::bigint,0) END AS latency,(record->>'ttft_ms')::bigint AS ttft,(record->>'reasoning_tokens')::bigint AS reasoning,(record->>'cache_write_tokens')::bigint AS cache_write,
 COALESCE((record->>'usage_known')::boolean,COALESCE((record->>'input_tokens')::bigint,0)<>0 OR COALESCE((record->>'output_tokens')::bigint,0)<>0 OR COALESCE((record->>'cached_tokens')::bigint,0)<>0) AS usage_known
,
 (record->>'account_selection_ms')::bigint AS observed_account_selection_ms,
 (record->>'authentication_ms')::bigint AS observed_authentication_ms,
 (record->>'image_input_tokens')::bigint AS observed_image_input_tokens,
 (record->>'image_output_tokens')::bigint AS observed_image_output_tokens,
 (record->>'proxy_selection_ms')::bigint AS observed_proxy_selection_ms,
 (record->>'request_preparation_ms')::bigint AS observed_request_preparation_ms,
 (record->>'response_transfer_ms')::bigint AS observed_response_transfer_ms,
 (`+cacheObservationEligibleSQL+`) AS cache_eligible,(`+cacheReadObservedSQL+`) AS cache_read_observed
 FROM %s WHERE %s
 ) `, materialization, category, severity, bucket, source, where)) + managedUsageGroupsSQL(q.accountsOnly)
	if managedUsageRollupEligible(q) {
		statement, args = managedUsageRollupQuery(q, out)
	} else if _, _, eligible := managedUsageClassRollup(q); eligible {
		fastSQL, fastArgs := managedUsageRollupQuery(q, out)
		statement, args = managedUsageAdaptiveClassSQL(q, fastSQL, fastArgs, statement, args)
	} else if projected, params, ok := managedUsageAccountProjectionSQL(q, statement, args); ok {
		statement, args = projected, params
	} else if filtered, params, ok := managedUsageModelFilterSQL(q, out, statement, args); ok {
		statement, args = filtered, params
	} else if filtered, params, ok := managedUsageConstantFilterSQL(q, out, statement, args); ok {
		statement, args = filtered, params
	}
	rows, err := q.managedStatsQueryer(m).Query(ctx, statement, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name, id string
		var point, first, last int64
		var t managedUsageTotals
		if err = rows.Scan(&kind, &name, &id, &point, &t.Requests, &t.Success, &t.Errors, &t.Aborted, &t.Input, &t.Output, &t.Cached, &t.AverageLatency, &t.LatencySamples, &first, &last, &t.UsageKnownRequests, &t.UsageUnknownRequests, &t.TTFTSamples, &t.AverageTTFT, &t.ReasoningSamples, &t.ReasoningTokens, &t.CacheWriteSamples, &t.CacheWriteTokens, &t.RequestPreparationSamples, &t.AverageRequestPreparation, &t.AccountSelectionSamples, &t.AverageAccountSelection, &t.ProxySelectionSamples, &t.AverageProxySelection, &t.AuthenticationSamples, &t.AverageAuthentication, &t.ResponseTransferSamples, &t.AverageResponseTransfer, &t.ImageInputSamples, &t.ImageInputTokens, &t.ImageOutputSamples, &t.ImageOutputTokens, &t.ImageInputTotalOverflow, &t.ImageOutputTotalOverflow, &t.CacheReadSamples, &t.CacheReadUnknownRequests, &t.CacheReadInputTokens, &t.CacheReadTokens); err != nil {
			return out, err
		}
		t.Tokens = t.Input + t.Output
		t.finishCacheRead()
		switch kind {
		case "total":
			out.managedUsageTotals = t
			out.FirstRecordAt, out.LastRecordAt = first, last
		case "account":
			if strings.HasPrefix(id, "name:") {
				id = ""
			}
			out.Accounts = append(out.Accounts, managedUsageResource{id, name, t})
		case "key":
			if strings.HasPrefix(id, "name:") {
				id = ""
			}
			out.Keys = append(out.Keys, managedUsageResource{id, name, t})
		case "group":
			out.Groups = append(out.Groups, managedUsageResource{id, name, t})
		case "model":
			out.Models = append(out.Models, managedUsageDimension{name, t})
		case "upstream_model":
			out.UpstreamModels = append(out.UpstreamModels, managedUsageDimension{name, t})
		case "sent_model":
			out.SentModels = append(out.SentModels, managedUsageDimension{name, t})
		case "returned_model":
			out.ReturnedModels = append(out.ReturnedModels, managedUsageDimension{name, t})
		case "inbound_path":
			out.InboundPaths = append(out.InboundPaths, managedUsageDimension{name, t})
		case "upstream_path":
			out.UpstreamPaths = append(out.UpstreamPaths, managedUsageDimension{name, t})
		case "endpoint":
			out.Endpoints = append(out.Endpoints, managedUsageDimension{name, t})
		case "category":
			out.ErrorCategories = append(out.ErrorCategories, managedUsageDimension{name, t})
		case "severity":
			out.ErrorSeverities = append(out.ErrorSeverities, managedUsageDimension{name, t})
		case "series":
			out.Series = append(out.Series, managedUsagePoint{point, t})
		}
	}
	for _, values := range [][]managedUsageDimension{out.Models, out.UpstreamModels, out.SentModels, out.ReturnedModels, out.InboundPaths, out.UpstreamPaths, out.Endpoints, out.ErrorCategories} {
		sort.Slice(values, func(i, j int) bool {
			if values[i].Requests == values[j].Requests {
				return values[i].Name < values[j].Name
			}
			return values[i].Requests > values[j].Requests
		})
	}
	sort.Slice(out.Series, func(i, j int) bool { return out.Series[i].Time < out.Series[j].Time })
	sort.Slice(out.ErrorSeverities, func(i, j int) bool {
		return managedErrorSeverityOrder(out.ErrorSeverities[i].Name) < managedErrorSeverityOrder(out.ErrorSeverities[j].Name)
	})
	sortManagedResources(&out)
	if err := rows.Err(); err != nil {
		return out, err
	}
	if q.accountsOnly {
		return out, nil
	}
	out.setCoverage()
	err = out.fillSeries()
	return out, err
}

const managedUsageAggregateSQL = `
 count(*)::bigint,count(*) FILTER(WHERE status='ok')::bigint,count(*) FILTER(WHERE status<>'ok')::bigint,count(*) FILTER(WHERE status='aborted')::bigint,
 COALESCE(sum(input),0)::bigint,COALESCE(sum(output),0)::bigint,COALESCE(sum(cached),0)::bigint,avg(latency)::float8,count(latency)::bigint,
 COALESCE(min(stamp),0)::bigint,COALESCE(max(stamp),0)::bigint,count(*) FILTER(WHERE usage_known)::bigint,count(*) FILTER(WHERE NOT usage_known)::bigint,
 count(ttft) FILTER(WHERE ttft>=0)::bigint,avg(ttft) FILTER(WHERE ttft>=0)::float8,
 count(reasoning) FILTER(WHERE reasoning>=0)::bigint,(sum(reasoning) FILTER(WHERE reasoning>=0))::bigint,
 count(cache_write) FILTER(WHERE cache_eligible AND cache_write>=0)::bigint,(sum(cache_write) FILTER(WHERE cache_eligible AND cache_write>=0))::bigint,
 count(*) FILTER(WHERE observed_request_preparation_ms>=0)::bigint,avg(observed_request_preparation_ms) FILTER(WHERE observed_request_preparation_ms>=0)::float8,
 count(*) FILTER(WHERE observed_account_selection_ms>=0)::bigint,avg(observed_account_selection_ms) FILTER(WHERE observed_account_selection_ms>=0)::float8,
 count(*) FILTER(WHERE observed_proxy_selection_ms>=0)::bigint,avg(observed_proxy_selection_ms) FILTER(WHERE observed_proxy_selection_ms>=0)::float8,
 count(*) FILTER(WHERE observed_authentication_ms>=0)::bigint,avg(observed_authentication_ms) FILTER(WHERE observed_authentication_ms>=0)::float8,
 count(*) FILTER(WHERE observed_response_transfer_ms>=0)::bigint,avg(observed_response_transfer_ms) FILTER(WHERE observed_response_transfer_ms>=0)::float8,
 count(*) FILTER(WHERE observed_image_input_tokens>=0)::bigint,CASE WHEN sum(observed_image_input_tokens) FILTER(WHERE observed_image_input_tokens>=0)<=9223372036854775807::numeric THEN (sum(observed_image_input_tokens) FILTER(WHERE observed_image_input_tokens>=0))::bigint END,
 count(*) FILTER(WHERE observed_image_output_tokens>=0)::bigint,CASE WHEN sum(observed_image_output_tokens) FILTER(WHERE observed_image_output_tokens>=0)<=9223372036854775807::numeric THEN (sum(observed_image_output_tokens) FILTER(WHERE observed_image_output_tokens>=0))::bigint END,
 COALESCE(sum(observed_image_input_tokens) FILTER(WHERE observed_image_input_tokens>=0)>9223372036854775807::numeric,false),
 COALESCE(sum(observed_image_output_tokens) FILTER(WHERE observed_image_output_tokens>=0)>9223372036854775807::numeric,false),
 count(*) FILTER(WHERE cache_eligible AND cache_read_observed)::bigint,
 count(*) FILTER(WHERE cache_eligible AND NOT cache_read_observed)::bigint,
 COALESCE(sum(input) FILTER(WHERE cache_eligible AND cache_read_observed),0)::bigint,
 COALESCE(sum(cached) FILTER(WHERE cache_eligible AND cache_read_observed),0)::bigint`
