package server

const managedErrorSeveritySource = "management_rules_v1"

// These are management interpretations of retained categories and HTTP facts,
// not an upstream error type or the severity assigned by another service.
func managedErrorSeverity(record RequestRecord) string {
	if record.Status == "ok" {
		return ""
	}
	if record.HTTPCode < 100 || record.HTTPCode > 599 {
		return "unknown"
	}
	switch managedFailureCategory(record) {
	case "unsupported_parameter", "system_role", "invalid_oauth", "unsupported_model", "encrypted_history", "authentication":
		return "P3"
	case "rate_limit", "overloaded", "upstream_error", "server_error", "timeout":
		return "P1"
	}
	if record.HTTPCode >= 500 || record.HTTPCode == 429 {
		return "P1"
	}
	if record.HTTPCode >= 400 {
		return "P2"
	}
	return "P3"
}

func managedErrorSeveritySQL() string { return managedErrorSeverityExpression }

const managedErrorSeverityExpression = `CASE WHEN status='ok' THEN ''
 WHEN COALESCE((record->>'http_code')::integer,0) NOT BETWEEN 100 AND 599 THEN 'unknown'
 WHEN (` + managedFailureSQL + `) IN ('unsupported_parameter','system_role','invalid_oauth','unsupported_model','encrypted_history','authentication') THEN 'P3'
 WHEN (` + managedFailureSQL + `) IN ('rate_limit','overloaded','upstream_error','server_error','timeout') THEN 'P1'
 WHEN (record->>'http_code')::integer>=500 OR (record->>'http_code')::integer=429 THEN 'P1'
 WHEN (record->>'http_code')::integer>=400 THEN 'P2' ELSE 'P3' END`

// The immutable classification function preserves the original CASE contract.
// Exact scalar indexes (022/023) avoid GIN's expensive per-row JSON recheck;
// the old JSON projection remains available to previously deployed readers.
const managedErrorClassProjection = "request_log_error_classes(record,status)"
const managedErrorCategoryProjection = "(" + managedErrorClassProjection + "->>'category')"
const managedErrorSeverityProjection = "(" + managedErrorClassProjection + "->>'severity')"

func managedErrorSeverityOrder(value string) int {
	switch value {
	case "P1":
		return 0
	case "P2":
		return 1
	case "P3":
		return 2
	default:
		return 3
	}
}
