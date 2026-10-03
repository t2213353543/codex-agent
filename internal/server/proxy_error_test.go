package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUpstreamStreamErrorKeepsDiagnostic(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Try again later."}}`,
		`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Try again later."}}}`,
	} {
		_, err := collectResponse(strings.NewReader("data: " + event + "\n\n"))
		var upstream *upstreamResponseError
		if !errors.As(err, &upstream) {
			t.Fatalf("expected diagnostic error, got %v", err)
		}
		var payload map[string]string
		if err := json.Unmarshal(upstream.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["code"] != "server_is_overloaded" || payload["message"] != "Try again later." {
			t.Fatalf("diagnostic lost: %s", upstream.Payload)
		}
	}
}
