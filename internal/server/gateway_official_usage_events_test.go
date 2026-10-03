package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOfficialCompactionUsageOnlyCurrentResponse(t *testing.T) {
	for _, usage := range []string{`null`, `{"inputTokens":50,"outputTokens":3,"totalTokens":53,"cachedInputTokens":40}`} {
		t.Run(usage, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "rollout.jsonl")
			var rpc *officialRPC
			rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
				officialResponsesReply(rpc, m, map[string]any{})
				checkpoint := `{"type":"compacted","payload":{"compaction_response_id":"new","replacement_history":[{"type":"compaction","encrypted_content":"opaque"}]}}` + "\n"
				if err := os.WriteFile(path, []byte(checkpoint), 0600); err != nil {
					t.Fatal(err)
				}
				officialResponsesQueue(rpc,
					`{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"old","responseId":"old","usage":{"inputTokens":9999}}}`,
					`{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"compact","status":"inProgress"}}}`,
					`{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"old","responseId":"old","usage":{"inputTokens":9999}}}`,
					`{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"compact","responseId":"new","usage":`+usage+`}}`,
					`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"compact","status":"completed"}}}`)
			})
			rpc.home = home
			thread := &officialThread{rpc: rpc, id: "thread", model: "observed-thread-model", rollout: path, compactResponseDetails: map[string]any{"usage": "stale"}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			thread.mu.Lock()
			_, err := thread.compactResponse(ctx, nil)
			thread.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(thread.compactResponseDetails)
			if strings.Contains(string(raw), "9999") || strings.Contains(string(raw), "stale") || !strings.Contains(string(raw), `"model":"observed-thread-model"`) {
				t.Fatalf("compaction metadata crossed turn: %s", raw)
			}
			if usage == "null" && strings.Contains(string(raw), `"usage"`) || usage != "null" && !strings.Contains(string(raw), `"cached_tokens":40`) {
				t.Fatalf("compaction usage changed: %s", raw)
			}
		})
	}
}

func TestOfficialResponseUsagePreservesUnknownAndZero(t *testing.T) {
	for _, raw := range []map[string]json.RawMessage{nil, {}, {"inputTokens": json.RawMessage(`null`)}} {
		if got := officialResponseUsage(raw); got != nil {
			t.Fatalf("missing usage must remain unknown: %#v", got)
		}
	}
	encoded, err := json.Marshal(officialResponseUsage(map[string]json.RawMessage{
		"inputTokens": json.RawMessage(`0`), "outputTokens": json.RawMessage(`7`),
	}))
	if err != nil || string(encoded) != `{"input_tokens":0,"output_tokens":7}` {
		t.Fatalf("reported zero changed or absent cache invented: %s, %v", encoded, err)
	}
}

func TestOfficialResponsesRuntimeIdentityAndReasoningStream(t *testing.T) {
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "turn"}})
		officialResponsesQueue(rpc,
			`{"method":"thread/settings/updated","params":{"threadId":"thread","threadSettings":{"model":"runtime-confirmed"}}}`,
			`{"method":"model/rerouted","params":{"threadId":"other","turnId":"turn","toModel":"unrelated"}}`,
			`{"method":"model/rerouted","params":{"threadId":"thread","turnId":"stale","toModel":"stale"}}`,
			`{"method":"item/reasoning/summaryPartAdded","params":{"threadId":"thread","turnId":"turn","itemId":"rs_1","summaryIndex":0}}`,
			`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"thread","turnId":"turn","itemId":"rs_1","summaryIndex":0,"delta":"Thinking "}}`,
			`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"thread","turnId":"turn","itemId":"rs_1","summaryIndex":0,"delta":"first."}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Thinking first."}],"encrypted_content":"opaque"}}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"OK"}]}}}`,
			`{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"turn","responseId":"upstream","usage":{"inputTokens":11832,"outputTokens":7,"totalTokens":11839,"cachedInputTokens":11648,"cacheWriteInputTokens":0,"reasoningOutputTokens":0},"usageMetadata":{"amount":null,"metadata":{"total_tokens":11839}}}}`,
			`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`)
	})
	thread := &officialThread{rpc: rpc, id: "thread", model: "thread-confirmed", pending: map[string]json.RawMessage{}, custom: map[string]bool{}}
	result, output, events, err := officialResponsesRun(t, thread, []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 2 || !strings.Contains(string(result), `"cached_tokens":11648`) || !strings.Contains(string(result), `"model":"runtime-confirmed"`) || !strings.Contains(string(result), `"app_server_model_source":"app_server_settings"`) {
		t.Fatalf("runtime usage or model lost: %s", result)
	}
	started := time.Now().Add(-time.Second)
	record := RequestRecord{}
	observation := gatewayObservation{record: &record, started: started}
	var deltaIndex, rawDoneIndex = -1, -1
	for i, event := range events {
		raw, _ := json.Marshal(event)
		observation.eventAt(raw, time.Now())
		if event["type"] == "response.reasoning_summary_text.delta" && deltaIndex < 0 {
			deltaIndex = i
		}
		if event["type"] == "response.output_item.done" && rawDoneIndex < 0 {
			rawDoneIndex = i
		}
	}
	if deltaIndex < 0 || deltaIndex >= rawDoneIndex || record.FirstOutputKind != "response.reasoning_summary_text.delta" || record.TTFTMs == nil {
		t.Fatalf("reasoning did not stream before terminal item: %#v", record)
	}
}

func TestOfficialResponsesMissingUsageAndRequestModelStayUnknown(t *testing.T) {
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "turn"}})
		officialResponsesQueue(rpc,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"message","role":"assistant","id":"msg_1","content":[{"type":"output_text","text":"OK"}]}}}`,
			`{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"turn","responseId":"upstream","usage":null}}`,
			`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`)
	})
	thread := &officialThread{rpc: rpc, id: "thread", pending: map[string]json.RawMessage{}, custom: map[string]bool{}}
	result, _, _, err := officialResponsesRun(t, thread, []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), `"usage"`) || strings.Contains(string(result), `"model"`) {
		t.Fatalf("unknown usage/model must not be fabricated: %s", result)
	}
}

func TestOfficialResponsesRejectChangedReasoningSummary(t *testing.T) {
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "turn"}})
		officialResponsesQueue(rpc,
			`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"thread","turnId":"turn","itemId":"rs_1","summaryIndex":0,"delta":"original"}}`,
			`{"method":"rawResponseItem/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"changed"}]}}}`)
	})
	thread := &officialThread{rpc: rpc, id: "thread", pending: map[string]json.RawMessage{}, custom: map[string]bool{}}
	_, _, _, err := officialResponsesRun(t, thread, []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)})
	if err == nil || !strings.Contains(err.Error(), "summary delta does not match") {
		t.Fatalf("changed summary accepted: %v", err)
	}
}
