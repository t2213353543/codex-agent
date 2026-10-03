package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// This checks the metadata parser contract consumed by management. Hooking
// every transport/phase remains the gateway owner's separate acceptance scope.
func TestManagedObservedUsageMetadataContract(t *testing.T) {
	request := []byte(`{"model":"requested-alias","service_tier":"flex","reasoning":{"effort":"low"},"stream":true,"input":[{"type":"compaction_trigger"},{"type":"message","content":"PRIVATE_PROMPT_FIXTURE"}]}`)
	unchanged := bytes.Clone(request)
	var record RequestRecord
	observeGatewayRequest(&record, "client-fixture", "http_sse", request)
	if !bytes.Equal(request, unchanged) || record.RequestedServiceTier != "flex" || record.ServiceTierSource != "request" || record.RequestedReasoningEffort != "low" || record.NativeCompaction == nil || !*record.NativeCompaction {
		t.Fatalf("request provenance/compaction changed: %+v", record)
	}
	observeGatewayHeaders(&record, http.Header{"Openai-Model": {"header-model"}, "X-Request-Id": {"upstream-id"}, "Authorization": {"Bearer PRIVATE_HEADER_FIXTURE"}})
	if record.UpstreamModel != "header-model" || record.UpstreamModelSource != "header" || record.RequestID != "upstream-id" {
		t.Fatal("header provenance missing")
	}
	response := []byte(`{"id":"response-id","model":"returned-model","service_tier":"priority","reasoning":{"effort":"high"},"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"codex_rollout_budget_units":0,"input_tokens_details":{"cache_write_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},"usage_metadata":{"amount":"0.125","untrusted":"PRIVATE_METADATA_FIXTURE"},"output":[{"type":"message","content":[{"type":"output_text","text":"PRIVATE_OUTPUT_FIXTURE"}]}]}`)
	observeGatewayResponse(&record, response)
	if record.UpstreamModel != "returned-model" || record.UpstreamModelSource != "response" || record.ServiceTier != "priority" || record.ServiceTierSource != "response" || record.RequestedServiceTier != "flex" || record.ReasoningEffort != "high" || record.ReasoningEffortSource != "response" || record.RequestedReasoningEffort != "low" {
		t.Fatalf("request/response source mixing: %+v", record)
	}
	if record.TerminalStatus != "incomplete" || record.IncompleteReason != "max_output_tokens" || record.UsageTotalTokens == nil || *record.UsageTotalTokens != 14 || record.UsageAmount != "0.125" || record.RolloutBudgetUnits == nil || *record.RolloutBudgetUnits != 0 || record.CacheWriteTokens == nil || *record.CacheWriteTokens != 0 || record.ReasoningTokens == nil || *record.ReasoningTokens != 0 || record.TTFTMs != nil {
		t.Fatalf("reported zero/unknown/terminal semantics: %+v", record)
	}
	// A later header must not downgrade an actual response's model provenance.
	observeGatewayHeaders(&record, http.Header{"Openai-Model": {"later-header"}})
	if record.UpstreamModel != "returned-model" || record.UpstreamModelSource != "response" {
		t.Fatal("response provenance overwritten by header")
	}
	raw, _ := json.Marshal(record)
	for _, private := range []string{"PRIVATE_PROMPT_FIXTURE", "PRIVATE_HEADER_FIXTURE", "PRIVATE_METADATA_FIXTURE", "PRIVATE_OUTPUT_FIXTURE"} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatalf("management metadata retained content %s", private)
		}
	}
	for _, payload := range []string{`{}`, `{"usage":null}`, `{"usage":"invalid"}`} {
		var empty RequestRecord
		observeGatewayResponse(&empty, []byte(payload))
		if empty.UsageKnown != nil || empty.UsageTotalTokens != nil || empty.ReasoningTokens != nil || empty.RolloutBudgetUnits != nil {
			t.Fatal("missing usage became zero")
		}
	}
	for _, payload := range []string{`{"stream":true,"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, `{"input":[{"type":"compaction_trigger"}]}`} {
		var legacy RequestRecord
		observeGatewayRequest(&legacy, "", "compact", []byte(payload))
		if legacy.NativeCompaction != nil {
			t.Fatal("legacy compact claimed native streaming compaction")
		}
	}
	var output RequestRecord
	start := time.Unix(1700000000, 0)
	observer := gatewayObservation{record: &output, started: start}
	observer.eventAt([]byte(`{"type":"response.created","response":{"id":"first"}}`), start.Add(10*time.Millisecond))
	observer.eventAt([]byte(`{"type":"response.output_text.delta","delta":""}`), start.Add(20*time.Millisecond))
	if output.TTFTMs != nil {
		t.Fatal("handshake or empty delta counted as output")
	}
	observer.eventAt([]byte(`{"type":"response.function_call_arguments.delta","delta":"PRIVATE_TOOL_ARGUMENT"}`), start.Add(30*time.Millisecond))
	observer.eventAt([]byte(`{"type":"response.failed","response":{"status":"failed"}}`), start.Add(40*time.Millisecond))
	if output.TTFTMs == nil || *output.TTFTMs != 30 || output.FirstOutputKind != "response.function_call_arguments.delta" || output.ErrorSource != "upstream" || output.ErrorPhase != "response_event" || output.TerminalStatus != "failed" {
		t.Fatalf("first output/terminal source: %+v", output)
	}
	raw, _ = json.Marshal(output)
	if strings.Contains(string(raw), "PRIVATE_TOOL_ARGUMENT") {
		t.Fatal("persisted tool argument content")
	}
}
