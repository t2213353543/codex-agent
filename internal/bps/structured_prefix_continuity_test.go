package bps

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const prefixReviewTool = `[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"n":{"const":9007199254740993,"type":"integer"}}}}]`
const prefixReviewReorderedTool = `[{"parameters":{"properties":{"n":{"type":"integer","const":9007199254740993}},"type":"object"},"name":"inspect","type":"function"}]`

func prefixReviewRequest(t *testing.T, tool, format, choice string, history []json.RawMessage, turn string) *Request {
	t.Helper()
	fields := map[string]any{"model": "gpt-6-astra", "instructions": "Stable client instructions.", "input": history, "tools": json.RawMessage(tool), "reasoning": map[string]string{"effort": "xhigh"}, "tool_choice": json.RawMessage(choice)}
	if format != "" {
		fields["text"] = json.RawMessage(format)
	}
	body, _ := json.Marshal(fields)
	request, err := Prepare(body, Options{TaskID: "stable-task", TurnID: turn, StructuredOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	return request
}
func prefixReviewInput(t *testing.T, r *Request) (json.RawMessage, []json.RawMessage) {
	t.Helper()
	var body map[string]json.RawMessage
	var items []json.RawMessage
	if json.Unmarshal(r.Body, &body) != nil || json.Unmarshal(body["input"], &items) != nil {
		t.Fatal("invalid body")
	}
	return body["input"], items
}
func prefixReviewLCP(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
func prefixReviewLongHistory() []json.RawMessage {
	item, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": "LONG_CONTEXT_BEGIN " + strings.Repeat("stable synthetic context alpha beta 0123456789\n", 2048)})
	return []json.RawMessage{item}
}
func prefixReviewFormat(value string) string {
	return `{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object","properties":{"value":{"type":"string","enum":["` + value + `"]}},"required":["value"],"additionalProperties":false}}}`
}
func TestPrefixReviewByteComparisonEvidence(t *testing.T) {
	history := prefixReviewLongHistory()
	plain := prefixReviewRequest(t, prefixReviewTool, "", `"auto"`, history, "turn-one")
	cases := []struct {
		name        string
		left, right *Request
	}{
		{"same_input_new_turn", plain, prefixReviewRequest(t, prefixReviewTool, "", `"auto"`, history, "turn-two")},
		{"client_tool_key_order_changes", plain, prefixReviewRequest(t, prefixReviewReorderedTool, "", `"auto"`, history, "turn-one")},
		{"auto_to_forced_suffix", plain, prefixReviewRequest(t, prefixReviewTool, "", `{"type":"function","name":"inspect"}`, history, "turn-one")},
		{"structured_schema_A_to_B", prefixReviewRequest(t, prefixReviewTool, prefixReviewFormat("A"), `"auto"`, history, "turn-one"), prefixReviewRequest(t, prefixReviewTool, prefixReviewFormat("B"), `"auto"`, history, "turn-one")},
	}
	search := json.RawMessage(`{"type":"additional_tools","id":"at_client_search","role":"developer","tools":[{"type":"tool_search","execution":"server"}]}`)
	searchHistory := append([]json.RawMessage{search}, history...)
	repeated := json.RawMessage(`{"type":"tool_search_output","execution":"server","id":"search_repeat","tools":` + prefixReviewTool + `}`)
	added := json.RawMessage(`{"type":"tool_search_output","execution":"server","id":"search_new","tools":[{"type":"function","name":"discovered","namespace":"found"}]}`)
	for _, entry := range []struct {
		name string
		item json.RawMessage
	}{
		{"discovery_repeats_top_level_set", repeated}, {"discovery_adds_new_identity", added},
	} {
		cases = append(cases, struct {
			name        string
			left, right *Request
		}{entry.name,
			prefixReviewRequest(t, prefixReviewTool, "", `"auto"`, searchHistory, "turn-one"),
			prefixReviewRequest(t, prefixReviewTool, "", `"auto"`, append(append([]json.RawMessage(nil), searchHistory...), entry.item), "turn-one")})
	}
	formatOrder := `{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"additionalProperties":false,"required":["value"],"properties":{"value":{"enum":["A"],"type":"string"}},"type":"object"}}}`
	cases = append(cases, struct {
		name        string
		left, right *Request
	}{"structured_equivalent_schema_key_order",
		prefixReviewRequest(t, prefixReviewTool, prefixReviewFormat("A"), `"auto"`, history, "turn-one"),
		prefixReviewRequest(t, prefixReviewTool, formatOrder, `"auto"`, history, "turn-one")})
	var report []map[string]any
	for _, tc := range cases {
		a, ai := prefixReviewInput(t, tc.left)
		b, bi := prefixReviewInput(t, tc.right)
		lcp := prefixReviewLCP(a, b)
		leftLong, rightLong := bytes.Index(a, []byte("LONG_CONTEXT_BEGIN")), bytes.Index(b, []byte("LONG_CONTEXT_BEGIN"))
		leftMetadata := bytes.Index(tc.left.Body, []byte(`"metadata":`))
		row := map[string]any{"case": tc.name, "input_lcp_bytes": lcp, "body_lcp_bytes": prefixReviewLCP(tc.left.Body, tc.right.Body), "left_input_bytes": len(a), "right_input_bytes": len(b), "left_input_items": len(ai), "right_input_items": len(bi), "left_long_context_at": leftLong, "right_long_context_at": rightLong, "catalog_equal": reflect.DeepEqual(tc.left.Tools.Identities(), tc.right.Tools.Identities()), "input_equal": bytes.Equal(a, b), "metadata_after_input": leftMetadata > len(a)}
		ids := func(items []json.RawMessage) []string {
			var result []string
			for _, item := range items {
				var v map[string]json.RawMessage
				_ = json.Unmarshal(item, &v)
				if stringField(v, "type") == "additional_tools" {
					result = append(result, stringField(v, "id"))
				}
			}
			return result
		}
		row["declaration_ids_equal"] = reflect.DeepEqual(ids(ai), ids(bi))
		report = append(report, row)
		t.Logf("case=%s inputLCP=%d longOffsets=%d/%d catalogSame=%t", tc.name, lcp, leftLong, rightLong, row["catalog_equal"])
	}
	stable := prefixReviewRequest(t, prefixReviewTool, "", `"auto"`, history, "fixed")
	for range 50 {
		if !bytes.Equal(stable.Body, prefixReviewRequest(t, prefixReviewTool, "", `"auto"`, history, "fixed").Body) {
			t.Fatal("identical Prepare nondeterminism")
		}
	}
	if path := os.Getenv("PREFIX_REVIEW_REPORT"); path != "" {
		raw, _ := json.MarshalIndent(map[string]any{"provider_calls": 0, "identical_runs": 50, "byte_prefix_only_not_provider_cache_proof": true, "cases": report}, "", "  ")
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("report write failed")
		}
	}
}
func TestPrefixReviewFormatterFollowsUnchangedClientItems(t *testing.T) {
	history := prefixReviewLongHistory()
	baseline := prefixReviewRequest(t, prefixReviewTool, "", `"auto"`, history, "turn")
	a := prefixReviewRequest(t, prefixReviewTool, prefixReviewFormat("A"), `"auto"`, history, "turn")
	b := prefixReviewRequest(t, prefixReviewTool, prefixReviewFormat("B"), `"auto"`, history, "turn")
	_, plainItems := prefixReviewInput(t, baseline)
	araw, ai := prefixReviewInput(t, a)
	braw, bi := prefixReviewInput(t, b)
	originalCount := len(plainItems) - 1 // Last item is the gateway policy suffix.
	if len(ai) != originalCount+2 || len(bi) != originalCount+2 {
		t.Fatal("formatter declaration count changed")
	}
	for i := 0; i < originalCount; i++ {
		if !bytes.Equal(ai[i], plainItems[i]) || !bytes.Equal(bi[i], plainItems[i]) {
			t.Fatalf("gateway formatter interrupts original item prefix at %d", i)
		}
	}
	for _, pair := range []struct {
		r     *Request
		items []json.RawMessage
	}{{a, ai}, {b, bi}} {
		var added map[string]json.RawMessage
		_ = json.Unmarshal(pair.items[originalCount], &added)
		if stringField(added, "type") != "additional_tools" || !bytes.Contains(added["tools"], []byte(pair.r.Structured.ToolName)) || !bytes.Contains(pair.items[len(pair.items)-1], []byte(pair.r.Structured.ToolName)) {
			t.Fatal("formatter definition/constraint identity lost")
		}
		if !reflect.DeepEqual(pair.r.Tools.Identities(), baseline.Tools.Identities()) {
			t.Fatal("internal formatter changed client tool authority")
		}
	}
	if prefixReviewLCP(araw, braw) < 90000 {
		t.Fatal("unchanged long client prefix not retained across schema changes")
	}
}
