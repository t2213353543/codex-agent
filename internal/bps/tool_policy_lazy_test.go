package bps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestToolPolicyLazyMatchesEagerAtActualRejections(t *testing.T) {
	for _, tc := range []struct {
		name, tools, choice, item string
		parallel                  bool
	}{
		{"reasoning_success", `[]`, `"auto"`, `{"type":"reasoning","encrypted_content":"PRIVATE_CIPHER"}`, false},
		{"message_success", `[]`, `"auto"`, `{"type":"message","role":"assistant","content":[]}`, false},
		{"tool_success", `[{"type":"function","name":"PRIVATE_TOOL","namespace":"ns"}]`, `"auto"`, `{"type":"function_call","name":"PRIVATE_TOOL"}`, false},
		{"forced_restore_reject", `[{"type":"function","name":"PRIVATE_REQUIRED","namespace":"a"},{"type":"function","name":"PRIVATE_OTHER","namespace":"b"}]`, `{"type":"function","name":"PRIVATE_REQUIRED","namespace":"a"}`, `{"type":"function_call","name":"PRIVATE_OTHER"}`, false},
		{"none_declared", `[{"type":"function","name":"PRIVATE_TOOL"}]`, `"none"`, `{"type":"function_call","name":"PRIVATE_TOOL"}`, false},
		{"ambiguous", `[{"type":"function","name":"PRIVATE_TOOL"},{"type":"function","name":"PRIVATE_TOOL","namespace":"ns"}]`, `"auto"`, `{"type":"function_call","name":"PRIVATE_TOOL"}`, false},
		{"unknown", `[]`, `"auto"`, `{"type":"function_call","name":"PRIVATE_UNKNOWN"}`, false},
		{"parallel_after_restore", `[{"type":"function","name":"PRIVATE_TOOL","namespace":"ns"}]`, `"auto"`, `{"type":"function_call","name":"PRIVATE_TOOL"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"model":"m","input":[],"tools":` + tc.tools + `,"tool_choice":` + tc.choice + `}`
			r := policyPrepare(t, input)
			item := policyItem(t, tc.item)
			eager := CaptureToolPolicyRejection(r, item)
			snapshot := CaptureToolPolicyItem(item)
			beforeDefs := make(map[ToolIdentity]json.RawMessage, len(r.Tools.definitions))
			for identity, definition := range r.Tools.definitions {
				beforeDefs[identity] = bytes.Clone(definition)
			}
			beforeIds := append([]ToolIdentity(nil), r.Tools.Declared...)
			err := r.Tools.NormalizeItem(item)
			if err == nil && !tc.parallel {
				return // Successful path never calls Resolve/counts.
			}
			actual := snapshot.Resolve(r)
			if !reflect.DeepEqual(eager, actual) {
				t.Fatalf("lazy pre-identity/count classification changed: eager=%+v lazy=%+v", eager, actual)
			}
			if !reflect.DeepEqual(beforeDefs, r.Tools.definitions) || !reflect.DeepEqual(beforeIds, r.Tools.Declared) {
				t.Fatal("failed normalize/parallel changed tool catalog")
			}
			obs := ObserveToolPolicy(r)
			obs.RecordRejection("normalize_item", actual)
			encoded, _ := json.Marshal(obs)
			if bytes.Contains(encoded, []byte("PRIVATE")) {
				t.Fatal("transient private identity escaped persistent metadata")
			}
			if tc.name == "forced_restore_reject" && (stringField(item, "namespace") != "b" || actual.identity != "unique_namespace" || actual.exact) {
				t.Fatal("pre-normalize namespace lost")
			}
		})
	}
}

func TestToolPolicyLazyZeroUnknownAndNoneRemainDistinct(t *testing.T) {
	item := policyItem(t, `{"type":"function_call","name":"undeclared"}`)
	snapshot := CaptureToolPolicyItem(item)
	if actual := snapshot.Resolve(nil); actual.known {
		t.Fatal("unknown request became a measured empty catalog")
	}
	r := policyPrepare(t, `{"model":"m","input":[],"tools":[],"tool_choice":"none"}`)
	o := ObserveToolPolicy(r)
	if err := r.Tools.NormalizeItem(item); err == nil {
		t.Fatal("tool_choice none accepted an action")
	}
	o.RecordRejection("normalize_item", snapshot.Resolve(r))
	if o.EffectiveChoice != "none" || o.CatalogAtPrepareCount != 0 || o.CatalogAtRejectionCount == nil || *o.CatalogAtRejectionCount != 0 || o.NamedNamespacesAtRejectionCount == nil || *o.NamedNamespacesAtRejectionCount != 0 || o.RejectedMatchesExactDeclaration == nil || *o.RejectedMatchesExactDeclaration {
		t.Fatal("known zero/false became unknown or policy none was lost")
	}
	encoded, _ := json.Marshal(o)
	for _, field := range []string{`"catalog_at_rejection_count":0`, `"named_namespaces_at_rejection_count":0`, `"rejected_matches_exact_declaration":false`, `"effective_choice":"none"`} {
		if !bytes.Contains(encoded, []byte(field)) {
			t.Fatal("persistent observation omitted known zero/false")
		}
	}
}

func TestToolPolicyLazyDiscoveryFailureIsAtomicAndSuccessCountsAreCurrent(t *testing.T) {
	r := policyPrepare(t, `{"model":"m","input":[],"tools":[{"type":"tool_search","execution":"server"},{"type":"function","name":"existing","namespace":"old"}]}`)
	observation := ObserveToolPolicy(r)
	for _, definitions := range []string{
		`[{"type":"function","name":"new","namespace":"new"},{"type":"function","name":"existing","namespace":"old","description":"conflict"}]`,
		`[{"type":"function","name":"new","namespace":"new"},{"type":"unsupported","name":"invalid"}]`,
		`[{"type":"function","name":"new","namespace":"new"},{"type":"web_search"}]`,
	} {
		item := policyItem(t, `{"type":"tool_search_output","execution":"server","tools":`+definitions+`}`)
		before := CaptureToolPolicyRejection(r, item)
		lazy := CaptureToolPolicyItem(item)
		beforeCount, beforeNamespaces := toolPolicyCatalogCounts(r.Tools)
		if err := r.Tools.NormalizeItem(item); err == nil {
			t.Fatal("invalid discovery accepted")
		}
		afterCount, afterNamespaces := toolPolicyCatalogCounts(r.Tools)
		if beforeCount != afterCount || beforeNamespaces != afterNamespaces || !reflect.DeepEqual(before, lazy.Resolve(r)) {
			t.Fatal("failed discovery partially modified catalog before rejection capture")
		}
	}
	valid := policyItem(t, `{"type":"tool_search_output","execution":"server","tools":[{"type":"function","name":"discovered","namespace":"new"}]}`)
	if err := r.Tools.NormalizeItem(valid); err != nil {
		t.Fatal(err)
	}
	bad := policyItem(t, `{"type":"function_call","name":"unknown"}`)
	snapshot := CaptureToolPolicyItem(bad)
	if err := r.Tools.NormalizeItem(bad); err == nil {
		t.Fatal("unknown tool accepted")
	}
	observation.RecordRejection("normalize_item", snapshot.Resolve(r))
	if observation.CatalogAtPrepareCount != 1 || *observation.CatalogAtRejectionCount != 2 || *observation.NamedNamespacesAtRejectionCount != 2 {
		t.Fatal("successful discovery did not update later failure counts")
	}
}

func TestToolPolicyLazyResponseNeedsPreValidationSnapshot(t *testing.T) {
	r := policyPrepare(t, `{"model":"m","input":[],"tools":[{"type":"tool_search","execution":"server"}]}`)
	observation := ObserveToolPolicy(r)
	before := CaptureToolPolicyRejection(r, nil)
	output := []json.RawMessage{
		json.RawMessage(`{"type":"tool_search_output","execution":"server","tools":[{"type":"function","name":"new","namespace":"dynamic"}]}`),
		json.RawMessage(`{"type":"function_call","name":"unknown","call_id":"c"}`),
	}
	if err := r.Tools.ValidateResponse(output, r.Policy); err == nil {
		t.Fatal("invalid response accepted")
	}
	// The actual current implementation shallow-copies definitions. This test
	// proves that after-failure counting is NOT equivalent for whole responses.
	after := CaptureToolPolicyRejection(r, nil)
	if before.count != 0 || after.count != 1 || after.namespaces != 1 {
		t.Fatal("fixture no longer demonstrates ValidateResponse partial catalog publication; review snapshot boundary")
	}
	observation.RecordRejection("validate_response", before)
	if *observation.CatalogAtRejectionCount != 0 || *observation.NamedNamespacesAtRejectionCount != 0 || observation.RejectedMatchesExactDeclaration != nil || observation.RejectedIdentity != "" {
		t.Fatal("whole-response pre-call zero/unknown contract changed")
	}
}

var toolPolicyLazyBenchmarkSink ToolPolicyItemSnapshot
var toolPolicyEagerBenchmarkSink ToolPolicyRejection

func BenchmarkToolPolicyCaptureSuccessfulItem(b *testing.B) {
	for _, count := range []int{10, 1000, 10000} {
		catalog := &ToolCatalog{definitions: make(map[ToolIdentity]json.RawMessage)}
		for i := 0; i < count; i++ {
			catalog.definitions[ToolIdentity{Type: "function", Name: fmt.Sprint("tool", i), Namespace: fmt.Sprint("ns", i%100)}] = json.RawMessage(`{"type":"function"}`)
		}
		r := &Request{Tools: catalog, Policy: Policy{choice: "auto"}}
		item := map[string]json.RawMessage{"type": raw("reasoning"), "encrypted_content": raw("PRIVATE_CIPHER")}
		b.Run(fmt.Sprintf("eager/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				toolPolicyEagerBenchmarkSink = CaptureToolPolicyRejection(r, item)
			}
		})
		b.Run(fmt.Sprintf("lazy/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				toolPolicyLazyBenchmarkSink = CaptureToolPolicyItem(item)
			}
		})
	}
}

func BenchmarkToolPolicyThousandSuccessfulItems(b *testing.B) {
	for _, count := range []int{200, 1600} {
		catalog := &ToolCatalog{definitions: make(map[ToolIdentity]json.RawMessage)}
		for i := 0; i < count; i++ {
			catalog.definitions[ToolIdentity{Type: "function", Name: fmt.Sprint("tool", i), Namespace: fmt.Sprint("ns", i%100)}] = json.RawMessage(`{"type":"function"}`)
		}
		r := &Request{Tools: catalog, Policy: Policy{choice: "auto"}}
		item := map[string]json.RawMessage{"type": raw("reasoning"), "encrypted_content": raw("PRIVATE_CIPHER")}
		for _, label := range []string{"baseline", "eager", "lazy"} {
			b.Run(fmt.Sprintf("%s/%d_tools_1000_items", label, count), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					for range 1000 {
						if label == "lazy" {
							toolPolicyLazyBenchmarkSink = CaptureToolPolicyItem(item)
						} else if label == "eager" {
							toolPolicyEagerBenchmarkSink = CaptureToolPolicyRejection(r, item)
						}
						if err := catalog.NormalizeItem(item); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
