package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
)

// Independent review of the shared-envelope optimization. Compare the legacy
// two observer calls with the new production entrypoint using detached states;
// exclude clocks/UUIDs while retaining ownership, completion, usage and cooldown.
func TestResearchWSReviewSharedEnvelopeMatchesSeparateObservation(t *testing.T) {
	for _, payload := range []string{
		`{"type":"response.output_text.delta","stream_id":"main","response_id":"current","delta":"normal <>& 你好"}`,
		`{"type":"response.output_text.delta","Type":"response.failed","stream_id":"main","STREAM_ID":"other","response_id":"current","RESPONSE_ID":"foreign","delta":"unchanged"}`,
		`{"type":"response.completed","stream_id":"main","response":{"id":"current","status":"completed","usage":{"input_tokens":7,"output_tokens":3}}}`,
		`{"type":"response.completed","TYPE":{},"stream_id":"main","STREAM_ID":{},"response":{"id":"current","ID":"foreign","status":"completed","Status":"failed","usage":{"input_tokens":7,"output_tokens":3}}}`,
		`{"type":"response.failed","stream_id":"main","response":{"id":"current","status":"failed","error":{"code":"rate_limit_exceeded","message":"synthetic"}},"headers":{"Retry-After":"60"}}`,
		`{"type":"response.failed","stream_id":"main","response":{"id":"foreign","status":"failed","error":{"code":"rate_limit_exceeded","message":"synthetic"}},"headers":{"Retry-After":"60"}}`,
		`{"type":"response.failed","stream_id":"main","response_id":"current","response":{"id":"foreign","error":{"code":"rate_limit_exceeded"}}}`,
		`{"type":"response.failed","stream_id":"main","response":{"id":"current","error":{"code":"bad_request"}},"Response":{"id":"foreign","error":{"code":"rate_limit_exceeded"}}}`,
		`{"type":"error","stream_id":"main","response_id":"current","error":{"code":"invalid_request","message":"synthetic"}}`,
		`{"type":"error","code":"server_error","message":"connection scoped"}`,
		`{"type":"response.completed","stream_id":"unknown","response":{"id":"foreign","status":"completed"}}`,
		`{"type":"response.output_item.done","stream_id":"main","response_id":"current","item":{"id":"opaque","future":{"duplicate":1,"duplicate":2},"encrypted_content":"opaque+/=<>&","number":9007199254740993}}`,
		`{"type":"response.completed","stream_id":7,"response":{"id":"current"}}`,
		`{"type":"response.completed","stream_id":"main","response":7}`,
		`{"type":"response.completed","stream_id":"main","response":{"id":7}}`,
		`{"type":`, `null`, `[]`, `true`, `"unknown"`,
	} {
		t.Run(fmt.Sprintf("payload=%s", payload), func(t *testing.T) {
			run := func(shared bool) []byte {
				p := cooldownNewTestProxy(t, 2)
				account, err := p.Store.Get("test")
				if err != nil {
					t.Fatal(err)
				}
				turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn)}
				defer turns.abort()
				var released atomic.Int32
				gatewayWSIdentityStart(t, &turns, account, "main", "model-fixture", "", &released)
				turns.observe([]byte(`{"type":"response.created","stream_id":"main","response":{"id":"current","status":"in_progress"}}`))
				raw := []byte(payload)
				before := bytes.Clone(raw)
				var stop bool
				if shared {
					stop = turns.observeFrame(t.Context(), raw, account)
				} else {
					turns.observeCooldown(t.Context(), raw, account)
					stop = turns.observe(raw)
				}
				if !bytes.Equal(raw, before) {
					t.Fatal("observer changed the source payload bytes before forwarding")
				}
				state := map[string]any{"stop": stop, "pending": turns.count, "released": released.Load(), "cooldown": p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "model-fixture"), account) != nil}
				if queue := turns.pending["main"]; len(queue) != 0 {
					turn := queue[0]
					state["active_id"], state["active_record_id"] = turn.responseID, turn.rec.ResponseID
					state["active_status"], state["active_terminal"] = turn.rec.Status, turn.rec.TerminalStatus
					state["active_error_source"], state["active_error_phase"] = turn.rec.ErrorSource, turn.rec.ErrorPhase
					state["active_ttft"], state["active_output_kind"] = turn.rec.TTFTMs != nil, turn.rec.FirstOutputKind
				}
				var records []map[string]any
				for _, record := range p.Metrics.RecentForAccount("test", 5) {
					records = append(records, map[string]any{"response": record.ResponseID, "status": record.Status, "terminal": record.TerminalStatus, "error": record.Error, "input": record.InputTokens, "output": record.OutputTokens, "model": record.Model})
				}
				state["records"] = records
				result, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			previous, shared := run(false), run(true)
			if !bytes.Equal(previous, shared) {
				t.Fatalf("shared envelope changed observation behavior: separate=%s combined=%s", previous, shared)
			}
		})
	}
}

func TestResearchWSReviewProductionPumpPreservesRawBytes(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "shared-envelope-review", func(settings *Settings) {
		settings.PromptCacheKeyEnabled = false
		settings.BuiltinPromptEnabled = false
		settings.CompatibilityNormalizationEnabled = false
	})
	defer s.client.CloseNow()
	request := `{"type":"response.create","model":"fixture","stream_id":"main","input":[]}`
	s.send(t, request)
	if string(s.forwarded(t)) != request {
		t.Fatal("review control request changed")
	}
	s.event(t, `{"type":"response.created","stream_id":"main","response":{"id":"current","status":"in_progress"}}`)
	for _, raw := range []string{
		`{"type":`, `null`, `[]`,
		`{"type":"response.output_text.delta","Type":"response.failed","stream_id":"main","response_id":"current","delta":"raw <>& 你好"}`,
		`{"type":"response.failed","stream_id":"main","response":{"id":"foreign","status":"failed","error":{"code":"rate_limit_exceeded"}},"headers":{"Retry-After":"60"}}`,
		`{"type":"response.completed","stream_id":"main","response_id":"current","response":{"id":"foreign","status":"completed"}}`,
	} {
		s.event(t, raw)
		if s.p.inFlight("test") != 1 || len(s.p.Metrics.RecentForAccount("test", 5)) != 0 {
			t.Fatal("invalid/unknown/foreign frame ended the active response")
		}
	}
	s.event(t, `{"type":"response.completed","TYPE":{},"stream_id":"main","response":{"id":"current","ID":"foreign","status":"completed","Status":"failed","usage":{"input_tokens":7,"output_tokens":3}}}`)
	if s.p.inFlight("test") != 0 {
		t.Fatal("canonical completion did not release admission")
	}
	account, err := s.p.Store.Get("test")
	if err != nil || s.p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "fixture"), account) != nil {
		t.Fatal("foreign failure installed a model cooldown")
	}
	if rec, ok := s.p.Metrics.LastFor("test"); !ok || rec.Status != "ok" || rec.ResponseID != "current" || rec.InputTokens != 7 || rec.OutputTokens != 3 {
		t.Fatal("canonical completion identity or usage changed")
	}
}
