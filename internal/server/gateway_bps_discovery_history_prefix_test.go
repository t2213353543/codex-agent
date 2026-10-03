package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This observes two real local gateway turns, not provider cache behavior.
func TestBPSDiscoveryPrefixFromStoredServerResult(t *testing.T) {
	const defs = `[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]`
	const searchDeclaration = `{"type":"additional_tools","id":"at_client_owned_search","role":"developer","tools":[{"type":"tool_search","execution":"server"}]}`
	const searchOutput = `{"type":"tool_search_output","id":"search_result_owned","call_id":null,"status":"completed","execution":"server","tools":` + defs + `}`
	firstOutput := `[{"type":"tool_search_call","id":"search_call_owned","call_id":null,"execution":"server","status":"completed","arguments":{}},` + searchOutput + `,{"type":"message","id":"msg_first","role":"assistant","content":[{"type":"output_text","text":"synthetic discovery complete"}]}]`
	var wireBodies [][]byte
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		wireBodies = append(wireBodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		output := `[]`
		if len(wireBodies) == 1 {
			output = firstOutput
		}
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", fmt.Sprintf("discovery-prefix-%d", len(wireBodies)), output))
	})
	longText := "SYNTHETIC_LONG_PREFIX " + strings.Repeat("stable history 0123456789 ", 2048)
	first, _ := json.Marshal(map[string]any{
		"model": "gpt-6-astra", "stream": false, "instructions": "Stable synthetic instructions.",
		"tools": json.RawMessage(defs),
		"input": []any{json.RawMessage(searchDeclaration), map[string]any{"role": "user", "content": longText}},
	})
	second := []byte(`{"model":"gpt-6-astra","stream":false,"previous_response_id":"discovery-prefix-1","input":[{"role":"user","content":"synthetic follow up"}]}`)
	for i, body := range [][]byte{first, second} {
		request := proxyRequest(http.MethodPost, "/v1/responses", string(body))
		request.Header.Set("Session-Id", "synthetic-discovery-prefix")
		response := httptest.NewRecorder()
		p.gatewayBPS(response, request)
		if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(fmt.Sprintf("discovery-prefix-%d", i+1))) {
			t.Fatalf("turn %d did not complete: status=%d", i+1, response.Code)
		}
	}
	if len(wireBodies) != 2 {
		t.Fatalf("unexpected local upstream count %d", len(wireBodies))
	}
	inputs := make([]json.RawMessage, 2)
	var topDeclarations, ownedSearchDeclarations, savedSearchOutputs [2]int
	var longOffsets [2]int
	for i, body := range wireBodies {
		var fields map[string]json.RawMessage
		var items []map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["input"], &items) != nil {
			t.Fatal("bad captured wire")
		}
		inputs[i] = fields["input"]
		longOffsets[i] = bytes.Index(inputs[i], []byte("SYNTHETIC_LONG_PREFIX"))
		for _, item := range items {
			var kind, id string
			_ = json.Unmarshal(item["type"], &kind)
			_ = json.Unmarshal(item["id"], &id)
			if kind == "additional_tools" && bytes.Contains(item["tools"], []byte(`"name":"inspect"`)) {
				topDeclarations[i]++
			}
			if kind == "additional_tools" && id == "at_client_owned_search" {
				ownedSearchDeclarations[i]++
			}
			if kind == "tool_search_output" && id == "search_result_owned" {
				savedSearchOutputs[i]++
			}
		}
	}
	stored, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "discovery-prefix-1")
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if json.Unmarshal(stored.Request, &saved) != nil {
		t.Fatal("bad stored request")
	}
	if !bytes.Equal(saved["tools"], json.RawMessage(defs)) {
		t.Fatal("stored top tools changed")
	}
	if !bytes.Contains(stored.Request, []byte("at_client_owned_search")) {
		t.Fatal("caller search declaration lost")
	}
	if bytes.Contains(stored.Request, []byte("search_result_owned")) {
		t.Fatal("upstream result unexpectedly in original request")
	}
	var storedDiscovery bool
	for _, item := range stored.Output {
		if bytes.Contains(item, []byte("search_result_owned")) {
			storedDiscovery = true
		}
	}
	lcp := 0
	for lcp < len(inputs[0]) && lcp < len(inputs[1]) && inputs[0][lcp] == inputs[1][lcp] {
		lcp++
	}
	evidence := map[string]any{
		"local_gateway_turns": 2, "local_upstream_posts": len(wireBodies), "both_completed": true,
		"second_request_has_top_tools": bytes.Contains(second, []byte(`"tools"`)),
		"top_function_declarations":    topDeclarations, "caller_search_declarations": ownedSearchDeclarations,
		"server_discovery_result_items": savedSearchOutputs, "stored_top_tools_preserved": true,
		"stored_output_contains_discovery": storedDiscovery, "long_prefix_offsets": longOffsets,
		"input_lcp_bytes": lcp, "lcp_ends_before_original_long_history": lcp < longOffsets[0],
		"provider_requests": 0, "provider_cache_claim": false,
	}
	raw, _ := json.Marshal(evidence)
	t.Logf("DISCOVERY_PREFIX_EVIDENCE %s", raw)
	if topDeclarations != [2]int{1, 1} || ownedSearchDeclarations != [2]int{1, 1} || savedSearchOutputs != [2]int{0, 1} || !storedDiscovery || longOffsets[1] != longOffsets[0] || lcp < longOffsets[0]+len(longText) {
		t.Fatal("stored discovery removed the stable declaration or changed caller history")
	}
}
