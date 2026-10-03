package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A 200 SSE response can still finish with a structured upstream error. Keep
// its allowlisted fields and a digest instead of losing them in a generic
// collector failure or retaining arbitrary error text in the report.
func qualityCaptureResponseFailure(row map[string]any, err error) {
	var upstream *upstreamResponseError
	if !errors.As(err, &upstream) {
		row["response_failure_kind"] = "stream_or_protocol_error"
		return
	}
	row["response_failure_kind"] = "structured_upstream_error"
	row["response_error"] = qualityErrorFields(upstream.Payload)
	row["error_payload_sha256"] = qualityDigest(upstream.Payload)
}

// Persist only structured error diagnostics. Model text, HTML, credentials and
// arbitrary upstream messages are deliberately not a status classifier.
func qualityErrorFields(body []byte) map[string]any {
	result := make(map[string]any)
	var root map[string]json.RawMessage
	if len(body) > 64<<10 || json.Unmarshal(body, &root) != nil {
		return result
	}
	if raw := root["error"]; len(raw) > 0 {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil && nested != nil {
			root = nested
		}
	}
	for _, name := range []string{"code", "type"} {
		var value string
		if json.Unmarshal(root[name], &value) != nil || value == "" || len(value) > 96 {
			continue
		}
		if strings.IndexFunc(value, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
		}) < 0 {
			result[name] = value
		}
	}
	for _, name := range []string{"resets_at", "reset_at", "retry_after_seconds"} {
		var value int64
		if json.Unmarshal(root[name], &value) == nil && value > 0 {
			result[name] = value
		}
	}
	return result
}

func qualityResponseHeaders(headers http.Header) map[string]string {
	result := make(map[string]string)
	for name, values := range headers {
		key := strings.ToLower(name)
		allowed := key == "openai-model" || key == "x-openai-model" || key == "cf-ray" || key == "server" || key == "retry-after"
		allowed = allowed || strings.HasPrefix(key, "x-") && (strings.HasSuffix(key, "-reset-at") || strings.HasSuffix(key, "-used-percent") || strings.HasSuffix(key, "-window-minutes"))
		if allowed && len(values) == 1 && len(values[0]) <= 256 && !strings.ContainsAny(values[0], "\r\n") {
			result[http.CanonicalHeaderKey(name)] = values[0]
		}
	}
	return result
}

func TestQualityStructuredFailureCapture(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"error":{"code":"usage_limit_reached","type":"rate_limit_error","resets_at":2000000000,"message":"secret","token":"secret"}}`, 3},
		{`{"usage":{"reasoning_tokens":429},"output":"429 21"}`, 0},
		{`{"error":{"code":"Bearer secret","type":"test@example.com","reset_at":-1}}`, 0},
		{`<html>429</html>`, 0},
		{`{"error":{"code":"rate_limit_exceeded","retry_after_seconds":"60"}}`, 1},
	} {
		if got := qualityErrorFields([]byte(tc.body)); len(got) != tc.want {
			t.Fatalf("structured fields=%v wanted=%d", got, tc.want)
		}
	}
	got := qualityResponseHeaders(http.Header{"Retry-After": {"13"}, "Set-Cookie": {"private"}, "X-Codex-Primary-Reset-At": {"2000000000"}, "X-Test-Used-Percent": {"90", "100"}})
	if len(got) != 2 || got["Retry-After"] != "13" {
		t.Fatal("response header allowlist changed")
	}
}

func TestQualitySSEFailureCaptureAfterHTTPAccepted(t *testing.T) {
	for _, tc := range []struct{ wire, wantCode string }{
		{"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\"}}\n\ndata: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"private diagnostic text\"}\n\n", "server_is_overloaded"},
		{"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"status\":\"failed\",\"error\":{\"code\":\"usage_limit_reached\",\"resets_at\":2000000000,\"message\":\"private diagnostic text\"}}}\n\n", "usage_limit_reached"},
		{"data: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"code\":\"server_is_overloaded\",\"message\":\"private diagnostic text\"}}\n\n", "server_is_overloaded"},
	} {
		_, err := collectResponse(strings.NewReader(tc.wire))
		if err == nil {
			t.Fatal("fixture unexpectedly completed")
		}
		row := map[string]any{"http_status": 200}
		qualityCaptureResponseFailure(row, err)
		fields, ok := row["response_error"].(map[string]any)
		if !ok || fields["code"] != tc.wantCode || row["http_status"] != 200 || row["response_failure_kind"] != "structured_upstream_error" {
			t.Fatalf("SSE failure lost its independently observed HTTP/code: %v", row)
		}
		encoded, _ := json.Marshal(row)
		if strings.Contains(string(encoded), "private diagnostic text") || len(row["error_payload_sha256"].(string)) != 64 {
			t.Fatal("error payload persistence boundary changed")
		}
	}
	_, err := collectResponse(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
	row := make(map[string]any)
	qualityCaptureResponseFailure(row, err)
	if row["response_failure_kind"] != "stream_or_protocol_error" || row["response_error"] != nil {
		t.Fatal("EOF was invented into an upstream error code")
	}
}
