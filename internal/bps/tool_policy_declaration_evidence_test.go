package bps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestToolPolicyDeclarationEvidenceBoundaries(t *testing.T) {
	const privateName = "PRIVATE_NAME_<secret>"
	for _, tc := range []struct {
		name, tools     string
		other, declared bool
	}{
		{"absent", `[]`, false, false},
		{"other_family", `[{"type":"custom","name":"` + privateName + `","format":{"type":"text"}}]`, true, false},
		{"declared_ns", `[{"type":"namespace","name":"PRIVATE_NAMESPACE","tools":[{"type":"function","name":"` + privateName + `"}]}]`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","input":"PRIVATE_BODY","tools":` + tc.tools + `,"tool_choice":"none"}`)
			r := policyPrepare(t, string(body))
			item := policyItem(t, `{"type":"function_call","name":"`+privateName+`","arguments":"PRIVATE_ARGUMENTS"}`)
			value := CaptureToolPolicyItem(item).Resolve(r).WithRequestDeclarationEvidence(body, r.Body, strings.Repeat("A", 64), strings.Repeat("b", 64))
			if err := r.Tools.NormalizeItem(item); err == nil {
				t.Fatal("negative fixture accepted")
			}
			o := ObserveToolPolicy(r)
			o.RecordRejection("normalize_item", value)
			sum := sha256.Sum256([]byte(privateName))
			if o.RejectedNameSHA256 != hex.EncodeToString(sum[:]) || o.RejectedNameOtherFamily == nil || *o.RejectedNameOtherFamily != tc.other || o.InboundFamilyNameDeclared == nil || *o.InboundFamilyNameDeclared != tc.declared || o.SerializedInitialFamilyNameDeclared == nil || *o.SerializedInitialFamilyNameDeclared != tc.declared || o.RejectedKnownBuiltin != "not_builtin" {
				t.Fatalf("evidence wrong: %+v", o)
			}
			if o.ResolvedToolsSHA256 != strings.Repeat("a", 64) || o.ResolvedPromptSHA256 != strings.Repeat("b", 64) {
				t.Fatal("valid header hash lost")
			}
			encoded, _ := json.Marshal(o)
			if bytes.Contains(encoded, []byte("PRIVATE")) {
				t.Fatalf("raw private fields leaked: %s", encoded)
			}
			o.RecordRejection("normalize_item", CaptureToolPolicyRejection(r, nil))
			after, _ := json.Marshal(o)
			if !bytes.Equal(encoded, after) {
				t.Fatal("first rejection overwritten")
			}
		})
	}
}
func TestToolPolicyDeclarationEvidenceDynamicDiscoveryIsInitial(t *testing.T) {
	body := []byte(`{"model":"m","input":[],"tools":[{"type":"tool_search","execution":"server"}]}`)
	r := policyPrepare(t, string(body))
	discovery := policyItem(t, `{"type":"tool_search_output","execution":"server","tools":[{"type":"function","name":"dynamic","namespace":"left"}]}`)
	if err := r.Tools.NormalizeItem(discovery); err != nil {
		t.Fatal(err)
	}
	item := policyItem(t, `{"type":"function_call","name":"dynamic","namespace":"left","call_id":"call"}`)
	if err := r.Tools.NormalizeItem(item); err != nil {
		t.Fatal("valid discovery broke", err)
	}
	// Simulate a later parallel-policy rejection of this genuinely discovered call.
	o := ObserveToolPolicy(r)
	o.RecordRejection("parallel_policy", CaptureToolPolicyRejection(r, item).WithRequestDeclarationEvidence(body, r.Body, "", ""))
	if o.RejectedMatchesExactDeclaration == nil || !*o.RejectedMatchesExactDeclaration || o.InboundFamilyNameDeclared == nil || *o.InboundFamilyNameDeclared || o.SerializedInitialFamilyNameDeclared == nil || *o.SerializedInitialFamilyNameDeclared {
		t.Fatalf("initial/effective discovery conflated: %+v", o)
	}
	// A client-returned discovery already present in outbound history is initial.
	history := []byte(`{"input":[{"type":"tool_search_output","execution":"client","call_id":"c","tools":[{"type":"function","name":"dynamic"}]}]}`)
	value := CaptureToolPolicyRejection(r, item).WithRequestDeclarationEvidence(history, history, "", "")
	if value.inboundFamilyNameDeclared == nil || !*value.inboundFamilyNameDeclared || value.serializedInitialFamilyNameDeclared == nil || !*value.serializedInitialFamilyNameDeclared {
		t.Fatal("historical discovery missing")
	}
}
func TestToolPolicyDeclarationEvidenceOmissionAndWriterDifference(t *testing.T) {
	inbound := []byte(`{"model":"m","tools":[{"type":"function","name":"update_plan"}],"input":[]}`)
	r := policyPrepare(t, string(inbound))
	item := policyItem(t, `{"type":"function_call","name":"update_plan"}`)
	value := CaptureToolPolicyRejection(r, item).WithRequestDeclarationEvidence(inbound, []byte(`{"input":[]}`), "bad", strings.Repeat("g", 64))
	o := ObserveToolPolicy(r)
	o.RecordRejection("normalize_item", value)
	if o.RejectedKnownBuiltin != "update_plan" || o.InboundFamilyNameDeclared == nil || !*o.InboundFamilyNameDeclared || o.SerializedInitialFamilyNameDeclared == nil || *o.SerializedInitialFamilyNameDeclared || o.ResolvedToolsSHA256 != "" || o.ResolvedPromptSHA256 != "" {
		t.Fatalf("writer distinction lost: %+v", o)
	}
	unknown := ObserveToolPolicy(r)
	unknown.RecordRejection("normalize_item", CaptureToolPolicyRejection(r, item).WithRequestDeclarationEvidence([]byte(`not-json`), []byte(`{"tools":[{"type":"namespace","name":"bad","functions":[]}],"input":[]}`), "", ""))
	if unknown.InboundFamilyNameDeclared != nil || unknown.SerializedInitialFamilyNameDeclared != nil {
		t.Fatal("invalid evidence invented false")
	}
	whole := ObserveToolPolicy(r)
	whole.RecordRejection("validate_response", CaptureToolPolicyRejection(r, nil).WithRequestDeclarationEvidence(inbound, r.Body, strings.Repeat("c", 64), ""))
	if whole.RejectedNameSHA256 != "" || whole.RejectedNameOtherFamily != nil || whole.InboundFamilyNameDeclared != nil || whole.RejectedKnownBuiltin != "" || whole.ResolvedToolsSHA256 == "" {
		t.Fatal("whole response invented an identity")
	}
	success, _ := json.Marshal(ObserveToolPolicy(r))
	for _, key := range []string{"rejected_name_", "declared\"", "resolved_", "rejected_known_builtin"} {
		if bytes.Contains(success, []byte(key)) {
			t.Fatalf("new fields leaked onto success: %s", success)
		}
	}
	for _, v := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), " " + strings.Repeat("a", 64), "PRIVATE_HEADER"} {
		if toolPolicyValidSHA256(v) != "" {
			t.Fatal("invalid header retained")
		}
	}
}
