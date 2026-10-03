package bps

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func policyPrepare(t *testing.T, source string) *Request {
	t.Helper()
	r, err := Prepare([]byte(source), Options{TaskID: "synthetic-task", TurnID: "synthetic-turn", StructuredOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func policyItem(t *testing.T, source string) map[string]json.RawMessage {
	t.Helper()
	var item map[string]json.RawMessage
	if err := json.Unmarshal([]byte(source), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestToolPolicyMetadataValidatedChoicesAndOldOmission(t *testing.T) {
	for _, choice := range []string{"auto", "none", "required", "forced"} {
		t.Run(choice, func(t *testing.T) {
			control := `"` + choice + `"`
			if choice == "forced" {
				control = `{"type":"function","name":"PRIVATE_TOOL"}`
			}
			r := policyPrepare(t, `{"model":"m","input":[],"tools":[{"type":"function","name":"PRIVATE_TOOL"}],"tool_choice":`+control+`}`)
			observation := ObserveToolPolicy(r)
			if observation.EffectiveChoice != choice || observation.CatalogAtPrepareCount != 1 || observation.NamedNamespacesAtPrepareCount != 0 || observation.Structured {
				t.Fatalf("wrong validated policy: %+v", observation)
			}
			raw, _ := json.Marshal(observation)
			if bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte("rejection")) {
				t.Fatal("private identity or nonexistent failure emitted")
			}
		})
	}
	if ObserveToolPolicy(nil) != nil || ObserveToolPolicy(&Request{Tools: &ToolCatalog{}}) != nil {
		t.Fatal("unknown request was treated as measured auto/zero")
	}
	var legacy struct {
		ToolPolicy *ToolPolicyObservation `json:"tool_policy,omitempty"`
	}
	raw, _ := json.Marshal(legacy)
	if string(raw) != `{}` {
		t.Fatal("legacy unobserved value was invented")
	}
	r := policyPrepare(t, `{"model":"m","input":[],"tools":[],"tool_choice":"none"}`)
	legacy.ToolPolicy = ObserveToolPolicy(r)
	raw, _ = json.Marshal(legacy)
	if !bytes.Contains(raw, []byte(`"catalog_at_prepare_count":0`)) || !bytes.Contains(raw, []byte(`"structured":false`)) {
		t.Fatal("known zero/false was omitted")
	}
}

func TestToolPolicyMetadataCountsEffectiveHistoryAndExcludesFormatter(t *testing.T) {
	r := policyPrepare(t, `{"model":"m","text":{"format":{"type":"json_object"}},"tools":[{"type":"namespace","name":"PRIVATE_NS","tools":[{"type":"function","name":"PRIVATE_TOP","parameters":{"private_schema":true}}]}],"input":[{"type":"additional_tools","id":"at_fixture","role":"developer","tools":[{"type":"function","name":"PRIVATE_HISTORY"}]}]}`)
	observation := ObserveToolPolicy(r)
	if !observation.Structured || observation.CatalogAtPrepareCount != 2 || observation.NamedNamespacesAtPrepareCount != 1 {
		t.Fatalf("effective catalog/formatter count wrong: %+v", observation)
	}
	item := map[string]json.RawMessage{"type": raw("function_call"), "name": raw(r.Structured.ToolName), "arguments": raw("PRIVATE_ARGUMENTS")}
	value := CaptureToolPolicyRejection(r, item)
	observation.RecordRejection("normalize_item", value)
	if observation.RejectedIdentity != "internal_formatter" || observation.RejectedMatchesExactDeclaration != nil {
		t.Fatal("internal formatter counted as client declaration")
	}
	encoded, _ := json.Marshal(observation)
	if bytes.Contains(encoded, []byte("PRIVATE")) || bytes.Contains(encoded, []byte(r.Structured.ToolName)) {
		t.Fatal("catalog/schema/name persisted in metadata")
	}
}

func TestToolPolicyMetadataPreFailureSnapshotAndDiscovery(t *testing.T) {
	r := policyPrepare(t, `{"model":"m","tools":[{"type":"tool_search","execution":"server"}],"input":[]}`)
	observation := ObserveToolPolicy(r)
	if observation.CatalogAtPrepareCount != 0 || !observation.ToolSearchAtPrepare {
		t.Fatal("native discovery counted as client action")
	}
	discovery := policyItem(t, `{"type":"tool_search_output","execution":"server","tools":[{"type":"function","name":"PRIVATE_DISCOVERED","namespace":"PRIVATE_NS"}]}`)
	prior := CaptureToolPolicyRejection(r, discovery)
	if err := r.Tools.NormalizeItem(discovery); err != nil {
		t.Fatal(err)
	}
	if observation.RejectionPhase != "" {
		t.Fatal("successful discovery became a rejection")
	}
	item := policyItem(t, `{"type":"function_call","name":"PRIVATE_UNKNOWN","arguments":"PRIVATE_PAYLOAD"}`)
	value := CaptureToolPolicyRejection(r, item)
	if err := r.Tools.NormalizeItem(item); err == nil {
		t.Fatal("negative fixture accepted")
	}
	observation.RecordRejection("normalize_item", value)
	if observation.CatalogAtPrepareCount != 0 || *observation.CatalogAtRejectionCount != 1 || *observation.NamedNamespacesAtRejectionCount != 1 || observation.RejectedIdentity != "undeclared" || *observation.RejectedMatchesExactDeclaration {
		t.Fatalf("dynamic effective count or match lost: %+v", observation)
	}
	// The value is a pre-call count snapshot, not a catalog pointer.
	other := ObserveToolPolicy(r)
	other.RecordRejection("normalize_item", prior)
	if *other.CatalogAtRejectionCount != 0 {
		t.Fatal("capture retained a mutable catalog alias")
	}
	// Failed discovery with conflicting schema cannot change the catalog count.
	conflict := policyItem(t, `{"type":"tool_search_output","execution":"server","tools":[{"type":"function","name":"PRIVATE_DISCOVERED","namespace":"PRIVATE_NS","description":"conflict"}]}`)
	before := CaptureToolPolicyRejection(r, conflict)
	if err := r.Tools.NormalizeItem(conflict); err == nil {
		t.Fatal("conflicting discovery accepted")
	}
	if next := CaptureToolPolicyRejection(r, nil); before.count != 1 || next.count != 1 {
		t.Fatal("failed discovery mutated catalog")
	}
}

func TestToolPolicyMetadataExactTypeNamespaceAndNoRawIdentity(t *testing.T) {
	for _, test := range []struct {
		name, definitions, item, identity string
		exact, reject                     bool
	}{
		{"global", `[{"type":"function","name":"PRIVATE_NAME"}]`, `{"type":"function_call","name":"PRIVATE_NAME"}`, "exact", true, false},
		{"unique_namespace", `[{"type":"function","name":"PRIVATE_NAME","namespace":"PRIVATE_NS"}]`, `{"type":"function_call","name":"PRIVATE_NAME"}`, "unique_namespace", false, false},
		{"explicit_namespace", `[{"type":"function","name":"PRIVATE_NAME","namespace":"PRIVATE_NS"}]`, `{"type":"function_call","name":"PRIVATE_NAME","namespace":"PRIVATE_NS"}`, "exact", true, false},
		{"wrong_type", `[{"type":"custom","name":"PRIVATE_NAME","format":{"type":"text"}}]`, `{"type":"function_call","name":"PRIVATE_NAME"}`, "undeclared", false, true},
		{"ambiguous", `[{"type":"function","name":"PRIVATE_NAME","namespace":"left"},{"type":"function","name":"PRIVATE_NAME","namespace":"right"}]`, `{"type":"function_call","name":"PRIVATE_NAME"}`, "ambiguous_namespace", false, true},
		{"global_and_namespace", `[{"type":"function","name":"PRIVATE_NAME"},{"type":"function","name":"PRIVATE_NAME","namespace":"left"}]`, `{"type":"function_call","name":"PRIVATE_NAME"}`, "ambiguous_namespace", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := policyPrepare(t, `{"model":"m","input":[],"tools":`+test.definitions+`}`)
			item := policyItem(t, test.item)
			original, _ := json.Marshal(item)
			captured := CaptureToolPolicyRejection(r, item)
			after, _ := json.Marshal(item)
			if !bytes.Equal(original, after) || captured.identity != test.identity || !captured.exactKnown || captured.exact != test.exact {
				t.Fatal("classification mutated input or confused exact identity")
			}
			err := r.Tools.NormalizeItem(item)
			if (err != nil) != test.reject {
				t.Fatal("existing normalizer changed semantics")
			}
			observation := ObserveToolPolicy(r)
			if err != nil {
				observation.RecordRejection("normalize_item", captured)
			}
			raw, _ := json.Marshal(observation)
			if bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte("left")) || bytes.Contains(raw, []byte("right")) {
				t.Fatal("raw identity leaked")
			}
		})
	}
}

func TestToolPolicyMetadataDeclaredButPolicyDeniedAndResponseUnknown(t *testing.T) {
	r := policyPrepare(t, `{"model":"m","input":[],"tools":[{"type":"function","name":"PRIVATE_TOOL"}],"tool_choice":"none"}`)
	item := policyItem(t, `{"type":"function_call","name":"PRIVATE_TOOL","arguments":"PRIVATE_ARGUMENT"}`)
	observation := ObserveToolPolicy(r)
	captured := CaptureToolPolicyRejection(r, item)
	if err := r.Tools.NormalizeItem(item); err == nil {
		t.Fatal("none denied fixture did not fail")
	}
	observation.RecordRejection("normalize_item", captured)
	if observation.EffectiveChoice != "none" || !*observation.RejectedMatchesExactDeclaration || observation.RejectedIdentity != "exact" {
		t.Fatal("declared policy violation was misreported as undeclared")
	}
	r = policyPrepare(t, `{"model":"m","input":[],"tools":[{"type":"function","name":"PRIVATE_TOOL"}],"tool_choice":"required"}`)
	observation = ObserveToolPolicy(r)
	captured = CaptureToolPolicyRejection(r, nil)
	if err := r.Tools.ValidateResponse(nil, r.Policy); err == nil {
		t.Fatal("missing required tool did not fail")
	}
	observation.RecordRejection("validate_response", captured)
	if observation.RejectedFamily != "" || observation.RejectedMatchesExactDeclaration != nil || observation.RejectionPhase != "validate_response" {
		t.Fatal("whole-response rejection invented a particular item")
	}
}

func TestToolPolicyMetadataSnapshotPrecedesForcedNamespaceMutation(t *testing.T) {
	r := policyPrepare(t, `{"model":"m","input":[],"tools":[{"type":"function","name":"PRIVATE_REQUIRED","namespace":"allowed"},{"type":"function","name":"PRIVATE_OTHER","namespace":"other"}],"tool_choice":{"type":"function","name":"PRIVATE_REQUIRED","namespace":"allowed"}}`)
	item := policyItem(t, `{"type":"function_call","name":"PRIVATE_OTHER","call_id":"fixture","arguments":"PRIVATE_ARGUMENT"}`)
	observation := ObserveToolPolicy(r)
	captured := CaptureToolPolicyRejection(r, item)
	if err := r.Tools.NormalizeItem(item); err == nil || stringField(item, "namespace") != "other" {
		t.Fatal("fixture must reject forced policy after namespace restoration")
	}
	observation.RecordRejection("normalize_item", captured)
	if observation.EffectiveChoice != "forced" || observation.RejectedIdentity != "unique_namespace" || *observation.RejectedMatchesExactDeclaration {
		t.Fatal("observer incorrectly classified the mutated item as exact input")
	}
	if value := CaptureToolPolicyRejection(r, item); !value.exact {
		t.Fatal("control must show why observing only after mutation is inaccurate")
	}
}

func TestToolPolicyMetadataDoesNotRetainPayloadOrCrossAttemptState(t *testing.T) {
	r := policyPrepare(t, `{"model":"m","input":[]}`)
	first, second := ObserveToolPolicy(r), ObserveToolPolicy(r)
	item := policyItem(t, `{"type":"PRIVATE_FAMILY_call","name":"PRIVATE_TOOL","arguments":"PRIVATE_SCHEMA_PROMPT_CIPHER"}`)
	value := CaptureToolPolicyRejection(r, item)
	first.RecordRejection("PRIVATE_ERROR_TEXT", value)
	if first.RejectionPhase != "" {
		t.Fatal("free text phase was accepted")
	}
	first.RecordRejection("normalize_item", value)
	before, _ := json.Marshal(first)
	first.RecordRejection("validate_response", CaptureToolPolicyRejection(r, nil))
	after, _ := json.Marshal(first)
	if !bytes.Equal(before, after) || second.RejectionPhase != "" || first.RejectedFamily != "other_call" {
		t.Fatal("rejection overwritten or copied into another attempt")
	}
	if strings.Contains(string(after), "PRIVATE") {
		t.Fatal("payload/free text persisted")
	}
	var decoded map[string]any
	_ = json.Unmarshal(after, &decoded)
	allowed := map[string]bool{"effective_choice": true, "catalog_at_prepare_count": true, "named_namespaces_at_prepare_count": true, "tool_search_at_prepare": true, "structured": true, "catalog_at_rejection_count": true, "named_namespaces_at_rejection_count": true, "rejection_phase": true, "rejected_family": true, "rejected_identity": true, "rejected_matches_exact_declaration": true}
	for key := range decoded {
		if !allowed[key] {
			t.Fatalf("unexpected producer field %q", key)
		}
	}
	for _, field := range []reflect.Type{reflect.TypeOf(ToolPolicyObservation{}), reflect.TypeOf(ToolPolicyRejection{})} {
		for i := 0; i < field.NumField(); i++ {
			kind := field.Field(i).Type.Kind()
			if kind == reflect.Slice || kind == reflect.Map || kind == reflect.Interface {
				t.Fatal("observation retained raw payload/schema/error storage")
			}
		}
	}
}
