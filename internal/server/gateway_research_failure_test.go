package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// HTTP 200 is already visible before this late failure arrives. The raw SSE
// diagnostic may still be forwarded, but an explicitly foreign/conflicting
// response ID must not install scheduling state for the accepted response.
func TestResearchFailureHTTPForeignEventCannotPoisonCooldown(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		t.Run(transport, func(t *testing.T) {
			if transport == "native" && (runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "") {
				t.Skip("Linux and explicit real Rust helper required")
			}
			for _, scenario := range []struct {
				name, failure string
				limited       bool
			}{
				{"foreign_nested_response", `{"type":"response.failed","headers":{"Retry-After":"60"},"response":{"id":"foreign-response","status":"failed","error":{"code":"rate_limit_exceeded","message":"foreign quota diagnostic"},"usage":{"input_tokens":999,"output_tokens":999}}}`, false},
				{"conflicting_top_and_nested", `{"type":"response.failed","response_id":"accepted-response","headers":{"Retry-After":"60"},"response":{"id":"foreign-response","status":"failed","error":{"code":"rate_limit_exceeded","message":"conflicting quota diagnostic"}}}`, false},
				{"foreign_top_level_error", `{"type":"error","response_id":"foreign-response","code":"rate_limit_exceeded","message":"foreign top-level quota diagnostic","headers":{"Retry-After":"60"}}`, false},
				{"same_response_control", `{"type":"response.failed","headers":{"Retry-After":"60"},"response":{"id":"accepted-response","status":"failed","error":{"code":"rate_limit_exceeded","message":"current response quota diagnostic"}}}`, true},
				{"anonymous_error_control", `{"type":"error","code":"rate_limit_exceeded","message":"current request quota diagnostic","headers":{"Retry-After":"60"}}`, true},
				{"anonymous_usage_limit_control", `{"type":"error","error":{"code":"usage_limit_reached","message":"legacy usage limit"},"headers":{"Retry-After":"60"}}`, true},
			} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%v", scenario.name, stream), func(t *testing.T) {
						var upstreamRequests atomic.Int32
						const created = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"accepted-response\",\"status\":\"in_progress\"}}\n\n"
						wire := created + "data: " + scenario.failure + "\n\n"
						p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
							_, _ = io.Copy(io.Discard, r.Body)
							w.Header().Set("Content-Type", "text/event-stream")
							if upstreamRequests.Add(1) == 1 {
								_, _ = io.WriteString(w, created)
								w.(http.Flusher).Flush()
								_, _ = io.WriteString(w, "data: "+scenario.failure+"\n\n")
								return
							}
							_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"independent-response\",\"status\":\"completed\",\"output\":[]}}\n\n")
						})
						if transport == "native" {
							realGatewayNativePool(t, p)
						}
						done := make(chan struct{}, 2)
						gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							defer func() { done <- struct{}{} }()
							p.ServeHTTP(w, r)
						}))
						defer gateway.Close()
						ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
						defer cancel()
						send := func() (int, []byte) {
							t.Helper()
							r, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/responses", strings.NewReader(fmt.Sprintf(`{"model":"research-model","stream":%v,"input":[]}`, stream)))
							if err != nil {
								t.Fatal(err)
							}
							r.Header.Set("Authorization", "Bearer test-client-key")
							res, err := gateway.Client().Do(r)
							if err != nil {
								t.Fatal(err)
							}
							body, err := io.ReadAll(res.Body)
							_ = res.Body.Close()
							if err != nil {
								t.Fatal(err)
							}
							select {
							case <-done:
							case <-ctx.Done():
								t.Fatal("gateway loopback handler did not finish")
							}
							return res.StatusCode, body
						}
						status, body := send()
						if stream && (status != http.StatusOK || string(body) != wire) {
							t.Fatal("late error changed already-accepted status or raw SSE bytes")
						}
						if !stream && status != http.StatusBadGateway {
							t.Fatalf("JSON adapter did not retain its existing failed-event status: %d", status)
						}
						first, ok := p.Metrics.LastFor("test")
						if !ok || first.ResponseID != "accepted-response" || first.InputTokens != 0 || first.OutputTokens != 0 {
							t.Fatal("foreign failure changed current response identity or usage")
						}
						status, _ = send()
						if scenario.limited {
							if status != http.StatusTooManyRequests || upstreamRequests.Load() != 1 {
								t.Fatal("current-response or anonymous error stopped enforcing ordinary cooldown")
							}
						} else if status != http.StatusOK || upstreamRequests.Load() != 2 {
							t.Errorf("foreign/conflicting SSE failure installed a cooldown for the current account: next_status=%d actual_upstream_requests=%d", status, upstreamRequests.Load())
						}
					})
				}
			}
		})
	}
}

func TestResearchFailureHTTPCooldownExactIdentityAndAnonymousCompatibility(t *testing.T) {
	event := func(topID, nestedID string) []byte {
		raw, err := json.Marshal(map[string]any{"type": "response.failed", "response_id": topID, "response": map[string]any{"id": nestedID}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	longPrefix := strings.Repeat("identical-prefix", 100)
	for _, original := range []string{"simple-response", " response with spaces ", longPrefix + "A", "合法响应🧪"} {
		t.Run(fmt.Sprintf("id_bytes=%d", len(original)), func(t *testing.T) {
			var identity gatewayHTTPCooldownIdentity
			if !identity.accepts(event("", original)) || !identity.accepts(event(original, "")) || !identity.accepts(event(original, original)) {
				t.Fatal("same response identity was rejected")
			}
			before := identity
			for _, foreign := range []string{"different", original + "B", strings.TrimSpace(original) + "extra"} {
				if identity.accepts(event("", foreign)) || identity.accepts(event(foreign, "")) || identity.accepts(event(original, foreign)) || identity.accepts(event(foreign, original)) {
					t.Fatal("foreign or contradictory exact identity was accepted")
				}
			}
			for _, anonymous := range []string{`{"type":"error","code":"usage_limit_reached"}`, `{"type":"error","response":null,"error":{"code":"rate_limit_exceeded"}}`} {
				if !identity.accepts([]byte(anonymous)) {
					t.Fatal("legacy anonymous quota event was incorrectly dropped")
				}
			}
			if identity != before {
				t.Fatal("rejected or anonymous event replaced the accepted response's identity")
			}
		})
	}
	var unknown gatewayHTTPCooldownIdentity
	for _, invalid := range [][]byte{event("one", "two"), []byte(`{"response_id":7}`), []byte(`{"response":{"id":{}}}`), []byte(`{broken`)} {
		if unknown.accepts(invalid) || unknown.known {
			t.Fatal("untrusted conflicting/invalid event established a response identity")
		}
	}
	// A failed response may be the first event: no preceding created is needed
	// to identify its structured quota error as belonging to this HTTP request.
	if !unknown.accepts(event("first-failure", "")) || !unknown.known {
		t.Fatal("first identified failure stopped being eligible for cooldown")
	}
}

func TestResearchFailureCooldownProtocolKeysAreCaseSensitive(t *testing.T) {
	for _, scenario := range []struct {
		name, payload, streamID, responseID string
		limited                             bool
	}{
		{"type_shadow_cannot_create_error", `{"type":"response.created","Type":"response.failed","error":{"code":"rate_limit_exceeded"}}`, "", "", false},
		{"uppercase_only_is_not_protocol_type", `{"Type":"response.failed","error":{"code":"rate_limit_exceeded"}}`, "", "", false},
		{"type_shadow_cannot_hide_error", `{"type":"response.failed","Type":"response.created","error":{"code":"rate_limit_exceeded"}}`, "", "", true},
		{"malformed_type_shadow_is_unknown", `{"type":"error","TYPE":{},"code":"usage_limit_reached"}`, "", "", true},
		{"stream_and_response_id_shadow", `{"type":"error","stream_id":"lane-a","STREAM_ID":"lane-b","response_id":"response-a","RESPONSE_ID":"response-b","code":"rate_limit_exceeded"}`, "lane-a", "response-a", true},
		{"nested_response_id_shadow", `{"type":"response.failed","response":{"id":"response-a","ID":"response-b","error":{"code":"rate_limit_exceeded"}}}`, "", "response-a", true},
		{"malformed_nested_id_shadow", `{"type":"response.failed","response":{"id":"response-a","ID":{},"error":{"code":"rate_limit_exceeded"}}}`, "", "response-a", true},
		{"unknown_response_cannot_add_quota", `{"type":"response.failed","response":{"id":"response-a","error":{"code":"bad_request"}},"Response":{"id":"response-b","error":{"code":"rate_limit_exceeded"}}}`, "", "", false},
		{"unknown_response_cannot_hide_quota", `{"type":"response.failed","response":{"id":"response-a","error":{"code":"rate_limit_exceeded"}},"Response":7}`, "", "response-a", true},
		{"nested_unknown_error_cannot_add_quota", `{"type":"response.failed","response":{"id":"response-a","error":{"code":"bad_request"},"Error":{"code":"rate_limit_exceeded"}}}`, "", "", false},
		{"conflicting_protocol_response_ids", `{"type":"response.failed","response_id":"response-a","response":{"id":"response-b","error":{"code":"rate_limit_exceeded"}}}`, "", "", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			streamID, responseID, _, limited := gatewayEventRateLimit([]byte(scenario.payload))
			if limited != scenario.limited || limited && (streamID != scenario.streamID || responseID != scenario.responseID) {
				t.Fatalf("unknown case-shadow field changed quota event ownership/type: limited=%t stream=%q response=%q", limited, streamID, responseID)
			}
		})
	}
	for _, payload := range []string{
		`{"response":{"id":"response-a","ID":"response-b"},"response_id":"response-a"}`,
		`{"response":{"id":"response-a","ID":{}},"response_id":"response-a"}`,
		`{"response_id":"response-a","RESPONSE_ID":"response-b"}`,
	} {
		var identity gatewayHTTPCooldownIdentity
		if !identity.accepts([]byte(payload)) || !identity.accepts([]byte(`{"response_id":"response-a"}`)) {
			t.Fatal("HTTP cooldown binding trusted an unknown case-shadow response ID")
		}
	}
}
