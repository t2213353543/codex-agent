package server

import (
	"fmt"
	"strings"
)

// PostgreSQL decompresses a toasted jsonb value for every independent ->>
// expression. Wide historical observations made the raw aggregate spend its
// entire statement budget repeatedly decoding the same record. Expand the
// required fields once, then retain compact scalar values in the shared CTE.
var managedUsageDecodedTextFields = []string{
	"account_id", "key_id", "endpoint", "upstream_model", "sent_model", "returned_model", "inbound_path", "upstream_path",
	"input_tokens", "output_tokens", "cached_tokens", "latency_known", "latency_ms", "ttft_ms", "reasoning_tokens", "cache_write_tokens", "usage_known",
	"account_selection_ms", "authentication_ms", "image_input_tokens", "image_output_tokens", "proxy_selection_ms", "request_preparation_ms", "response_transfer_ms",
	"websocket_warmup", "cached_tokens_known", "error", "http_code", "error_source", "final_upstream_error_source", "final_upstream_error_code",
}

func managedUsageDecodedExpression(expression string) string {
	expression = strings.ReplaceAll(expression, "lower(COALESCE(record->>'error',''))", "decoded_error_lower")
	for _, name := range managedUsageDecodedTextFields {
		expression = strings.ReplaceAll(expression, "record->>'"+name+"'", "decoded_"+name)
	}
	return strings.ReplaceAll(expression, "record->'groups'", "decoded_groups")
}

func managedUsageDecodedSource(q managedLogQuery) (prefix, source, where string, args []any) {
	// Scalar classification equality is exact, unlike a lossy GIN projection.
	// Do not recompute a long category CASE after that exact predicate has
	// established it. Other predicates retain their existing index choices.
	prefilter := q
	prefilter.ErrorCategory, prefilter.ErrorSeverity = "", ""
	source, where, args = prefilter.readSourceSQL()
	if q.ErrorCategory != "" {
		args = append(args, q.ErrorCategory)
		where += fmt.Sprintf(" AND %s=$%d", managedErrorCategoryProjection, len(args))
	}
	if q.ErrorSeverity != "" {
		args = append(args, q.ErrorSeverity)
		where += fmt.Sprintf(" AND %s=$%d", managedErrorSeverityProjection, len(args))
	}
	present, missing := managedUsageAnalyticsBranches(source, where)
	fields, projected := []string{"groups jsonb"}, []string{"observed.groups AS decoded_groups"}
	for _, name := range managedUsageDecodedTextFields {
		if q.ErrorCategory != "" && stringIn(name, "error", "error_source", "final_upstream_error_source", "final_upstream_error_code") {
			continue // The indexed category already supplies the classification.
		}
		fields = append(fields, name+" text")
		projected = append(projected, "observed."+name+" AS decoded_"+name)
	}
	if q.ErrorCategory == "" {
		projected = append(projected, "lower(COALESCE(observed.error,'')) AS decoded_error_lower")
	}
	// Decode inside the disjoint branches. A common function scan above their
	// UNION cannot be pushed into parallel workers when the missing branch has
	// a parameterized original-record lookup, even if that branch returns zero.
	decode := func(branch string) string {
		return "SELECT created_at,status,model,account_name,key_name," + strings.Join(projected, ",") +
			" FROM (" + branch + ") analytical_rows CROSS JOIN LATERAL jsonb_to_record(record) AS observed(" + strings.Join(fields, ",") + ") WHERE TRUE"
	}
	prefix = "WITH decoded AS MATERIALIZED (" + decode(present) + " UNION ALL " + decode(missing) + "), "
	return prefix, "decoded", "TRUE", args
}

func managedUsageSelectedClassExpressions(q managedLogQuery, args []any) (category, severity string, params []any) {
	category, severity, params = managedFailureSQL, managedErrorSeveritySQL(), args
	if q.ErrorCategory != "" {
		params = append(params, q.ErrorCategory)
		category = fmt.Sprintf("$%d::text", len(params))
		severity = strings.ReplaceAll(severity, "("+managedFailureSQL+")", category)
	}
	if q.ErrorSeverity != "" {
		params = append(params, q.ErrorSeverity)
		severity = fmt.Sprintf("$%d::text", len(params))
	}
	return category, severity, params
}
