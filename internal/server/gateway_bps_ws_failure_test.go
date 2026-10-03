package server

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBPSWebSocketRateFailureUsesOfficialHTTPErrorEnvelope(t *testing.T) {
	raw := []byte(`{"type":"response.failed","response":{"id":"observed","status":"failed","error":{"code":"rate_limit_exceeded","message":"Try later"}},"headers":{"retry-after":"5"},"unknown":9007199254740993}`)
	out := bpsWebSocketFailurePayload(raw)
	var fields map[string]json.RawMessage
	if json.Unmarshal(out, &fields) != nil || string(fields["type"]) != `"error"` || string(fields["status"]) != "429" || !bytes.Contains(fields["error"], []byte("Try later")) || string(fields["unknown"]) != "9007199254740993" || !bytes.Contains(fields["headers"], []byte("retry-after")) {
		t.Fatalf("rate-limit semantics lost: %s", out)
	}
	for _, unchanged := range []string{
		`{"type":"response.failed","response":{"error":{"code":"unknown_private"}}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
		`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}},"status":200}`,
	} {
		if got := bpsWebSocketFailurePayload([]byte(unchanged)); string(got) != unchanged {
			t.Fatal("unrelated event was rewritten")
		}
	}
}
