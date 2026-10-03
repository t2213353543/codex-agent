package bps

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func options() Options { return Options{TaskID: "task-fixture", TurnID: "turn-fixture"} }
func mustPrepare(t *testing.T, s string) *Request {
	t.Helper()
	r, e := Prepare([]byte(s), options())
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestPreparePreservesNativeHistoryAndLargeIntegers(t *testing.T) {
	s := `{"model":"gpt-6-astra","instructions":"Keep original instructions","reasoning":{"effort":"xhigh","context":"all_turns"},"input":[{"type":"additional_tools","id":"at_fixed","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","format":{"type":"grammar","syntax":"lark","definition":"start: /[\\s\\S]+/"}}]}]},{"type":"custom_tool_call","id":"ctc_fixed","call_id":"call_fixed","name":"exec","namespace":"functions","input":"text(900719925474099312345);\n"},{"type":"custom_tool_call_output","call_id":"call_fixed","output":[{"type":"input_text","text":"exact\nresult"}]},{"type":"reasoning","encrypted_content":"gAAAA_exact_ciphertext","summary":[],"big":900719925474099312345},{"type":"future_item","unknown":{"large":184467440737095516161}}],"parallel_tool_calls":false,"include":["reasoning.encrypted_content"],"stream":true}`
	r := mustPrepare(t, s)
	var original, body map[string]json.RawMessage
	json.Unmarshal([]byte(s), &original)
	json.Unmarshal(r.Body, &body)
	var before, after []json.RawMessage
	json.Unmarshal(original["input"], &before)
	json.Unmarshal(body["input"], &after)
	if len(after) != len(before)+2 {
		t.Fatalf("input count: %d", len(after))
	}
	for i := range before {
		if !equalJSON(before[i], after[i+1]) {
			t.Fatalf("native item %d changed: %s", i, after[i+1])
		}
	}
	if !bytes.Contains(r.Body, []byte("184467440737095516161")) {
		t.Fatal("large integer was rounded")
	}
	for _, key := range []string{"tools", "instructions", "include", "parallel_tool_calls", "reasoning"} {
		if _, ok := body[key]; ok {
			t.Fatalf("unproven field forwarded: %s", key)
		}
	}
	if string(body["reasoning_effort"]) != `"xhigh"` || len(r.Policy.ClientControls["reasoning.context"]) == 0 {
		t.Fatal("reasoning mapping/control accounting lost")
	}
}

func TestCompactTriggerRetainsAllContext(t *testing.T) {
	r := mustPrepare(t, `{"model":"m","stream":false,"input":[{"type":"message","role":"user","content":"KEEP"},{"type":"compaction_trigger","scope":"all"},{"type":"compaction","encrypted_content":"cipher"},{"type":"function_call_output","call_id":"id","output":"result"}]}`)
	if !r.Compact || r.ClientStream || len(r.Policy.CompactionTriggers) != 1 {
		t.Fatal("compaction mode/control lost")
	}
	var body map[string]json.RawMessage
	json.Unmarshal(r.Body, &body)
	var items []json.RawMessage
	json.Unmarshal(body["input"], &items)
	if len(body) != 3 || len(body["model"]) == 0 || len(body["metadata"]) == 0 {
		t.Fatal("standalone compact must only emit model/input/metadata", string(r.Body))
	}
	if len(items) != 3 || !bytes.Contains(r.Body, []byte("KEEP")) || !bytes.Contains(r.Body, []byte("cipher")) || !bytes.Contains(r.Body, []byte("result")) {
		t.Fatal("compaction discarded context")
	}
	if bytes.Contains(r.Body, []byte("compaction_trigger")) {
		t.Fatal("trigger forwarded upstream")
	}
}

func TestOldToolsBecomeAdditionalTools(t *testing.T) {
	r := mustPrepare(t, `{"model":"m","input":[],"tools":[{"type":"function","name":"echo","parameters":{"type":"object","maximum":9007199254740993}}]}`)
	if bytes.Contains(r.Body, []byte(`"tools":[]`)) || !bytes.Contains(r.Body, []byte("additional_tools")) || !bytes.Contains(r.Body, []byte("9007199254740993")) {
		t.Fatal(string(r.Body))
	}
	if len(r.Tools.Declared) != 1 {
		t.Fatal("catalog not retained")
	}
}

func TestRejectUnsupportedSemanticControls(t *testing.T) {
	for _, extra := range []string{`"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}`, `"service_tier":"priority"`, `"store":true`, `"max_output_tokens":5`, `"previous_response_id":"resp_x"`, `"unknown_control":true`, `"background":true`} {
		_, e := Prepare([]byte(`{"model":"m","input":[],`+extra+`}`), options())
		if e == nil {
			t.Errorf("silently accepted %s", extra)
		}
	}
	r := mustPrepare(t, `{"model":"m","input":[],"text":{"verbosity":"low","format":{"type":"text"}},"client_metadata":{"opaque":123},"prompt_cache_key":"key"}`)
	if len(r.Policy.ClientControls) != 2 || !strings.Contains(strings.Join(r.Adaptations, ";"), "text") {
		t.Fatal("accepted controls not accounted for")
	}
	if !bytes.Contains(r.Body, []byte(`"prompt_cache_key":"key"`)) {
		t.Fatal("verified cache key field was discarded")
	}
}

func TestExplicitSearchPolicy(t *testing.T) {
	op := options()
	op.SearchDisabled = true
	r, e := Prepare([]byte(`{"model":"m","input":[],"tools":[]}`), op)
	if e != nil {
		t.Fatal(e)
	}
	if !r.SearchDisabled || !bytes.Contains(r.Body, []byte("Web search is disabled")) {
		t.Fatal("missing search constraint")
	}
	if _, e = r.Tools.NormalizeOutput(json.RawMessage(`{"type":"web_search_call","id":"ws"}`)); e == nil {
		t.Fatal("disabled search leaked")
	}
	if _, e = r.Tools.NormalizeOutput(json.RawMessage(`{"type":"function_call","name":"list_slides","status":"completed"}`)); e == nil {
		t.Fatal("undeclared office call leaked")
	}
	inherit := mustPrepare(t, `{"model":"m","input":[]}`)
	if inherit.ClientStream {
		t.Fatal("HTTP stream omission must default to non-streaming")
	}
	if _, e = inherit.Tools.NormalizeOutput(json.RawMessage(`{"type":"web_search_call","id":"ws"}`)); e != nil {
		t.Fatal("missing tools must not implicitly prohibit inherited search", e)
	}
}

func TestServiceTierUsesJSONValueSemantics(t *testing.T) {
	for _, value := range []string{`"auto"`, `"default"`, `"\u0061uto"`, `"d\u0065fault"`, `"\u0061\u0075\u0074\u006f"`, " \n\t\"auto\" \r", " \rnull\t", "null"} {
		t.Run(value, func(t *testing.T) {
			request := `{"model":"gpt-5.6-terra","input":[],"service_tier":` + value + `}`
			prepared, err := Prepare([]byte(request), options())
			if err != nil {
				t.Fatalf("valid supported JSON tier rejected: %v", err)
			}
			if prepared.Policy.ClientControls["service_tier"] == nil {
				t.Fatal("accepted tier not recorded as client control")
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(prepared.Body, &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["service_tier"]; ok {
				t.Fatal("unsupported upstream wire field emitted")
			}
		})
	}
	for _, value := range []string{`"priority"`, `"\u0070riority"`, `"flex"`, `""`, `" auto"`, `"default "`, `true`, `false`, `1`, `[]`, `{}`} {
		t.Run("reject_"+value, func(t *testing.T) {
			_, err := Prepare([]byte(`{"model":"gpt-5.6-terra","input":[],"service_tier":`+value+`}`), options())
			if err == nil {
				t.Fatal("accepted unsupported tier or non-string")
			}
		})
	}
}

func TestStoreAndTopPWhitespaceControlBoundaries(t *testing.T) {
	for _, value := range []string{"false", " \n\tfalse \r", "null", " \rnull\t"} {
		if _, err := Prepare([]byte(`{"model":"gpt-5.6-terra","input":[],"store":`+value+`}`), options()); err != nil {
			t.Fatalf("valid no-store control rejected: %v", err)
		}
	}
	for _, extra := range []string{`"store":true`, `"store": "false"`, `"store":0`, `"store":[]`, `"top_p":0.9`, `"top_p": 1.0 `, `"top_p":null`} {
		if _, err := Prepare([]byte(`{"model":"gpt-5.6-terra","input":[],`+extra+`}`), options()); err == nil {
			t.Fatalf("unsupported control accepted: %s", extra)
		}
	}
}
