package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOfficialClientSearchReplayUsesFunctionIdentity(t *testing.T) {
	_, custom, err := officialTools([]json.RawMessage{json.RawMessage(`{"type":"tool_search","execution":"client"}`)})
	if err != nil {
		t.Fatal(err)
	}
	thread := &officialThread{custom: custom, searchExecution: "client"}
	items, err := thread.injectItems([]json.RawMessage{
		json.RawMessage(`{"type":"tool_search_call","id":"tsc_opaque-original","call_id":"call_original","execution":"client","status":"completed","arguments":{"query":"marker"}}`),
		json.RawMessage(`{"type":"tool_search_output","call_id":"call_original","execution":"client","tools":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(items[0]), `"id":"fc_opaque-original"`) || !strings.Contains(string(items[0]), `"call_id":"call_original"`) || !strings.Contains(string(items[1]), `"type":"function_call_output"`) {
		t.Fatalf("search replay identity changed: %s", items)
	}
}

func TestOfficialImplicitNativeDiscoveryRemainsServerOwned(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"type":"function","name":"lookup","defer_loading":true,"parameters":{"type":"object","properties":{}}}`)}
	_, custom, err := officialTools(tools)
	if err != nil {
		t.Fatal(err)
	}
	thread := &officialThread{custom: custom, definitions: officialOriginalToolDefinitions(tools)}
	call := json.RawMessage(`{"type":"tool_search_call","id":"tsc_original","call_id":"search_original","execution":"client","arguments":{"query":"lookup"}}`)
	restored, err := thread.restoreItem(call)
	if err != nil || !strings.Contains(string(restored), `"execution":"server"`) {
		t.Fatalf("implicit official discovery rejected: %s %v", restored, err)
	}
	output, _ := json.Marshal(map[string]any{"type": "tool_search_output", "call_id": "search_original", "execution": "client", "tools": []any{map[string]any{"type": "function", "name": officialToolAlias("tool", "", "lookup"), "parameters": map[string]any{"type": "object"}}}})
	restored, err = thread.restoreItem(output)
	if err != nil || !strings.Contains(string(restored), `"name":"lookup"`) || !strings.Contains(string(restored), `"execution":"server"`) {
		t.Fatalf("implicit discovery definition lost: %s %v", restored, err)
	}
	thread.searchExecution = "client"
	if _, err := thread.restoreItem(call); err == nil {
		t.Fatal("runtime search silently replaced explicit client discovery")
	}
}

func TestOfficialDiscoveryReplayActivatesOnlySameSchema(t *testing.T) {
	for _, foundFlag := range []string{"", `,"defer_loading":false`} {
		input := officialResponseInput{Tools: []json.RawMessage{json.RawMessage(`{"type":"namespace","name":"db","tools":[{"type":"function","name":"lookup","defer_loading":true,"parameters":{"type":"object","properties":{}}}]}`)}, Input: json.RawMessage(`[{"type":"tool_search_output","execution":"server","call_id":"searched","tools":[{"type":"function","namespace":"db","name":"lookup","parameters":{"type":"object","properties":{}}` + foundFlag + `}]}]`)}
		if _, err := officialPrepareInput(&input); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(input.Tools)
		if strings.Contains(string(encoded), "defer_loading") {
			t.Fatalf("discovered tool remained deferred: %s", encoded)
		}
		// A subsequent replay of the same search output remains stable.
		if _, err := officialPrepareInput(&input); err != nil {
			t.Fatal(err)
		}
	}
	input := officialResponseInput{Tools: []json.RawMessage{json.RawMessage(`{"type":"function","name":"lookup","defer_loading":true,"parameters":{"type":"object","properties":{}}}`)}, Input: json.RawMessage(`[{"type":"tool_search_output","execution":"client","call_id":"searched","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"different":{"type":"string"}}}}]}]`)}
	if _, err := officialPrepareInput(&input); err == nil {
		t.Fatal("discovery replaced an existing callable schema")
	}
}
