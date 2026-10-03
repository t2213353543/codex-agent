package server

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayResponseWriterRecordsClientHTTPStatus(t *testing.T) {
	const complete = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_status\",\"status\":\"completed\",\"output\":[]}}\n\n"
	const failed = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"model failed\"}}}\n\n"
	for _, tc := range []struct {
		name           string
		method         string
		path           string
		request        string
		upstreamStatus int
		upstreamBody   string
		wantHTTP       int
		wantStatus     string
		wantCalls      int32
	}{
		{"stream_success", "POST", "/v1/responses", `{"model":"test","stream":true}`, 200, complete, 200, "ok", 1},
		{"collected_json_success", "POST", "/v1/responses", `{"model":"test","stream":false}`, 200, complete, 200, "ok", 1},
		{"compact_json_success", "POST", "/v1/responses/compact", `{"model":"test","input":[]}`, 200, `{"object":"response.compaction","output":[]}`, 200, "ok", 1},
		{"models_success", "GET", "/v1/models", "", 200, `{"models":[]}`, 200, "ok", 1},
		{"models_empty_success", "GET", "/v1/models", "", 200, "", 200, "ok", 1},
		{"models_not_modified", "GET", "/v1/models", "", 304, "", 304, "ok", 1},
		{"upstream_unauthorized", "POST", "/v1/responses", `{"model":"test","stream":true}`, 401, `{"error":{"message":"token revoked","code":"token_revoked"}}`, 401, "error", 1},
		{"upstream_rate_limited", "POST", "/v1/responses", `{"model":"test","stream":true}`, 429, `{"error":{"message":"rate limited"}}`, 429, "error", 1},
		{"invalid_client_body", "POST", "/v1/responses", `{"input":[]}`, 200, complete, 400, "error", 0},
		{"damaged_nonstream_completion", "POST", "/v1/responses", `{"model":"test","stream":false}`, 200, "data: {\"type\":\"response.completed\",\"response\":null}\n\n", 502, "error", 1},
		{"damaged_compact_json", "POST", "/v1/responses/compact", `{"model":"test","input":[]}`, 200, "not-json", 502, "error", 1},
		{"stream_model_failure_after_http_success", "POST", "/v1/responses", `{"model":"test","stream":true}`, 200, failed, 200, "error", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.upstreamStatus)
				_, _ = io.WriteString(w, tc.upstreamBody)
			})
			// A terminal 401 with no refresh credential must reach the client;
			// credential refresh and retry have their own behavioral tests.
			if tc.upstreamStatus == http.StatusUnauthorized {
				if err := p.Store.UpdateAccount("test", func(a *Account) error {
					a.RefreshToken = ""
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, proxyRequest(tc.method, tc.path, tc.request))
			if w.Code != tc.wantHTTP {
				t.Fatalf("client HTTP status = %d, want %d; body=%s", w.Code, tc.wantHTTP, w.Body)
			}
			page, err := p.Metrics.Logs(LogQuery{Size: 10})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("request records: count=%d err=%v", len(page.Items), err)
			}
			rec := page.Items[0]
			if rec.HTTPCode != w.Code || rec.Status != tc.wantStatus {
				t.Errorf("stored HTTP/status = %d/%s, client HTTP=%d, want outcome=%s", rec.HTTPCode, rec.Status, w.Code, tc.wantStatus)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("upstream requests = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// Both FlushError and write deadlines are used by writeStreamPipe. The status
// wrapper must preserve controller access to the underlying writer capabilities.
func TestGatewayResponseWriterPreservesResponseController(t *testing.T) {
	const stream = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, stream)
	})
	w := &gatewayControllerProbe{ResponseRecorder: httptest.NewRecorder()}
	p.ServeHTTP(w, proxyRequest("POST", "/v1/responses", `{"model":"test","stream":true}`))
	if w.Code != 200 || w.Body.String() != stream || !w.Flushed {
		t.Fatalf("stream forwarding changed: HTTP=%d flushed=%t body=%q", w.Code, w.Flushed, w.Body.String())
	}
	if w.flushes < 2 || w.deadlines == 0 {
		t.Fatalf("controller failed to reach underlying writer: flushes=%d deadlines=%d", w.flushes, w.deadlines)
	}
	rec, ok := p.Metrics.LastFor("test")
	if !ok || rec.HTTPCode != 200 || rec.Status != "ok" {
		t.Fatalf("stream record: found=%t HTTP=%d status=%s", ok, rec.HTTPCode, rec.Status)
	}
}

type gatewayControllerProbe struct {
	*httptest.ResponseRecorder
	flushes   int
	deadlines int
}

func (w *gatewayControllerProbe) FlushError() error {
	w.flushes++
	w.ResponseRecorder.Flush()
	return nil
}

func (w *gatewayControllerProbe) SetWriteDeadline(time.Time) error {
	w.deadlines++
	return nil
}

func TestGatewayResponseWriterFlushesBeforeUpstreamCompletion(t *testing.T) {
	const first = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n"
	const terminal = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("fixture flush: %v", err)
			return
		}
		select {
		case <-release:
			_, _ = io.WriteString(w, terminal)
		case <-r.Context().Done():
		}
	})
	finished := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(gateway.Close)
	t.Cleanup(unblock)
	client := gateway.Client()
	client.Timeout = 5 * time.Second
	r, err := http.NewRequest("POST", gateway.URL+"/v1/responses", strings.NewReader(`{"model":"test","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer test-client-key")
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != strings.SplitAfter(first, "\n")[0] {
		t.Fatalf("first event was not delivered while upstream remained open: line=%q err=%v", line, err)
	}
	select {
	case <-finished:
		t.Fatal("gateway completed before upstream emitted a terminal event")
	default:
	}
	unblock()
	rest, err := io.ReadAll(reader)
	if err != nil || line+string(rest) != first+terminal {
		t.Fatalf("stream bytes changed: err=%v body=%q", err, line+string(rest))
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not finish after the terminal event")
	}
	rec, ok := p.Metrics.LastFor("test")
	if res.StatusCode != 200 || !ok || rec.HTTPCode != 200 || rec.Status != "ok" {
		t.Fatalf("stream outcome: HTTP=%d record_found=%t record_HTTP=%d status=%s", res.StatusCode, ok, rec.HTTPCode, rec.Status)
	}
}
