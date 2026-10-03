package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestIndependentTerminalErrorTypeSeparateFromCode(t *testing.T) {
	for _, kind := range []string{"server_error", "invalid_request_error", "rate_limit_error", "authentication_error"} {
		for _, code := range []string{"", "context_length_exceeded"} {
			t.Run(kind+"/"+code, func(t *testing.T) {
				var codeValue any
				if code != "" {
					codeValue = code
				}
				frame, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"id": "resp_type", "status": "failed", "output": []any{}, "error": map[string]any{"code": codeValue, "type": kind, "message": "PowerPoint PRIVATE_MESSAGE sk-PRIVATE_CREDENTIAL"}}})
				var output bytes.Buffer
				result, err := Stream(context.Background(), &output, strings.NewReader("data: "+string(frame)+"\n\n"), StreamOptions{})
				var terminal *TerminalError
				if !errors.As(err, &terminal) || terminal.Code != code || result.Completed {
					t.Fatal("error type changed code/completion")
				}
				if !strings.Contains(terminal.Error(), "(error_type="+kind+")") {
					t.Fatal("recognized type missing from safe terminal diagnostic")
				}
				field := reflect.ValueOf(terminal).Elem().FieldByName("ErrorType")
				if !field.IsValid() || field.Kind() != reflect.String || field.String() != kind {
					t.Fatal("independent typed error field missing")
				}
				for _, secret := range []string{"PRIVATE", "sk-", "PowerPoint"} {
					if strings.Contains(terminal.Error(), secret) {
						t.Fatal("raw diagnostic content entered safe Error()")
					}
				}
				var event struct {
					Response struct{ Error map[string]json.RawMessage }
				}
				if json.Unmarshal(terminal.Payload, &event) != nil || string(event.Response.Error["type"]) != `"`+kind+`"` {
					t.Fatal("original native type was replaced")
				}
				if code == "" && string(event.Response.Error["code"]) != "null" {
					t.Fatal("type was promoted to a native code")
				}
			})
		}
	}
}

func TestIndependentTerminalErrorTypePrecedenceAndUnknownOmission(t *testing.T) {
	for _, tc := range []struct{ name, outer, nested, want string }{
		{"nested", `null`, `{"type":"server_error","code":null}`, "server_error"},
		{"event_first", `{"type":"invalid_request_error","code":null}`, `{"type":"server_error","code":"server_error"}`, "invalid_request_error"},
		{"unknown_outer_stops_fallback", `{"type":"PRIVATE_SECRET","code":null}`, `{"type":"server_error","code":"server_error"}`, ""},
		{"empty_outer_stops_fallback", `{}`, `{"type":"server_error","code":"server_error"}`, ""},
		{"absent_type_not_code", `null`, `{"code":"server_error"}`, ""},
		{"null_type", `null`, `{"type":null}`, ""},
		{"array_type", `null`, `{"type":["server_error"]}`, ""},
		{"object_type", `null`, `{"type":{"private":"server_error"}}`, ""},
		{"case_sensitive", `null`, `{"type":"SERVER_ERROR"}`, ""},
		{"nonobject_outer_uses_nested", `"PRIVATE_MESSAGE"`, `{"type":"server_error","code":null}`, "server_error"},
		{"list_error_no_guess", `null`, `[{"type":"server_error"}]`, ""},
		{"message_no_guess", `null`, `{"message":"type=server_error"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"type":"response.failed","error":` + tc.outer + `,"response":{"id":"resp_precedence","status":"failed","output":[],"error":` + tc.nested + `}}`
			_, err := Stream(context.Background(), &bytes.Buffer{}, strings.NewReader("data: "+frame+"\n\n"), StreamOptions{})
			var terminal *TerminalError
			if !errors.As(err, &terminal) {
				t.Fatal("fixture did not fail")
			}
			if tc.want == "" {
				if strings.Contains(terminal.Error(), "error_type=") {
					t.Fatal("unknown/absent type guessed from other data")
				}
			} else if !strings.Contains(terminal.Error(), "(error_type="+tc.want+")") {
				t.Fatal("error object precedence changed")
			}
			if strings.Contains(terminal.Error(), "PRIVATE") {
				t.Fatal("private type/message leaked")
			}
		})
	}
}

func TestIndependentTerminalErrorTypeGuardAndStructuredCopy(t *testing.T) {
	manual := &TerminalError{Type: "response.failed", Status: "failed"}
	field := reflect.ValueOf(manual).Elem().FieldByName("ErrorType")
	if !field.IsValid() || !field.CanSet() {
		t.Fatal("independent type field unavailable")
	}
	field.SetString("PRIVATE_SECRET")
	if strings.Contains(manual.Error(), "PRIVATE") || strings.Contains(manual.Error(), "error_type=") {
		t.Fatal("manually assigned untrusted type bypassed final allowlist")
	}
	format, err := ParseStructuredFormat([]byte(`{"format":{"type":"json_object"}}`))
	if err != nil {
		t.Fatal(err)
	}
	frame := `{"type":"response.failed","response":{"id":"resp_structured_type","status":"failed","output":[],"error":{"code":null,"type":"server_error","message":"PowerPoint synthetic failure"}}}`
	var output bytes.Buffer
	result, err := StreamStructured(context.Background(), &output, strings.NewReader("data: "+frame+"\n\n"), StreamOptions{}, format)
	var terminal *TerminalError
	if !errors.As(err, &terminal) || result.Completed || terminal.Code != "" || !strings.Contains(terminal.Error(), "(error_type=server_error)") {
		t.Fatal("structured copy lost error type or invented code")
	}
	if strings.Contains(output.String(), "error_type") || bytes.Contains(bytes.ToLower(output.Bytes()), []byte("powerpoint")) {
		t.Fatal("internal metadata changed public protocol or unsanitized payload")
	}
	flat := `{"type":"error","code":"server_error","message":"private fixture"}`
	_, err = Stream(context.Background(), &bytes.Buffer{}, strings.NewReader("data: "+flat+"\n\n"), StreamOptions{})
	if !errors.As(err, &terminal) || terminal.Code != "server_error" || strings.Contains(terminal.Error(), "error_type=") {
		t.Fatal(fmt.Sprint("top-level event type error was treated as response error type: ", err))
	}
}
