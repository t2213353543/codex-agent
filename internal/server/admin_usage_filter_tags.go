package server

import "encoding/json"

const managedLogFilterProjection = "request_log_filter_tags(record,model,key_name,status)"

// Preserve typed predicate semantics in the index payload. The original SQL
// expressions still run as rechecks, so future fields cannot be silently
// ignored just because this acceleration projection has not been extended.
func (q managedLogQuery) indexedFilterTags() []byte {
	values := map[string]any{}
	for _, field := range []struct{ key, value string }{
		{"model", q.Model}, {"key_name", q.KeyName}, {"key_id", q.KeyID}, {"endpoint", q.Endpoint}, {"ip", q.IP},
		{"upstream_model", q.UpstreamModel}, {"request_type", q.RequestType}, {"service_tier", q.ServiceTier},
		{"reasoning_effort", q.ReasoningEffort}, {"error_phase", q.ErrorPhase}, {"sent_model", q.SentModel},
		{"returned_model", q.ReturnedModel}, {"inbound_path", q.InboundPath}, {"upstream_path", q.UpstreamPath},
	} {
		if field.value != "" {
			values[field.key] = field.value
		}
	}
	if q.Status != "" && q.Status != "failed" {
		values["status"] = q.Status
	}
	if q.HTTPCode != 0 {
		values["http_code"] = q.HTTPCode
	}
	for _, field := range []struct {
		key   string
		value *bool
	}{
		{"stream", q.Stream}, {"websocket_warmup", q.WebSocketWarmup}, {"native_compaction", q.NativeCompaction}, {"error_truncated", q.ErrorTruncated},
	} {
		if field.value != nil {
			values[field.key] = *field.value
		}
	}
	if q.GroupID != "" {
		values["groups"] = []string{q.GroupID}
	}
	if len(values) == 0 {
		return nil
	}
	jsonPredicate := false
	for name := range values {
		if name != "model" && name != "key_name" && name != "status" {
			jsonPredicate = true
			break
		}
	}
	if !jsonPredicate {
		// These fields already live in indexed SQL columns. Adding the JSON
		// projection forces snapshot/count readers to decompress wide records
		// even though they only need an ID and a native column predicate.
		return nil
	}
	raw, _ := json.Marshal(values)
	return raw
}
