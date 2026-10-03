package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

func TestResponseEventsErrorDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, event, code, message, param, diagnostic string
		input, output, cached                         int64
	}{
		{
			name:  "standard top-level error",
			event: `{"type":"error","code":"invalid_value","message":"Unsupported reasoning effort","param":"reasoning.effort","sequence_number":4}`,
			code:  "invalid_value", message: "Unsupported reasoning effort", param: "reasoning.effort", diagnostic: "Unsupported reasoning effort",
		},
		{
			name:  "top-level code without message",
			event: `{"type":"error","code":"rate_limit_exceeded","param":null,"sequence_number":4}`,
			code:  "rate_limit_exceeded", diagnostic: "rate_limit_exceeded",
		},
		{
			name:  "nested backend error",
			event: `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Unknown tool","param":"tools[0].name"}}`,
			code:  "invalid_value", message: "Unknown tool", param: "tools[0].name", diagnostic: "Unknown tool",
		},
		{
			name:  "failed response",
			event: `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"server_error","message":"Generation failed","param":null},"usage":{"input_tokens":120,"output_tokens":45,"input_tokens_details":{"cached_tokens":80}}}}`,
			code:  "server_error", message: "Generation failed", diagnostic: "Generation failed",
			input: 120, output: 45, cached: 80,
		},
	} {
		for _, stream := range []bool{false, true} {
			mode := "json"
			if stream {
				mode = "sse"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				wire := "data: " + test.event + "\n\n"
				p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, wire)
				})
				request, _ := json.Marshal(map[string]any{"model": "test", "stream": stream})
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", string(request)))
				if stream {
					if w.Code != http.StatusOK || w.Body.String() != wire {
						t.Fatalf("stream altered: status=%d body=%q", w.Code, w.Body.String())
					}
				} else {
					if w.Code != http.StatusBadGateway {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
					var result struct {
						Error struct {
							Code    string  `json:"code"`
							Message string  `json:"message"`
							Param   *string `json:"param"`
						} `json:"error"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					param := ""
					if result.Error.Param != nil {
						param = *result.Error.Param
					}
					if result.Error.Code != test.code || result.Error.Message != test.message || param != test.param {
						t.Fatalf("upstream diagnostics lost: %s", w.Body.String())
					}
				}
				rec, ok := p.Metrics.LastFor("test")
				if !ok || rec.Status != "error" || rec.Error != test.diagnostic {
					t.Fatalf("failure not recorded: %+v (found=%v)", rec, ok)
				}
				if rec.InputTokens != test.input || rec.OutputTokens != test.output || rec.CachedTokens != test.cached {
					t.Fatalf("failed response usage lost: %+v", rec)
				}
			})
		}
	}
}

func TestResponseEventsIncompleteRetainsResponseAndUsage(t *testing.T) {
	const response = `{"id":"resp_partial","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Partial answer"}]}],"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":80},"output_tokens":45}}`
	wire := "data: {\"type\":\"response.incomplete\",\"response\":" + response + "}\n\n"
	for _, stream := range []bool{false, true} {
		mode := "json"
		if stream {
			mode = "sse"
		}
		t.Run(mode, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wire)
			})
			request, _ := json.Marshal(map[string]any{"model": "test", "stream": stream})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", string(request)))
			if w.Code != http.StatusOK {
				t.Fatalf("incomplete response is still a response: status=%d body=%s", w.Code, w.Body.String())
			}
			if stream {
				if w.Body.String() != wire {
					t.Fatalf("incomplete SSE altered: %q", w.Body.String())
				}
			} else {
				assertJSONEqual(t, w.Body.Bytes(), []byte(response))
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.Status != "error" || !strings.Contains(rec.Error, "max_output_tokens") {
				t.Fatalf("incomplete response counted as success or reason lost: %+v", rec)
			}
			if rec.InputTokens != 120 || rec.OutputTokens != 45 || rec.CachedTokens != 80 {
				t.Fatalf("incomplete response usage lost: %+v", rec)
			}
		})
	}
}

func TestResponseEventsCompletionRequiresResponseObject(t *testing.T) {
	for name, payload := range map[string]string{
		"missing": `{"type":"response.completed"}`,
		"null":    `{"type":"response.completed","response":null}`,
		"array":   `{"type":"response.completed","response":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			wire := "data: " + payload + "\n\n"
			if response, err := collectResponse(strings.NewReader(wire)); err == nil || response != nil {
				t.Fatalf("malformed completion accepted: response=%s err=%v", response, err)
			}
			rec := RequestRecord{}
			observer := sseScanner{rec: &rec}
			_, _ = observer.Write([]byte(wire))
			observer.Finish()
			if rec.Status != "error" || rec.Error == "" {
				t.Fatalf("malformed completion counted as success: %+v", rec)
			}
		})
	}
}

func TestResponseEventsChunkedCRLFAndMultipleDataLines(t *testing.T) {
	const response = `{"id":"resp_unicode","status":"completed","output":[{"type":"reasoning","encrypted_content":"opaque+/="},{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"你好"}]}],"usage":{"input_tokens":21,"input_tokens_details":{"cached_tokens":13},"output_tokens":8}}`
	wire := ": heartbeat\r\n\r\nevent: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_unicode\"}}\r\n\r\n" +
		"id: event-2\r\nevent: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":" + response + "}\r\n\r\n"
	result, err := collectResponse(iotest.OneByteReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, result, []byte(response))
	rec := RequestRecord{}
	observer := sseScanner{rec: &rec}
	for i := 0; i < len(wire); i++ {
		if n, err := observer.Write([]byte{wire[i]}); n != 1 || err != nil {
			t.Fatalf("byte %d: n=%d err=%v", i, n, err)
		}
	}
	observer.Finish()
	if rec.Status != "ok" || rec.Error != "" || rec.InputTokens != 21 || rec.OutputTokens != 8 || rec.CachedTokens != 13 {
		t.Fatalf("fragmentation changed response outcome: %+v", rec)
	}
}

func TestResponseEventsCleanEOFDoesNotDispatchPendingStreamEvent(t *testing.T) {
	const response = `{"id":"resp_eof","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":3}}`
	for name, ending := range map[string]string{"no newline": "", "LF": "\n", "CRLF": "\r\n"} {
		t.Run(name, func(t *testing.T) {
			wire := "data: {\"type\":\"response.completed\",\"response\":" + response + "}" + ending
			result, err := collectResponse(strings.NewReader(wire))
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, result, []byte(response))
			rec := RequestRecord{}
			observer := sseScanner{rec: &rec}
			_, _ = observer.Write([]byte(wire))
			if rec.Status != "" {
				t.Fatalf("event completed before its boundary or EOF: %+v", rec)
			}
			observer.Finish()
			observer.Finish()
			// Codex 0.154.0 locks eventsource-stream 0.2.3, whose parser
			// requires a blank line before dispatch, including at clean EOF.
			// JSON collection above intentionally retains its compatibility path.
			if rec.Status != "aborted" || rec.InputTokens != 0 || rec.OutputTokens != 0 || !strings.Contains(rec.Error, "without a terminal") {
				t.Fatalf("unterminated stream event counted as a completion: %+v", rec)
			}
		})
	}
}

func TestResponseEventsGatewayCleanEOF(t *testing.T) {
	const wire = `data: {"type":"response.completed","response":{"id":"resp_eof","status":"completed","output":[],"usage":{"input_tokens":9,"output_tokens":3}}}`
	p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":true}`))
	if w.Code != http.StatusOK || w.Body.String() != wire {
		t.Fatalf("clean EOF stream altered: status=%d body=%q", w.Code, w.Body.String())
	}
	rec, ok := p.Metrics.LastFor("test")
	if !ok || rec.Status != "aborted" || rec.InputTokens != 0 || rec.OutputTokens != 0 || !strings.Contains(rec.Error, "without a terminal") {
		t.Fatalf("gateway accepted a terminal event the CLI would not dispatch: %+v", rec)
	}
}

func TestResponseEventsCompletedWinsOverDeferredErrors(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"id":"resp_first","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3}}}`
	const failed = `{"type":"error","code":"server_error","message":"First failure","param":null}`
	const incomplete = `{"type":"response.incomplete","response":{"id":"resp_first","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":11,"output_tokens":2}}}`
	for _, test := range []struct {
		name, first, second, status, diagnostic string
		input, output                           int64
		collectFailure                          bool
	}{
		{"success then failure", completed, failed, "ok", "", 7, 3, false},
		{"failure then success", failed, completed, "ok", "", 7, 3, false},
		{"incomplete then success", incomplete, completed, "ok", "", 7, 3, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			first, second := "data: "+test.first+"\n\n", "data: "+test.second+"\n\n"
			result, err := collectResponse(strings.NewReader(first + second))
			if (err != nil) != test.collectFailure {
				t.Fatalf("collection did not retain completion: response=%s err=%v", result, err)
			}
			if !test.collectFailure {
				var event struct {
					Response json.RawMessage `json:"response"`
				}
				_ = json.Unmarshal([]byte(completed), &event)
				assertJSONEqual(t, result, event.Response)
			}
			for _, combined := range []bool{false, true} {
				rec := RequestRecord{}
				observer := sseScanner{rec: &rec}
				if combined {
					_, _ = observer.Write([]byte(first + second))
				} else {
					_, _ = observer.Write([]byte(first))
					_, _ = observer.Write([]byte(second))
				}
				observer.Finish()
				if rec.Status != test.status || rec.InputTokens != test.input || rec.OutputTokens != test.output ||
					(test.diagnostic == "" && rec.Error != "") || (test.diagnostic != "" && !strings.Contains(rec.Error, test.diagnostic)) {
					t.Fatalf("deferred error changed completed outcome (combined=%v): %+v", combined, rec)
				}
			}
		})
	}
}

// Counting reads verifies that an oversized event is rejected before consuming
// the rest of the upstream stream, without allocating the whole event at once.
type responseEventCountingReader struct {
	io.Reader
	n int
}

func (r *responseEventCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += n
	return n, err
}

func TestResponseEventsOversizedEventIsBounded(t *testing.T) {
	const chunkSize = 64 << 10
	const success = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"later\",\"status\":\"completed\",\"output\":[]}}\n\n"
	for _, multiline := range []bool{false, true} {
		name := "single line"
		prefix := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"large\",\"padding\":\""
		chunk := strings.Repeat("x", chunkSize)
		suffix := "\",\"status\":\"completed\",\"output\":[]}}\n\n"
		if multiline {
			name = "multiple data lines"
			prefix = "data: {\"type\":\"response.completed\",\n"
			chunk = "data: " + strings.Repeat(" ", chunkSize) + "\n"
			suffix = "data: \"response\":{\"id\":\"large\",\"status\":\"completed\",\"output\":[]}}\n\n"
		}
		t.Run(name, func(t *testing.T) {
			parts := []io.Reader{strings.NewReader(prefix)}
			for i := 0; i < maxBody/chunkSize+16; i++ {
				parts = append(parts, strings.NewReader(chunk))
			}
			parts = append(parts, strings.NewReader(suffix+success))
			reader := &responseEventCountingReader{Reader: io.MultiReader(parts...)}
			if result, err := collectResponse(reader); err == nil || result != nil {
				t.Fatalf("oversized response accepted: len=%d err=%v", len(result), err)
			}
			if reader.n > maxBody+2*len(chunk) {
				t.Fatalf("oversized event was not rejected promptly: read=%d limit=%d", reader.n, maxBody)
			}

			rec := RequestRecord{}
			observer := sseScanner{rec: &rec}
			_, _ = observer.Write([]byte(prefix))
			bytesChunk := []byte(chunk)
			for i := 0; i < maxBody/chunkSize+1; i++ {
				if n, err := observer.Write(bytesChunk); n != len(bytesChunk) || err != nil {
					t.Fatalf("metrics observer interrupted byte forwarding: n=%d err=%v", n, err)
				}
			}
			if rec.Status != "error" || !strings.Contains(rec.Error, "exceeds") {
				t.Fatalf("oversized event was not recorded when the limit was crossed: %+v", rec)
			}
			_, _ = observer.Write([]byte(suffix + success))
			observer.Finish()
			if rec.Status != "error" || !strings.Contains(rec.Error, "exceeds") {
				t.Fatalf("later completion hid the oversized event: %+v", rec)
			}
		})
	}
}
