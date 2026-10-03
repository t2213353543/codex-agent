package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGatewayCustomToolExplicitIndexCannotDuplicateCompletedCall(t *testing.T) {
	const original = `{"id":"custom-one","type":"custom_tool_call","call_id":"call-one","name":"apply_patch","namespace":"functions","input":"*** Begin Patch\n*** End Patch"}`
	const updated = `{"id":"custom-one","type":"custom_tool_call","call_id":"call-one","name":"apply_patch","namespace":"functions","input":"*** Begin Patch\n*** End Patch","future":{"counter":9007199254740993}}`
	for _, first := range []string{
		"data: {\"type\":\"response.output_item.done\",\"item\":" + original + "}\n\n",
		outputFixtureEvent(0, original),
	} {
		wire := first + outputFixtureEvent(2, updated) + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"custom-result\",\"status\":\"completed\",\"output\":[]}}\n\n"
		result, err := collectResponse(strings.NewReader(wire))
		if err != nil {
			// Conflicting explicit identities may fail closed. They must never
			// produce two executable tool calls for the same completed item.
			continue
		}
		var response struct {
			Output []json.RawMessage `json:"output"`
		}
		if json.Unmarshal(result, &response) != nil || len(response.Output) != 1 {
			t.Fatal("one completed custom call acquired two output slots when its explicit index changed")
		}
		var item struct {
			ID, CallID, Name, Namespace, Input string
			Future                             struct {
				Counter json.Number `json:"counter"`
			} `json:"future"`
		}
		if json.Unmarshal(response.Output[0], &item) != nil || item.ID != "custom-one" || item.Name != "apply_patch" || item.Namespace != "functions" || item.Input != "*** Begin Patch\n*** End Patch" || item.Future.Counter.String() != "9007199254740993" {
			t.Fatal("deduplicated custom item lost its final opaque fields")
		}
	}
}

func TestGatewayCustomToolParallelDoneKeepsOpaqueInputAndNamespace(t *testing.T) {
	const first = `{"id":"custom-a","type":"custom_tool_call","call_id":"call-a","name":"apply_patch","namespace":"functions","input":"*** Begin Patch\n*** Add File: a.txt\n+one\n*** End Patch","future":{"counter":9007199254740993,"empty":null}}`
	const second = `{"id":"custom-b","type":"custom_tool_call","call_id":"call-b","name":"custom_fixture","namespace":"nested.fixture","input":"opaque\u0000input","future_array":[null,{"value":123456789012345678901234567890}]}`
	wire := "data: {\"type\":\"response.custom_tool_call_input.delta\",\"item_id\":\"custom-a\",\"output_index\":1,\"delta\":\"NOT_ADDITIONAL_FINAL_INPUT\"}\n\n" +
		"data: {\"type\":\"response.custom_tool_call_input.done\",\"item_id\":\"custom-a\",\"output_index\":1,\"input\":\"NOT_AUTHORITATIVE_ITEM\"}\n\n" +
		outputFixtureEvent(7, second) + outputFixtureEvent(1, first) +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"parallel-custom\",\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":3}}}\n\n"
	result, err := collectResponse(strings.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(result, &response) != nil || len(response.Output) != 2 {
		t.Fatal("parallel custom tools lost items")
	}
	for index, expected := range []string{first, second} {
		var actualMap, expectedMap map[string]json.RawMessage
		if json.Unmarshal(response.Output[index], &actualMap) != nil || json.Unmarshal([]byte(expected), &expectedMap) != nil || len(actualMap) != len(expectedMap) {
			t.Fatal("custom item schema changed")
		}
		for key, expectedValue := range expectedMap {
			if string(actualMap[key]) != string(expectedValue) {
				t.Fatalf("opaque custom field changed at item %d key %s", index, key)
			}
		}
	}
	if strings.Contains(string(result), "NOT_ADDITIONAL_FINAL_INPUT") || strings.Contains(string(result), "NOT_AUTHORITATIVE_ITEM") {
		t.Fatal("delta/done input was double-appended to final custom item")
	}
	if input, output, _, ok := usageFromResponse(result); !ok || input != 11 || output != 3 {
		t.Fatal("custom output reconstruction changed usage")
	}
}

func TestGatewayCustomToolExplicitPromotionCannotRetainStaleOpaqueCopy(t *testing.T) {
	const reasoning = `{"id":"reasoning-one","type":"reasoning","encrypted_content":"opaque-history"}`
	const tool = `{"id":"custom-one","type":"custom_tool_call","call_id":"call-one","name":"apply_patch","input":"fixture-patch"}`
	wire := "data: {\"type\":\"response.output_item.done\",\"item\":" + tool + "}\n\n" +
		outputFixtureEvent(2, tool) + outputFixtureEvent(0, reasoning) +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"promotion\",\"status\":\"completed\"}}\n\n"
	result, err := collectResponse(strings.NewReader(wire))
	if err != nil {
		return
	}
	var response struct {
		Output []struct {
			ID string `json:"id"`
		}
	}
	if json.Unmarshal(result, &response) != nil || len(response.Output) != 2 || response.Output[0].ID != "reasoning-one" || response.Output[1].ID != "custom-one" {
		t.Fatalf("promoted tool index retained stale implicit copy: items=%d", len(response.Output))
	}
}

func TestGatewayCustomToolIndexIdentityConflictDoesNotMutate(t *testing.T) {
	const initial = `{"id":"custom-one","type":"custom_tool_call","call_id":"call-one","name":"apply_patch","input":"initial"}`
	const updated = `{"id":"custom-one","type":"custom_tool_call","call_id":"call-one","name":"apply_patch","input":"updated"}`
	for _, first := range []string{
		`{"type":"response.output_item.done","item":` + initial + `}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + initial + `}`,
	} {
		var accumulator responseOutputAccumulator
		if err := accumulator.observe([]byte(first)); err != nil {
			t.Fatal(err)
		}
		beforeBytes := accumulator.bytes
		if err := accumulator.observe([]byte(`{"type":"response.output_item.done","output_index":2,"item":` + updated + `}`)); err == nil {
			t.Fatal("same custom ID moved between indices without explicit conflict")
		}
		if len(accumulator.items) != 1 || string(accumulator.items[0]) != initial || accumulator.bytes != beforeBytes || accumulator.byID["custom-one"] != 0 {
			t.Fatal("rejected index conflict mutated earlier completed item")
		}
		if err := accumulator.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":` + updated + `}`)); err != nil {
			t.Fatal("same index identity update or promotion was rejected")
		}
		if len(accumulator.items) != 1 || string(accumulator.items[0]) != updated {
			t.Fatal("same index identity update duplicated or lost custom input")
		}
	}
}

func TestGatewayCustomToolConflictFailsJSONButKeepsStreamingBytes(t *testing.T) {
	const item = `{"id":"custom-duplicate","type":"custom_tool_call","call_id":"call-duplicate","name":"apply_patch","input":"fixture-patch"}`
	wire := `data: {"type":"response.output_item.done","item":` + item + "}\n\n" + outputFixtureEvent(2, item) + "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture\",\"status\":\"completed\",\"output\":[]}}\n\n"
	for _, stream := range []bool{false, true} {
		var requests atomic.Int32
		p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); _, _ = io.WriteString(w, wire) })
		body := `{"model":"fixture","stream":false}`
		if stream {
			body = `{"model":"fixture","stream":true}`
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, proxyRequest("POST", "/responses", body))
		if requests.Load() != 1 {
			t.Fatal("output conflict replayed request")
		}
		if stream {
			if w.Code != 200 || w.Body.String() != wire {
				t.Fatal("JSON reconstruction guard changed streaming bytes")
			}
		} else {
			if w.Code != 502 || strings.Contains(w.Body.String(), "call-duplicate") || strings.Contains(w.Body.String(), "fixture-patch") {
				t.Fatal("conflicting JSON output returned executable duplicated tool calls")
			}
		}
	}
}
