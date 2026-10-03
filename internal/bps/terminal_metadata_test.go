package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBPSTerminalMetadataPrePolicyBoundary(t *testing.T) {
	const output = `[{"type":"function_call","id":"fc_fixture","call_id":"c_fixture","name":"PRIVATE_TOOL","arguments":"PRIVATE_PAYLOAD"}]`
	const usage = `{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":0,"private":"PRIVATE_USAGE"}}`
	for _, test := range []struct {
		name, prefix, kind, fields, response string
		observed                             bool
	}{
		{"valid_rejected", "", "response.completed", "", `{"id":"same","status":"completed","model":"gpt-6-astra","output":` + output + `,"usage":` + usage + `}`, true},
		{"nested_id_changed", `data: {"type":"response.created","response":{"id":"original"}}` + "\n\n", "response.completed", "", `{"id":"foreign","status":"completed","output":` + output + `,"usage":` + usage + `}`, false},
		{"outer_id_changed", "", "response.completed", `,"response_id":"foreign"`, `{"id":"same","status":"completed","output":` + output + `,"usage":` + usage + `}`, false},
		{"nonterminal", "", "response.in_progress", "", `{"id":"same","status":"in_progress","output":` + output + `,"usage":` + usage + `}`, false},
		{"wrong_status", "", "response.completed", "", `{"id":"same","status":"failed","output":` + output + `,"usage":` + usage + `}`, false},
		{"completion_error", "", "response.completed", "", `{"id":"same","status":"completed","error":{"code":"bad"},"output":` + output + `,"usage":` + usage + `}`, false},
		{"missing_id", "", "response.completed", "", `{"status":"completed","output":` + output + `,"usage":` + usage + `}`, false},
		{"null_output", "", "response.completed", "", `{"id":"same","status":"completed","output":null,"usage":` + usage + `}`, false},
		{"invalid_item_schema", "", "response.completed", "", `{"id":"same","status":"completed","output":[42],"usage":` + usage + `}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var captured []byte
			calls := 0
			denied := errors.New("synthetic rejected tool")
			var visible bytes.Buffer
			result, err := Stream(context.Background(), &visible, strings.NewReader(test.prefix+`data: {"type":"`+test.kind+`"`+test.fields+`,"response":`+test.response+`}`+"\n\n"), StreamOptions{Unbounded: true, NormalizeItem: func(map[string]json.RawMessage) error { return denied }, OnTerminalMetadata: func(metadata []byte) { calls++; captured = bytes.Clone(metadata) }})
			if err == nil || result.Completed || len(result.Response) > 0 || bytes.Contains(visible.Bytes(), []byte("PRIVATE")) {
				t.Fatal("invalid output became complete, reusable or visible")
			}
			if (calls == 1) != test.observed {
				t.Fatalf("metadata observation calls=%d wanted=%t", calls, test.observed)
			}
			if test.observed {
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(captured, &fields)
				if fields["usage"] == nil || fields["model"] == nil || fields["id"] == nil || len(fields) != 3 || bytes.Contains(captured, []byte("PRIVATE")) {
					t.Fatal("early metadata contains executable/private fields or lost usage")
				}
				if !bytes.Contains(captured, []byte(`"cache_write_tokens":0`)) {
					t.Fatal("measured zero cache creation missing")
				}
			}
		})
	}
}

func TestBPSStructuredTerminalMetadataDoesNotEnterProjection(t *testing.T) {
	format, err := ParseStructuredFormat([]byte(`{"format":{"type":"json_object"}}`))
	if err != nil {
		t.Fatal(err)
	}
	observed := 0
	wire := `data: {"type":"response.completed","response":{"id":"same","status":"completed","model":"gpt-6-astra","output":[{"type":"function_call","name":"PRIVATE_TOOL","call_id":"c","arguments":"PRIVATE_PAYLOAD"}],"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80}}}}` + "\n\n"
	var visible bytes.Buffer
	result, err := StreamStructured(context.Background(), &visible, strings.NewReader(wire), StreamOptions{Unbounded: true, NormalizeItem: func(map[string]json.RawMessage) error { return errors.New("synthetic policy rejected") }, OnTerminalMetadata: func(raw []byte) {
		observed++
		if bytes.Contains(raw, []byte("PRIVATE")) {
			t.Fatal("private terminal content leaked through metadata")
		}
	}}, format)
	if err == nil || observed != 1 || result.Completed || len(result.Response) > 0 || len(result.Output) > 0 || visible.Len() != 0 {
		t.Fatal("metadata callback altered structured public/private result boundary")
	}
}

func TestBPSTerminalMetadataNoDoubleObservationOrDrain(t *testing.T) {
	valid := `data: {"type":"response.completed","response":{"id":"same","status":"completed","output":[],"usage":{"input_tokens":100,"output_tokens":0}}}` + "\n\n"
	failed := `data: {"type":"response.failed","response":{"id":"same","status":"failed","output":[],"error":{"code":"rate_limit_exceeded"},"usage":{"input_tokens":100,"output_tokens":0}}}` + "\n\n"
	for _, wire := range []string{valid, failed} {
		calls := 0
		var output bytes.Buffer
		result, _ := Stream(context.Background(), &output, strings.NewReader(wire), StreamOptions{OnTerminalMetadata: func([]byte) { calls++ }})
		if calls != 0 || len(result.Usage) == 0 || len(result.Response) == 0 {
			t.Fatal("normally accepted terminal was observed twice or lost ordinary accounting")
		}
	}
	calls := 0
	var output bytes.Buffer
	early := `data: {"type":"response.output_item.added","item":{"type":"function_call","name":"PRIVATE_TOOL"}}` + "\n\n"
	result, err := Stream(context.Background(), &output, strings.NewReader(early+valid), StreamOptions{NormalizeItem: func(map[string]json.RawMessage) error { return errors.New("rejected early") }, OnTerminalMetadata: func([]byte) { calls++ }})
	if err == nil || calls != 0 || len(result.Response) != 0 || len(result.Usage) != 0 || output.Len() != 0 {
		t.Fatal("early rejected output drained later terminal or fabricated usage")
	}
}
