package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStreamTerminalErrorKeepsMachineCodeWithoutLoggingPayload(t *testing.T) {
	for _, tc := range []struct{ name, event, code, status string }{
		{"flat", `{"type":"error","code":"serverOverloaded","message":"PowerPoint private-upstream-message Bearer sk-test-private-token"}`, "serverOverloaded", "failed"},
		{"nested-event", `{"type":"error","error":{"code":"invalid_encrypted_content","message":"PowerPoint private-upstream-message"}}`, "invalid_encrypted_content", "failed"},
		{"nested-response", `{"type":"response.failed","response":{"id":"resp_failure","status":"failed","output":[],"error":{"code":"context_length_exceeded","message":"PowerPoint private-upstream-message"}}}`, "context_length_exceeded", "failed"},
		{"credential-code", `{"type":"error","code":"sk-test-private-token","message":"private-upstream-message"}`, "", "failed"},
		{"unknown-code", `{"type":"error","code":"unknown-private-diagnostic","message":"private-upstream-message"}`, "", "failed"},
		{"unknown-outer-does-not-become-busy", `{"type":"error","code":"serverOverloaded","error":{"code":"unknown-private-diagnostic","message":"private-upstream-message"},"response":{"id":"resp_failure","status":"failed","output":[],"error":{"code":"serverOverloaded"}}}`, "", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			result, err := Stream(context.Background(), &output, strings.NewReader(streamTestFrame(tc.event)), StreamOptions{})
			var terminal *TerminalError
			if !errors.As(err, &terminal) || result.Completed || terminal.Code != tc.code {
				t.Fatalf("terminal classification lost: result=%+v error=%v", result, err)
			}
			if !strings.Contains(err.Error(), "status "+tc.status) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "PowerPoint") || strings.Contains(err.Error(), "Bearer") || strings.Contains(err.Error(), "sk-") {
				t.Fatalf("unsafe or missing log diagnostic: %v", err)
			}
			if tc.code != "" && !strings.Contains(err.Error(), "code="+tc.code) {
				t.Fatalf("machine code absent from diagnostic: %v", err)
			}
			if strings.Contains(output.String(), "PowerPoint") || bytes.Count(output.Bytes(), []byte("data: ")) != 1 {
				t.Fatalf("original failure duplicated or unsanitized: %s", output.String())
			}
			if !bytes.Contains(terminal.Payload, []byte("private-upstream-message")) {
				t.Fatal("internal event payload was destroyed")
			}
		})
	}
}

func TestStreamErrorBecomesFailedTerminalWithoutInventingResponseMetadata(t *testing.T) {
	var output bytes.Buffer
	upstream := `{"type":"error","code":"rate_limit_exceeded","message":"Try later","headers":{"retry-after":"7"},"unknown":9007199254740993}`
	result, err := Stream(context.Background(), &output, strings.NewReader("event: error\ndata: "+upstream+"\n\n"), StreamOptions{})
	var terminal *TerminalError
	if !errors.As(err, &terminal) || result.Completed || result.TerminalType != "response.failed" || terminal.Code != "rate_limit_exceeded" {
		t.Fatalf("error not mapped to failed terminal: %+v %v", result, err)
	}
	events := streamTestEvents(t, output.String())
	if len(events) != 1 || !strings.HasPrefix(output.String(), "event: response.failed\n") {
		t.Fatalf("SSE event envelope disagrees with terminal: %s", output.String())
	}
	var response, detail map[string]json.RawMessage
	if json.Unmarshal(events[0]["response"], &response) != nil || json.Unmarshal(response["error"], &detail) != nil {
		t.Fatal("failed response error missing")
	}
	for _, field := range []string{"id", "model", "usage", "output"} {
		if _, present := response[field]; present {
			t.Fatalf("fabricated upstream response field: %s", field)
		}
	}
	if string(detail["code"]) != `"rate_limit_exceeded"` || string(detail["message"]) != `"Try later"` || string(events[0]["unknown"]) != "9007199254740993" || !bytes.Contains(events[0]["headers"], []byte("retry-after")) {
		t.Fatal("upstream failure semantics changed")
	}
}
