package bps

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Exercise the same declaration-normalization and final-policy callbacks as the
// gateway, so the hosted completion exception cannot authorize native actions.
func TestStructuredHostedCompletionRespectsGatewayToolPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, item, tools, choice string
		allowed                   bool
	}{
		{"completed_web", `{"type":"web_search_call","id":"ws_ok","status":"completed","action":{"type":"search","query":"test"}}`, `[]`, `"auto"`, true},
		{"completed_server_search", `{"type":"tool_search_call","id":"ts_ok","status":"completed","execution":"server","arguments":{"query":"test"}}`, `[{"type":"tool_search","execution":"server"}]`, `"auto"`, true},
		{"web_missing_id", `{"type":"web_search_call","status":"completed","action":{"type":"search","query":"test"}}`, `[]`, `"auto"`, false},
		{"web_invalid_id", `{"type":"web_search_call","id":7,"status":"completed","action":{"type":"search","query":"test"}}`, `[]`, `"auto"`, false},
		{"web_invalid_status", `{"type":"web_search_call","id":"ws_bad","status":true,"action":{"type":"search","query":"test"}}`, `[]`, `"auto"`, false},
		{"web_client_execution", `{"type":"web_search_call","id":"ws_bad","status":"completed","execution":"client","action":{"type":"search","query":"test"}}`, `[]`, `"auto"`, false},
		{"web_disabled_policy", `{"type":"web_search_call","id":"ws_bad","status":"completed","action":{"type":"search","query":"test"}}`, `[]`, `"none"`, false},
		{"undeclared_server_search", `{"type":"tool_search_call","id":"ts_bad","status":"completed","execution":"server","arguments":{"query":"test"}}`, `[]`, `"auto"`, false},
		{"undeclared_update_plan", `{"type":"function_call","id":"fc_bad","status":"completed","name":"update_plan","call_id":"plan_bad","arguments":"{}"}`, `[]`, `"auto"`, false},
		{"undeclared_native_call", `{"type":"unknown_native_call","id":"native_bad","status":"completed","arguments":"{}"}`, `[]`, `"auto"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := Prepare([]byte(`{"model":"fixture","input":"test","text":{"format":{"type":"json_object"}},"tools":`+tc.tools+`,"tool_choice":`+tc.choice+`}`), structuredRequestOptions())
			if err != nil {
				t.Fatal(err)
			}
			format := prepared.Structured
			wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_policy", "status": "completed", "output": []json.RawMessage{json.RawMessage(tc.item), structuredStreamCall(format, "fc_private", "call_private", `{"ok":true}`)}}})
			var dst bytes.Buffer
			commits := 0
			result, err := StreamStructured(t.Context(), &dst, strings.NewReader(wire), StreamOptions{NormalizeItem: func(item map[string]json.RawMessage) error {
				if IsStructuredFormattingItem(format, item) {
					return nil
				}
				return prepared.Tools.NormalizeItem(item)
			}, OnComplete: func(result StreamResult) error {
				if err := prepared.Tools.ValidateResponse(result.Output, prepared.Policy); err != nil {
					return err
				}
				commits++
				return nil
			}}, format)
			if tc.allowed {
				if err != nil || !result.Completed || commits != 1 || !strings.Contains(dst.String(), "response.completed") {
					t.Fatalf("allowed work rejected: %v", err)
				}
			} else if err == nil || result.Completed || commits != 0 || strings.Contains(dst.String(), "response.completed") {
				t.Fatalf("invalid or undeclared work became successful: err=%v completed=%v commits=%d", err, result.Completed, commits)
			}
		})
	}
}
