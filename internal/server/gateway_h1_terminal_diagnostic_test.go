package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

const h1DiagnosticTerminal = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic\",\"status\":\"completed\"}}\n\n"

type h1DiagnosticSample struct {
	Mode                        string  `json:"upstream_mode"`
	Consumer                    string  `json:"consumer"`
	Requests                    int     `json:"requests"`
	NewConnections              int32   `json:"new_connections"`
	Reused                      int32   `json:"httptrace_reused"`
	ObservedEOF                 int     `json:"body_reads_observed_eof"`
	TerminalDelivered           int     `json:"terminal_delivered"`
	CanceledHandlers            int32   `json:"upstream_handlers_canceled"`
	ReturnedBeforeDelayedEOF    int     `json:"returned_before_delayed_eof"`
	P50Millis                   float64 `json:"p50_milliseconds"`
	P95Millis                   float64 `json:"p95_milliseconds"`
	ActiveHandlersAfterClose    int32   `json:"active_handlers_after_close"`
	ActiveConnectionsAfterClose int32   `json:"active_connections_after_close"`
}

type h1DiagnosticBody struct {
	io.ReadCloser
	eof bool
}

func (b *h1DiagnosticBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if errors.Is(err, io.EOF) {
		b.eof = true
	}
	return n, err
}

// A real loopback HTTP/1.1 connection, not a fake RoundTripper, is required to
// distinguish application terminal delivery from the transport observing EOF.
// read_to_eof is a diagnostic control only, never a proposed production policy.
func TestGatewayH1TerminalConnectionDiagnostic(t *testing.T) {
	var results []h1DiagnosticSample
	baseline := runtime.NumGoroutine()
	for _, mode := range []string{"content_length_immediate_eof", "chunked_immediate_eof", "chunked_delayed_eof", "chunked_held_open"} {
		for _, consumer := range []string{"gateway_terminal", "raw_terminal", "read_to_eof_control"} {
			if mode == "chunked_held_open" && consumer == "read_to_eof_control" {
				continue
			}
			t.Run(mode+"/"+consumer, func(t *testing.T) {
				result := runH1TerminalDiagnostic(t, mode, consumer, 6)
				results = append(results, result)
				t.Logf("new=%d reused=%d EOF=%d terminal=%d p50=%.2fms p95=%.2fms canceled=%d", result.NewConnections, result.Reused, result.ObservedEOF, result.TerminalDelivered, result.P50Millis, result.P95Millis, result.CanceledHandlers)
			})
		}
	}
	report := map[string]any{
		"passed": !t.Failed(), "controlled_loopback_upstream": true, "live_model_inference": false,
		"http_protocol": "HTTP/1.1", "go_version": runtime.Version(), "os": runtime.GOOS,
		"samples": results, "completed_at": time.Now().UTC().Format(time.RFC3339),
		"process_goroutines_before": baseline, "process_goroutines_after": runtime.NumGoroutine(),
		"goroutine_count_scope": "Whole test process counts are observational; handler and connection counters explicitly verify every fixture has stopped. No background drain is started.",
		"source_references": []string{
			"proxy.go writeStreamPipe returns immediately after a complete terminal event is delivered.",
			"gateway_timeout.go idleStreamBody.Close cancels the upstream context and closes its body once.",
			"Go net/http/transport.go bodyEOFSignal requires body EOF before a HTTP/1 connection can return to the idle pool; earlyCloseFn reports false.",
			"Official codex-cli 0.154.0 codex-rs/codex-api/src/sse/responses.rs:669-674 sends Completed then returns, dropping the SSE byte stream without awaiting HTTP EOF.",
		},
		"limits": []string{"These are local Go forwarding samples plus source comparison, not runtime measurements of the official Rust CLI.", "HTTP/2 stream cancellation has different connection reuse semantics and is outside this HTTP/1.1 fixture.", "The EOF consumer deliberately waits for framing completion and is not enabled in gateway runtime.", "Immediate chunked EOF may race with terminal reads; report measured reuse rather than asserting it must be zero or full."},
	}
	if path := os.Getenv("GATEWAY_H1_DIAGNOSTIC_REPORT"); path != "" {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatal("cannot write sanitized diagnostic report")
		}
	}
}

func runH1TerminalDiagnostic(t *testing.T, mode, consumer string, samples int) h1DiagnosticSample {
	t.Helper()
	result := h1DiagnosticSample{Mode: mode, Consumer: consumer, Requests: samples}
	var activeHandlers, connections, activeConnections, reused, canceled atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeHandlers.Add(1)
		defer activeHandlers.Add(-1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if mode == "content_length_immediate_eof" {
			w.Header().Set("Content-Length", strconv.Itoa(len(h1DiagnosticTerminal)))
		}
		_, _ = io.WriteString(w, h1DiagnosticTerminal)
		if mode == "content_length_immediate_eof" {
			return
		}
		w.(http.Flusher).Flush()
		if mode == "chunked_delayed_eof" || mode == "chunked_held_open" {
			delay := 100 * time.Millisecond
			if mode == "chunked_held_open" {
				delay = 3 * time.Second
			}
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				canceled.Add(1)
			}
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
			activeConnections.Add(1)
		}
		if state == http.StateClosed {
			activeConnections.Add(-1)
		}
	}
	server.Start()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.ForceAttemptHTTP2 = false
	client := &http.Client{Transport: transport}
	defer func() { transport.CloseIdleConnections(); server.Close() }()
	durations := make([]float64, 0, samples)
	for range samples {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				reused.Add(1)
			}
		}})
		req, err := http.NewRequestWithContext(ctx, "POST", server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		var response *http.Response
		if consumer == "gateway_terminal" {
			response, err = doStreamingRequestWithIdleTimeout(client, req, time.Second)
		} else {
			response, err = client.Do(req)
		}
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if response.ProtoMajor != 1 {
			cancel()
			t.Fatal("fixture negotiated non-H1 protocol")
		}
		body := &h1DiagnosticBody{ReadCloser: response.Body}
		response.Body = body
		var record RequestRecord
		scanner := &sseScanner{rec: &record}
		out := httptest.NewRecorder()
		if consumer == "read_to_eof_control" {
			_, err = io.Copy(io.MultiWriter(out, scanner), response.Body)
		} else {
			err = writeStreamPipe(out, http.NewResponseController(out), response, scanner, true)
		}
		duration := time.Since(started)
		if body.eof {
			result.ObservedEOF++
		}
		if scanner.terminal && out.Body.String() == h1DiagnosticTerminal {
			result.TerminalDelivered++
		}
		if mode == "chunked_delayed_eof" && duration < 75*time.Millisecond {
			result.ReturnedBeforeDelayedEOF++
		}
		_ = response.Body.Close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		durations = append(durations, float64(duration)/float64(time.Millisecond))
	}
	transport.CloseIdleConnections()
	server.Close()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for activeConnections.Load() != 0 || activeHandlers.Load() != 0 {
		select {
		case <-deadline.C:
			t.Fatal("fixture leaked an active handler or connection")
		case <-tick.C:
		}
	}
	sort.Float64s(durations)
	result.P50Millis, result.P95Millis = durations[len(durations)/2], durations[len(durations)-1]
	result.NewConnections, result.Reused, result.CanceledHandlers = connections.Load(), reused.Load(), canceled.Load()
	result.ActiveHandlersAfterClose, result.ActiveConnectionsAfterClose = activeHandlers.Load(), activeConnections.Load()
	if result.TerminalDelivered != samples {
		t.Fatal("a terminal event was lost")
	}
	if mode == "chunked_delayed_eof" || mode == "chunked_held_open" {
		if consumer != "read_to_eof_control" && (result.Reused != 0 || result.ObservedEOF != 0 || result.CanceledHandlers != int32(samples)) {
			t.Fatal("held-open body did not close and cancel as expected")
		}
	}
	if consumer == "read_to_eof_control" && (result.Reused != int32(samples-1) || result.ObservedEOF != samples) {
		t.Fatal("fully consumed H1 responses were not reused")
	}
	return result
}
