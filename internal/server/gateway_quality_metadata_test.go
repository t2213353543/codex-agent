package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

const qualityMetadataLimit = 32

// Test-only, bounded evidence. Reuse the production observer's precedence and
// response identity guards; never retain arbitrary headers, IDs or metadata.
type qualityMetadataCapture struct {
	responseID        string
	signals           []map[string]string
	seen              map[string]bool
	first             map[string]string
	valueChanges      map[string]bool
	sourceDifferences map[string]bool
	eventIndex        uint64
	dropped           int
	identityConflict  bool
}

func (q *qualityMetadataCapture) add(field, source, value, eventType, phase string, index uint64) {
	if value == "" {
		return
	}
	if q.first == nil {
		q.first = map[string]string{}
		q.valueChanges = map[string]bool{}
		q.sourceDifferences = map[string]bool{}
		q.seen = map[string]bool{}
	}
	digest := qualityDigest([]byte(value))
	if previous, exists := q.first[field]; exists && previous != digest {
		q.valueChanges[field] = true
	} else if !exists {
		q.first[field] = digest
	}
	key := field + ":" + source + ":" + eventType + ":" + phase + ":" + digest
	if q.seen[key] {
		return
	}
	if len(q.signals) >= qualityMetadataLimit {
		q.dropped++
		return
	}
	q.seen[key] = true
	signal := map[string]string{"field": field, "source": source, "event_type": eventType, "phase": phase,
		"first_observed_event_index": strconv.FormatUint(index, 10)}
	// Unfamiliar metadata is hashed, rather than accidentally exporting an
	// address, token or upstream free text in a model/tier/effort field.
	if field != "request_id" && qualityMetadataScalar(value) {
		signal["value"] = value
	} else {
		signal["value_sha256"] = digest
	}
	q.signals = append(q.signals, signal)
}

func qualityMetadataScalar(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (q *qualityMetadataCapture) headers(headers http.Header) {
	var record RequestRecord
	observeGatewayHeaders(&record, headers)
	q.add("model", "http_headers", record.UpstreamModel, "http_headers", "headers", 0)
	q.add("request_id", "http_headers", record.RequestID, "http_headers", "headers", 0)
}

func qualityMetadataEventPhase(kind string) (string, string) {
	// Preserve only known event names; an upstream extension name can contain
	// private text. No phase is inferred from the model's generated answer.
	switch kind {
	case "response.created":
		return kind, "created"
	case "response.in_progress":
		return kind, "in_progress"
	case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
		return kind, "terminal"
	case "response.metadata", "codex.response.metadata":
		return kind, "metadata"
	case "response.output_item.added", "response.output_item.done", "response.output_text.delta", "response.output_text.done",
		"response.content_part.added", "response.content_part.done", "response.reasoning_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		return kind, "output"
	default:
		return "unrecognized", "unknown"
	}
}

func (q *qualityMetadataCapture) event(raw []byte) {
	if !validSSELine(raw) {
		return
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return
	}
	q.eventIndex++
	eventType, phase := qualityMetadataEventPhase(observedGatewayString(envelope["type"]))
	response := gatewayResponseObject(envelope["response"])
	id := boundedGatewayMetadata(observedGatewayString(response["id"]), 1024)
	topID := boundedGatewayMetadata(observedGatewayString(envelope["response_id"]), 1024)
	if id != "" && topID != "" && id != topID || q.responseID != "" && (id != "" && id != q.responseID || topID != "" && topID != q.responseID) {
		q.identityConflict = true
		return
	}
	// The original event parser may overwrite a header model with the response
	// model. Observe a metadata-only projection first to preserve both signals.
	projection := map[string]json.RawMessage{}
	for _, key := range []string{"type", "headers", "response_id"} {
		if value := envelope[key]; len(value) > 0 {
			projection[key] = value
		}
	}
	if response != nil {
		selected := map[string]json.RawMessage{}
		for _, key := range []string{"id", "headers"} {
			if value := response[key]; len(value) > 0 {
				selected[key] = value
			}
		}
		projection["response"], _ = json.Marshal(selected)
	}
	projected, _ := json.Marshal(projection)
	headerRecord := RequestRecord{ResponseID: q.responseID}
	(gatewayObservation{record: &headerRecord}).event(projected)
	q.add("model", "sse_headers", headerRecord.UpstreamModel, eventType, phase, q.eventIndex)
	q.add("request_id", "sse_headers", headerRecord.RequestID, eventType, phase, q.eventIndex)
	record := RequestRecord{ResponseID: q.responseID}
	(gatewayObservation{record: &record}).event(raw)
	q.responseID = record.ResponseID
	if record.UpstreamModelSource == "response" {
		q.add("model", "sse_response", record.UpstreamModel, eventType, phase, q.eventIndex)
		if headerRecord.UpstreamModel != "" && headerRecord.UpstreamModel != record.UpstreamModel {
			// The same event exposes two different labels. This is still only
			// a source difference, not proof that the server contradicted itself
			// or routed to different compute: the labels may have different roles.
			q.sourceDifferences["model"] = true
		}
	}
	q.add("service_tier", "sse_response", record.ServiceTier, eventType, phase, q.eventIndex)
	q.add("reasoning_effort", "sse_response", record.ReasoningEffort, eventType, phase, q.eventIndex)
}

func (q *qualityMetadataCapture) summary() map[string]any {
	changes, differences := map[string]bool{}, map[string]bool{}
	for _, field := range []string{"model", "service_tier", "reasoning_effort", "request_id"} {
		changes[field] = q.valueChanges[field]
		differences[field] = q.sourceDifferences[field]
	}
	return map[string]any{"schema": 2, "signals": q.signals, "observed_value_changes": changes,
		"same_event_source_differences": differences,
		"response_identity_conflict":    q.identityConflict, "signals_dropped": q.dropped,
		"capture_complete": q.dropped == 0, "request_id_hash_basis": "normalized_gateway_observation",
		"interpretation": "visible_value_changes_and_source_differences_not_server_contradiction_or_internal_cluster_proof"}
}

func TestQualityMetadataCaptureSourcesAndConflicts(t *testing.T) {
	var q qualityMetadataCapture
	q.headers(http.Header{"Openai-Model": {"header-model"}, "X-Request-Id": {"private-request-id"}, "Set-Cookie": {"private-cookie"}})
	q.event([]byte(`{"type":"response.metadata","headers":{"openai-model":"ignored-top"},"response":{"id":"private-response-id","headers":{"openai-model":"sse-header-model","x-request-id":"private-request-id"},"model":"body-model","service_tier":"default","reasoning":{"effort":"medium"}},"metadata":{"secret":"private-metadata"}}`))
	if len(q.signals) != 7 || !q.valueChanges["model"] || !q.sourceDifferences["model"] || q.valueChanges["request_id"] {
		t.Fatalf("unexpected metadata sources or conflicts: %v", q.summary())
	}
	encoded, _ := json.Marshal(q.summary())
	for _, secret := range []string{"private-request-id", "private-response-id", "private-cookie", "private-metadata", "ignored-top"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("unapproved metadata persisted: %s", secret)
		}
	}
	if !strings.Contains(string(encoded), qualityDigest([]byte("private-request-id"))) {
		t.Fatal("request ID hash unavailable for local correlation")
	}
	q.event([]byte(`{"type":"response.completed","headers":{"openai-model":"foreign-header"},"response":{"id":"foreign-response","model":"foreign-model","service_tier":"priority"}}`))
	if !q.identityConflict || len(q.signals) != 7 {
		t.Fatal("foreign response changed accepted metadata")
	}
}

func TestQualityMetadataCaptureBoundedAndUnknown(t *testing.T) {
	var q qualityMetadataCapture
	q.event([]byte(`{"type":"response.metadata","headers":{"openai-model":"unknown model @ example.test","x-request-id":"private-id"}}`))
	for i := 0; i < qualityMetadataLimit+10; i++ {
		q.headers(http.Header{"Openai-Model": {strings.Repeat("x", i+1)}})
	}
	if len(q.signals) != qualityMetadataLimit || q.dropped == 0 || !q.valueChanges["model"] || q.summary()["capture_complete"] != false {
		t.Fatalf("metadata cap not enforced: %v", q.summary())
	}
	encoded, _ := json.Marshal(q.summary())
	if strings.Contains(string(encoded), "unknown model") || strings.Contains(string(encoded), "private-id") {
		t.Fatal("unknown metadata leaked")
	}
}

func TestQualityMetadataCaptureLifecycleChangeIsNotContradiction(t *testing.T) {
	var q qualityMetadataCapture
	q.headers(http.Header{"Openai-Model": {"initial-header"}})
	q.event([]byte(`{"type":"response.created","response":{"id":"same","model":"resolved-model","service_tier":"auto","reasoning":{"effort":"medium"}}}`))
	q.event([]byte(`{"type":"response.in_progress","response":{"id":"same","model":"resolved-model","service_tier":"auto"}}`))
	q.event([]byte(`{"type":"response.completed","response":{"id":"same","model":"resolved-model","service_tier":"default","reasoning":{"effort":"medium"}}}`))
	if !q.valueChanges["service_tier"] || !q.valueChanges["model"] || q.sourceDifferences["service_tier"] || q.sourceDifferences["model"] {
		t.Fatalf("lifecycle changes misreported as simultaneous differences: %v", q.summary())
	}
	if q.summary()["schema"] != 2 || q.summary()["conflicts"] != nil {
		t.Fatal("schema 1 conflict semantics must not remain")
	}
	wanted := map[string]string{"created": "auto", "in_progress": "auto", "terminal": "default"}
	for _, signal := range q.signals {
		if signal["field"] == "service_tier" {
			if wanted[signal["phase"]] != signal["value"] || signal["event_type"] == "" || signal["first_observed_event_index"] == "0" {
				t.Fatalf("lifecycle evidence lost: %v", signal)
			}
			delete(wanted, signal["phase"])
		}
	}
	if len(wanted) != 0 {
		t.Fatalf("missing lifecycle observations: %v", wanted)
	}
}

func TestQualityMetadataCaptureRejectsInvalidAndDeduplicates(t *testing.T) {
	var q qualityMetadataCapture
	q.event([]byte(`{"type":"response.metadata","response_id":"a","response":{"id":"b","model":"wrong"}}`))
	q.event([]byte(`invalid`))
	q.event([]byte(`{"type":"response.metadata","headers":{"openai-model":123}}`))
	if len(q.signals) != 0 || !q.identityConflict {
		t.Fatal("invalid metadata accepted")
	}
	for i := 0; i < 100; i++ {
		q.event([]byte(`{"type":"response.created","response":{"id":"a","model":"same","reasoning":{"effort":"medium"}}}`))
	}
	if len(q.signals) != 2 || q.dropped != 0 {
		t.Fatal("repeated metadata grew without bound")
	}
}
