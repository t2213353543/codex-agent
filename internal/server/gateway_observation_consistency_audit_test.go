package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

func TestAuditObservationUsageSchemaAndNumericBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, raw       string
		in, out, cached int64
		ok              bool
	}{
		{"native_zero", `{"usage":{"input_tokens":0,"prompt_tokens":99,"output_tokens":0,"completion_tokens":98,"input_tokens_details":{"cached_tokens":0},"cached_tokens":97,"prompt_tokens_details":{"cached_tokens":96}}}`, 0, 0, 0, true},
		{"native_cache_priority", `{"usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":2},"cached_tokens":6}}`, 7, 3, 2, true},
		{"compatible", `{"usage":{"prompt_tokens":7,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}}`, 7, 3, 2, true},
		{"nested", `{"usage":{"input_tokens":900},"response":{"usage":{"input_tokens":7,"output_tokens":3}}}`, 7, 3, 0, true},
		{"null_usage", `{"usage":null}`, 0, 0, 0, false},
		{"invalid_nested", `{"response":false,"usage":{"input_tokens":900}}`, 0, 0, 0, false},
		{"null_nested", `{"response":null,"usage":{"input_tokens":900}}`, 0, 0, 0, false},
		{"missing_nested_usage", `{"response":{},"usage":{"input_tokens":900}}`, 0, 0, 0, false},
		{"negative", `{"usage":{"input_tokens":-1,"output_tokens":2}}`, 0, 0, 0, false},
		{"negative_cache", `{"usage":{"input_tokens_details":{"cached_tokens":-1}}}`, 0, 0, 0, false},
		{"null_counter", `{"usage":{"input_tokens":null,"prompt_tokens":7}}`, 0, 0, 0, false},
		{"fraction", `{"usage":{"input_tokens":1.5}}`, 0, 0, 0, false},
		{"string_counter", `{"usage":{"input_tokens":"7"}}`, 0, 0, 0, false},
		{"counter_overflow", `{"usage":{"input_tokens":9223372036854775808}}`, 0, 0, 0, false},
		{"sum_overflow", `{"usage":{"input_tokens":9223372036854775807,"output_tokens":1}}`, 0, 0, 0, false},
		{"max_boundary", `{"usage":{"input_tokens":9223372036854775807,"output_tokens":0}}`, math.MaxInt64, 0, 0, true},
		{"malformed_details", `{"usage":{"input_tokens":7,"input_tokens_details":"invalid"}}`, 0, 0, 0, false},
		{"unknown_object", `{"usage":{}}`, 0, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, out, cached, ok := usageFromResponse([]byte(tc.raw))
			if in != tc.in || out != tc.out || cached != tc.cached || ok != tc.ok {
				t.Fatalf("usage = (%d,%d,%d,%v), want (%d,%d,%d,%v)", in, out, cached, ok, tc.in, tc.out, tc.cached, tc.ok)
			}
			var rec RequestRecord
			observeGatewayResponse(&rec, []byte(tc.raw))
			if rec.UsageKnown != nil && *rec.UsageKnown != tc.ok {
				t.Fatalf("usage validity disagrees with accounting: %+v", rec)
			}
		})
	}
	var rec RequestRecord
	observeGatewayResponse(&rec, []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	if rec.UsageSchemaSource != "chat_completions_usage" {
		t.Fatalf("compatible usage source mislabeled: %s", rec.UsageSchemaSource)
	}
}

func TestAuditObservationSSEChunkingBOMAndEventDispatch(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for chunk := 1; chunk <= 7; chunk++ {
			t.Run(fmt.Sprintf("%q/chunk=%d", ending, chunk), func(t *testing.T) {
				wire := "\ufeff: 你好🛠" + ending + "event: ignored" + ending + "data: {\"type\":\"response.completed\"," + ending + "data: \"response\":{\"id\":\"resp_你好\",\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}" + ending + ending
				var rec RequestRecord
				observer := gatewayObservation{record: &rec}
				scanner := sseScanner{rec: &rec, observe: observer.event}
				for data := []byte(wire); len(data) > 0; {
					size := min(chunk, len(data))
					_, _ = scanner.Write(data[:size])
					data = data[size:]
				}
				scanner.Finish()
				if !scanner.terminal || rec.Status != "ok" || rec.InputTokens != 7 || rec.OutputTokens != 2 || rec.ResponseID != "resp_你好" {
					t.Fatalf("valid fragmented SSE failed: %+v", rec)
				}
			})
		}
	}
	for _, tail := range []string{"", "\n", "\r\n"} {
		var rec RequestRecord
		scanner := sseScanner{rec: &rec}
		_, _ = scanner.Write([]byte(`data: {"type":"response.completed","response":{}}` + tail))
		scanner.Finish()
		if scanner.terminal || rec.Status != "aborted" {
			t.Fatalf("unterminated event dispatched with tail %q: %+v", tail, rec)
		}
	}
}

func TestAuditObservationSSEInvalidUTF8DoesNotTruncateForwarding(t *testing.T) {
	for _, prefix := range []string{":\xff\n", "event: \xff\n", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"\xff\"}\n\n"} {
		wire := prefix + "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":99}}}\n\n" + strings.Repeat("trailing opaque bytes\n", 4096)
		var rec RequestRecord
		scanner := sseScanner{rec: &rec}
		w := httptest.NewRecorder()
		res := &http.Response{Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(wire)))}
		if err := writeStreamPipe(w, http.NewResponseController(w), res, &scanner, false); err != nil {
			t.Fatal(err)
		}
		scanner.Finish()
		if w.Body.String() != wire || !scanner.finished || scanner.terminal || rec.Status != "error" || rec.InputTokens != 0 || rec.ResponseID != "" {
			t.Fatalf("invalid UTF-8 changed forwarding or counted terminal: %+v", rec)
		}
	}
}

func TestAuditObservationTerminalContradictionsAcrossHTTP(t *testing.T) {
	for _, tc := range []struct{ event, status, want string }{
		{"response.incomplete", "completed", "incomplete"},
		{"response.incomplete", "", "incomplete"},
		{"response.incomplete", "failed", "failed"},
		{"response.failed", "completed", "failed"},
		{"response.completed", "failed", "completed"},
		{"response.completed", "incomplete", "completed"},
		{"response.done", "cancelled", "cancelled"},
	} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.event, tc.status, stream), func(t *testing.T) {
				wire := fmt.Sprintf("data: {\"type\":%q,\"response\":{\"id\":\"resp_one\",\"status\":%q,\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n", tc.event, tc.status)
				p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, wire) })
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest("POST", "/responses", fmt.Sprintf(`{"model":"test","stream":%v}`, stream)))
				rec, ok := p.Metrics.LastFor("test")
				wantStatus, wantTerminal, wantSource, wantTokens := "error", tc.want, "upstream", int64(7)
				if tc.event == "response.completed" {
					wantStatus, wantSource = "ok", ""
				}
				if tc.event == "response.done" && stream {
					wantStatus, wantTerminal, wantTokens = "aborted", "", 0
				}
				if !ok || rec.Status != wantStatus || rec.TerminalStatus != wantTerminal || rec.InputTokens != wantTokens || rec.ErrorSource != wantSource {
					t.Fatalf("HTTP event/compatibility outcome differs: %+v", rec)
				}
				if stream && w.Body.String() != wire {
					t.Fatal("status reconciliation changed SSE bytes")
				}
			})
		}
	}
}

func TestAuditObservationSSEIdleAndScannerYieldTogether(t *testing.T) {
	for _, wire := range []string{
		"\ufeffdata: {}\r\n\r\n", "data\n\n", "data:\n\n", ":comment\n\n", "id:x\n\n", "data: {}\r\rx", "data: {}\r\r", "data: {}\n", "data: 你好\n\n", "data: \xff\n\n",
	} {
		var rec RequestRecord
		var progress gatewaySSEIdleProgress
		events := 0
		scanner := sseScanner{rec: &rec, observe: func([]byte) { events++ }}
		for _, b := range []byte(wire) {
			before := events
			yielded := progress.consume([]byte{b})
			_, _ = scanner.Write([]byte{b})
			if yielded != (events > before) {
				t.Fatalf("idle and observer disagree on byte %q in %q: idle=%v events=%d", b, wire, yielded, events)
			}
		}
	}
}

func TestAuditObservationResponseIdentityCannotPolluteMetadata(t *testing.T) {
	var rec RequestRecord
	start := time.Now()
	observer := gatewayObservation{record: &rec, started: start}
	observer.event([]byte(`{"type":"response.created","response":{"id":"resp_a","model":"actual"}}`))
	for _, raw := range []string{
		`{"type":"response.output_text.delta","response_id":"resp_b","delta":"private_output"}`,
		`{"type":"response.completed","response_id":"resp_a","response":{"id":"resp_b","model":"polluted","status":"failed"}}`,
		`{"type":"response.created","response":{"id":"resp_b","model":"polluted"},"headers":{"x-request-id":"polluted"}}`,
	} {
		observer.event([]byte(raw))
	}
	if rec.ResponseID != "resp_a" || rec.UpstreamModel != "actual" || rec.TTFTMs != nil || rec.TerminalStatus != "" || rec.RequestID != "" {
		t.Fatalf("foreign event polluted response observations: %+v", rec)
	}
	recordResponseOutcome(&rec, []byte(`{"id":"resp_b","model":"polluted","status":"completed","usage":{"input_tokens":999}}`))
	if rec.ResponseID != "resp_a" || rec.UpstreamModel != "actual" || rec.InputTokens != 0 || rec.Status != "error" {
		t.Fatalf("foreign terminal polluted accounting: %+v", rec)
	}
}

func TestAuditObservationSecretsRemainRedactedInBoundedMetadata(t *testing.T) {
	const secret = "Bearer PRIVATE_CREDENTIAL"
	var rec RequestRecord
	request, _ := json.Marshal(map[string]any{"service_tier": secret, "reasoning": map[string]string{"effort": secret}})
	observeGatewayRequest(&rec, secret, "http_sse", request)
	observeGatewayHeaders(&rec, http.Header{"X-Request-Id": {secret}, "Openai-Model": {secret}})
	response, _ := json.Marshal(map[string]any{"id": secret, "model": secret, "service_tier": secret, "status": "incomplete", "incomplete_details": map[string]string{"reason": secret}})
	observeGatewayResponse(&rec, response)
	_, attempts := withGatewayAttemptObservation(t.Context())
	model, _ := json.Marshal(map[string]string{"model": secret})
	attempts.ObserveSent("https://user:PRIVATE_CREDENTIAL@example.invalid/responses?token=PRIVATE_CREDENTIAL", model)
	applyGatewayAttemptSnapshot(&rec, attempts)
	raw, _ := json.Marshal(rec)
	if bytes.Contains(raw, []byte("PRIVATE_CREDENTIAL")) || !bytes.Contains(raw, []byte("[REDACTED]")) {
		t.Fatalf("metadata retained credentials: %s", raw)
	}
}

type auditObservationFaultWriter struct {
	*httptest.ResponseRecorder
	short    bool
	flushErr error
}

func (w *auditObservationFaultWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) / 2, nil
	}
	return 0, io.ErrClosedPipe
}
func (w *auditObservationFaultWriter) FlushError() error { return w.flushErr }

func TestAuditObservationWriteFailuresPreserveUpstreamTruth(t *testing.T) {
	for _, short := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			rec := RequestRecord{Status: "ok", TerminalStatus: "completed"}
			if failed {
				rec.Status, rec.Error, rec.ErrorSource, rec.ErrorPhase = "error", "upstream failure", "upstream", "response_event"
			}
			writer := gatewayResponseWriter{ResponseWriter: &auditObservationFaultWriter{ResponseRecorder: httptest.NewRecorder(), short: short}, record: &rec}
			_, err := writer.Write([]byte("body"))
			if err == nil || rec.HTTPCode != 200 || rec.TerminalStatus != "completed" {
				t.Fatalf("write result/status missing: %+v err=%v", rec, err)
			}
			if failed {
				if rec.Status != "error" || rec.ErrorSource != "upstream" || rec.Error != "upstream failure" {
					t.Fatalf("client failure replaced upstream truth: %+v", rec)
				}
			} else if rec.Status != "aborted" || rec.ErrorSource != "client" || rec.ErrorPhase != "response_write" {
				t.Fatalf("client failure reported success: %+v", rec)
			}
		}
	}
	for _, flushErr := range []error{io.ErrClosedPipe, http.ErrNotSupported} {
		var rec RequestRecord
		writer := gatewayResponseWriter{ResponseWriter: &auditObservationFaultWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: flushErr}, record: &rec}
		if !errors.Is(writer.FlushError(), flushErr) {
			t.Fatal("flush error was not preserved")
		}
		if (rec.Status == "aborted") != (flushErr == io.ErrClosedPipe) {
			t.Fatalf("unsupported flush confused with write failure: %+v", rec)
		}
	}
}

func TestAuditObservationHTTPJSONWriteFailureKeepsAttemptSuccessful(t *testing.T) {
	for _, path := range []string{"/responses", "/responses/compact", "/models"} {
		t.Run(path, func(t *testing.T) {
			wire := `{"id":"resp_write","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}`
			if path == "/responses" {
				wire = "data: {\"type\":\"response.completed\",\"response\":" + wire + "}\n\n"
			}
			p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, wire) })
			writer := &auditObservationFaultWriter{ResponseRecorder: httptest.NewRecorder()}
			method := "POST"
			if path == "/models" {
				method = "GET"
			}
			p.ServeHTTP(writer, proxyRequest(method, path, `{"model":"test","stream":false}`))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.Status != "aborted" || rec.ErrorSource != "client" || rec.ErrorPhase != "response_write" || rec.HTTPCode != 200 || len(rec.Attempts) != 1 {
				t.Fatalf("downstream write failure lost: %+v", rec)
			}
			attempt := rec.Attempts[0]
			if attempt.StatusCode == nil || *attempt.StatusCode != 200 || attempt.ErrorCode != "" || attempt.ErrorSource != "" {
				t.Fatalf("request attempt overwritten by later client failure: %+v", attempt)
			}
		})
	}
}

func TestAuditObservationModelsReadFailureAndLimit(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint("oversized=", oversized), func(t *testing.T) {
			p := newTestProxy(t, 1)
			p.Client = &http.Client{Transport: observationFaultTransport(func(r *http.Request) (*http.Response, error) {
				var body io.Reader = io.MultiReader(strings.NewReader(`{"models":[`), iotest.ErrReader(io.ErrUnexpectedEOF))
				if oversized {
					body = strings.NewReader(strings.Repeat("x", (8<<20)+1))
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(body), Request: r}, nil
			})}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest("GET", "/models", ""))
			rec, ok := p.Metrics.LastFor("test")
			if !ok || w.Code != 502 || rec.HTTPCode != 502 || rec.Status != "error" || rec.ErrorSource != "upstream" || rec.ErrorPhase != "response_body" || strings.Contains(w.Body.String(), `"models":[`) {
				t.Fatalf("invalid catalog committed success: %+v HTTP=%d", rec, w.Code)
			}
		})
	}
}

func TestAuditObservationWSWarmupScopedIncompleteAccounting(t *testing.T) {
	p := newTestProxy(t, 2)
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	turns := gatewayWSTurns{proxy: p, keyID: "admin", pending: make(map[string][]*gatewayWSTurn)}
	var releasedA, releasedB atomic.Int32
	gatewayWSIdentityStart(t, &turns, account, "a", "model-a", "", &releasedA)
	gatewayWSIdentityStart(t, &turns, account, "b", "model-b", "", &releasedB)
	observeGatewayRequest(&turns.pending["a"][0].rec, "test", "websocket", []byte(`{"type":"response.create","generate":false}`))
	turns.observe([]byte(`{"type":"response.incomplete","stream_id":"a","response":{"id":"resp_a","status":"completed","usage":{"input_tokens":4,"output_tokens":0}}}`))
	if turns.count != 1 || releasedA.Load() != 1 || releasedB.Load() != 0 {
		t.Fatal("incomplete warmup consumed another lane")
	}
	rec, ok := p.Metrics.LastFor("test")
	if !ok || rec.Status != "error" || rec.TerminalStatus != "incomplete" || rec.InputTokens != 4 || rec.WebSocketWarmup == nil || !*rec.WebSocketWarmup || rec.TTFTMs != nil {
		t.Fatalf("warmup/incomplete accounting inconsistent: %+v", rec)
	}
	turns.observe([]byte(`{"type":"response.completed","stream_id":"b","response":{"id":"resp_b","status":"completed","usage":{"input_tokens":6,"output_tokens":1}}}`))
	if turns.count != 0 || releasedB.Load() != 1 || len(p.Metrics.RecentForAccount("test", 10)) != 2 {
		t.Fatal("second lane did not finish independently")
	}
}

func TestAuditObservationAttemptModelRejectsInvalidEnvelope(t *testing.T) {
	_, observation := withGatewayAttemptObservation(t.Context())
	observation.ObserveReturned([]byte(`{"model":"valid"}`))
	for _, raw := range []string{`{"type":"response.output_text.delta","model":"event-level"}`, `{"response":false,"model":"event-level"}`, "{\"model\":\"bad\xff\"}"} {
		observation.ObserveReturned([]byte(raw))
	}
	if observation.Snapshot().ReturnedModel != "valid" {
		t.Fatal("invalid response envelope polluted returned model")
	}
}

func TestAuditObservationEventHeaderModelFollowsCLI(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"type":"response.metadata","headers":{"OPENAI-MODEL":"top"}}`, "top"},
		{`{"type":"response.created","headers":{"openai-model":"top"},"response":{"id":"resp_a","headers":{"OpenAI-Model":"nested"}}}`, "nested"},
		{`{"type":"response.created","headers":{"openai-model":"top"},"response":{"id":"resp_a","headers":{"openai-model":false}}}`, "top"},
		{`{"type":"response.created","headers":false,"response":{"id":"resp_a","headers":{"openai-model":"nested"}}}`, "nested"},
	} {
		var rec RequestRecord
		(gatewayObservation{record: &rec}).event([]byte(tc.raw))
		if rec.UpstreamModel != tc.want || rec.UpstreamModelSource != "header" {
			t.Fatalf("event model header precedence differs: %+v", rec)
		}
	}
	var rec RequestRecord
	observer := gatewayObservation{record: &rec, started: time.Now()}
	observer.event([]byte(`{"type":"response.output_text.delta","delta":"output","headers":false}`))
	if rec.TTFTMs == nil {
		t.Fatal("malformed optional headers hid valid output")
	}
}

func TestAuditObservationLocalParserLimitIsNotProtocolTerminal(t *testing.T) {
	var rec RequestRecord
	scanner := sseScanner{rec: &rec}
	_, _ = scanner.Write(bytes.Repeat([]byte{'x'}, maxBody+1))
	if !scanner.finished || scanner.terminal || rec.Status != "error" {
		t.Fatalf("local observer limit became upstream terminal: %+v", rec)
	}
	_, _ = scanner.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	if scanner.terminal || rec.Status != "error" {
		t.Fatal("bytes after local observer failure changed outcome")
	}
	var invalid RequestRecord
	scanner = sseScanner{rec: &invalid}
	_, _ = scanner.Write([]byte("data: \xe4\xbd"))
	scanner.Finish()
	if !scanner.finished || scanner.terminal || invalid.Status != "error" {
		t.Fatalf("incomplete UTF-8 at EOF escaped parser failure: %+v", invalid)
	}
}

func TestAuditObservationAbortedDeliveryIsNotResetByTerminal(t *testing.T) {
	rec := RequestRecord{Status: "aborted", Error: "client connection lost", ErrorSource: "client", ErrorPhase: "response_write"}
	recordResponseOutcome(&rec, []byte(`{"id":"resp","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}`))
	if rec.Status != "aborted" || rec.TerminalStatus != "completed" || rec.InputTokens != 3 || rec.ErrorSource != "client" {
		t.Fatalf("upstream completion erased downstream failure: %+v", rec)
	}
}
