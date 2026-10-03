package server

import (
	"encoding/json"
	"testing"
	"time"
)

func TestGatewayToolSearchMarksFirstActualOutput(t *testing.T) {
	record := RequestRecord{}
	observation := gatewayObservation{record: &record, started: time.Now().Add(-time.Second)}
	observation.event([]byte(`{"type":"response.output_item.added","item":{"type":"tool_search_call","arguments":{},"id":"search"}}`))
	if record.TTFTMs != nil {
		t.Fatal("empty search item counted as output")
	}
	observation.event([]byte(`{"type":"response.output_item.done","item":{"type":"tool_search_call","arguments":{"query":"lookup marker"},"id":"search"}}`))
	if record.TTFTMs == nil || *record.TTFTMs < 1000 || record.FirstOutputKind != "response.output_item.done" {
		t.Fatal("actual discovery query omitted first-output timing")
	}
	for _, raw := range []string{`{"type":"function_call","arguments":{}}`, `{"type":"tool_search_call","arguments":"not-an-object"}`, `{"type":"tool_search_call","arguments":null}`} {
		if gatewayItemHasOutput(json.RawMessage(raw)) {
			t.Fatal("invalid output shape accepted")
		}
	}
}
