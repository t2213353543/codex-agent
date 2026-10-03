package bps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const rootCompactAccountingUsage = `"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":7}}`

func TestRootCompactMetadataSharedStrictAccountingView(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"legacy_success", `{"id":"c","output":[{"type":"compaction","encrypted_content":"PRIVATE"}],` + rootCompactAccountingUsage + `}`, true},
		{"failed_no_output", `{"id":"c","status":"failed","error":{"message":"PRIVATE"},` + rootCompactAccountingUsage + `}`, true},
		{"incomplete_array", `{"id":"c","status":"incomplete","output":[],` + rootCompactAccountingUsage + `}`, true},
		{"cancelled_array", `{"id":"c","status":"cancelled","output":[],` + rootCompactAccountingUsage + `}`, true},
		{"completed_missing_state", `{"id":"c","status":"completed","output":[],` + rootCompactAccountingUsage + `}`, true},
		{"legacy_error_with_array", `{"id":"c","output":[],"error":{"message":"PRIVATE"},` + rootCompactAccountingUsage + `}`, true},
		{"nested_response_ignored", `{"id":"c","model":"gpt-6-astra","output":[],"status":"failed","response":{"id":"FOREIGN","status":"completed","usage":{"input_tokens":999}},"private":"PRIVATE",` + rootCompactAccountingUsage + `}`, true},
		{"missing_id", `{"status":"failed","output":[],` + rootCompactAccountingUsage + `}`, false},
		{"number_id", `{"id":123,"status":"failed","output":[],` + rootCompactAccountingUsage + `}`, false},
		{"null_id", `{"id":null,"status":"failed","output":[],` + rootCompactAccountingUsage + `}`, false},
		{"null_status", `{"id":"c","status":null,"output":[],` + rootCompactAccountingUsage + `}`, false},
		{"string_error", `{"id":"c","status":"failed","output":[],"error":"PRIVATE",` + rootCompactAccountingUsage + `}`, false},
		{"empty_status", `{"id":"c","status":"","output":[],` + rootCompactAccountingUsage + `}`, false},
		{"in_progress_status", `{"id":"c","status":"in_progress","output":[],` + rootCompactAccountingUsage + `}`, false},
		{"incomplete_missing_output", `{"id":"c","status":"incomplete",` + rootCompactAccountingUsage + `}`, false},
		{"legacy_missing_output", `{"id":"c",` + rootCompactAccountingUsage + `}`, false},
		{"null_output", `{"id":"c","status":"failed","output":null,` + rootCompactAccountingUsage + `}`, false},
		{"output_nonobject", `{"id":"c","status":"failed","output":[42],` + rootCompactAccountingUsage + `}`, false},
		{"output_missing_type", `{"id":"c","status":"failed","output":[{}],` + rootCompactAccountingUsage + `}`, false},
		{"output_bad_type", `{"id":"c","status":"failed","output":[{"type":42}],` + rootCompactAccountingUsage + `}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := CompactResponseMetadata([]byte(tc.body))
			if (metadata != nil) != tc.valid {
				t.Fatalf("valid=%t metadata=%s", tc.valid, metadata)
			}
			if !tc.valid {
				return
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(metadata, &fields) != nil || len(fields) > 3 || fields["id"] == nil || fields["usage"] == nil {
				t.Fatal("metadata shape changed")
			}
			for key := range fields {
				if key != "id" && key != "model" && key != "usage" {
					t.Fatal("non-whitelisted field")
				}
			}
			if bytes.Contains(metadata, []byte("PRIVATE")) || bytes.Contains(metadata, []byte("FOREIGN")) || !bytes.Contains(metadata, []byte(`"input_tokens":100`)) || !bytes.Contains(metadata, []byte(`"cached_tokens":80`)) || !bytes.Contains(metadata, []byte(`"cache_write_tokens":7`)) {
				t.Fatal("private or nested values leaked / original counters changed")
			}
		})
	}
	for _, bad := range []string{`{"id":"c","output":[]} {}`, `null`, `[]`, "{\"id\":\"c\",\"output\":[],\"private\":\"\xff\"}"} {
		if CompactResponseMetadata([]byte(bad)) != nil {
			t.Fatal("invalid complete JSON document accepted")
		}
	}
}

func TestRootCompactMetadataRetainsCounterUnknownAndLargeInteger(t *testing.T) {
	metadata := CompactResponseMetadata([]byte(`{"id":"c","output":[],"status":"failed","usage":{"input_tokens":9007199254740993,"output_tokens":0,"input_tokens_details":{"cached_tokens":"PRIVATE","cache_write_tokens":0},"prompt_tokens_details":{"cached_tokens":77},"cached_tokens":55,"unknown":"PRIVATE"}}`))
	if !bytes.Contains(metadata, []byte(`"input_tokens":9007199254740993`)) || !bytes.Contains(metadata, []byte(`"cached_tokens":null`)) || !bytes.Contains(metadata, []byte(`"cache_write_tokens":0`)) || bytes.Contains(metadata, []byte("PRIVATE")) {
		t.Fatalf("precise values or present invalid sentinels lost: %s", metadata)
	}
	metadata = CompactResponseMetadata([]byte(`{"id":"c","output":[],"usage":{"input_tokens":"PRIVATE","prompt_tokens":88,"input_tokens_details":"PRIVATE","cached_tokens":22}}`))
	if !bytes.Contains(metadata, []byte(`"input_tokens":null`)) || !bytes.Contains(metadata, []byte(`"input_tokens_details":false`)) || bytes.Contains(metadata, []byte("PRIVATE")) {
		t.Fatal("invalid higher-priority fields were deleted")
	}
}

func TestRootCompactCandidateNeverCommitsWithoutCompactedState(t *testing.T) {
	for _, output := range []string{`[]`, `[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"PRIVATE"}]}]`, `[{"type":"compaction","encrypted_content":""}]`, `[{"type":"compaction","encrypted_content":null}]`, `[{"type":"compaction","encrypted_content":42}]`} {
		commits, calls := 0, 0
		var visible bytes.Buffer
		result, err := CompactJSONToSSE(context.Background(), &visible, strings.NewReader(`{"id":"c","status":"completed","output":`+output+`,`+rootCompactAccountingUsage+`}`), StreamOptions{OnComplete: func(StreamResult) error { commits++; return nil }, OnTerminalMetadata: func(raw []byte) {
			calls++
			if !bytes.Contains(raw, []byte(`"cached_tokens":80`)) || bytes.Contains(raw, []byte("PRIVATE")) {
				t.Error("accounting metadata lost or leaked output")
			}
		}})
		if !errors.Is(err, ErrInvalidEvent) || result.Completed || commits != 0 || calls != 1 || visible.Len() != 0 || len(result.Output) != 0 {
			t.Fatalf("invalid compaction became visible/reusable or lost observation: err=%v completed=%t calls=%d commits=%d", err, result.Completed, calls, commits)
		}
	}
}

func TestRootCompactCandidateKeepsLegacySuccessAndRealFailureMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, status, output     string
		success, terminal, early bool
	}{
		{"legacy_success", "", `[{"type":"compaction","encrypted_content":"ORIGINAL"}]`, true, false, false},
		{"multiple_compactions_existing_contract", "", `[{"type":"compaction","encrypted_content":"ONE"},{"type":"compaction","encrypted_content":"TWO"}]`, true, false, false},
		{"failed_empty", `"failed"`, `[]`, false, true, false},
		{"failed_output_absent", `"failed"`, "", false, false, true},
		{"incomplete_empty", `"incomplete"`, `[]`, false, true, false},
		{"cancelled_empty", `"cancelled"`, `[]`, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]json.RawMessage{"id": json.RawMessage(`"c"`), "usage": json.RawMessage(`{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":7}}`)}
			if tc.status != "" {
				fields["status"] = json.RawMessage(tc.status)
			}
			if tc.output != "" {
				fields["output"] = json.RawMessage(tc.output)
			}
			raw, _ := json.Marshal(fields)
			commits, calls := 0, 0
			var visible bytes.Buffer
			result, err := CompactJSONToSSE(context.Background(), &visible, bytes.NewReader(raw), StreamOptions{OnComplete: func(StreamResult) error { commits++; return nil }, OnTerminalMetadata: func(metadata []byte) {
				calls++
				if !bytes.Contains(metadata, []byte(`"cache_write_tokens":7`)) {
					t.Error("early real usage missing")
				}
			}})
			var terminal *TerminalError
			if result.Completed != tc.success || errors.As(err, &terminal) != tc.terminal || (commits == 1) != tc.success || (calls == 1) != tc.early {
				t.Fatalf("successful/failure boundary changed: result=%+v err=%v commits=%d callbacks=%d", result, err, commits, calls)
			}
			if !tc.early && (!bytes.Contains(result.Usage, []byte(`"cached_tokens":80`)) || !bytes.Contains(result.Usage, []byte(`"cache_write_tokens":7`))) {
				t.Fatal("real terminal usage lost")
			}
			if !tc.success && strings.Contains(visible.String(), `"type":"response.completed"`) {
				t.Fatal("failure became successful event")
			}
		})
	}
}

func TestRootCompactInvalidTerminalCannotExposeStateBeforeRejection(t *testing.T) {
	for _, tail := range []string{`,"status":"completed","error":{"message":"contradictory failure"}`, `,"status":"in_progress"`, `,"status":null`} {
		var visible bytes.Buffer
		commits := 0
		result, err := CompactJSONToSSE(context.Background(), &visible, strings.NewReader(`{"id":"compact_invalid_final","output":[{"type":"compaction","encrypted_content":"PRIVATE_INVALID_STATE"}]`+tail+`}`), StreamOptions{OnComplete: func(StreamResult) error { commits++; return nil }})
		if err == nil || result.Completed || commits != 0 || visible.Len() != 0 {
			t.Fatalf("invalid terminal exposed unusable compact state: success=%t commits=%d bytes=%d", result.Completed, commits, visible.Len())
		}
	}
}
