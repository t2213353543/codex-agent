package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-agent/internal/bps"
)

// Pure serializer contract; the independent reviewer owns the full gateway
// JSON/SSE/history matrix. No upstream or runtime handler is invoked here.
func TestBPSStructuredMixedRecordAndPublicFailureSerialization(t *testing.T) {
	format, err := bps.ParseStructuredFormat(json.RawMessage(`{"format":{"type":"json_object"}}`))
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{"type": "function_call", "name": format.ToolName, "id": "formatter", "call_id": "call", "arguments": "{}", "status": "completed"}
	message := map[string]any{"type": "message", "id": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "private_answer"}}}
	terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "real-id", "model": "real-model", "status": "completed", "output": []any{item, message}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 7, "input_tokens_details": map[string]int{"cached_tokens": 80, "cache_write_tokens": 20}}}})
	var visible bytes.Buffer
	result, typed := bps.StreamStructured(t.Context(), &visible, strings.NewReader("data: "+string(terminal)+"\n\n"), bps.StreamOptions{}, format)
	observation := bps.ObserveStructuredMixed(typed)
	if typed == nil || observation == nil || visible.Len() != 0 {
		t.Fatal("typed mixed fixture not produced")
	}
	old := errors.New("structured output mixed incompatible final outputs")
	newPayload := bpsStructuredValidationFailure(result, typed)
	oldPayload := bpsStructuredValidationFailure(result, old)
	if !bytes.Equal(newPayload, oldPayload) {
		t.Fatalf("public SSE payload changed: new=%s old=%s", newPayload, oldPayload)
	}
	one, two := httptest.NewRecorder(), httptest.NewRecorder()
	writeBPSStructuredValidationJSON(one, newPayload)
	writeBPSStructuredValidationJSON(two, oldPayload)
	if one.Code != two.Code || one.Header().Get("Content-Type") != two.Header().Get("Content-Type") || !bytes.Equal(one.Body.Bytes(), two.Body.Bytes()) {
		t.Fatal("public JSON error serialization changed")
	}
	for _, forbidden := range []string{"structured_mixed", "formatter_count", format.ToolName, "private_answer"} {
		if bytes.Contains(newPayload, []byte(forbidden)) {
			t.Fatalf("private metadata/content entered public error: %s", forbidden)
		}
	}
	record := RequestRecord{Status: "error", StructuredMixed: observation}
	encoded, _ := json.Marshal(record)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	var details map[string]json.RawMessage
	_ = json.Unmarshal(fields["structured_mixed"], &details)
	if len(details) != 5 || string(details["formatter_count"]) != "1" || string(details["client_call_count"]) != "0" || string(details["other_call_count"]) != "0" || string(details["has_text"]) != "true" || string(details["has_refusal"]) != "false" {
		t.Fatalf("record shape changed/zero omitted: %s", encoded)
	}
	oldRecord, _ := json.Marshal(RequestRecord{Status: "error"})
	if bytes.Contains(oldRecord, []byte("structured_mixed")) {
		t.Fatal("unobserved old record acquired default metadata")
	}
}
