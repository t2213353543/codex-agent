package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSourceSSETerminalClientClosePreservesOutcome(t *testing.T) {
	// codex-api/src/sse/responses.rs returns immediately after Completed.
	// A client is therefore allowed to close a fully delivered terminal event
	// before the upstream server sends its HTTP EOF. A deferred failed event
	// alone is not a completion; closing then aborts the still-active request.
	for _, test := range []struct {
		name, event, status, message string
		input, output                int64
	}{
		{"completed", `{"type":"response.completed","response":{"id":"resp_terminal","status":"completed","usage":{"input_tokens":23,"output_tokens":7,"total_tokens":30}}}`, "ok", "", 23, 7},
		{"failed", `{"type":"response.failed","response":{"id":"resp_terminal","status":"failed","error":{"code":"server_error","message":"upstream failure"},"usage":{"input_tokens":23,"output_tokens":7,"total_tokens":30}}}`, "aborted", "", 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstreamCanceled := make(chan struct{})
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+test.event+"\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					close(upstreamCanceled)
				case <-time.After(3 * time.Second):
					t.Error("upstream was not canceled after the client closed")
				}
			})
			handlerDone := make(chan struct{})
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				p.ServeHTTP(w, r)
			}))
			defer gateway.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/responses", strings.NewReader(`{"model":"test","stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer test-client-key")
			res, err := gateway.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(res.Body)
			for _, want := range []string{"data: " + test.event + "\n", "\n"} {
				line, readErr := reader.ReadString('\n')
				if readErr != nil || line != want {
					_ = res.Body.Close()
					t.Fatalf("terminal event was not fully delivered: line=%q err=%v", line, readErr)
				}
			}
			_ = res.Body.Close()
			for _, done := range []<-chan struct{}{handlerDone, upstreamCanceled} {
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("gateway or upstream did not finish after terminal client close")
				}
			}
			rec, ok := p.Metrics.LastFor("test")
			if !ok || rec.Status != test.status || (test.message != "" && rec.Error != test.message) || rec.InputTokens != test.input || rec.OutputTokens != test.output {
				t.Fatalf("client close overwrote the delivered terminal outcome: %+v", rec)
			}
		})
	}
}

func TestSourceSSETerminalReleasesAccountBeforeUpstreamEOF(t *testing.T) {
	const event = "data: " + `{"type":"response.completed","response":{"id":"resp_release","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}` + "\n\n"
	upstreamCanceled := make(chan struct{})
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, event)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	})
	handlerDone := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		p.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/responses", strings.NewReader(`{"model":"test","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-client-key")
	res, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	received := make([]byte, len(event))
	if _, err := io.ReadFull(res.Body, received); err != nil || string(received) != event {
		t.Fatalf("terminal event not delivered: body=%q err=%v", received, err)
	}
	// Keep the downstream body open: a terminal model event must release the
	// account without waiting for either the client or backend connection EOF.
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("completed response still occupies the gateway handler/account")
	}
	if p.inFlight("test") != 0 {
		t.Fatal("completed response did not release account admission")
	}
	select {
	case <-upstreamCanceled:
	case <-ctx.Done():
		t.Fatal("completed response left its upstream request open")
	}
	if tail, err := io.ReadAll(res.Body); err != nil || len(tail) != 0 {
		t.Fatalf("terminal stream did not finish cleanly: tail=%q err=%v", tail, err)
	}
}

func TestSourceSSECollectorHandlesCRSeparatedEvents(t *testing.T) {
	// eventsource-stream accepts CR, LF and CRLF line endings. HTTP JSON
	// collection should recognize the same events as the streaming observer.
	const response = `{"id":"resp_cr","status":"completed","output":[]}`
	wire := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cr\"}}\r\r" +
		"data: {\"type\":\"response.completed\",\"response\":" + response + "}\r\r"
	result, err := collectResponse(strings.NewReader(wire))
	if err != nil {
		t.Fatalf("collector rejected valid CR SSE: %v", err)
	}
	assertJSONEqual(t, result, []byte(response))
}

func TestSourceSSEFailedWithoutDiagnosticRetainsUsage(t *testing.T) {
	// The CLI accepts a response.failed event without a nested error and emits
	// its generic failure. Token usage in that response must survive as well.
	const wire = "data: " + `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","usage":{"input_tokens":17,"output_tokens":3,"total_tokens":20}}}` + "\n\n"
	for status, statusField := range map[string]string{"failed": `"status":"failed",`, "unknown": `"status":"unknown",`, "absent": ""} {
		for name, stream := range map[string]string{"json": "false", "stream": "true"} {
			t.Run(status+"/"+name, func(t *testing.T) {
				p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, strings.Replace(wire, `"status":"failed",`, statusField, 1))
				})
				w := httptest.NewRecorder()
				p.ServeHTTP(w, proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":`+stream+`}`))
				rec, ok := p.Metrics.LastFor("test")
				if !ok || rec.Status != "error" || rec.Error == "" || rec.InputTokens != 17 || rec.OutputTokens != 3 {
					t.Fatalf("failure without nested diagnostics lost usage: %+v", rec)
				}
			})
		}
	}
}
