package bps

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A completed native search is prior work in the same response. Its transcript
// must coexist with the one schema-validated final answer, just as it already
// does when the answer arrives as a native assistant message.
func TestStreamStructuredCompletedHostedWorkPreserved(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, kind := range []string{"web_search_call", "tool_search_call"} {
		for _, hostedCount := range []int{1, 2, 4} {
			t.Run(kind+string(rune('0'+hostedCount)), func(t *testing.T) {
				var items []json.RawMessage
				var wire strings.Builder
				usage := json.RawMessage(`{"input_tokens":9007199254740993,"output_tokens":17,"input_tokens_details":{"cached_tokens":8}}`)
				reasoning := json.RawMessage(`{"type":"reasoning","id":"rs_preserved","summary":[],"encrypted_content":"opaque-unchanged"}`)
				items = append(items, reasoning)
				for i := 0; i < hostedCount; i++ {
					id := "search_" + string(rune('0'+i))
					item := raw(map[string]any{"id": id, "type": kind, "status": "completed", "execution": "server", "action": map[string]any{"type": "search", "query": "synthetic query"}, "arguments": map[string]string{"query": "synthetic query"}})
					wire.WriteString(structuredStreamFrame(map[string]any{"type": "response.output_item.done", "output_index": len(items), "item": item}))
					items = append(items, item)
				}
				arguments := ` {"value":"OK","n":9007199254740993} `
				items = append(items, structuredStreamCall(format, "fc_internal", "call_internal", arguments))
				wire.WriteString(structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_hosted", "status": "completed", "output": items, "usage": usage}}))
				var dst bytes.Buffer
				commits := 0
				result, err := StreamStructured(t.Context(), &dst, strings.NewReader(wire.String()), StreamOptions{OnComplete: func(public StreamResult) error {
					commits++
					if dst.Len() != 0 || len(public.Output) != len(items) {
						t.Error("output escaped before commit or history was truncated")
					}
					return nil
				}}, format)
				if err != nil || !result.Completed || !result.HistoryComplete || commits != 1 {
					t.Fatalf("completed hosted search with validated final answer rejected: completed=%v history=%v commits=%d err=%v", result.Completed, result.HistoryComplete, commits, err)
				}
				if len(result.Output) != len(items) || !equalJSON(result.Usage, usage) {
					t.Fatal("transcript size or upstream usage changed")
				}
				for i := 0; i < len(items)-1; i++ {
					if !equalJSON(result.Output[i], items[i]) {
						t.Fatalf("hosted work/reasoning changed at index %d", i)
					}
				}
				message := mustStreamObject(t, result.Output[len(items)-1])
				if text, refusal := structuredMessageText(message); text != arguments || refusal {
					t.Fatal("exact final JSON not projected")
				}
				if strings.Contains(dst.String(), format.ToolName) || strings.Contains(dst.String(), "fc_internal") || strings.Contains(dst.String(), "call_internal") {
					t.Fatal("private formatter escaped")
				}
				terminals, textDeltas, hostedDone := 0, 0, 0
				for _, event := range streamTestEvents(t, dst.String()) {
					switch streamString(event, "type") {
					case "response.completed":
						terminals++
					case "response.output_text.delta":
						textDeltas++
						if streamString(event, "delta") != arguments {
							t.Fatal("projected arguments changed")
						}
					case "response.output_item.done":
						item := mustStreamObject(t, event["item"])
						if streamString(item, "type") == kind {
							hostedDone++
						}
					}
				}
				if terminals != 1 || textDeltas != 1 || hostedDone != hostedCount {
					t.Fatalf("wrong replay counts terminals=%d text=%d hosted=%d", terminals, textDeltas, hostedDone)
				}
			})
		}
	}
}

func TestStreamStructuredHostedCompletionStillRejectsPendingOrCallerWork(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, tc := range []struct{ kind, execution, status string }{
		{"web_search_call", "", "in_progress"}, {"web_search_call", "", "searching"}, {"web_search_call", "", "failed"}, {"web_search_call", "", "incomplete"}, {"web_search_call", "", ""},
		{"tool_search_call", "server", "in_progress"}, {"tool_search_call", "server", "incomplete"}, {"tool_search_call", "", "completed"}, {"tool_search_call", "client", "completed"},
		{"function_call", "", "completed"}, {"custom_tool_call", "", "completed"}, {"computer_call", "", "completed"}, {"local_shell_call", "", "completed"}, {"apply_patch_call", "", "completed"},
	} {
		t.Run(tc.kind+tc.execution+tc.status, func(t *testing.T) {
			item := raw(map[string]any{"id": "pending", "type": tc.kind, "execution": tc.execution, "status": tc.status, "call_id": "call_public", "name": "public_tool", "arguments": "{}"})
			wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_pending", "status": "completed", "output": []json.RawMessage{item, structuredStreamCall(format, "fc_internal", "call_internal", `{"value":"OK"}`)}}})
			var dst bytes.Buffer
			commits := 0
			result, err := StreamStructured(t.Context(), &dst, strings.NewReader(wire), StreamOptions{OnComplete: func(StreamResult) error { commits++; return nil }}, format)
			if err == nil || result.Completed || dst.Len() != 0 || commits != 0 || ObserveStructuredMixed(err) == nil {
				t.Fatalf("pending/client work hidden by final formatter: err=%v completed=%v commits=%d visible=%d", err, result.Completed, commits, dst.Len())
			}
		})
	}
}

func TestStreamStructuredHostedCompletionKeepsSchemaAndTerminalChecks(t *testing.T) {
	format := structuredStreamFormat(t)
	search := json.RawMessage(`{"id":"ws_done","type":"web_search_call","status":"completed","action":{"type":"search","query":"test"}}`)
	for _, tc := range []struct{ kind, status, args string }{
		{"response.completed", "completed", `{"value":7}`},
		{"response.failed", "failed", `{"value":"OK"}`},
		{"response.incomplete", "incomplete", `{"value":"OK"}`},
	} {
		t.Run(tc.kind+tc.args, func(t *testing.T) {
			wire := structuredStreamFrame(map[string]any{"type": tc.kind, "response": map[string]any{"id": "resp_check", "status": tc.status, "output": []json.RawMessage{search, structuredStreamCall(format, "fc_internal", "call_internal", tc.args)}, "error": map[string]string{"code": "server_error"}}})
			var dst bytes.Buffer
			result, err := StreamStructured(t.Context(), &dst, strings.NewReader(wire), StreamOptions{}, format)
			if err == nil || result.Completed || strings.Contains(dst.String(), "response.completed") || strings.Contains(dst.String(), format.ToolName) {
				t.Fatalf("real validation or upstream failure became success: err=%v completed=%v", err, result.Completed)
			}
			if tc.status != "completed" && !reflect.DeepEqual(mustStreamObject(t, result.Output[0]), mustStreamObject(t, search)) {
				t.Fatal("failed upstream hosted transcript lost")
			}
		})
	}
}
