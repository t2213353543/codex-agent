package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestIndependentTerminalExistingUsageLimitCodeIsObserved(t *testing.T) {
	const event = `{"type":"response.failed","response":{"id":"r_usage","status":"failed","output":[],"error":{"code":"usage_limit_reached","type":"rate_limit_error","message":"PowerPoint synthetic request limit"}}}`
	var public bytes.Buffer
	result, err := Stream(context.Background(), &public, strings.NewReader("data: "+event+"\n\n"), StreamOptions{})
	var terminal *TerminalError
	if !errors.As(err, &terminal) || result.Completed || terminal.Code != "usage_limit_reached" || !strings.Contains(terminal.Error(), "code=usage_limit_reached") {
		t.Fatal("existing recognized server quota code omitted from safe terminal diagnostic")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(terminal.Payload, &fields)
	var response struct {
		Error struct{ Code, Type, Message string }
	}
	if json.Unmarshal(fields["response"], &response) != nil || response.Error.Code != "usage_limit_reached" || response.Error.Type != "rate_limit_error" || response.Error.Message != "upstream synthetic request limit" {
		t.Fatal("observing known code changed original error semantics")
	}
}

func TestIndependentFailedTerminalFreeformTextOmitProvider(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete", "response.cancelled", "basispoints.response.cancelled"} {
		t.Run(kind, func(t *testing.T) {
			status := strings.TrimPrefix(kind, "response.")
			if kind == "basispoints.response.cancelled" {
				status = "cancelled"
			}
			fields := map[string]any{"type": kind, "code": "event_machine_code", "message": "PowerPoint synthetic root message", "detail": "PowerPoint synthetic root detail", "response": map[string]any{"id": "r_text", "status": status, "code": "response_machine_code", "message": "PowerPoint synthetic response message", "detail": "PowerPoint synthetic response detail", "output": []any{}, "error": map[string]any{"code": "server_error", "type": "server_error", "message": "PowerPoint synthetic nested message"}}}
			raw, _ := json.Marshal(fields)
			payload, terminal, err := NewTransformer(StreamOptions{}).Transform(raw)
			if err == nil || !terminal || bytes.Contains(bytes.ToLower(payload), []byte("powerpoint")) {
				t.Fatal("provider text remains in accepted failed terminal free-text fields")
			}
			var emitted map[string]json.RawMessage
			_ = json.Unmarshal(payload, &emitted)
			var response map[string]json.RawMessage
			_ = json.Unmarshal(emitted["response"], &response)
			if string(emitted["code"]) != `"event_machine_code"` || string(response["code"]) != `"response_machine_code"` {
				t.Fatal("sanitizing free text changed machine identifiers")
			}
		})
	}
	const success = `{"type":"response.completed","message":"PowerPoint successful metadata","response":{"id":"r_ok","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PowerPoint successful answer"}]}]}}`
	payload, terminal, err := NewTransformer(StreamOptions{}).Transform([]byte(success))
	if err != nil || !terminal || !bytes.Contains(payload, []byte("PowerPoint successful answer")) || !bytes.Contains(payload, []byte("PowerPoint successful metadata")) {
		t.Fatal("failure-only sanitizer changed successful content")
	}
}
