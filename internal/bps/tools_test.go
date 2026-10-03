package bps

import (
	"encoding/json"
	"testing"
)

func TestUniqueNamespaceRestorationAndAmbiguity(t *testing.T) {
	r := mustPrepare(t, `{"model":"m","input":[],"tools":[{"type":"namespace","name":"left","tools":[{"type":"function","name":"echo","parameters":{}}]},{"type":"namespace","name":"right","tools":[{"type":"custom","name":"echo","format":{"type":"text"}}]}]}`)
	out, e := r.Tools.NormalizeOutput(json.RawMessage(`{"type":"function_call","name":"echo","call_id":"id","arguments":"{\"n\":9007199254740993}"}`))
	if e != nil {
		t.Fatal(e)
	}
	m, _ := object(out)
	if stringField(m, "namespace") != "left" || stringField(m, "arguments") != `{"n":9007199254740993}` {
		t.Fatal(string(out))
	}
	if _, e = r.Tools.NormalizeOutput(json.RawMessage(`{"type":"function_call","name":"echo","namespace":"right"}`)); e == nil {
		t.Fatal("type mismatch was guessed")
	}
	ambiguous := mustPrepare(t, `{"model":"m","input":[],"tools":[{"type":"namespace","name":"left","tools":[{"type":"function","name":"echo"}]},{"type":"namespace","name":"right","tools":[{"type":"function","name":"echo"}]}]}`)
	if _, e = ambiguous.Tools.NormalizeOutput(json.RawMessage(`{"type":"function_call","name":"echo"}`)); e == nil {
		t.Fatal("ambiguous name guessed")
	}
	if _, e = ambiguous.Tools.NormalizeOutput(json.RawMessage(`{"type":"function_call","name":"echo","namespace":"right"}`)); e != nil {
		t.Fatal(e)
	}
}

func TestCatalogDuplicateAndCustomBytes(t *testing.T) {
	r := mustPrepare(t, `{"model":"m","tools":[{"type":"custom","name":"patch","format":{"type":"text"}}],"input":[{"type":"additional_tools","id":"at_fixed","role":"developer","tools":[{"type":"custom","name":"patch","format":{"type":"text"}}]}]}`)
	if len(r.Tools.Declared) != 1 {
		t.Fatal("identical tools not deduplicated")
	}
	var body map[string]json.RawMessage
	json.Unmarshal(r.Body, &body)
	var items []json.RawMessage
	json.Unmarshal(body["input"], &items)
	if len(items) != 2 {
		t.Fatal("duplicate declaration inserted")
	}
	custom := `{"type":"custom_tool_call","name":"patch","input":"*** Begin Patch\n+中文 $() ` + "`" + `\n*** End Patch\n"}`
	out, e := r.Tools.NormalizeOutput(json.RawMessage(custom))
	if e != nil {
		t.Fatal(e)
	}
	if !equalJSON(out, json.RawMessage(custom)) {
		t.Fatal("freeform modified")
	}
	_, e = Prepare([]byte(`{"model":"m","tools":[{"type":"function","name":"x","description":"A"},{"type":"function","name":"x","description":"B"}]}`), options())
	if e == nil {
		t.Fatal("conflicting duplicate accepted")
	}
}

func TestResponsePolicies(t *testing.T) {
	r := mustPrepare(t, `{"model":"m","tools":[{"type":"function","name":"echo"}],"input":[],"parallel_tool_calls":false,"tool_choice":"required"}`)
	one := json.RawMessage(`{"type":"function_call","name":"echo","call_id":"a","arguments":"{}"}`)
	if e := r.Tools.ValidateResponse(nil, r.Policy); e == nil {
		t.Fatal("required tool ignored")
	}
	if e := r.Tools.ValidateResponse([]json.RawMessage{one, one}, r.Policy); e == nil {
		t.Fatal("parallel false ignored")
	}
	if e := r.Tools.ValidateResponse([]json.RawMessage{one}, r.Policy); e != nil {
		t.Fatal(e)
	}
	none := mustPrepare(t, `{"model":"m","tools":[{"type":"function","name":"echo"}],"input":[],"tool_choice":"none"}`)
	if _, e := none.Tools.NormalizeOutput(one); e == nil {
		t.Fatal("none policy ignored")
	}
	stub := map[string]json.RawMessage{"type": raw("function_call"), "id": raw("fc"), "status": raw("in_progress")}
	if e := r.Tools.NormalizeItem(stub); e != nil {
		t.Fatal(e)
	}
	if e := r.Tools.ValidateResponse([]json.RawMessage{raw(stub)}, r.Policy); e == nil {
		t.Fatal("terminal nameless stub allowed")
	}
	if e := r.Tools.ValidateResponse([]json.RawMessage{json.RawMessage(`{"type":"function_call","name":"echo","arguments":"{}"}`)}, r.Policy); e == nil {
		t.Fatal("terminal tool without call_id allowed")
	}
}
