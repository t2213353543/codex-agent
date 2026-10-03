package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPrepareHistoricalNamespaceRepairPreservesOpaqueItems(t *testing.T) {
	request := `{"model":"m","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"list_agents","parameters":{"type":"object"}},{"type":"custom","name":"exec","format":{"type":"text"}}]}],"input":[{"type":"function_call","id":"fc","call_id":"c","name":"list_agents","arguments":"{\"large\":9007199254740993}","opaque":9007199254740993},{"type":"custom_tool_call","call_id":"c2","name":"exec","input":"echo exact\n"},{"type":"function_call_output","call_id":"c","output":"exact result"},{"type":"reasoning","encrypted_content":"opaque-cipher"},{"type":"function_call","call_id":"old","name":"no_longer_enabled","arguments":"{}"}]}`
	r := mustPrepare(t, request)
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal([]byte(request), &before)
	_ = json.Unmarshal(r.Body, &after)
	var original, actual []json.RawMessage
	_ = json.Unmarshal(before["input"], &original)
	_ = json.Unmarshal(after["input"], &actual)
	actual = actual[1 : len(actual)-1] // declaration and gateway constraint
	for i := range original {
		if i < 2 {
			item, _ := object(original[i])
			item["namespace"] = raw("functions")
			original[i] = raw(item)
		}
		if !equalJSON(original[i], actual[i]) {
			t.Fatalf("history item %d changed beyond a unique namespace repair: %s", i, actual[i])
		}
	}
	if !bytes.Contains(r.Body, []byte("9007199254740993")) {
		t.Fatal("large historical integer rounded")
	}
}

func TestPrepareHistoricalNamespaceAmbiguityIsRejectedAndExplicitIdentityPreserved(t *testing.T) {
	base := `{"model":"m","tools":[{"type":"function","name":"run","namespace":"one"},{"type":"function","name":"run","namespace":"two"}],"input":[%s]}`
	call := `{"type":"function_call","call_id":"c","name":"run","arguments":"{}"}`
	_, err := Prepare([]byte(strings.Replace(base, "%s", call, 1)), options())
	if err == nil || !strings.Contains(err.Error(), "2 matching namespaces") {
		t.Fatalf("ambiguous history was routed: %v", err)
	}
	call = `{"type":"function_call","call_id":"c","name":"run","namespace":"historic","arguments":"{}"}`
	r := mustPrepare(t, strings.Replace(base, "%s", call, 1))
	if !bytes.Contains(r.Body, []byte(`"namespace":"historic"`)) {
		t.Fatal("historical explicit namespace overwritten")
	}
}

func TestPrepareTypedUnsupportedParameters(t *testing.T) {
	for _, tc := range []struct{ field, param string }{
		{`"reasoning":{"effort":"max"}`, "reasoning.effort"},
		{`"reasoning_effort":"ultra"`, "reasoning.effort"},
		{`"service_tier":"priority"`, "service_tier"},
		{`"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}`, "text.format"},
		{`"max_output_tokens":100`, "max_output_tokens"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			_, err := Prepare([]byte(`{"model":"m","input":[],`+tc.field+`}`), options())
			var parameter *ParameterError
			if !errors.As(err, &parameter) || parameter.Param != tc.param || !strings.Contains(parameter.Error(), "/models") || strings.Contains(parameter.Error(), "PowerPoint") {
				t.Fatalf("not an actionable parameter error: %v", err)
			}
		})
	}
}

func TestToolSearchClientOutputRegistersDiscoveredNamespaceWithoutChangingResult(t *testing.T) {
	searchResult := `{"type":"tool_search_output","call_id":"discover","execution":"client","tools":[{"type":"namespace","name":"local_probe","tools":[{"type":"function","name":"echo_marker","parameters":{"type":"object","properties":{"marker":{"type":"string"}}}}]}]}`
	r := mustPrepare(t, `{"model":"m","tools":[{"type":"tool_search","execution":"client","description":"Discover tools"}],"input":[`+searchResult+`,{"type":"function_call","name":"echo_marker","call_id":"historic","arguments":"{}"}]}`)
	if !bytes.Contains(r.Body, []byte(searchResult)) || !bytes.Contains(r.Body, []byte(`"namespace":"local_probe"`)) {
		t.Fatal("native discovery result/history lost")
	}
	call := json.RawMessage(`{"type":"function_call","id":"fc","call_id":"call","name":"echo_marker","arguments":"{\"marker\":\"DISCOVERY_OK\"}"}`)
	got, err := r.Tools.NormalizeOutput(call)
	if err != nil || !bytes.Contains(got, []byte(`"namespace":"local_probe"`)) {
		t.Fatalf("discovered tool not routed: %s %v", got, err)
	}
	if _, err := r.Tools.NormalizeOutput(json.RawMessage(searchResult)); err == nil {
		t.Fatal("upstream fabricated client discovery result accepted")
	}
}

func TestToolSearchRequiresDeclarationAndPreservesNativeCall(t *testing.T) {
	call := json.RawMessage(`{"id":"tsc","type":"tool_search_call","status":"completed","arguments":{"query":"echo_marker"},"call_id":"discover","execution":"client"}`)
	r := mustPrepare(t, `{"model":"m","tools":[{"type":"tool_search","execution":"client","description":"Discover tools"}],"tool_choice":"required","input":[]}`)
	got, err := r.Tools.NormalizeOutput(call)
	if err != nil || !equalJSON(got, call) {
		t.Fatalf("native search call changed: %s %v", got, err)
	}
	if err := r.Tools.ValidateResponse([]json.RawMessage{got}, r.Policy); err != nil {
		t.Fatal(err)
	}
	noTools := mustPrepare(t, `{"model":"m","input":[]}`)
	if _, err := noTools.Tools.NormalizeOutput(call); err == nil {
		t.Fatal("undeclared search call accepted")
	}
	if _, err := r.Tools.NormalizeOutput(json.RawMessage(`{"type":"function_call","name":"list_slides","call_id":"forged"}`)); err == nil {
		t.Fatal("tool_search enabled an undiscovered Office tool")
	}
	op := options()
	op.SearchDisabled = true
	if _, err := Prepare([]byte(`{"model":"m","tools":[{"type":"tool_search","execution":"client","description":"Discover tools"}],"tool_choice":"required","input":[]}`), op); err != nil {
		t.Fatalf("web search policy incorrectly disabled client discovery: %v", err)
	}
}

func TestToolSearchServerDiscoveryStreamsBeforeDiscoveredCall(t *testing.T) {
	r := mustPrepare(t, `{"model":"m","tools":[{"type":"tool_search","execution":"server"}],"input":[]}`)
	search := `{"id":"tsc","type":"tool_search_call","execution":"server","status":"completed","arguments":{"query":"echo"}}`
	result := `{"id":"tso","type":"tool_search_output","execution":"server","tools":[{"type":"namespace","name":"discovered","tools":[{"type":"function","name":"echo","parameters":{"type":"object"}}]}]}`
	call := `{"id":"fc","type":"function_call","name":"echo","call_id":"c","arguments":"{}","status":"completed"}`
	wire := ""
	for _, item := range []string{search, result, call} {
		wire += streamTestFrame(`{"type":"response.output_item.done","item":` + item + `}`)
	}
	wire += streamTestFrame(`{"type":"response.completed","response":{"id":"resp","status":"completed","output":[` + search + `,` + result + `,` + call + `]}}`)
	var dst bytes.Buffer
	_, err := Stream(context.Background(), &dst, strings.NewReader(wire), StreamOptions{NormalizeItem: r.Tools.NormalizeItem, OnComplete: func(result StreamResult) error { return r.Tools.ValidateResponse(result.Output, r.Policy) }})
	if err != nil || !strings.Contains(dst.String(), `"namespace":"discovered"`) {
		t.Fatalf("server discovery stream failed: %v %s", err, dst.String())
	}
	conflict := json.RawMessage(`{"type":"tool_search_output","execution":"server","tools":[{"type":"function","name":"echo","namespace":"discovered","parameters":{"type":"string"}}]}`)
	if _, err := r.Tools.NormalizeOutput(conflict); err == nil {
		t.Fatal("conflicting discovery silently replaced a schema")
	}
}
