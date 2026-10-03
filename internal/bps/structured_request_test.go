package bps

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

const structuredRequestFormat = `{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object","properties":{"n":{"type":"integer","const":9007199254740993}},"required":["n"],"additionalProperties":false}}}`

func structuredRequestOptions() Options {
	return Options{TaskID: "synthetic-task", TurnID: "synthetic-turn", StructuredOutput: true}
}

func TestStructuredRequestKeepsClientPolicySeparate(t *testing.T) {
	for _, choice := range []string{`"none"`, `"auto"`, `"required"`, `{"type":"function","name":"lookup"}`} {
		t.Run(choice, func(t *testing.T) {
			input := []byte(`{"model":"fixture","input":[{"role":"user","content":"Original question"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}],"tool_choice":` + choice + `,"parallel_tool_calls":false,"text":` + structuredRequestFormat + `}`)
			before := bytes.Clone(input)
			prepared, err := Prepare(input, structuredRequestOptions())
			if err != nil || prepared.Structured == nil {
				t.Fatalf("structured request was not accepted: %v", err)
			}
			if !bytes.Equal(input, before) || len(prepared.Tools.Declared) != 1 || prepared.Tools.Declared[0].Name != "lookup" {
				t.Fatal("internal function altered the caller's request or public tool catalog")
			}
			if !bytes.Equal(prepared.Policy.ToolChoice, []byte(choice)) || prepared.Policy.ParallelToolCalls == nil || *prepared.Policy.ParallelToolCalls {
				t.Fatal("client tool policy changed")
			}
			var body map[string]json.RawMessage
			_ = json.Unmarshal(prepared.Body, &body)
			if len(body["text"]) != 0 || !bytes.Contains(prepared.Body, []byte(`9007199254740993`)) {
				t.Fatal("unsupported root format was forwarded or schema precision was lost")
			}
			internal := raw(map[string]any{"type": "function_call", "id": "fc_format", "call_id": "call_format", "name": prepared.Structured.ToolName, "arguments": `{"n":9007199254740993}`})
			if prepared.Tools.ValidateResponse([]json.RawMessage{internal}, prepared.Policy) == nil {
				t.Fatal("internal formatter was treated as a public client action")
			}
			projected := raw(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": `{"n":9007199254740993}`}}})
			err = prepared.Tools.ValidateResponse([]json.RawMessage{projected}, prepared.Policy)
			if (choice == `"required"` || choice[0] == '{') != (err != nil) {
				t.Fatal("formatting output incorrectly satisfied or violated client tool choice")
			}
		})
	}
}

func TestStructuredRequestFormattingIdentityCollision(t *testing.T) {
	format, err := ParseStructuredFormat(json.RawMessage(structuredRequestFormat))
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"model": "fixture", "input": "Original question", "text": json.RawMessage(structuredRequestFormat), "tools": []any{map[string]any{"type": "function", "name": format.ToolName, "parameters": map[string]any{"type": "object"}}}}
	_, err = Prepare(raw(request), structuredRequestOptions())
	var parameter *ParameterError
	if !errors.As(err, &parameter) {
		t.Fatal("client/internal tool collision was accepted")
	}
	request["tools"] = []any{map[string]any{"type": "namespace", "name": "client", "tools": []any{map[string]any{"type": "function", "name": format.ToolName, "parameters": map[string]any{"type": "object"}}}}}
	if _, err = Prepare(raw(request), structuredRequestOptions()); !errors.As(err, &parameter) {
		t.Fatal("same-name namespaced client function was accepted despite ambiguous upstream namespace omission")
	}
	item := map[string]json.RawMessage{"type": raw("function_call"), "name": raw(format.ToolName), "namespace": raw("client")}
	if IsStructuredFormattingItem(format, item) {
		t.Fatal("namespaced public tool was hidden")
	}
}

func TestStructuredRequestCompactNeverInjectsFormatter(t *testing.T) {
	options := structuredRequestOptions()
	options.Compact = true
	prepared, err := Prepare([]byte(`{"model":"fixture","input":[{"type":"reasoning","encrypted_content":"original-cipher"}],"text":`+structuredRequestFormat+`}`), options)
	if err != nil || prepared.Structured != nil {
		t.Fatalf("compact inherited format became a generation: %v", err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(prepared.Body, &fields)
	if len(fields) != 3 || len(fields["model"]) == 0 || len(fields["input"]) == 0 || len(fields["metadata"]) == 0 || bytes.Contains(prepared.Body, []byte("additional_tools")) || !bytes.Contains(prepared.Body, []byte("original-cipher")) {
		t.Fatal("compact protocol or opaque history changed")
	}
}
