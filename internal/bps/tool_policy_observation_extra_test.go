package bps

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestToolPolicyExtraNormalizeObservedEqualsOriginal(t *testing.T) {
	for _, test := range []struct {
		name, declarations, choice, item string
	}{
		{"undeclared", `[]`, `"auto"`, `{"type":"function_call","name":"private_undeclared","call_id":"c","arguments":"{}"}`},
		{"none_declared", `[{"type":"function","name":"private_known"}]`, `"none"`, `{"type":"function_call","name":"private_known","call_id":"c","arguments":"{}"}`},
		{"namespace_restore_success", `[{"type":"function","name":"private_known","namespace":"private_ns"}]`, `"auto"`, `{"type":"function_call","name":"private_known","call_id":"c","arguments":"{}"}`},
		{"namespace_restore_then_forced_error", `[{"type":"function","name":"private_forced","namespace":"a"},{"type":"function","name":"private_other","namespace":"b"}]`, `{"type":"function","name":"private_forced","namespace":"a"}`, `{"type":"function_call","name":"private_other","call_id":"c","arguments":"{}"}`},
		{"discovery_success", `[{"type":"tool_search","execution":"server"}]`, `"auto"`, `{"type":"tool_search_output","execution":"server","tools":[{"type":"function","name":"private_found"}]}`},
		{"future_call_rejected", `[]`, `"auto"`, `{"type":"private_unknown_call","secret":{"parameter":"do not retain"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := `{"model":"m","input":[],"tools":` + test.declarations + `,"tool_choice":` + test.choice + `}`
			baseline, observed := policyPrepare(t, input), policyPrepare(t, input)
			left, right := policyItem(t, test.item), policyItem(t, test.item)
			originalError := baseline.Tools.NormalizeItem(left)
			metadata := ObserveToolPolicy(observed)
			snapshot := CaptureToolPolicyRejection(observed, right)
			observedError := observed.Tools.NormalizeItem(right)
			if observedError != nil {
				metadata.RecordRejection("normalize_item", snapshot)
			}
			if (originalError == nil) != (observedError == nil) || originalError != nil && originalError.Error() != observedError.Error() {
				t.Fatal("observer changed normalizer rejection/result")
			}
			leftRaw, _ := json.Marshal(left)
			rightRaw, _ := json.Marshal(right)
			if !bytes.Equal(leftRaw, rightRaw) || !reflect.DeepEqual(baseline.Tools.Declared, observed.Tools.Declared) || !reflect.DeepEqual(baseline.Tools.definitions, observed.Tools.definitions) {
				t.Fatal("observer changed namespace/output/catalog mutation")
			}
			if (metadata.RejectionPhase != "") != (observedError != nil) {
				t.Fatal("rejection marker differs from actual error")
			}
		})
	}
}

func TestToolPolicyExtraValidateResponseObservedEqualsOriginal(t *testing.T) {
	for _, test := range []struct {
		name, policy, output string
	}{
		{"required_no_call", `"tool_choice":"required"`, `[]`},
		{"parallel_denied", `"tool_choice":"auto","parallel_tool_calls":false`, `[{"type":"function_call","name":"private_known","call_id":"a","arguments":"{}"},{"type":"function_call","name":"private_known","call_id":"b","arguments":"{}"}]`},
		{"completed_stub_missing_name", `"tool_choice":"auto"`, `[{"type":"function_call","call_id":"a","arguments":"{}","status":"completed"}]`},
		{"valid_call", `"tool_choice":"required"`, `[{"type":"function_call","name":"private_known","call_id":"a","arguments":"{}"}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := `{"model":"m","input":[],"tools":[{"type":"function","name":"private_known"}],` + test.policy + `}`
			baseline, observed := policyPrepare(t, input), policyPrepare(t, input)
			var left, right []json.RawMessage
			_ = json.Unmarshal([]byte(test.output), &left)
			_ = json.Unmarshal([]byte(test.output), &right)
			originalError := baseline.Tools.ValidateResponse(left, baseline.Policy)
			metadata := ObserveToolPolicy(observed)
			snapshot := CaptureToolPolicyRejection(observed, nil)
			observedError := observed.Tools.ValidateResponse(right, observed.Policy)
			if observedError != nil {
				metadata.RecordRejection("validate_response", snapshot)
			}
			if (originalError == nil) != (observedError == nil) || originalError != nil && originalError.Error() != observedError.Error() {
				t.Fatal("observer changed whole-response rejection/result")
			}
			if !reflect.DeepEqual(left, right) || !reflect.DeepEqual(baseline.Tools.definitions, observed.Tools.definitions) {
				t.Fatal("observer changed response bytes or catalog")
			}
			if metadata.RejectedMatchesExactDeclaration != nil || metadata.RejectedFamily != "" || metadata.RejectedIdentity != "" {
				t.Fatal("whole-response snapshot guessed an item from failure text")
			}
		})
	}
}
