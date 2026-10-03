package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCollabErrorBoundary20260928JSONFailureDetails(t *testing.T) {
	const detail = `{"code":"invalid_encrypted_content","message":"synthetic ciphertext fixture rejected","param":"input[0].encrypted_content"}`
	for _, streaming := range []bool{true, false} {
		name, stream := "json", "false"
		if streaming {
			name, stream = "sse", "true"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.Copy(io.Discard, r.Body)
				wire := "data: " + `{"type":"response.failed","response":{"id":"collab_failed","status":"failed","error":` + detail + `}}` + "\n\n"
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
				_, _ = io.WriteString(w, wire)
			})
			response := httptest.NewRecorder()
			p.gatewayBPS(response, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":"synthetic fixture","stream":`+stream+`}`))
			if calls != 1 {
				t.Fatalf("upstream calls=%d", calls)
			}
			for _, value := range []string{"invalid_encrypted_content", "synthetic ciphertext fixture rejected", "input[0].encrypted_content"} {
				if !strings.Contains(response.Body.String(), value) {
					t.Errorf("upstream error detail %q lost: HTTP %d %s", value, response.Code, response.Body.String())
				}
			}
			if !streaming {
				if response.Code != http.StatusBadGateway || response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Content-Length") != "" {
					t.Fatalf("invalid JSON error framing: status=%d headers=%v", response.Code, response.Header())
				}
				var result struct {
					Error struct{ Code, Message, Param string }
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Error.Code != "invalid_encrypted_content" || result.Error.Message != "synthetic ciphertext fixture rejected" || result.Error.Param != "input[0].encrypted_content" {
					t.Errorf("structured upstream failure lost: %+v", result.Error)
				}
			}
			record, ok := p.Metrics.LastFor("test")
			if !ok || record.Status != "error" || !strings.Contains(record.Error, "code=invalid_encrypted_content") {
				t.Fatalf("terminal log summary missing: %+v", record)
			}
			for _, value := range []string{"synthetic ciphertext fixture rejected", "input[0].encrypted_content"} {
				if strings.Contains(record.Error, value) {
					t.Fatalf("wire detail leaked into request log: %s", record.Error)
				}
			}
		})
	}
}

func TestCollabErrorBoundary20260928JSONTerminalCases(t *testing.T) {
	for _, tc := range []struct {
		name, event, expected string
	}{
		{
			name:     "flat-error",
			event:    `{"type":"error","code":"invalid_request_error","message":"PowerPoint synthetic-private-marker","param":null}`,
			expected: `{"code":"invalid_request_error","message":"upstream synthetic-private-marker","param":null}`,
		},
		{
			name:     "event-error-precedence-and-exact-numbers",
			event:    `{"type":"response.failed","error":{"code":"invalid_request_error","message":"PowerPoint synthetic-private-marker","param":null,"opaque":9007199254740993,"decimal":1.2500e+02,"nested":{"n":9007199254740993}},"response":{"id":"r","status":"failed","error":{"code":"server_error","message":"do not choose this detail"}}}`,
			expected: `{"code":"invalid_request_error","message":"upstream synthetic-private-marker","param":null,"opaque":9007199254740993,"decimal":1.2500e+02,"nested":{"n":9007199254740993}}`,
		},
		{
			name:     "invalid-event-error-falls-back-to-response",
			event:    `{"type":"response.failed","error":"invalid-shape","response":{"status":"failed","error":{"code":"future_error","message":"synthetic-private-marker","param":"input[0]","opaque":9007199254740993}}}`,
			expected: `{"code":"future_error","message":"synthetic-private-marker","param":"input[0]","opaque":9007199254740993}`,
		},
		{
			name:     "missing-code-and-param-not-invented",
			event:    `{"type":"response.failed","response":{"status":"failed","error":{"message":"synthetic-private-marker"}}}`,
			expected: `{"message":"synthetic-private-marker"}`,
		},
		{
			name:     "empty-error-object-preserved",
			event:    `{"type":"response.failed","error":{},"response":{"status":"failed","error":{"message":"do not choose this detail"}}}`,
			expected: `{}`,
		},
		{
			name:  "missing-error-remains-generic",
			event: `{"type":"response.failed","response":{"status":"failed"}}`,
		},
		{
			name:  "non-object-error-remains-generic",
			event: `{"type":"response.failed","error":[],"response":{"status":"failed","error":"synthetic-private-marker"}}`,
		},
		{
			name:  "incomplete-remains-generic",
			event: `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
		},
		{
			name:  "cancelled-remains-generic",
			event: `{"type":"response.cancelled","response":{"status":"cancelled"}}`,
		},
		{
			name:  "invalid-stream-remains-generic",
			event: `{"type":"response.failed"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+tc.event+"\n\n")
			})
			response := httptest.NewRecorder()
			p.gatewayBPS(response, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","input":"synthetic fixture","stream":false}`))
			if calls != 1 || response.Code != http.StatusBadGateway || response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected dispatch/error framing: calls=%d status=%d headers=%v", calls, response.Code, response.Header())
			}
			var result struct{ Error map[string]json.RawMessage }
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if tc.expected == "" {
				if string(result.Error["type"]) != `"proxy_error"` || len(result.Error) != 2 {
					t.Fatalf("generic fallback changed: %s", response.Body.String())
				}
			} else {
				var expected map[string]json.RawMessage
				if err := json.Unmarshal([]byte(tc.expected), &expected); err != nil {
					t.Fatal(err)
				}
				if len(result.Error) != len(expected) {
					t.Errorf("error fields changed: got=%s want=%s", response.Body.String(), tc.expected)
				}
				for field, value := range expected {
					if string(result.Error[field]) != string(value) {
						t.Errorf("field %s changed: got=%s want=%s", field, result.Error[field], value)
					}
				}
			}
			record, ok := p.Metrics.LastFor("test")
			if !ok || record.Status != "error" || strings.Contains(record.Error, "synthetic-private-marker") || strings.Contains(record.Error, "input[0]") || strings.Contains(record.Error, "PowerPoint") {
				t.Fatalf("wire payload entered log summary: %+v", record)
			}
		})
	}
}
