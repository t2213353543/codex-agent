package bps

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const discoveryTopTools = `[{"type":"function","name":"inspect","namespace":"client_scope","parameters":{"type":"object","properties":{"n":{"type":"integer","const":9007199254740993}},"required":["n"],"additionalProperties":false}}]`
const discoverySearchDeclaration = `{"type":"additional_tools","id":"at_client_search","role":"developer","tools":[{"type":"tool_search","execution":"client","description":"Discover client tools","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"],"additionalProperties":false}}]}`
const discoverySearchCall = `{"type":"tool_search_call","id":"tsc_fixture","call_id":"call_search_fixture","execution":"client","status":"completed","arguments":{"query":"inspect"}}`
const discoveryHistoricalCall = `{"type":"function_call","id":"fc_historic","call_id":"call_historic","name":"inspect","namespace":"client_scope","arguments":"{\"n\":9007199254740993}"}`
const discoveryHistoricalResult = `{"type":"function_call_output","call_id":"call_historic","output":"original synthetic result"}`

func discoveryHistory() []json.RawMessage {
	return []json.RawMessage{json.RawMessage(discoverySearchDeclaration), message("user", "LONG_CONTEXT_BEGIN "+strings.Repeat("stable synthetic context alpha beta 0123456789\n", 2048)), json.RawMessage(discoveryHistoricalCall), json.RawMessage(discoveryHistoricalResult)}
}
func discoveryResult(tools string) json.RawMessage {
	return json.RawMessage(`{"type":"tool_search_output","call_id":"call_search_fixture","execution":"client","status":"completed","tools":` + tools + `}`)
}
func discoveryPrepare(t *testing.T, history []json.RawMessage, choice string, compact bool) *Request {
	t.Helper()
	body := raw(map[string]any{"model": "gpt-6-astra", "instructions": "Stable client instructions.", "tools": json.RawMessage(discoveryTopTools), "input": history, "tool_choice": json.RawMessage(choice), "parallel_tool_calls": false})
	r, err := Prepare(body, Options{TaskID: "stable-task", TurnID: "same-turn", Compact: compact, StructuredOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func discoveryItems(t *testing.T, r *Request) (map[string]json.RawMessage, []json.RawMessage) {
	t.Helper()
	var body map[string]json.RawMessage
	var input []json.RawMessage
	if json.Unmarshal(r.Body, &body) != nil || json.Unmarshal(body["input"], &input) != nil {
		t.Fatal("prepared body invalid")
	}
	return body, input
}
func discoveryPrefixLength(a, b []byte) int {
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func TestDiscoveryPrefixLateSearchDoesNotRemoveCurrentDeclaration(t *testing.T) {
	history := discoveryHistory()
	first := discoveryPrepare(t, history, `"auto"`, false)
	nextHistory := append(append([]json.RawMessage(nil), history...), json.RawMessage(discoverySearchCall), discoveryResult(discoveryTopTools))
	next := discoveryPrepare(t, nextHistory, `"auto"`, false)
	firstBody, firstItems := discoveryItems(t, first)
	nextBody, nextItems := discoveryItems(t, next)
	lcp := discoveryPrefixLength(firstBody["input"], nextBody["input"])
	report := map[string]any{"provider_calls": 0, "input_lcp_bytes": lcp, "first_long_context_offset": bytes.Index(firstBody["input"], []byte("LONG_CONTEXT_BEGIN")), "next_long_context_offset": bytes.Index(nextBody["input"], []byte("LONG_CONTEXT_BEGIN")), "first_input_items": len(firstItems), "next_input_items": len(nextItems), "effective_catalog_equal": reflect.DeepEqual(first.Tools.Identities(), next.Tools.Identities()), "history_call_id_exact": bytes.Contains(next.Body, []byte(discoveryHistoricalCall)), "history_output_exact": bytes.Contains(next.Body, []byte(discoveryHistoricalResult))}
	if path := os.Getenv("DISCOVERY_PREFIX_REPORT"); path != "" {
		encoded, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("actual Prepare input LCP=%d oldItems=%d newItems=%d oldLong=%d newLong=%d", lcp, len(firstItems), len(nextItems), report["first_long_context_offset"], report["next_long_context_offset"])
	// Everything before the old per-request policy suffix must remain exact.
	for i := 0; i < len(firstItems)-1; i++ {
		if i >= len(nextItems) || !bytes.Equal(firstItems[i], nextItems[i]) {
			t.Fatalf("new tail search result deleted/reordered current declaration at input[%d]", i)
		}
	}
	if lcp < 90000 || !reflect.DeepEqual(first.Tools.Identities(), next.Tools.Identities()) {
		t.Fatal("repeated discovery changed original long prefix or effective authority")
	}
	if !bytes.Contains(next.Body, []byte(discoverySearchCall)) || !bytes.Contains(next.Body, discoveryResult(discoveryTopTools)) {
		t.Fatal("real call/result pair changed")
	}
}

func TestDiscoveryPrefixExplicitAdditionalToolsCallerIDStillDeduplicates(t *testing.T) {
	explicit := json.RawMessage(`{"type":"additional_tools","id":"at_client_owned","role":"developer","tools":` + discoveryTopTools + `}`)
	history := append([]json.RawMessage{explicit}, discoveryHistory()...)
	history = append(history, json.RawMessage(discoverySearchCall), discoveryResult(discoveryTopTools))
	prepared := discoveryPrepare(t, history, `"auto"`, false)
	_, items := discoveryItems(t, prepared)
	declarations := 0
	for _, item := range items {
		var value map[string]json.RawMessage
		_ = json.Unmarshal(item, &value)
		if stringField(value, "type") == "additional_tools" && equalJSON(value["tools"], []byte(discoveryTopTools)) {
			declarations++
			if stringField(value, "id") != "at_client_owned" || !bytes.Equal(item, explicit) {
				t.Fatal("existing caller declaration identity/bytes changed")
			}
		}
	}
	if declarations != 1 || len(prepared.Tools.Identities()) != 1 {
		t.Fatal("explicit client declaration duplicated")
	}
}

func TestDiscoveryPrefixCatalogConflictNamespaceAndRequiredRemainStrict(t *testing.T) {
	history := append(discoveryHistory(), json.RawMessage(discoverySearchCall), discoveryResult(discoveryTopTools))
	for _, choice := range []string{`"required"`, `{"type":"function","name":"inspect"}`} {
		r := discoveryPrepare(t, history, choice, false)
		if !r.Tools.ToolSearchDeclared || r.Tools.ToolSearchExecution != "client" || len(r.Tools.Identities()) != 1 {
			t.Fatal("search declaration or deduplicated client catalog changed")
		}
		if r.Tools.ValidateResponse(nil, r.Policy) == nil {
			t.Fatal("required/forced call restriction weakened")
		}
		call := map[string]json.RawMessage{"type": raw("function_call"), "id": raw("fc_new"), "name": raw("inspect"), "call_id": raw("call_new"), "arguments": raw("{}")}
		if err := r.Tools.NormalizeItem(call); err != nil || stringField(call, "namespace") != "client_scope" {
			t.Fatal("discovered unique namespace unavailable")
		}
		if err := r.Tools.ValidateResponse([]json.RawMessage{raw(call)}, r.Policy); err != nil {
			t.Fatal("declared output rejected")
		}
		call["name"] = raw("run_officejs")
		if err := r.Tools.NormalizeItem(call); err == nil {
			t.Fatal("undeclared executable gained authority")
		}
		_, items := discoveryItems(t, r)
		if !bytes.Contains(raw(items), []byte(discoveryHistoricalCall)) || !bytes.Contains(raw(items), []byte(discoveryHistoricalResult)) {
			t.Fatal("historical function/result IDs or arguments changed")
		}
	}
	conflicting := strings.Replace(discoveryTopTools, "9007199254740993", "9007199254740992", 1)
	body := raw(map[string]any{"model": "gpt-6-astra", "tools": json.RawMessage(discoveryTopTools), "input": append(discoveryHistory(), json.RawMessage(discoverySearchCall), discoveryResult(conflicting))})
	if _, err := Prepare(body, Options{TaskID: "t", TurnID: "u"}); err == nil {
		t.Fatal("discovered schema conflict silently accepted")
	}
	// Different JSON key order is equivalent, but remains caller-authored bytes.
	reordered := `[{"parameters":{"additionalProperties":false,"required":["n"],"properties":{"n":{"const":9007199254740993,"type":"integer"}},"type":"object"},"namespace":"client_scope","name":"inspect","type":"function"}]`
	r := discoveryPrepare(t, append(discoveryHistory(), json.RawMessage(discoverySearchCall), discoveryResult(reordered)), `"auto"`, false)
	if !bytes.Contains(r.Body, discoveryResult(reordered)) {
		t.Fatal("equivalent schema bytes were normalized")
	}
}

func TestDiscoveryPrefixCompactionRetainsContextAndFixedSchema(t *testing.T) {
	history := append(discoveryHistory(), json.RawMessage(discoverySearchCall), discoveryResult(discoveryTopTools))
	r := discoveryPrepare(t, history, `"required"`, true)
	body, items := discoveryItems(t, r)
	if !r.Compact || len(body) != 3 || body["input"] == nil || body["metadata"] == nil || body["model"] == nil {
		t.Fatal("compact wire schema changed")
	}
	for _, item := range history {
		found := false
		for _, sent := range items {
			found = found || bytes.Equal(item, sent)
		}
		if !found {
			t.Fatal("compaction lost caller history or discovery")
		}
	}
	if len(r.Tools.Identities()) != 1 || !r.Tools.ToolSearchDeclared {
		t.Fatal("compact effective catalog lost discovered tools")
	}
}
