package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

const sequenceCompleted = `{"type":"response.completed","response":{"id":"sequence-a","status":"completed","output":[],"usage":{"input_tokens":19,"output_tokens":7,"total_tokens":26}}}`

// Separate flushed upstream chunks prove that an intermediary cannot stop at
// an error event before the official SSE parser's later Completed arrives.
func TestHTTPTerminalSequenceLaterCompletedWins(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, first := range []struct{ name, event string }{
			{"failed", `{"type":"response.failed","response":{"id":"sequence-a","status":"failed","error":{"code":"server_error","message":"temporary failure"},"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`},
			{"incomplete", `{"type":"response.incomplete","response":{"id":"sequence-a","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`},
			{"quota", `{"type":"response.failed","headers":{"Retry-After":"60"},"response":{"id":"sequence-a","status":"failed","error":{"code":"rate_limit_exceeded","message":"temporary quota failure"}}}`},
			{"invalid_prompt", `{"type":"response.failed","response":{"id":"sequence-a","status":"failed","error":{"code":"invalid_prompt","message":"temporary validation failure"}}}`},
			{"top_error_extension", `{"type":"error","response_id":"sequence-a","code":"server_error","message":"temporary extension error"}`},
			{"malformed_completed", `{"type":"response.completed","response":{"id":"sequence-a","usage":{"input_tokens":"invalid"}}}`},
			{"missing_response_completed", `{"type":"response.completed"}`},
			{"done_extension", `{"type":"response.done","response":{"id":"sequence-a","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`},
			{"failed_unknown_Type_completed", `{"type":"response.failed","Type":"response.completed","response":{"id":"sequence-a","status":"failed","error":{"code":"server_error","message":"deferred"}}}`},
		} {
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", transport, first.name, stream), func(t *testing.T) {
					var calls atomic.Int32
					firstSent := make(chan struct{})
					continueStream := make(chan struct{})
					p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", "text/event-stream")
						if calls.Add(1) == 1 {
							_, _ = io.WriteString(w, "data: "+first.event+"\n\n")
							w.(http.Flusher).Flush()
							close(firstSent)
							select {
							case <-continueStream:
							case <-r.Context().Done():
								return
							}
						}
						_, _ = io.WriteString(w, "data: "+sequenceCompleted+"\n\n")
					})
					if transport == "native" {
						realGatewayNativePool(t, p)
					}
					done := make(chan struct{}, 2)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer func() { done <- struct{}{} }(); p.ServeHTTP(w, r) }))
					defer server.Close()
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					type result struct {
						status int
						body   []byte
						err    error
					}
					request := func() result {
						req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/responses", strings.NewReader(fmt.Sprintf(`{"model":"test","stream":%v,"input":[]}`, stream)))
						req.Header.Set("Authorization", "Bearer test-client-key")
						res, err := server.Client().Do(req)
						if err != nil {
							return result{err: err}
						}
						defer res.Body.Close()
						raw, err := io.ReadAll(res.Body)
						return result{res.StatusCode, raw, err}
					}
					resultReady := make(chan result, 1)
					go func() { resultReady <- request() }()
					<-firstSent
					var early *result
					select {
					case r := <-resultReady:
						early = &r
					case <-time.After(40 * time.Millisecond):
					}
					if records := p.Metrics.RecentForAccount("test", 10); len(records) != 0 {
						t.Error("pending error finalized accounting before EOF/completion")
					}
					close(continueStream)
					var got result
					if early != nil {
						got = *early
					} else {
						got = <-resultReady
					}
					<-done
					if got.err != nil || got.status != 200 || !strings.Contains(string(got.body), `"status":"completed"`) {
						t.Fatalf("later Completed lost: HTTP=%d err=%v body=%s", got.status, got.err, got.body)
					}
					if stream && string(got.body) != "data: "+first.event+"\n\ndata: "+sequenceCompleted+"\n\n" {
						t.Fatal("SSE event bytes changed")
					}
					rec, ok := p.Metrics.LastFor("test")
					if !ok || rec.Status != "ok" || rec.TerminalStatus != "completed" || rec.Error != "" || rec.ErrorSource != "" || rec.InputTokens != 19 || rec.OutputTokens != 7 {
						t.Fatalf("temporary failure contaminated final record: %+v", rec)
					}
					if p.inFlight("test") != 0 || calls.Load() != 1 {
						t.Fatal("sequence leaked admission or replayed")
					}
					independent := request()
					<-done
					if independent.err != nil || independent.status != 200 || calls.Load() != 2 {
						t.Fatalf("temporary failure left account cooldown: status=%d calls=%d", independent.status, calls.Load())
					}
				})
			}
		}
	}
}

func TestHTTPTerminalSequenceCompletedEnvelopeAndFieldTypes(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, tc := range []struct {
			name, event string
			valid       bool
		}{
			{"nested_failed", strings.Replace(sequenceCompleted, `"status":"completed"`, `"status":"failed"`, 1), true},
			{"nested_incomplete", strings.Replace(sequenceCompleted, `"status":"completed"`, `"status":"incomplete"`, 1), true},
			{"unknown_Type_failed", strings.Replace(sequenceCompleted, `"type":"response.completed"`, `"type":"response.completed","Type":"response.failed"`, 1), true},
			{"unknown_Response_null", strings.TrimSuffix(sequenceCompleted, "}") + `,"Response":null}`, true},
			{"bad_id", `{"type":"response.completed","response":{"id":false}}`, false},
			{"null_id", `{"type":"response.completed","response":{"id":null}}`, false},
			{"bad_usage", `{"type":"response.completed","response":{"id":"sequence-a","usage":{"input_tokens":true}}}`, false},
			{"bad_usage_array", `{"type":"response.completed","response":{"id":"sequence-a","usage":[]}}`, false},
			{"bad_details", `{"type":"response.completed","response":{"id":"sequence-a","usage":{"input_tokens_details":{"cached_tokens":"wrong"}}}}`, false},
			{"bad_end_turn", `{"type":"response.completed","response":{"id":"sequence-a","end_turn":7}}`, false},
			{"overflow_usage", `{"type":"response.completed","response":{"id":"sequence-a","usage":{"input_tokens":9223372036854775808}}}`, false},
		} {
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", transport, tc.name, stream), func(t *testing.T) {
					wire := "data: " + tc.event + "\n\n"
					p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, wire)
					})
					if transport == "native" {
						realGatewayNativePool(t, p)
					}
					w := httptest.NewRecorder()
					p.ServeHTTP(w, proxyRequest("POST", "/responses", fmt.Sprintf(`{"model":"test","stream":%v}`, stream)))
					if stream && w.Body.String() != wire {
						t.Fatal("stream bytes changed")
					}
					if !stream && w.Code != map[bool]int{true: 200, false: 502}[tc.valid] {
						t.Fatalf("JSON status %d", w.Code)
					}
					rec, _ := p.Metrics.LastFor("test")
					if tc.valid && (rec.Status != "ok" || rec.TerminalStatus != "completed" || rec.Error != "" || rec.ErrorSource != "" || rec.InputTokens != 19) {
						t.Fatalf("valid event outcome changed: %+v", rec)
					}
					if !tc.valid && (rec.Status != "error" || rec.InputTokens != 0) {
						t.Fatalf("malformed completion accepted: %+v", rec)
					}
				})
			}
		}
	}
}

func TestHTTPTerminalSequenceDoneAndErrorExtensionsDoNotReplaceTypedError(t *testing.T) {
	failed := `{"type":"response.failed","response":{"id":"sequence-a","status":"failed","error":{"message":"typed failure"}}}`
	for _, extension := range []string{`{"type":"error","message":"extension failure"}`, `{"type":"response.done","response":{"id":"sequence-a","status":"completed"}}`} {
		wire := "data: " + failed + "\n\ndata: " + extension + "\n\n"
		_, err := collectResponse(strings.NewReader(wire))
		var upstream *upstreamResponseError
		if !errors.As(err, &upstream) || !strings.Contains(string(upstream.Payload), "typed failure") {
			t.Fatalf("extension replaced typed EOF failure: %v", err)
		}
	}
	const done = `{"type":"response.done","response":{"id":"sequence-a","status":"completed","output":[]}}`
	result, err := collectResponse(strings.NewReader("data: " + done + "\n\n"))
	if err != nil || !strings.Contains(string(result), "sequence-a") {
		t.Fatal("EOF JSON compatibility lost")
	}
	var rec RequestRecord
	s := sseScanner{rec: &rec}
	_, _ = s.Write([]byte("data: " + done + "\n\n"))
	s.Finish()
	if s.terminal || rec.Status != "aborted" {
		t.Fatal("unknown done event became SSE completion")
	}
}

func TestHTTPTerminalSequenceEOFUsesLastError(t *testing.T) {
	failed := func(code, message string, input int) string {
		return fmt.Sprintf(`{"type":"response.failed","headers":{"Retry-After":"60"},"response":{"id":"sequence-a","status":"failed","error":{"code":%q,"message":%q},"usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d}}}`, code, message, input, input+1)
	}
	first := failed("rate_limit_exceeded", "first quota error", 3)
	for _, transport := range []string{"go", "native"} {
		for _, tc := range []struct {
			name, last, message string
			status              int
			input               int64
			limited             bool
		}{
			{"server_last", failed("server_error", "last server error", 9), "last server error", 502, 9, false},
			{"quota_last", failed("rate_limit_exceeded", "last quota error", 11), "last quota error", 502, 11, true},
			{"incomplete_last", `{"type":"response.incomplete","response":{"id":"sequence-a","status":"incomplete","incomplete_details":{"reason":"last reason"},"usage":{"input_tokens":17,"output_tokens":2,"total_tokens":19}}}`, "last reason", 200, 17, false},
		} {
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", transport, tc.name, stream), func(t *testing.T) {
					wire := "data: " + first + "\n\ndata: " + tc.last + "\n\n"
					var calls atomic.Int32
					p := cooldownProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, wire)
					})
					if transport == "native" {
						realGatewayNativePool(t, p)
					}
					w := httptest.NewRecorder()
					p.ServeHTTP(w, proxyRequest("POST", "/responses", fmt.Sprintf(`{"model":"test","stream":%v,"input":[]}`, stream)))
					wantHTTP := tc.status
					if stream {
						wantHTTP = 200
					}
					if w.Code != wantHTTP || stream && w.Body.String() != wire {
						t.Fatalf("EOF adaptation: HTTP=%d", w.Code)
					}
					rec, ok := p.Metrics.LastFor("test")
					if !ok || rec.Status != "error" || !strings.Contains(rec.Error, tc.message) || rec.InputTokens != tc.input {
						t.Fatalf("last error lost: %+v", rec)
					}
					account, err := p.Store.Get("test")
					if err != nil {
						t.Fatal(err)
					}
					if limited := p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "test"), account) != nil; limited != tc.limited {
						t.Fatalf("unresolved last error cooldown=%v want=%v", limited, tc.limited)
					}
					if calls.Load() != 1 || p.inFlight("test") != 0 {
						t.Fatal("EOF sequence replayed or leaked")
					}
				})
			}
		}
	}
}

func TestHTTPTerminalSequenceReadErrorOverridesPendingError(t *testing.T) {
	first := "data: " + `{"type":"response.failed","response":{"id":"sequence-a","status":"failed","error":{"message":"pending error"}}}` + "\n\n"
	for _, cause := range []error{io.ErrUnexpectedEOF, context.Canceled, errUpstreamStreamIdleTimeout} {
		var observed [][]byte
		result, err := collectResponseObserved(io.MultiReader(strings.NewReader(first), iotest.ErrReader(cause)), func(raw []byte) { observed = append(observed, append([]byte(nil), raw...)) })
		if result != nil || !errors.Is(err, cause) {
			t.Fatalf("read failure lost: %v", err)
		}
		if len(observed) != 0 {
			t.Fatal("read failure committed a pending event error/cooldown")
		}
	}
}

func TestHTTPTerminalSequenceDeferredBuffersAreReleased(t *testing.T) {
	var rec RequestRecord
	s := sseScanner{rec: &rec}
	_, _ = s.Write([]byte("data: " + `{"type":"response.failed","response":{"id":"sequence-a","error":{"message":"pending"}}}` + "\n\n"))
	if s.pending == nil || s.finished || s.terminal || rec.Status != "" {
		t.Fatal("failure became final before EOF")
	}
	_, _ = s.Write([]byte("data: " + sequenceCompleted + "\n\n"))
	s.Finish()
	if s.pending != nil || s.line != nil || s.eventData != nil || rec.Status != "ok" {
		t.Fatal("completion retained error buffer/outcome")
	}
}
