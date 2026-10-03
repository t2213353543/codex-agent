package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOfficialClientToolPolicyDisablesOnlyRuntimeOwnedTools(t *testing.T) {
	config := map[string]any{"model_reasoning_effort": "xhigh", "web_search": "live"}
	officialClientToolFeaturePolicy(config)
	if config["features.goals"] != false || config["model_reasoning_effort"] != "xhigh" || config["web_search"] != "live" {
		t.Fatal("runtime ownership policy changed inference configuration")
	}
	raw := []byte(`{"models":[{"slug":"fixture","tool_mode":"code_mode_only","apply_patch_tool_type":"freeform","multi_agent_version":"v2","experimental_supported_tools":["clock","request_user_input_async","send_user_message_async","send_message_to_user_async","test_sync_tool","opaque_future_capability"],"model_messages":{"instructions_template":"exact source prompt"},"supports_search_tool":true,"default_reasoning_level":"xhigh"}]}`)
	catalog, err := officialClientToolCatalog(raw, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var target struct{ Models []map[string]json.RawMessage }
	_ = json.Unmarshal(catalog, &target)
	model := target.Models[0]
	if string(model["experimental_supported_tools"]) != `["opaque_future_capability"]` || string(model["apply_patch_tool_type"]) != "null" || string(model["multi_agent_version"]) != `"disabled"` || string(model["supports_search_tool"]) != "true" || string(model["default_reasoning_level"]) != `"xhigh"` || !strings.Contains(string(model["model_messages"]), "exact source prompt") {
		t.Fatal("runtime catalog did not isolate tool capabilities")
	}
	_, custom, err := officialTools([]json.RawMessage{json.RawMessage(`{"type":"function","name":"get_goal","parameters":{"type":"object"}}`), json.RawMessage(`{"type":"custom","name":"apply_patch"}`)})
	if err != nil || len(custom) != 2 || !custom["\x00apply_patch"] {
		t.Fatal("customer-declared builtin names must remain client tools")
	}
	thread := &officialThread{custom: custom}
	if _, _, ok := thread.originalTool("", officialToolAlias("tool", "", "get_goal")); !ok {
		t.Fatal("declared get_goal alias lost")
	}
	if _, _, ok := thread.originalTool("", "get_goal"); ok {
		t.Fatal("unaliased undeclared runtime goal still executable")
	}
}

func TestOfficialCustomInputPreservesRawAndWrappedArguments(t *testing.T) {
	thread := &officialThread{custom: map[string]bool{"\x00execute": true}}
	for _, input := range []string{"const text = '糖果';\ntext(text)", "*** Begin Patch\n*** End Patch\n", `{"code":"text(1)","limit":9007199254740993}`, ""} {
		for _, wrapped := range []bool{false, true} {
			arguments := input
			if wrapped {
				raw, _ := json.Marshal(map[string]string{"input": input})
				arguments = string(raw)
			}
			raw, _ := json.Marshal(map[string]any{"type": "function_call", "id": "fc_fixture", "call_id": "call_fixture", "name": officialToolAlias("tool", "", "execute"), "arguments": arguments, "opaque": "preserve"})
			out, err := thread.restoreItem(raw)
			if err != nil {
				t.Fatal(err)
			}
			var got struct{ Type, Input, Opaque string }
			_ = json.Unmarshal(out, &got)
			if got.Type != "custom_tool_call" || got.Input != input || got.Opaque != "preserve" {
				t.Fatal("custom input was invented, trimmed or re-encoded")
			}
			reinjected, err := thread.injectItems([]json.RawMessage{out})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := thread.restoreItem(reinjected[0])
			if err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(restored, &got)
			if got.Input != input {
				t.Fatal("custom client history changed after replay")
			}
		}
	}
	for _, arguments := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`"{\"input\":null}"`), json.RawMessage(`"{\"input\":3}"`), json.RawMessage(`"{\"input\":\"x\",\"extra\":true}"`)} {
		if _, err := officialCustomToolInput(arguments); err == nil {
			t.Fatalf("malformed or ambiguous wrapper accepted: %s", arguments)
		}
	}
}

func TestOfficialCustomUnwrappedJSONKeepsDynamicPauseResume(t *testing.T) {
	const literal = `{"code":"text(1)","opaque":9007199254740993}`
	alias := officialToolAlias("tool", "", "execute")
	var rpc *officialRPC
	rpc = officialResponsesFixture(t, func(m map[string]json.RawMessage) {
		officialResponsesReply(rpc, m, map[string]any{"turn": map[string]string{"id": "turn"}})
		request, _ := json.Marshal(map[string]any{"id": "rpc-custom", "method": "item/tool/call", "params": map[string]any{"threadId": "thread", "turnId": "turn", "callId": "call-custom", "tool": alias, "arguments": json.RawMessage(literal)}})
		item, _ := json.Marshal(map[string]any{"method": "rawResponseItem/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": map[string]any{"type": "function_call", "id": "fc_custom", "call_id": "call-custom", "name": alias, "arguments": literal}}})
		officialResponsesQueue(rpc, string(request), string(item), `{"method":"rawResponse/completed","params":{"threadId":"thread","turnId":"turn","responseId":"upstream","usage":{"inputTokens":1,"outputTokens":1}}}`)
	})
	thread := &officialThread{rpc: rpc, id: "thread", pending: map[string]json.RawMessage{}, custom: map[string]bool{"\x00execute": true}}
	_, output, _, err := officialResponsesRun(t, thread, []json.RawMessage{json.RawMessage(`{"role":"user","content":"synthetic"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var item struct{ Input string }
	_ = json.Unmarshal(output[0], &item)
	if len(output) != 1 || item.Input != literal || !thread.awaiting || len(thread.pending) != 1 {
		t.Fatal("raw JSON fallback lost exact input or client-owned dynamic tool pause")
	}
}
