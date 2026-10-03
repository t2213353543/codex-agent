package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const cancelPendingFailure = `{"type":"response.failed","headers":{"Retry-After":"600"},"response":{"id":"cancel-a","status":"failed","error":{"code":"rate_limit_exceeded","message":"pending quota must not commit"},"usage":{"input_tokens":777,"output_tokens":333,"total_tokens":1110}}}`

// A real upstream stream observes cancellation; pending failure remains data,
// not a final account error, cooldown or usage record when the caller cancels.
func TestHTTPCancelPendingFailureDoesNotBecomeUpstreamCredentialError(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", transport, stream), func(t *testing.T) {
				if transport == "native" && !stream {
					t.Skip("native JSON caller body interruption covered by deadline/next-request case; this cancel case needs a read barrier")
				}
				firstSent, upstreamDone := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+cancelPendingFailure+"\n\n")
					w.(http.Flusher).Flush()
					close(firstSent)
					<-r.Context().Done()
					close(upstreamDone)
				})
				if transport == "native" {
					realGatewayNativePool(t, p)
				}
				bodyWaiting := make(chan struct{}, 1)
				if !stream {
					p.Client.Transport = cancelBoundaryTransport{base: p.Client.Transport, waiting: bodyWaiting}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				req := proxyRequest("POST", "/responses", fmt.Sprintf(`{"model":"test","stream":%v}`, stream)).WithContext(ctx)
				w := &cancelBoundaryWriter{ResponseRecorder: httptest.NewRecorder(), waiting: bodyWaiting}
				finished := make(chan any, 1)
				go func() { defer func() { finished <- recover() }(); p.ServeHTTP(w, req) }()
				select {
				case <-firstSent:
				case <-time.After(3 * time.Second):
					t.Fatal("upstream never started")
				}
				select {
				case <-bodyWaiting:
				case <-time.After(3 * time.Second):
					t.Fatal("response body read was not observed")
				}
				cancel()
				select {
				case value := <-finished:
					if value != nil && value != http.ErrAbortHandler {
						t.Fatalf("unexpected panic: %v", value)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("cancel did not stop handler")
				}
				select {
				case <-upstreamDone:
				case <-time.After(time.Second):
					t.Fatal("upstream socket not released")
				}
				rec, ok := p.Metrics.LastFor("test")
				if !ok {
					t.Fatal("missing request record")
				}
				if rec.Status != "aborted" || rec.ErrorSource != "client" || rec.ErrorPhase != "canceled" {
					t.Errorf("caller cancellation was attributed to upstream: status=%s source=%s phase=%s error=%q", rec.Status, rec.ErrorSource, rec.ErrorPhase, rec.Error)
				}
				if rec.InputTokens != 0 || rec.OutputTokens != 0 || rec.TerminalStatus != "" || rec.httpCompleted {
					t.Errorf("pending failure finalized on cancellation: %+v", rec)
				}
				account, _ := p.Store.Get("test")
				if p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "test"), account) != nil {
					t.Error("pending quota poisoned next request")
				}
				if calls.Load() != 1 || p.inFlight("test") != 0 {
					t.Error("cancel replayed or retained admission")
				}
			})
		}
	}
}

type cancelBoundaryWriter struct {
	*httptest.ResponseRecorder
	waiting chan struct{}
}

func (w *cancelBoundaryWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if n > 0 {
		select {
		case w.waiting <- struct{}{}:
		default:
		}
	}
	return n, err
}

type cancelBoundaryTransport struct {
	base    http.RoundTripper
	waiting chan struct{}
}

func (t cancelBoundaryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(r)
	if err == nil {
		res.Body = &cancelBoundaryObservedBody{ReadCloser: res.Body, waiting: t.waiting}
	}
	return res, err
}

type cancelBoundaryObservedBody struct {
	io.ReadCloser
	read    bool
	waiting chan struct{}
}

func (b *cancelBoundaryObservedBody) Read(p []byte) (int, error) {
	if b.read {
		select {
		case b.waiting <- struct{}{}:
		default:
		}
	}
	n, err := b.ReadCloser.Read(p)
	b.read = b.read || n > 0
	return n, err
}

func TestHTTPPendingErrorReadFailuresKeepAccountingUncommitted(t *testing.T) {
	for _, cause := range []error{io.ErrUnexpectedEOF, errUpstreamStreamIdleTimeout, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			var record RequestRecord
			var observed [][]byte
			reader := &cancelBoundaryReader{first: []byte("data: " + cancelPendingFailure + "\n\n"), cause: cause}
			result, err := collectResponseObserved(reader, func(data []byte) { observed = append(observed, append([]byte(nil), data...)) }, &record)
			if result != nil || !errors.Is(err, cause) || len(observed) != 0 {
				t.Fatalf("read failure was replaced by pending failure: %v", err)
			}
			if record.InputTokens != 0 || record.OutputTokens != 0 || record.TerminalStatus != "" || record.httpCompleted {
				t.Fatal("read failure committed pending accounting")
			}
			if record.ResponseID != "cancel-a" {
				t.Fatal("pending response identity lost")
			}
		})
	}
}

type cancelBoundaryReader struct {
	first []byte
	cause error
}

func (r *cancelBoundaryReader) Read(p []byte) (int, error) {
	if len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		return n, nil
	}
	return 0, r.cause
}

func TestHTTPPendingErrorCleanEOFKeepsActualFinalFailure(t *testing.T) {
	var record RequestRecord
	result, err := collectResponseObserved(strings.NewReader("data: "+cancelPendingFailure+"\n\n"), nil, &record)
	var upstream *upstreamResponseError
	if result != nil || !errors.As(err, &upstream) || !strings.Contains(string(upstream.Payload), "rate_limit_exceeded") {
		t.Fatalf("normal EOF lost pending error: %v", err)
	}
}

func TestHTTPReadCancellationRequiresMatchingCauseAndUnfinishedResponse(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	for _, test := range []struct {
		name   string
		ctx    context.Context
		cause  error
		record RequestRecord
		want   bool
		phase  string
	}{
		{"caller_cancel", canceled, fmt.Errorf("read: %w", context.Canceled), RequestRecord{}, true, "canceled"},
		{"caller_deadline", expired, fmt.Errorf("read: %w", context.DeadlineExceeded), RequestRecord{}, true, "timeout"},
		{"internal_deadline", t.Context(), context.DeadlineExceeded, RequestRecord{}, false, ""},
		{"internal_cancel", t.Context(), context.Canceled, RequestRecord{}, false, ""},
		{"late_cancel_after_truncation", canceled, io.ErrUnexpectedEOF, RequestRecord{Status: "error", Error: "actual upstream truncation", ErrorSource: "upstream"}, false, ""},
		{"late_cancel_after_idle", canceled, errUpstreamStreamIdleTimeout, RequestRecord{}, false, ""},
		{"late_deadline_after_native_total", expired, gatewayNativeError("total_timeout"), RequestRecord{}, false, ""},
		{"opposite_cancel_cause", canceled, context.DeadlineExceeded, RequestRecord{}, false, ""},
		{"complete_before_cancel", canceled, context.Canceled, RequestRecord{Status: "ok", TerminalStatus: "completed", httpCompleted: true, InputTokens: 19}, false, ""},
		{"typed_failure_before_cancel", canceled, context.Canceled, RequestRecord{Status: "error", TerminalStatus: "failed", Error: "actual final failure", InputTokens: 17}, false, ""},
		{"parser_failure_before_cancel", canceled, context.Canceled, RequestRecord{Status: "error", Error: "upstream SSE contains invalid UTF-8"}, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, _ := json.Marshal(test.record)
			got := recordGatewayHTTPReadCancellation(&test.record, test.ctx, test.cause)
			if got != test.want {
				t.Fatalf("classification %v want %v", got, test.want)
			}
			if got {
				if test.record.Status != "aborted" || test.record.ErrorSource != "client" || test.record.ErrorPhase != test.phase {
					t.Fatal("caller attribution lost")
				}
			} else {
				after, _ := json.Marshal(test.record)
				if string(before) != string(after) {
					t.Fatal("unrelated/final record mutated")
				}
			}
		})
	}
}

func TestHTTPCancelBoundaryParserErrorPrecedesLaterCancellation(t *testing.T) {
	var rec RequestRecord
	s := &sseScanner{rec: &rec}
	_, _ = s.Write([]byte("data: " + cancelPendingFailure + "\n\n"))
	_, _ = s.Write([]byte("data: \xff\n\n"))
	if rec.Status != "error" || !strings.Contains(rec.Error, "UTF-8") || s.terminal {
		t.Fatal("fixture did not observe nonterminal decoder failure")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if recordGatewayHTTPReadCancellation(&rec, ctx, fmt.Errorf("upstream stream read failed: %w", context.Canceled)) {
		t.Fatal("later caller cancellation replaced already observed parser error")
	}
	if rec.Status != "error" || !strings.Contains(rec.Error, "UTF-8") || rec.InputTokens != 0 || rec.TerminalStatus != "" {
		t.Fatalf("parser error or pending accounting changed: %+v", rec)
	}
}

// Include the compact adapter and a real caller deadline. A subsequent request
// on the same account/session must retain its own ID, usage and cache metadata.
func TestHTTPCancelBoundaryDeadlineCompactAndNextRequest(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, route := range []string{"json", "sse", "compact_v2"} {
			t.Run(transport+"/"+route, func(t *testing.T) {
				var calls atomic.Int32
				firstSent, upstreamDone := make(chan struct{}), make(chan struct{})
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					if strings.Contains(string(raw), "cancel-a") || strings.Contains(string(raw), "pending quota") {
						t.Error("old response entered next request body")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if calls.Add(1) == 1 {
						_, _ = io.WriteString(w, "data: "+cancelPendingFailure+"\n\n")
						w.(http.Flusher).Flush()
						close(firstSent)
						<-r.Context().Done()
						close(upstreamDone)
						return
					}
					_, _ = io.WriteString(w, "data: "+strings.Replace(sequenceCompleted, "sequence-a", "next-clean", 1)+"\n\n")
				})
				if route == "compact_v2" {
					p.BaseURL += "/backend-api/codex"
				}
				if transport == "native" {
					realGatewayNativePool(t, p)
				}
				var err error
				p.PromptCacheObserver, err = newGatewayPromptCacheObserver()
				if err != nil {
					t.Fatal(err)
				}
				accountBefore, _ := p.Store.Get("test")
				ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
				defer cancel()
				path := "/responses"
				if route == "compact_v2" {
					path = "/responses/compact"
				}
				r := proxyRequest("POST", path, fmt.Sprintf(`{"model":"test","stream":%v,"prompt_cache_key":"cancel-cache","input":[]}`, route == "sse")).WithContext(ctx)
				r.Header.Set("Session-Id", "cancel-session")
				r.Header.Set("Thread-Id", "cancel-thread")
				w := httptest.NewRecorder()
				finished := make(chan any, 1)
				go func() { defer func() { finished <- recover() }(); p.ServeHTTP(w, r) }()
				select {
				case <-firstSent:
				case <-time.After(3 * time.Second):
					t.Fatal("deadline fixture never dispatched")
				}
				select {
				case value := <-finished:
					if value != nil && value != http.ErrAbortHandler {
						t.Fatalf("unexpected panic: %v", value)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("deadline retained handler")
				}
				select {
				case <-upstreamDone:
				case <-time.After(time.Second):
					t.Fatal("deadline retained upstream")
				}
				rec, _ := p.Metrics.LastFor("test")
				if rec.Status != "aborted" || rec.ErrorSource != "client" || rec.ErrorPhase != "timeout" || rec.InputTokens != 0 || rec.TerminalStatus != "" {
					t.Fatalf("deadline misattributed or committed pending usage: %+v", rec)
				}
				account, _ := p.Store.Get("test")
				if account.CredentialStatus != accountBefore.CredentialStatus || account.LastError != accountBefore.LastError || account.CredentialVersion != accountBefore.CredentialVersion {
					t.Fatal("cancellation mutated credential health")
				}
				statuses, err := p.managedAccountLastStatuses(t.Context(), []Account{account})
				if err != nil || statuses[account.ID] != "aborted" {
					t.Fatalf("current-credential request status: %v %v", statuses, err)
				}
				if rec.PromptCache == nil || p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "test"), account) != nil || p.inFlight("test") != 0 {
					t.Fatal("pending response polluted cooldown or admission")
				}
				next := proxyRequest("POST", "/responses", `{"model":"test","stream":false,"prompt_cache_key":"cancel-cache","input":[]}`)
				next.Header.Set("Session-Id", "cancel-session")
				next.Header.Set("Thread-Id", "cancel-thread")
				answer := httptest.NewRecorder()
				p.ServeHTTP(answer, next)
				final, _ := p.Metrics.LastFor("test")
				if answer.Code != 200 || final.Status != "ok" || final.ResponseID != "next-clean" || final.InputTokens != 19 || final.OutputTokens != 7 || final.PromptCache == nil || final.PromptCache.AccountChanged || final.PromptCache.CacheKeyChanged {
					t.Fatalf("next independent response inherited canceled state: %+v", final)
				}
				if final.PromptCache.CacheKeyHash != rec.PromptCache.CacheKeyHash || calls.Load() != 2 || p.inFlight("test") != 0 {
					t.Fatal("canceled response changed cache scope or retried")
				}
			})
		}
	}
}

func TestHTTPCancelBoundaryActualTruncationRemainsUpstream(t *testing.T) {
	for _, transport := range []string{"go", "native"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", transport, stream), func(t *testing.T) {
				p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Content-Length", "9000")
					_, _ = io.WriteString(w, "data: "+cancelPendingFailure+"\n\n")
					w.(http.Flusher).Flush() // Return before the promised body is complete.
				})
				if transport == "native" {
					realGatewayNativePool(t, p)
				}
				w := httptest.NewRecorder()
				func() {
					defer func() {
						if value := recover(); value != nil && value != http.ErrAbortHandler {
							panic(value)
						}
					}()
					p.ServeHTTP(w, proxyRequest("POST", "/responses", fmt.Sprintf(`{"model":"test","stream":%v}`, stream)))
				}()
				rec, _ := p.Metrics.LastFor("test")
				if rec.ErrorSource != "upstream" || rec.ErrorPhase != "response_body" || rec.Status == "ok" || rec.InputTokens != 0 || rec.TerminalStatus != "" {
					t.Fatalf("truncation confused with caller cancellation or pending failure: %+v", rec)
				}
				account, _ := p.Store.Get("test")
				if p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "test"), account) != nil || p.inFlight("test") != 0 {
					t.Fatal("truncated pending response poisoned account or admission")
				}
			})
		}
	}
}

func TestHTTPCancelBoundaryActualIdleTimeoutIsNotCallerDeadline(t *testing.T) {
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+cancelPendingFailure+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", server.URL, nil)
	res, err := doStreamingRequestWithIdleTimeout(server.Client(), req, 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var rec RequestRecord
	var observed int
	result, err := collectResponseObserved(res.Body, func([]byte) { observed++ }, &rec)
	if result != nil || !errors.Is(err, errUpstreamStreamIdleTimeout) || recordGatewayHTTPReadCancellation(&rec, req.Context(), err) || observed != 0 || rec.TerminalStatus != "" || rec.InputTokens != 0 {
		t.Fatalf("idle timeout committed pending or attributed to caller: %v %+v", err, rec)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("idle deadline retained upstream")
	}
}
