package bps

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func forcedReviewPrepare(tools, choice, input string, structured bool) (*Request, error) {
	body := `{"model":"fixture","tools":` + tools + `,"tool_choice":` + choice + `,"input":` + input + `,"parallel_tool_calls":false`
	if structured {
		body += `,"text":{"format":{"type":"json_object"}}`
	}
	return Prepare([]byte(body+`}`), Options{TaskID: "forced-task", TurnID: "forced-turn", StructuredOutput: true})
}

func TestRootForcedToolMissingNamespaceUniqueDeclaredIdentity(t *testing.T) {
	for _, structured := range []bool{false, true} {
		for _, family := range []string{"function", "custom"} {
			t.Run(family+map[bool]string{false: "/plain", true: "/structured"}[structured], func(t *testing.T) {
				definition := `{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"n":{"const":900719925474099312345}}}}`
				if family == "custom" {
					definition = `{"type":"custom","name":"inspect","format":{"type":"text"}}`
				}
				tools := `[{"type":"namespace","name":"client","description":"synthetic","tools":[` + definition + `]}]`
				choice := `{"type":"` + family + `","name":"inspect"}`
				p, err := forcedReviewPrepare(tools, choice, `[]`, structured)
				if err != nil {
					t.Fatalf("unique declared forced identity rejected: %v", err)
				}
				if p.Policy.forced == nil || *p.Policy.forced != (ToolIdentity{Type: family, Name: "inspect", Namespace: "client"}) || p.Policy.choice != "forced" {
					t.Fatal("forced policy did not bind exact unique identity")
				}
				if string(p.Policy.ToolChoice) != choice {
					t.Fatal("original client tool_choice was rewritten")
				}
				constraint := p.Policy.developerConstraint(p.Tools)
				if structured {
					constraint = p.Policy.structuredDeveloperConstraint(p.Tools, p.Structured)
				}
				if !strings.Contains(constraint, `namespace="client"`) {
					t.Fatal("effective namespace absent from model-facing constraint")
				}
				callType, payloadField := "function_call", "arguments"
				if family == "custom" {
					callType, payloadField = "custom_tool_call", "input"
				}
				call := json.RawMessage(`{"type":"` + callType + `","name":"inspect","call_id":"selected","` + payloadField + `":"900719925474099312345"}`)
				normalized, err := p.Tools.NormalizeOutput(call)
				if err != nil || !bytes.Contains(normalized, []byte(`"namespace":"client"`)) || !bytes.Contains(normalized, []byte(`"`+payloadField+`":"900719925474099312345"`)) {
					t.Fatalf("selected tool failed or payload changed: %v", err)
				}
				if err = p.Tools.ValidateResponse([]json.RawMessage{normalized}, p.Policy); err != nil {
					t.Fatal(err)
				}
				if p.Tools.ValidateResponse(nil, p.Policy) == nil {
					t.Fatal("forced choice no longer requires a call")
				}
				if family == "function" && !bytes.Contains(p.Body, []byte("900719925474099312345")) {
					t.Fatal("declaration schema precision lost")
				}
			})
		}
	}
}

func TestRootForcedToolMissingNamespaceUsesEffectiveHistoryCatalog(t *testing.T) {
	for _, kind := range []string{"additional_tools", "tool_search_output"} {
		input := `[{"type":"` + kind + `","id":"at_synthetic","role":"developer","tools":[{"type":"function","name":"discovered","namespace":"history","parameters":{"type":"object"}}]}]`
		p, err := forcedReviewPrepare(`[]`, `{"type":"function","name":"discovered"}`, input, false)
		if err != nil || p.Policy.forced == nil || p.Policy.forced.Namespace != "history" {
			t.Fatalf("effective history declaration ignored: kind=%s err=%v", kind, err)
		}
	}
	_, err := forcedReviewPrepare(`[]`, `{"type":"function","name":"ghost"}`, `[{"type":"function_call","name":"ghost","namespace":"history","call_id":"old","arguments":"{}"},{"type":"function_call_output","call_id":"old","output":"synthetic"}]`, false)
	if err == nil {
		t.Fatal("historical call promoted to declared forced tool")
	}
}

func TestRootForcedToolDoesNotGuessWrongOrAmbiguousIdentity(t *testing.T) {
	for name, tc := range map[string]struct{ tools, choice string }{
		"wrong_namespace":             {`[{"type":"function","name":"inspect","namespace":"one"}]`, `{"type":"function","name":"inspect","namespace":"wrong"}`},
		"two_namespaces":              {`[{"type":"function","name":"inspect","namespace":"one"},{"type":"function","name":"inspect","namespace":"two"}]`, `{"type":"function","name":"inspect"}`},
		"global_and_namespace":        {`[{"type":"function","name":"inspect"},{"type":"function","name":"inspect","namespace":"one"}]`, `{"type":"function","name":"inspect"}`},
		"wrong_family":                {`[{"type":"custom","name":"inspect","namespace":"one","format":{"type":"text"}}]`, `{"type":"function","name":"inspect"}`},
		"namespace_null":              {`[{"type":"function","name":"inspect","namespace":"one"}]`, `{"type":"function","name":"inspect","namespace":null}`},
		"namespace_number":            {`[{"type":"function","name":"inspect","namespace":"one"}]`, `{"type":"function","name":"inspect","namespace":123}`},
		"explicit_empty_not_restored": {`[{"type":"function","name":"inspect","namespace":"one"}]`, `{"type":"function","name":"inspect","namespace":""}`},
		"undeclared":                  {`[]`, `{"type":"function","name":"inspect"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := forcedReviewPrepare(tc.tools, tc.choice, `[]`, false); err == nil {
				t.Fatal("invalid/ambiguous identity selected")
			}
		})
	}
}

func TestRootForcedToolExplicitAndMixedFamiliesStaySeparated(t *testing.T) {
	tools := `[{"type":"function","name":"inspect","namespace":"one"},{"type":"custom","name":"inspect","namespace":"two","format":{"type":"text"}}]`
	for _, family := range []string{"function", "custom"} {
		p, err := forcedReviewPrepare(tools, `{"type":"`+family+`","name":"inspect"}`, `[]`, false)
		if err != nil || p.Policy.forced == nil || p.Policy.forced.Type != family {
			t.Fatalf("family-specific uniqueness was not preserved: %v", err)
		}
	}
	explicit := `[{"type":"function","name":"inspect"},{"type":"function","name":"inspect","namespace":"one"}]`
	p, err := forcedReviewPrepare(explicit, `{"type":"function","name":"inspect","namespace":"one"}`, `[]`, false)
	if err != nil || p.Policy.forced.Namespace != "one" {
		t.Fatal("explicit correct namespace was ignored")
	}
	p, err = forcedReviewPrepare(explicit, `{"type":"function","name":"inspect","namespace":""}`, `[]`, false)
	if err != nil || p.Policy.forced.Namespace != "" {
		t.Fatal("explicit global namespace changed")
	}
}
