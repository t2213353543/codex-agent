package bps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestStructuredMixedObservationExistingRejectionShapes(t *testing.T) {
	format := structuredStreamFormat(t)
	formatter := func(id string) json.RawMessage {
		return structuredStreamCall(format, id, "call_"+id, `{"value":"PRIVATE_ARGUMENT"}`)
	}
	client := json.RawMessage(`{"type":"function_call","id":"fc_client","call_id":"client","name":"PRIVATE_CUSTOMER_NAME","namespace":"PRIVATE_NAMESPACE","arguments":"PRIVATE_CUSTOMER_ARGUMENTS"}`)
	other := json.RawMessage(`{"type":"web_search_call","id":"search","status":"in_progress"}`)
	text := json.RawMessage(`{"type":"message","id":"text","role":"assistant","content":[{"type":"output_text","text":"PRIVATE_TEXT"}]}`)
	refusal := json.RawMessage(`{"type":"message","id":"refusal","role":"assistant","content":[{"type":"refusal","refusal":"PRIVATE_REFUSAL"}]}`)
	for _, tc := range []struct {
		name  string
		items []json.RawMessage
		want  StructuredMixedObservation
	}{
		{"multiple_formatter", []json.RawMessage{formatter("one"), formatter("two")}, StructuredMixedObservation{FormatterCount: 2}},
		{"formatter_client_call", []json.RawMessage{formatter("one"), client}, StructuredMixedObservation{FormatterCount: 1, ClientCallCount: 1}},
		{"formatter_other_call", []json.RawMessage{formatter("one"), other}, StructuredMixedObservation{FormatterCount: 1, OtherCallCount: 1}},
		{"formatter_text", []json.RawMessage{formatter("one"), text}, StructuredMixedObservation{FormatterCount: 1, HasText: true}},
		{"formatter_refusal", []json.RawMessage{formatter("one"), refusal}, StructuredMixedObservation{FormatterCount: 1, HasRefusal: true}},
		{"multiple_with_calls_text_and_refusal", []json.RawMessage{formatter("one"), formatter("two"), client, other, text, refusal}, StructuredMixedObservation{FormatterCount: 2, ClientCallCount: 1, OtherCallCount: 1, HasText: true, HasRefusal: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "real_response", "status": "completed", "model": "actual_model", "output": tc.items, "usage": json.RawMessage(`{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":20}}`)}})
			var dst bytes.Buffer
			commits, events := 0, 0
			result, err := StreamStructured(t.Context(), &dst, strings.NewReader(wire), StreamOptions{OnComplete: func(StreamResult) error { commits++; return nil }, OnEvent: func([]byte) error { events++; return nil }}, format)
			var validation *StructuredValidationError
			if !errors.As(err, &validation) || err.Error() != "structured output mixed incompatible final outputs" || result.Completed || dst.Len() != 0 || commits != 0 || events != 0 {
				t.Fatalf("mixed rejection changed: result=%+v err=%v visible=%d commits=%d events=%d", result, err, dst.Len(), commits, events)
			}
			got := ObserveStructuredMixed(err)
			if got == nil || *got != tc.want {
				t.Fatalf("wrong existing shape classification: got=%+v want=%+v", got, tc.want)
			}
			if !bytes.Contains(result.Usage, []byte(`"input_tokens":100`)) || !bytes.Contains(result.Usage, []byte(`"cache_write_tokens":20`)) {
				t.Fatal("real rejected terminal usage changed")
			}
			encoded, marshalErr := json.Marshal(got)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(encoded, &fields)
			if len(fields) != 5 {
				t.Fatalf("observation added/omitted fields: %s", encoded)
			}
			for _, key := range []string{"formatter_count", "client_call_count", "other_call_count", "has_text", "has_refusal"} {
				if _, ok := fields[key]; !ok {
					t.Fatalf("observed zero/false omitted: %s", key)
				}
			}
			if bytes.Contains(encoded, []byte("PRIVATE")) || bytes.Contains(encoded, []byte(format.ToolName)) {
				t.Fatal("private identity/content retained in observation")
			}
		})
	}
}

func TestStructuredMixedObservationGetterCopiesAndDoesNotParseText(t *testing.T) {
	original := StructuredMixedObservation{FormatterCount: 2, ClientCallCount: 3, OtherCallCount: 4, HasText: true}
	inner := &structuredMixedError{observation: original}
	wrapped := fmt.Errorf("outer: %w", &StructuredValidationError{Err: inner})
	one := ObserveStructuredMixed(wrapped)
	if one == nil || *one != original {
		t.Fatal("wrapped typed observation lost")
	}
	one.FormatterCount = 99
	one.ClientCallCount = 0
	one.OtherCallCount = 0
	one.HasText = false
	one.HasRefusal = true
	two := ObserveStructuredMixed(wrapped)
	if two == nil || *two != original || one == two || inner.observation != original {
		t.Fatal("getter shares mutable state")
	}
	for _, err := range []error{nil, errors.New(inner.Error()), fmt.Errorf("wrapped: %w", errors.New(inner.Error())), &StructuredValidationError{Err: errors.New("structured output does not satisfy the requested schema")}, (*structuredMixedError)(nil)} {
		if ObserveStructuredMixed(err) != nil {
			t.Fatalf("nonmixed or typed nil acquired observation: %T", err)
		}
	}
	zero, _ := json.Marshal(&StructuredMixedObservation{})
	var fields map[string]any
	_ = json.Unmarshal(zero, &fields)
	if len(fields) != 5 || fields["formatter_count"] != float64(0) || fields["has_text"] != false || fields["has_refusal"] != false {
		t.Fatal("zero values became unknown")
	}
}

func TestStructuredMixedObservationAbsentForOtherOutcomes(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, tc := range []struct {
		name      string
		items     []json.RawMessage
		wantError bool
	}{
		{"single_formatter", []json.RawMessage{structuredStreamCall(format, "formatter", "call", `{"value":"OK"}`)}, false},
		{"pure_refusal", []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","id":"refusal","content":[{"type":"refusal","refusal":"declined"}]}`)}, false},
		{"client_action_turn", []json.RawMessage{json.RawMessage(`{"type":"function_call","id":"fc_client","call_id":"client","name":"public_client","arguments":"{}"}`)}, false},
		{"schema_invalid", []json.RawMessage{structuredStreamCall(format, "formatter", "call", `{"not_value":"bad"}`)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response", "status": "completed", "output": tc.items}})
			var dst bytes.Buffer
			_, err := StreamStructured(t.Context(), &dst, strings.NewReader(wire), StreamOptions{}, format)
			if (err != nil) != tc.wantError || ObserveStructuredMixed(err) != nil {
				t.Fatalf("unrelated outcome reclassified: %v", err)
			}
		})
	}
}

func TestStructuredMixedObservationUsesExistingClientAndOtherCallDefinitions(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, tc := range []struct {
		kind, execution string
		client, other   int
	}{
		{"custom_tool_call", "", 1, 0}, {"local_shell_call", "", 1, 0}, {"shell_call", "", 1, 0}, {"tool_search_call", "client", 1, 0}, {"tool_search_call", "server", 0, 1}, {"web_search_call", "", 0, 1},
	} {
		t.Run(tc.kind+tc.execution, func(t *testing.T) {
			item := map[string]any{"id": "public", "call_id": "public_call", "type": tc.kind, "execution": tc.execution, "name": "public_name", "arguments": "{}"}
			wire := structuredStreamFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response", "status": "completed", "output": []any{structuredStreamCall(format, "formatter", "call", `{"value":"OK"}`), item}}})
			var dst bytes.Buffer
			_, err := StreamStructured(t.Context(), &dst, strings.NewReader(wire), StreamOptions{}, format)
			want := &StructuredMixedObservation{FormatterCount: 1, ClientCallCount: tc.client, OtherCallCount: tc.other}
			if !reflect.DeepEqual(ObserveStructuredMixed(err), want) {
				t.Fatalf("did not use existing call-family counters: got=%+v want=%+v err=%v", ObserveStructuredMixed(err), want, err)
			}
		})
	}
}
