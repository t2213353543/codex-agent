package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func independentStructuredFailureRun(t *testing.T, shape string, flatMentionsFormatter bool) (StreamResult, *TerminalError, []byte) {
	t.Helper()
	format := structuredStreamFormat(t)
	var internal map[string]any
	_ = json.Unmarshal(structuredStreamCall(format, "fc_private", "call_private", `{"value":"OK"}`), &internal)
	internal["status"] = "in_progress"
	usage := map[string]any{"input_tokens": 100, "output_tokens": 7, "input_tokens_details": map[string]int{"cached_tokens": 80, "cache_write_tokens": 20}}
	detail := map[string]any{"code": "server_error", "message": "failed " + format.ToolName, "param": format.ToolName, "ordinal": json.RawMessage(`9007199254740993`)}
	response := map[string]any{"id": "resp_structured_error", "status": "in_progress", "output": []any{internal}, "usage": usage, "tools": []any{format.ToolDefinition()}}
	event := map[string]any{"type": "error", "response": response}
	switch shape {
	case "event_nested":
		event["error"] = detail
		response["error"] = map[string]any{"code": "invalid_request", "message": "lower priority"}
	case "response_nested":
		response["error"] = detail
	case "flat":
		event["code"] = "server_error"
		event["message"] = "plain failure"
		event["param"] = "input"
		if flatMentionsFormatter {
			event["message"] = "failed " + format.ToolName
			event["param"] = format.ToolName
		}
	}
	wire := structuredStreamFrame(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_structured_error", "status": "in_progress", "output": []any{}, "tools": []any{format.ToolDefinition()}}}) +
		structuredStreamFrame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": internal}) +
		structuredStreamFrame(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_private", "delta": `{"value":"OK"}`}) +
		structuredStreamFrame(event)
	var output bytes.Buffer
	commits, terminals := 0, 0
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(wire), StreamOptions{Unbounded: true, OnComplete: func(StreamResult) error { commits++; return nil }, OnUpstreamTerminal: func([]byte) error { terminals++; return nil }}, format)
	var terminal *TerminalError
	if !errors.As(err, &terminal) || result.Completed || commits != 0 || terminals != 1 || terminal.Type != "response.failed" || terminal.Status != "failed" || terminal.Code != "server_error" {
		t.Fatalf("failed consumer contract changed: complete=%v commits=%d terminals=%d err=%v", result.Completed, commits, terminals, err)
	}
	if result.ResponseID != "resp_structured_error" || len(result.Output) != 0 || !bytes.Contains(result.Usage, []byte(`"cache_write_tokens":20`)) {
		t.Fatal("terminal identity/real usage/internal-output removal changed")
	}
	terminalFrames := 0
	for _, line := range strings.Split(output.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]json.RawMessage
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
		var kind string
		_ = json.Unmarshal(event["type"], &kind)
		if kind == "response.completed" || kind == "error" || strings.Contains(kind, "function_call_arguments") {
			t.Fatal("public completion/raw error/internal argument emitted")
		}
		if kind == "response.failed" {
			terminalFrames++
			if !bytes.Equal(terminal.Payload, []byte(strings.TrimPrefix(line, "data: "))) {
				t.Fatal("TerminalError payload differs from public terminal")
			}
		}
	}
	if terminalFrames != 1 {
		t.Fatalf("public failure terminals=%d", terminalFrames)
	}
	if bytes.Contains(result.Response, []byte(format.ToolName)) || bytes.Contains(output.Bytes(), []byte("fc_private")) {
		t.Fatal("internal response or argument identity leaked")
	}
	return result, terminal, output.Bytes()
}

func TestIndependentStructuredMappedFailureContract(t *testing.T) {
	for _, shape := range []string{"event_nested", "response_nested", "flat"} {
		t.Run(shape, func(t *testing.T) {
			_, terminal, wire := independentStructuredFailureRun(t, shape, false)
			if bytes.Contains(wire, []byte(structuredStreamFormat(t).ToolName)) || bytes.Contains(terminal.Payload, []byte(structuredStreamFormat(t).ToolName)) {
				t.Fatal("structured formatter exposed")
			}
		})
	}
}

func TestIndependentStructuredMappedFlatErrorHidesFormatter(t *testing.T) {
	_, terminal, wire := independentStructuredFailureRun(t, "flat", true)
	name := []byte(structuredStreamFormat(t).ToolName)
	if bytes.Contains(wire, name) || bytes.Contains(terminal.Payload, name) {
		var event map[string]json.RawMessage
		_ = json.Unmarshal(terminal.Payload, &event)
		t.Fatalf("formatter survives top-level mapped error fields: message=%t param=%t response=%t", bytes.Contains(event["message"], name), bytes.Contains(event["param"], name), bytes.Contains(event["response"], name))
	}
}

func TestIndependentStructuredFailedFlatDiagnosticFields(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, field := range []string{"message", "param", "detail", "code"} {
		t.Run(field, func(t *testing.T) {
			event := map[string]any{"type": "error", "code": "server_error", "message": "ordinary message", "param": "input", "detail": "ordinary detail", "extension": map[string]any{"ordinal": json.RawMessage(`9007199254740993`), "label": "untouched public metadata"}, "response": map[string]any{"id": "resp_flat_fields", "usage": map[string]int{"input_tokens": 17, "output_tokens": 2}}}
			event[field] = "before " + format.ToolName + " after"
			var output bytes.Buffer
			commits := 0
			result, err := StreamStructured(context.Background(), &output, strings.NewReader(structuredStreamFrame(event)), StreamOptions{OnComplete: func(StreamResult) error { commits++; return nil }}, format)
			var terminal *TerminalError
			if !errors.As(err, &terminal) || result.Completed || commits != 0 {
				t.Fatal("failure became completion")
			}
			var projected map[string]json.RawMessage
			_ = json.Unmarshal(terminal.Payload, &projected)
			var diagnostic string
			_ = json.Unmarshal(projected[field], &diagnostic)
			if diagnostic != "before structured output after" {
				t.Fatalf("%s diagnostic formatter not scrubbed", field)
			}
			for _, other := range []string{"code", "message", "param", "detail"} {
				if other == field {
					continue
				}
				var actual string
				_ = json.Unmarshal(projected[other], &actual)
				if actual != event[other] {
					t.Fatalf("ordinary %s changed", other)
				}
			}
			if !bytes.Contains(projected["extension"], []byte("9007199254740993")) || !bytes.Contains(projected["extension"], []byte("untouched public metadata")) {
				t.Fatal("unrelated extension changed")
			}
		})
	}
}

func TestIndependentStructuredFlatDiagnosticSuccessUntouched(t *testing.T) {
	format := structuredStreamFormat(t)
	item := structuredStreamCall(format, "fc_success", "call_success", `{"value":"OK"}`)
	response := map[string]any{"id": "resp_success", "status": "completed", "output": []any{item}}
	event := map[string]any{"type": "response.completed", "response": response}
	for _, field := range []string{"code", "message", "param", "detail"} {
		event[field] = "public success " + format.ToolName
		response[field] = "public success " + format.ToolName
	}
	var output bytes.Buffer
	commits := 0
	result, err := StreamStructured(context.Background(), &output, strings.NewReader(structuredStreamFrame(event)), StreamOptions{OnComplete: func(StreamResult) error { commits++; return nil }}, format)
	if err != nil || !result.Completed || commits != 1 {
		t.Fatalf("success changed: %v", err)
	}
	var terminal map[string]json.RawMessage
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &terminal)
		}
	}
	var projectedResponse map[string]json.RawMessage
	_ = json.Unmarshal(terminal["response"], &projectedResponse)
	for _, field := range []string{"code", "message", "param", "detail"} {
		for _, scope := range []map[string]json.RawMessage{terminal, projectedResponse} {
			var value string
			_ = json.Unmarshal(scope[field], &value)
			if value != event[field] {
				t.Fatalf("successful %s rewritten", field)
			}
		}
	}
}

func TestIndependentStructuredFailedResponseDiagnosticFields(t *testing.T) {
	format := structuredStreamFormat(t)
	for _, kind := range []string{"response.failed", "response.incomplete", "response.cancelled", "basispoints.response.cancelled"} {
		t.Run(kind, func(t *testing.T) {
			response := map[string]any{"id": "resp_failed_diagnostic", "output": []any{}, "usage": map[string]int{"input_tokens": 31, "output_tokens": 2}, "error": map[string]any{"code": "server_error", "message": "ordinary message"}, "extension": map[string]any{"ordinal": json.RawMessage(`9007199254740993`), "label": "unchanged"}}
			// A failed terminal need not have a response.status field. The terminal
			// outcome, not an optional status echo, selects diagnostic scrubbing.
			event := map[string]any{"type": kind, "response": response}
			for _, field := range []string{"code", "message", "param", "detail"} {
				response[field] = "before " + format.ToolName + " after"
				event[field] = "before " + format.ToolName + " after"
			}
			var output bytes.Buffer
			commits := 0
			result, err := StreamStructured(context.Background(), &output, strings.NewReader(structuredStreamFrame(event)), StreamOptions{OnComplete: func(StreamResult) error { commits++; return nil }}, format)
			var terminal *TerminalError
			if !errors.As(err, &terminal) || result.Completed || commits != 0 || terminal.Type != kind || terminal.Code != "server_error" {
				t.Fatal("failed outcome changed")
			}
			if bytes.Contains(result.Response, []byte(format.ToolName)) || bytes.Contains(terminal.Payload, []byte(format.ToolName)) || bytes.Contains(output.Bytes(), []byte(format.ToolName)) {
				t.Fatal("root or nested response diagnostic leaks formatter")
			}
			var projected map[string]json.RawMessage
			_ = json.Unmarshal(result.Response, &projected)
			for _, field := range []string{"code", "message", "param", "detail"} {
				var value string
				_ = json.Unmarshal(projected[field], &value)
				if value != "before structured output after" {
					t.Fatalf("response %s changed beyond formatter", field)
				}
			}
			if !bytes.Contains(projected["extension"], []byte("9007199254740993")) || !bytes.Contains(result.Usage, []byte(`"input_tokens":31`)) {
				t.Fatal("extension or usage changed")
			}
		})
	}
}
