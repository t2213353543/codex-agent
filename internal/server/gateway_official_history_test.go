package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// Codex can replace a model's tools between requests (for example code mode vs
// separate functions). Completed history remains valid even after its tools go
// out of scope; replaying it must not register those tools for future execution.
func TestOfficialResponsesHistoryToolsSurviveCatalogChanges(t *testing.T) {
	for _, raw := range []string{
		`{"type":"custom_tool_call","id":"old-custom","call_id":"old-custom-call","namespace":"functions","name":"exec","input":"return 17 + 26;","future":{"exact":9007199254740993}}`,
		`{"type":"function_call","id":"old-function","call_id":"old-function-call","namespace":"functions","name":"exec_command","arguments":"{\"cmd\":\"echo 43\"}"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			thread := &officialThread{custom: map[string]bool{"functions\x00new_tool": false}}
			got, err := thread.injectItems([]json.RawMessage{json.RawMessage(raw)})
			if err != nil {
				t.Fatalf("completed historical tool rejected after catalog change: %v", err)
			}
			if len(got) != 1 || !officialJSONEqual(got[0], json.RawMessage(raw)) {
				t.Fatalf("unavailable historical tool was rewritten: %s", got)
			}
			if len(thread.custom) != 1 {
				t.Fatalf("historical tool became executable: %#v", thread.custom)
			}
		})
	}
}

func TestOfficialResponsesHistoryReplayAfterModelOrToolChange(t *testing.T) {
	var rpc *officialRPC
	var injected json.RawMessage
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		var method string
		_ = json.Unmarshal(m["method"], &method)
		var params map[string]json.RawMessage
		_ = json.Unmarshal(m["params"], &params)
		switch method {
		case "thread/inject_items":
			injected = params["items"]
			officialResponsesReply(rpc, m, map[string]any{})
		case "turn/start":
			officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "resumed"}})
			officialResponsesQueue(rpc,
				`{"method":"rawResponseItem/completed","params":{"threadId":"new-thread","turnId":"resumed","item":{"type":"message","role":"assistant","id":"answer","content":[{"type":"output_text","text":"43"}]}}}`,
				`{"method":"rawResponse/completed","params":{"threadId":"new-thread","turnId":"resumed","responseId":"upstream"}}`,
				`{"method":"turn/completed","params":{"threadId":"new-thread","turn":{"id":"resumed","status":"completed"}}}`,
			)
		default:
			t.Fatalf("unexpected RPC %s", method)
		}
	})
	input := officialResponseInput{Input: json.RawMessage(`[
		{"type":"additional_tools","tools":[{"type":"function","namespace":"functions","name":"new_tool","parameters":{"type":"object"}}]},
		{"role":"user","content":"Calculate 17 + 26 using the tool."},
		{"type":"custom_tool_call","call_id":"old-exec","namespace":"functions","name":"exec","input":"return 17 + 26"},
		{"type":"custom_tool_call_output","call_id":"old-exec","output":"43"},
		{"role":"user","content":"Recall the result. Do not use tools."}
	]`)}
	history, err := officialPrepareInput(&input)
	if err != nil {
		t.Fatal(err)
	}
	_, custom, err := officialTools(input.Tools)
	if err != nil {
		t.Fatal(err)
	}
	thread := &officialThread{rpc: rpc, id: "new-thread", pending: map[string]json.RawMessage{}, custom: custom}
	_, output, _, err := officialResponsesRun(t, thread, history)
	if err != nil {
		t.Fatal(err)
	}
	if len(output) != 1 || !strings.Contains(string(output[0]), `"text":"43"`) || !strings.Contains(string(injected), `"type":"custom_tool_call"`) || !strings.Contains(string(injected), `"type":"custom_tool_call_output"`) {
		t.Fatalf("native history replay failed: output=%s injected=%s", output, injected)
	}
	if len(custom) != 1 {
		t.Fatalf("retired historical tool registered for future execution: %#v", custom)
	}
	retiredCall, _ := json.Marshal(map[string]any{"id": "bad", "method": "item/tool/call", "params": map[string]string{"threadId": thread.id, "turnId": thread.turn, "callId": "new-exec", "namespace": "functions", "tool": "exec"}})
	if err := thread.capturePending(retiredCall); err == nil {
		t.Fatal("retired tool was authorized to execute again")
	}
}

func TestOfficialResponsesHistoryCustomOutputMatchesReplayCall(t *testing.T) {
	thread := &officialThread{custom: map[string]bool{"functions\x00patch": true, "functions\x00changed": false}}
	items := []json.RawMessage{
		json.RawMessage(`{"type":"custom_tool_call","call_id":"wrapped","namespace":"functions","name":"patch","input":"exact\npatch"}`),
		json.RawMessage(`{"type":"custom_tool_call","call_id":"native","namespace":"functions","name":"exec","input":"return 43"}`),
		json.RawMessage(`{"type":"custom_tool_call","call_id":"changed","namespace":"functions","name":"changed","input":"old free form"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"wrapped","output":[{"type":"input_text","text":"patched"}]}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"native","output":"43"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"changed","output":"old output"}`),
		json.RawMessage(`{"type":"custom_tool_call_output","call_id":"compacted-away","output":"keep native"}`),
	}
	got, err := thread.injectItems(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 3} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(got[index], &fields)
		want := `"function_call"`
		if index == 3 {
			want = `"function_call_output"`
		}
		if string(fields["type"]) != want {
			t.Fatalf("bridge wrapper at %d was not restored: %s", index, got[index])
		}
	}
	for _, index := range []int{1, 2, 4, 5, 6} {
		if !officialJSONEqual(items[index], got[index]) {
			t.Fatalf("native historical item at %d changed: %s", index, got[index])
		}
	}
}

func TestOfficialResponsesHistoryItemIDsFollowReplayType(t *testing.T) {
	for _, test := range []struct {
		name                                                                                           string
		custom                                                                                         map[string]bool
		callType, outputType, callID, outputID, wantCallType, wantCallID, wantOutputType, wantOutputID string
	}{
		{"retired bridge wrapper", nil, "custom_tool_call", "custom_tool_call_output", "fc_opaque", "fco_opaque", "custom_tool_call", "ctc_opaque", "custom_tool_call_output", "ctco_opaque"},
		{"native custom to live wrapper", map[string]bool{"\x00exec": true}, "custom_tool_call", "custom_tool_call_output", "ctc_opaque", "ctco_opaque", "function_call", "fc_opaque", "function_call_output", "fco_opaque"},
		{"valid native custom", nil, "custom_tool_call", "custom_tool_call_output", "ctc_opaque", "ctco_opaque", "custom_tool_call", "ctc_opaque", "custom_tool_call_output", "ctco_opaque"},
		{"valid native function", nil, "function_call", "function_call_output", "fc_opaque", "fco_opaque", "function_call", "fc_opaque", "function_call_output", "fco_opaque"},
	} {
		t.Run(test.name, func(t *testing.T) {
			thread := &officialThread{custom: test.custom}
			call, _ := json.Marshal(map[string]any{"type": test.callType, "id": test.callID, "call_id": "unchanged-pair", "name": "exec", "input": "return 43", "arguments": "{}"})
			output, _ := json.Marshal(map[string]any{"type": test.outputType, "id": test.outputID, "call_id": "unchanged-pair", "output": "43"})
			items, err := thread.injectItems([]json.RawMessage{call, output})
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range []struct{ kind, id string }{{test.wantCallType, test.wantCallID}, {test.wantOutputType, test.wantOutputID}} {
				var item struct {
					Type, ID string
					CallID   string `json:"call_id"`
				}
				if err := json.Unmarshal(items[i], &item); err != nil || item.Type != want.kind || item.ID != want.id || item.CallID != "unchanged-pair" {
					t.Fatalf("invalid typed replay identity: %s (%v)", items[i], err)
				}
			}
		})
	}
}

func TestOfficialResponsesFailurePreservesBoundedRedactedMessage(t *testing.T) {
	message := "Invalid input[10].id: expected ctc prefix; Authorization: Bearer sk-secret-for-test " + strings.Repeat("x", 4096)
	raw, _ := json.Marshal(map[string]any{"message": message, "codexErrorInfo": "badRequest", "additionalDetails": "must-not-include-this-field"})
	err := officialResponseFailure("official App Server inference error", raw)
	if !strings.Contains(err.Error(), "expected ctc prefix") || !strings.Contains(err.Error(), "badRequest") || strings.Contains(err.Error(), "sk-secret-for-test") || strings.Contains(err.Error(), "must-not-include-this-field") || len(err.Error()) > 2300 {
		t.Fatalf("upstream error details not safely retained: %s", err)
	}
}
