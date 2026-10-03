package server

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestAppServerToolsLiteOrderedGrouping(t *testing.T) {
	adapter, err := buildAppServerTools(json.RawMessage(`[{"type":"web_search","external_web_access":false,"search_context_size":"high"},{"type":"function","name":"lookup","description":"Lookup","strict":true,"parameters":{"type":"object","properties":{"n":{"const":9007199254740993}},"additionalProperties":false}},{"type":"namespace","name":"editor","description":"Editing","tools":[{"type":"function","name":"edit","parameters":{"type":"object","properties":{}}}]},{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"namespace","name":"functions","description":"Client declared default tools","tools":[{"type":"function","name":"existing","parameters":{"type":"object","properties":{}}}]}]`), true)
	if err != nil {
		t.Fatal(err)
	}
	expected := `[{"type":"web_search","external_web_access":false,"search_context_size":"high"},{"type":"namespace","name":"functions","description":"Client declared default tools","tools":[{"type":"function","name":"lookup","description":"Lookup","strict":true,"parameters":{"type":"object","properties":{"n":{"const":9007199254740993}},"additionalProperties":false}},{"type":"custom","name":"patch","description":"","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"function","name":"existing","description":"","strict":false,"parameters":{"type":"object","properties":{}}}]},{"type":"namespace","name":"editor","description":"Editing","tools":[{"type":"function","name":"edit","description":"","strict":false,"parameters":{"type":"object","properties":{}}}]}]`
	// Exact bytes cover Rust struct field order used for the prefix UUID,
	// including client parameter field order and exact large integer tokens.
	if string(adapter.Tools) != expected {
		t.Fatalf("ordered ToolSpec mismatch\ngot: %s\nwant: %s", adapter.Tools, expected)
	}
	if len(adapter.identities) != 4 {
		t.Fatalf("unexpected implicit tool identity count %d", len(adapter.identities))
	}
}

func TestAppServerToolsCustomAndFunctionRoundTrip(t *testing.T) {
	adapter, err := buildAppServerTools(json.RawMessage(`[{"type":"function","name":"read","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark","definition":"start: /[\\s\\S]+/"}},{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"regex","definition":".*"}}]}]`), true)
	if err != nil {
		t.Fatal(err)
	}
	clientCall := json.RawMessage(`{"type":"custom_tool_call","id":"ctc_server","call_id":"call_1","name":"exec","input":"printf 'x'\n# preserve exact text"}`)
	upstream, err := adapter.PrepareInput([]json.RawMessage{clientCall})
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, upstream[0], `{"type":"custom_tool_call","id":"ctc_server","call_id":"call_1","name":"exec","namespace":"functions","input":"printf 'x'\n# preserve exact text"}`)
	roundtrip, err := adapter.MapOutputItem(upstream[0])
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, roundtrip, string(clientCall))
	output := json.RawMessage(`{"type":"custom_tool_call_output","id":"ctco_server","call_id":"call_1","output":[{"type":"input_text","text":"line1\nline2"},{"type":"input_image","image_url":"data:image/png;base64,fixture"}],"metadata":{"exact":9007199254740993}}`)
	reasoning := json.RawMessage(`{"type":"reasoning","id":"rs_server","summary":[],"encrypted_content":"opaque-ciphertext=="}`)
	retired := json.RawMessage(`{"type":"function_call","name":"retired","namespace":"old","call_id":"old_1","arguments":"{}"}`)
	history, err := adapter.PrepareInput([]json.RawMessage{roundtrip, output, reasoning, retired})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(history[1], output) || !bytes.Equal(history[2], reasoning) || !bytes.Equal(history[3], retired) {
		t.Fatal("opaque outputs/reasoning/retired tool history changed")
	}
	call := json.RawMessage(`{"type":"function_call","id":"fc_server","call_id":"call_2","name":"read","namespace":"functions","arguments":"{\"n\":9007199254740993}","encrypted_function_args":["opaque-args"]}`)
	client, err := adapter.MapOutputItem(call)
	if err != nil {
		t.Fatal(err)
	}
	back, err := adapter.PrepareInput([]json.RawMessage{client})
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, back[0], string(call))
	namespaced := json.RawMessage(`{"type":"custom_tool_call","id":"ctc_editor","call_id":"call_3","name":"exec","namespace":"editor","input":"editor source"}`)
	client, err = adapter.MapOutputItem(namespaced)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(client, namespaced) {
		t.Fatal("explicit namespace changed")
	}
}

func TestAppServerToolsLiteResponseEvents(t *testing.T) {
	adapter, err := buildAppServerTools(json.RawMessage(`[{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"web_search","external_web_access":true}]`), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"response.output_item.added", "response.output_item.done"} {
		encoded, _ := marshalGatewayRequestJSON(map[string]any{"type": kind, "sequence_number": json.Number("9007199254740993"), "item": map[string]any{"type": "custom_tool_call", "name": "apply_patch", "namespace": "functions", "input": "*** Begin Patch\n*** End Patch", "call_id": "call_patch", "id": "ctc_patch"}})
		mapped, err := adapter.MapEvent(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var event struct{ Item map[string]json.RawMessage }
		_ = json.Unmarshal(mapped, &event)
		if _, exists := event.Item["namespace"]; exists {
			t.Fatal("implicit default namespace leaked to client")
		}
		if string(event.Item["type"]) != `"custom_tool_call"` || len(event.Item["input"]) == 0 {
			t.Fatal("custom tool event was converted to function wrapper")
		}
	}
	delta := []byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_patch","delta":"*** Begin"}`)
	mapped, err := adapter.MapEvent(delta)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(delta, mapped) {
		t.Fatal("partial grammar delta changed")
	}
	search := []byte(`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"Codex docs"}}}`)
	mapped, err = adapter.MapEvent(search)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(search, mapped) {
		t.Fatal("native web search event changed")
	}
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"custom_tool_call","name":"apply_patch","namespace":"functions","id":"ctc_patch","call_id":"call_patch","input":"patch"}],"usage":{"total_tokens":9007199254740993}}}`)
	mapped, err = adapter.MapEvent(completed)
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, mapped, `{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"custom_tool_call","name":"apply_patch","id":"ctc_patch","call_id":"call_patch","input":"patch"}],"usage":{"total_tokens":9007199254740993}}}`)
}

func TestAppServerToolsDeclaredScopesAndValidation(t *testing.T) {
	for _, raw := range []string{
		`[{"type":"function","name":"same"},{"type":"namespace","name":"functions","tools":[{"type":"function","name":"same"}]}]`,
		`[{"type":"namespace","name":"editor","tools":[{"type":"namespace","name":"nested","tools":[]}]}]`,
		`[{"type":"function","name":"illegal.name"}]`,
	} {
		if _, err := buildAppServerTools(json.RawMessage(raw), true); err == nil {
			t.Fatalf("accepted invalid declaration: %s", raw)
		}
	}
	adapter, err := buildAppServerTools(json.RawMessage(`[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"kept"}]}]`), true)
	if err != nil {
		t.Fatal(err)
	}
	call := json.RawMessage(`{"type":"function_call","name":"kept","namespace":"functions","call_id":"call_1","arguments":"{}"}`)
	mapped, err := adapter.MapOutputItem(call)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mapped, call) {
		t.Fatal("explicit functions namespace was stripped")
	}
	empty, err := buildAppServerTools(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(empty.Tools) != "[]" || len(empty.identities) != 0 {
		t.Fatal("empty client catalog acquired hidden builtins")
	}
}

func TestAppServerToolsExplicitChoice(t *testing.T) {
	adapter, err := buildAppServerTools(json.RawMessage(`[{"type":"function","name":"read"},{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"web_search","search_content_types":[]}]`), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{`"auto"`, `"none"`, `"required"`} {
		got, err := adapter.PrepareToolChoice(json.RawMessage(mode))
		if err != nil || string(got) != mode {
			t.Fatalf("choice changed: %s %v", got, err)
		}
	}
	got, err := adapter.PrepareToolChoice(json.RawMessage(`{"type":"function","name":"read"}`))
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, got, `{"type":"function","name":"read"}`)
	// Actual Lite upstream rejected the adapter-added namespace as an unknown
	// tool_choice parameter. Preserve a caller's explicit namespace unchanged,
	// but never manufacture it for a top-level declaration.
	got, err = adapter.PrepareToolChoice(json.RawMessage(`{"type":"function","name":"read","namespace":"functions"}`))
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, got, `{"type":"function","name":"read","namespace":"functions"}`)
	got, err = adapter.PrepareToolChoice(json.RawMessage(`{"type":"allowed_tools","mode":"required","tools":[{"type":"custom","name":"exec"},{"type":"web_search"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, got, `{"type":"allowed_tools","mode":"required","tools":[{"type":"custom","name":"exec"},{"type":"web_search"}]}`)
	if _, err = adapter.PrepareToolChoice(json.RawMessage(`{"type":"function","name":"not_declared"}`)); err == nil {
		t.Fatal("undeclared selected tool accepted")
	}
	if !bytes.Contains(adapter.Tools, []byte(`"search_content_types":[]`)) {
		t.Fatal("explicit empty search content types were omitted")
	}
}

func TestAppServerToolsCapturedSerializers(t *testing.T) {
	// Actual native AppServer code_mode_only capture demonstrates that custom
	// grammar definitions are native ToolSpec, not function wrappers. The direct
	// client profile selects only the supplied tool, not the runtime's extras.
	raw, err := os.ReadFile("testdata/appserver_tools_native_grammar.json")
	if err != nil {
		t.Fatal(err)
	}
	var grammar struct{ Tool, Call, Result json.RawMessage }
	if err = json.Unmarshal(raw, &grammar); err != nil {
		t.Fatal(err)
	}
	adapter, err := buildAppServerTools(append(append([]byte("["), grammar.Tool...), ']'), true)
	if err != nil {
		t.Fatal(err)
	}
	var groups []struct{ Tools []json.RawMessage }
	if err = json.Unmarshal(adapter.Tools, &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Tools) != 1 {
		t.Fatal("native grammar acquired implicit runtime tools")
	}
	var compact bytes.Buffer
	if err = json.Compact(&compact, grammar.Tool); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(groups[0].Tools[0], compact.Bytes()) {
		t.Fatal("actual native grammar serializer field order or values differ")
	}
	client, err := adapter.MapOutputItem(grammar.Call)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(client, grammar.Call) {
		t.Fatal("native custom call with omitted default namespace changed for client")
	}
	history, err := adapter.PrepareInput([]json.RawMessage{client, grammar.Result})
	if err != nil {
		t.Fatal(err)
	}
	back, err := adapter.MapOutputItem(history[0])
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, back, string(grammar.Call))
	if !bytes.Equal(history[1], grammar.Result) {
		t.Fatal("actual structured custom output changed")
	}
	// Separately compare exact function serialization from the bridge's direct
	// catalog capture. Its hash alias is fixture input only, not generated by us.
	raw, err = os.ReadFile("testdata/appserver_tools_bridge_function.json")
	if err != nil {
		t.Fatal(err)
	}
	var bridge struct {
		Tools []struct {
			Namespace string
			Tool      json.RawMessage
		}
	}
	if err = json.Unmarshal(raw, &bridge); err != nil {
		t.Fatal(err)
	}
	tool := bridge.Tools[0].Tool
	adapter, err = buildAppServerTools(append(append([]byte("["), tool...), ']'), true)
	if err != nil {
		t.Fatal(err)
	}
	groups = nil
	if err = json.Unmarshal(adapter.Tools, &groups); err != nil {
		t.Fatal(err)
	}
	compact.Reset()
	if err = json.Compact(&compact, tool); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(groups[0].Tools[0], compact.Bytes()) {
		t.Fatal("actual direct function serializer differs")
	}
}
