package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
)

const managedUsageExportSchema = "codex-usage-v4"

// Usage exports contain request observations only. Pricing catalogs are neither
// read nor evaluated, including when old data still contains pricing metadata.
func (m *Metrics) exportManagedLogsWithPrivacy(ctx context.Context, q managedLogQuery, format string, writer io.Writer, redacted bool) (int64, error) {
	var count int64
	encoder := json.NewEncoder(writer)
	var csvWriter *csv.Writer
	if format == "csv" {
		csvWriter = csv.NewWriter(writer)
		columns := []string{"id", "event_id", "time", "account_id", "account", "key_id", "key_name", "model", "endpoint", "stream", "status", "http_code", "input_tokens", "output_tokens", "cached_tokens", "latency_ms", "error_category", "error", "proxy_name", "ip", "request_id", "response_id", "upstream_model", "request_type", "service_tier", "reasoning_effort", "ttft_ms", "reasoning_tokens", "cache_write_tokens", "usage_known", "user_agent", "account_name_snapshot", "groups", "requested_service_tier", "requested_reasoning_effort", "service_tier_source", "reasoning_effort_source", "upstream_model_source", "native_compaction", "usage_total_tokens", "upstream_header_ms", "first_output_kind", "terminal_status", "incomplete_reason", "error_source", "error_phase", "sent_model", "returned_model", "inbound_path", "upstream_path", "request_preparation_ms", "account_selection_ms", "proxy_selection_ms", "attempts", "attempts_truncated", "observation_schema_version", "usage_schema_source", "error_truncated"}
		columns = append(columns, "websocket_warmup", "error_severity", "error_severity_source")
		columns = append(columns, "client_request_id", "client_request_id_source", "client_request_id_scope", "authentication_ms", "response_transfer_ms", "timing_source", "image_input_tokens", "image_output_tokens", "image_input_tokens_source", "image_output_tokens_source")
		columns = append(columns, "model_consistency")
		columns = append(columns, "official_runtime_ms", "official_inference_ms")
		columns = append(columns, "sent_reasoning_effort")
		columns = append(columns, "final_upstream_error_code", "final_upstream_error_source")
		if err := csvWriter.Write(columns); err != nil {
			return 0, err
		}
	}
	err := m.walkManagedLogsOrdered(ctx, q, func(r RequestRecord) error {
		out := managedExportRecord(r, redacted)
		r = out.RequestRecord
		if csvWriter == nil {
			if err := encoder.Encode(out); err != nil {
				return err
			}
		} else {
			row := []string{strconv.FormatInt(r.ID, 10), r.EventID, strconv.FormatInt(r.Time, 10), r.AccountID, r.Account, r.KeyID, r.KeyName, r.Model, r.Endpoint, strconv.FormatBool(r.Stream), r.Status, strconv.Itoa(r.HTTPCode), strconv.FormatInt(r.InputTokens, 10), strconv.FormatInt(r.OutputTokens, 10), strconv.FormatInt(r.CachedTokens, 10), managedNullableNumber(r.measuredLatency()), out.ErrorCategory, r.Error, r.ProxyName, r.IP, r.RequestID, r.ResponseID, r.UpstreamModel, r.RequestType, r.ServiceTier, r.ReasoningEffort, managedNullableNumber(r.TTFTMs), managedNullableNumber(r.ReasoningTokens), managedNullableNumber(r.CacheWriteTokens), managedKnownText(r), r.UserAgent}
			if known := managedUsageKnown(r); known == nil || !*known {
				row[12], row[13], row[14] = "", "", ""
			}
			if !cacheReadObserved(r) {
				row[14] = ""
			}
			groups, _ := json.Marshal(r.Groups)
			row = append(row, r.AccountNameSnapshot, string(groups), r.RequestedServiceTier, r.RequestedReasoningEffort, r.ServiceTierSource, r.ReasoningEffortSource, r.UpstreamModelSource, managedNullableBool(r.NativeCompaction), managedNullableNumber(r.UsageTotalTokens), managedNullableNumber(r.UpstreamHeaderMs), r.FirstOutputKind, r.TerminalStatus, r.IncompleteReason, r.ErrorSource, r.ErrorPhase)
			attempts, schema := "", ""
			if r.Attempts != nil {
				raw, _ := json.Marshal(r.Attempts)
				attempts = string(raw)
			}
			if r.ObservationSchemaVersion > 0 {
				schema = strconv.Itoa(r.ObservationSchemaVersion)
			}
			row = append(row, r.SentModel, r.ReturnedModel, r.InboundPath, r.UpstreamPath, managedNullableNumber(r.RequestPreparationMs), managedNullableNumber(r.AccountSelectionMs), managedNullableNumber(r.ProxySelectionMs), attempts, managedNullableBool(r.AttemptsTruncated), schema, r.UsageSchemaSource, managedNullableBool(r.ErrorTruncated))
			row = append(row, managedNullableBool(r.WebSocketWarmup), out.ErrorSeverity, out.ErrorSeveritySource)
			row = append(row, r.ClientRequestID, r.ClientRequestIDSource, r.ClientRequestIDScope, managedNullableNumber(r.AuthenticationMs), managedNullableNumber(r.ResponseTransferMs), r.TimingSource, managedNullableNumber(r.ImageInputTokens), managedNullableNumber(r.ImageOutputTokens), r.ImageInputTokensSource, r.ImageOutputTokensSource)
			row = append(row, out.ModelConsistency)
			row = append(row, managedNullableNumber(r.OfficialRuntimeMs), managedNullableNumber(r.OfficialInferenceMs))
			row = append(row, r.SentReasoningEffort)
			row = append(row, r.FinalUpstreamErrorCode, r.FinalUpstreamErrorSource)
			for i := range row {
				row[i] = safeCSV(row[i])
			}
			if err := csvWriter.Write(row); err != nil {
				return err
			}
		}
		count++
		return nil
	})
	if csvWriter != nil {
		csvWriter.Flush()
		if err == nil {
			err = csvWriter.Error()
		}
	}
	return count, err
}
