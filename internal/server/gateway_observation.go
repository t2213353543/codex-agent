package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Observations never retain prompt, output, tool arguments, or encrypted
// content. Only the bounded metadata fields declared by RequestRecord persist.
type gatewayObservation struct {
	record  *RequestRecord
	started time.Time
}

func boundedGatewayMetadata(value string, limit int) string {
	value = strings.TrimSpace(redactMetricError(value))
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func observeGatewayRequest(rec *RequestRecord, userAgent, requestType string, payload []byte) {
	rec.ObservationSchemaVersion = 1
	rec.UserAgent = boundedGatewayMetadata(userAgent, 1024)
	rec.RequestType = requestType
	rec.WebSocketWarmup = nil
	var request struct {
		ServiceTier string          `json:"service_tier"`
		Stream      *bool           `json:"stream"`
		Type        string          `json:"type"`
		Generate    json.RawMessage `json:"generate"`
		Input       json.RawMessage `json:"input"`
		Reasoning   struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if json.Unmarshal(payload, &request) == nil {
		if requestType == "websocket" && request.Type == "response.create" {
			// The CLI uses generate=false for prewarm. Missing means ordinary
			// generation; null or invalid types are not an observed boolean.
			switch string(bytes.TrimSpace(request.Generate)) {
			case "", "true", "false":
				warmup := string(bytes.TrimSpace(request.Generate)) == "false"
				rec.WebSocketWarmup = &warmup
			}
		}
		rec.RequestedServiceTier = boundedGatewayMetadata(request.ServiceTier, 128)
		rec.RequestedReasoningEffort = boundedGatewayMetadata(request.Reasoning.Effort, 128)
		if rec.ServiceTierSource != "response" {
			rec.ServiceTier = rec.RequestedServiceTier
			if rec.ServiceTier != "" {
				rec.ServiceTierSource = "request"
			}
		}
		if rec.ReasoningEffortSource != "response" {
			rec.ReasoningEffort = rec.RequestedReasoningEffort
			if rec.ReasoningEffort != "" {
				rec.ReasoningEffortSource = "request"
			}
		}
		// Official remote compaction v2 appends ResponseItem::CompactionTrigger
		// to a streaming Responses input (compact_remote_v2_attempt.rs:76).
		// Encrypted compaction history and the legacy /compact route are not
		// evidence that this request is a native streaming compaction attempt.
		streaming := (requestType == "http_json" || requestType == "http_sse") && request.Stream != nil && *request.Stream
		// BPS observes the already validated WebSocket dispatch body after the
		// envelope type/generate fields are removed and stream=true is set.
		streaming = streaming || (requestType == "websocket" && (request.Type == "response.create" ||
			request.Type == "" && request.Stream != nil && *request.Stream))
		// An explicit non-Responses endpoint must not inherit this meaning
		// merely because another request schema contains the same item type.
		streaming = streaming && (rec.Endpoint == "" || rec.Endpoint == "responses")
		if streaming && len(bytes.TrimSpace(request.Input)) > 0 && bytes.TrimSpace(request.Input)[0] == '[' {
			var input []struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(request.Input, &input) == nil {
				compaction := false
				for _, item := range input {
					if item.Type == "compaction_trigger" {
						compaction = true
						break
					}
				}
				rec.NativeCompaction = &compaction
			}
		}
	}
}

func observeGatewayHeaders(rec *RequestRecord, headers http.Header) {
	for _, name := range []string{"X-Request-Id", "X-Oai-Request-Id", "Request-Id"} {
		if value := headers.Get(name); value != "" {
			rec.RequestID = boundedGatewayMetadata(value, 1024)
			break
		}
	}
	for _, name := range []string{"Openai-Model", "X-Openai-Model"} {
		if value := headers.Get(name); value != "" && rec.UpstreamModelSource != "response" {
			rec.UpstreamModel = boundedGatewayMetadata(value, 512)
			rec.UpstreamModelSource = "header"
			break
		}
	}
}

func gatewayResponseObject(raw []byte) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if !validSSELine(raw) || json.Unmarshal(raw, &object) != nil || object == nil {
		return nil
	}
	if nested, ok := object["response"]; ok {
		if !jsonObject(nested) {
			return nil
		}
		var response map[string]json.RawMessage
		_ = json.Unmarshal(nested, &response)
		return response
	}
	return object
}

func observedGatewayString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func observeGatewayResponseMetadata(rec *RequestRecord, object map[string]json.RawMessage) {
	if !gatewayResponseMatchesRecord(rec, object) {
		return
	}
	for _, field := range []struct {
		name   string
		target *string
		limit  int
	}{
		{"id", &rec.ResponseID, 1024},
		{"model", &rec.UpstreamModel, 512},
		{"service_tier", &rec.ServiceTier, 128},
	} {
		if value := observedGatewayString(object[field.name]); value != "" {
			*field.target = boundedGatewayMetadata(value, field.limit)
			switch field.name {
			case "id":
				rec.responseIdentityDigest = sha256.Sum256([]byte(value))
				rec.responseIdentityKnown = true
			case "model":
				rec.UpstreamModelSource = "response"
				rec.ReturnedModel = *field.target
			case "service_tier":
				rec.ServiceTierSource = "response"
			}
		}
	}
	var reasoning struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(object["reasoning"], &reasoning) == nil && reasoning.Effort != "" {
		rec.ReasoningEffort = boundedGatewayMetadata(reasoning.Effort, 128)
		rec.ReasoningEffortSource = "response"
	}
}

func gatewayResponseMatchesRecord(rec *RequestRecord, object map[string]json.RawMessage) bool {
	return gatewayResponseIDMatchesRecord(rec, observedGatewayString(object["id"]))
}

func gatewayResponseIDMatchesRecord(rec *RequestRecord, id string) bool {
	if id == "" {
		return true
	}
	if rec.responseIdentityKnown {
		return rec.responseIdentityDigest == sha256.Sum256([]byte(id))
	}
	// Historical or explicitly seeded records have only their displayed ID.
	// Fresh event observation establishes a digest before any normalization.
	return rec.ResponseID == "" || id == rec.ResponseID
}

func observeGatewayHeaderTiming(rec *RequestRecord, started, observed time.Time) {
	if started.IsZero() {
		return
	}
	elapsed := max(int64(0), observed.Sub(started).Milliseconds())
	rec.UpstreamHeaderMs = &elapsed
}

func observeGatewayError(rec *RequestRecord, source, phase string) {
	if rec.ErrorSource == "" {
		rec.ErrorSource, rec.ErrorPhase = source, phase
	}
}

func observeGatewayTerminal(rec *RequestRecord, object map[string]json.RawMessage, eventType string) {
	status := observedGatewayString(object["status"])
	// A failure signal cannot be promoted to success by a contradictory
	// response.status or by re-observing the nested object after its event.
	switch {
	case status == "failed" || eventType == "response.failed" || eventType == "error" || rec.TerminalStatus == "failed":
		status = "failed"
	case eventType == "response.incomplete" || status == "incomplete" || rec.TerminalStatus == "incomplete":
		status = "incomplete"
	case status == "cancelled" || rec.TerminalStatus == "cancelled":
		status = "cancelled"
	case eventType == "response.completed" || eventType == "response.done":
		status = "completed"
	}
	switch status {
	case "completed", "failed", "incomplete", "cancelled":
		rec.TerminalStatus = status
	default:
		return
	}
	var details struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(object["incomplete_details"], &details) == nil {
		rec.IncompleteReason = boundedGatewayMetadata(details.Reason, 128)
	}
	if status != "completed" {
		observeGatewayError(rec, "upstream", "response_event")
	}
}

func observedGatewayNumber(raw json.RawMessage) *float64 {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var number float64
	if json.Unmarshal(raw, &number) != nil || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return &number
}

// The official amount field is an opaque string with unspecified units. Keep
// only a small decimal scalar, without interpreting it as money or retaining
// arbitrary text / metadata objects supplied alongside it.
func observedGatewayAmount(raw json.RawMessage) string {
	value := observedGatewayString(raw)
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
		return ""
	}
	if !json.Valid([]byte(value)) {
		return ""
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return ""
	}
	return value
}

// A missing/null/malformed count stays unknown. A valid explicit zero is a
// measurement, rather than being inferred from the absence of the field.
func observedGatewayCount(raw json.RawMessage) *int64 {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var count int64
	if json.Unmarshal(raw, &count) != nil || count < 0 {
		return nil
	}
	return &count
}

func observeGatewayResponse(rec *RequestRecord, raw []byte) {
	object := gatewayResponseObject(raw)
	if object == nil || !gatewayResponseMatchesRecord(rec, object) {
		return
	}
	observeGatewayResponseMetadata(rec, object)
	observeGatewayTerminal(rec, object, "")
	observeGatewayImageUsage(rec, object)
	var metadata map[string]json.RawMessage
	if json.Unmarshal(object["usage_metadata"], &metadata) == nil {
		rec.UsageAmount = observedGatewayAmount(metadata["amount"])
	}
	if !jsonObject(object["usage"]) {
		return
	}
	inputTokens, _, _, known := usageFromResponse(raw)
	rec.UsageKnown = &known
	rec.UsageSchemaSource = "responses_usage"
	var usage map[string]json.RawMessage
	_ = json.Unmarshal(object["usage"], &usage)
	if _, native := usage["input_tokens"]; !native {
		if _, native = usage["output_tokens"]; !native {
			_, prompt := usage["prompt_tokens"]
			_, completion := usage["completion_tokens"]
			if prompt || completion {
				rec.UsageSchemaSource = "chat_completions_usage"
			}
		}
	}
	var input, output, compatibleInput map[string]json.RawMessage
	_ = json.Unmarshal(usage["input_tokens_details"], &input)
	_ = json.Unmarshal(usage["output_tokens_details"], &output)
	_ = json.Unmarshal(usage["prompt_tokens_details"], &compatibleInput)
	// A missing cache-read field is not a measured zero. Keep this common
	// to every Responses transport, including direct Codex HTTP and WS.
	cacheKnown := false
	for _, value := range []json.RawMessage{input["cached_tokens"], usage["cached_tokens"], compatibleInput["cached_tokens"]} {
		if len(value) == 0 {
			continue
		}
		count := observedGatewayCount(value)
		cacheKnown = known && count != nil && *count <= inputTokens
		break
	}
	rec.CachedTokensKnown = &cacheKnown
	// These are the native 0.154.0 ResponseCompleted*Details fields.
	rec.ReasoningTokens = observedGatewayCount(output["reasoning_tokens"])
	rec.CacheWriteTokens = observedGatewayCount(input["cache_write_tokens"])
	rec.UsageTotalTokens = observedGatewayCount(usage["total_tokens"])
	rec.RolloutBudgetUnits = observedGatewayNumber(usage["codex_rollout_budget_units"])
}

func (o gatewayObservation) event(payload []byte) {
	o.eventAt(payload, time.Now())
}

func (o gatewayObservation) eventAt(payload []byte, observed time.Time) {
	var event struct {
		Type       string          `json:"type"`
		ResponseID string          `json:"response_id"`
		Response   json.RawMessage `json:"response"`
		Delta      json.RawMessage `json:"delta"`
		Text       json.RawMessage `json:"text"`
		Arguments  json.RawMessage `json:"arguments"`
		Input      json.RawMessage `json:"input"`
		Item       json.RawMessage `json:"item"`
		Part       json.RawMessage `json:"part"`
		Headers    json.RawMessage `json:"headers"`
	}
	if o.record == nil || !validSSELine(payload) || json.Unmarshal(payload, &event) != nil {
		return
	}
	response := gatewayResponseObject(event.Response)
	id := observedGatewayString(response["id"])
	topID := event.ResponseID
	if id != "" && topID != "" && id != topID {
		return
	}
	if id == "" {
		id = topID
	}
	if id != "" {
		if !gatewayResponseIDMatchesRecord(o.record, id) {
			return
		}
		o.record.ResponseID = boundedGatewayMetadata(id, 1024)
		o.record.responseIdentityDigest = sha256.Sum256([]byte(id))
		o.record.responseIdentityKnown = true
	}
	// Codex response_model() prefers response.headers to top-level metadata
	// headers. Malformed optional headers cannot hide otherwise valid output.
	headers := make(http.Header)
	for _, rawHeaders := range []json.RawMessage{event.Headers, response["headers"]} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(rawHeaders, &fields)
		for key, raw := range fields {
			switch strings.ToLower(key) {
			case "x-request-id", "x-oai-request-id", "request-id", "openai-model", "x-openai-model":
				if value := observedGatewayString(raw); value != "" {
					headers.Set(key, value)
				}
			}
		}
	}
	observeGatewayHeaders(o.record, headers)
	observeGatewayResponseMetadata(o.record, response)
	switch event.Type {
	case "response.completed", "response.done", "response.failed", "response.incomplete":
		observeGatewayTerminal(o.record, response, event.Type)
	case "error":
		observeGatewayTerminal(o.record, response, event.Type)
	}
	if o.record.TTFTMs != nil || o.started.IsZero() {
		return
	}
	output := false
	switch event.Type {
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.refusal.delta":
		output = observedGatewayString(event.Delta) != ""
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done":
		output = observedGatewayString(event.Text) != ""
	case "response.function_call_arguments.done":
		output = observedGatewayString(event.Arguments) != ""
	case "response.custom_tool_call_input.done":
		output = observedGatewayString(event.Input) != ""
	case "response.output_item.added", "response.output_item.done":
		output = gatewayItemHasOutput(event.Item)
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		output = gatewayPartHasOutput(event.Part)
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		var items []json.RawMessage
		if json.Unmarshal(response["output"], &items) == nil {
			for _, item := range items {
				if gatewayItemHasOutput(item) {
					output = true
					break
				}
			}
		}
	}
	if output {
		elapsed := max(int64(0), observed.Sub(o.started).Milliseconds())
		o.record.TTFTMs = &elapsed
		o.record.FirstOutputKind = boundedGatewayMetadata(event.Type, 128)
	}
}

func gatewayPartHasOutput(raw json.RawMessage) bool {
	var part struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	if json.Unmarshal(raw, &part) != nil {
		return false
	}
	switch part.Type {
	case "output_text", "text", "summary_text", "reasoning_text":
		return part.Text != ""
	case "refusal":
		return part.Refusal != ""
	}
	return false
}

func gatewayItemHasOutput(raw json.RawMessage) bool {
	var item struct {
		Type             string            `json:"type"`
		Role             string            `json:"role"`
		Arguments        json.RawMessage   `json:"arguments"`
		Input            string            `json:"input"`
		EncryptedContent string            `json:"encrypted_content"`
		Content          []json.RawMessage `json:"content"`
		Summary          []json.RawMessage `json:"summary"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return false
	}
	switch item.Type {
	case "function_call":
		var arguments string
		return json.Unmarshal(item.Arguments, &arguments) == nil && arguments != ""
	case "tool_search_call":
		var arguments map[string]json.RawMessage
		return json.Unmarshal(item.Arguments, &arguments) == nil && len(arguments) > 0
	case "custom_tool_call":
		return item.Input != ""
	case "reasoning":
		if item.EncryptedContent != "" {
			return true
		}
		for _, part := range item.Summary {
			if gatewayPartHasOutput(part) {
				return true
			}
		}
		fallthrough
	case "message":
		if item.Type == "message" && item.Role != "" && item.Role != "assistant" {
			return false
		}
		for _, part := range item.Content {
			if gatewayPartHasOutput(part) {
				return true
			}
		}
	}
	return false
}
