package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGatewayFailoverAbortRecordPreservesFailureAndPanic(t *testing.T) {
	for _, status := range []string{"error", "aborted", "ok", ""} {
		t.Run("status_"+status, func(t *testing.T) {
			p := newTestProxy(t, 1)
			writer := httptest.NewRecorder()
			closed := 0
			result := newAppServerConnectionResult()
			result.bind(func(success bool) {
				if success {
					t.Error("aborted connection reused")
				}
				closed++
			})
			var original any = http.ErrAbortHandler
			if status == "ok" {
				original = errors.New("private panic text must never enter metrics")
			}
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				p.gatewayWithAccountFailover(writer, proxyRequest(http.MethodPost, "/v1/responses", `{}`), func(w http.ResponseWriter, r *http.Request) {
					state := officialFailoverFrom(r.Context())
					usageKnown, cachedKnown := true, true
					write := int64(7)
					record := RequestRecord{Account: "test", Status: status, InputTokens: 100, CachedTokens: 80, CacheWriteTokens: &write, UsageKnown: &usageKnown, CachedTokensKnown: &cachedKnown}
					if status == "error" || status == "aborted" {
						record.Error = "observed business failure"
						record.ErrorSource = "upstream"
						record.ErrorPhase = "response_event"
					}
					defer func() { state.record = &record; state.key = "admin" }()
					state.connectionResult = result
					panic(original)
				})
			}()
			if recovered != original {
				t.Fatalf("original abort/panic replaced: %v", recovered)
			}
			got, ok := p.Metrics.LastFor("test")
			wantStatus := status
			if status == "" || status == "ok" {
				wantStatus = "aborted"
			}
			if !ok || got.Status != wantStatus || got.InputTokens != 100 || got.CachedTokens != 80 || got.CacheWriteTokens == nil || *got.CacheWriteTokens != 7 {
				t.Fatalf("abort observation lost: %+v", got)
			}
			if got.HTTPCode != 0 || len(got.Attempts) != 1 || got.Attempts[0].StatusCode != nil {
				t.Fatalf("invented HTTP status: %+v", got)
			}
			if status == "error" || status == "aborted" {
				if got.Error != "observed business failure" || got.ErrorSource != "upstream" || got.ErrorPhase != "response_event" {
					t.Fatalf("business error replaced: %+v", got)
				}
			}
			if strings.Contains(got.Error, "private panic") || got.Error == "" {
				t.Fatal("panic text leaked or empty failure observation")
			}
			if closed != 1 || !result.confirmed || result.success || writer.Body.Len() != 0 {
				t.Fatalf("cleanup or wire changed: closed=%d body=%s", closed, writer.Body.String())
			}
			if p.Metrics.Totals(0, time.Now().Add(time.Minute).Unix()).Requests != 1 {
				t.Fatal("aborted request recorded more than once")
			}
		})
	}
}

func TestGatewayFailoverAbortRecordRetainsEarlierAttemptsAndTTFT(t *testing.T) {
	p := newTestProxy(t, 1)
	writer := httptest.NewRecorder()
	calls, closed := 0, 0
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		p.gatewayWithAccountFailover(writer, proxyRequest(http.MethodPost, "/v1/responses", `{}`), func(w http.ResponseWriter, r *http.Request) {
			calls++
			state := officialFailoverFrom(r.Context())
			result := newAppServerConnectionResult()
			result.bind(func(success bool) {
				if success {
					t.Error("failed attempt connection reused")
				}
				closed++
			})
			state.connectionResult = result
			record := RequestRecord{Account: "test", Status: "error", Error: "synthetic quota", HTTPCode: 429}
			defer func() { state.record = &record; state.key = "admin" }()
			if calls == 1 {
				state.account = Account{ID: "first-account"}
				state.failure = &officialProtocolError{message: "synthetic quota", status: 429, code: "rateLimitExceeded", source: "upstream"}
				time.Sleep(15 * time.Millisecond)
				return
			}
			if calls != 2 {
				t.Fatal("unexpected replay after abnormal exit")
			}
			ttft := int64(7)
			record = RequestRecord{Account: "test", Status: "error", Error: "structured output rejected", HTTPCode: 200, InputTokens: 100, TTFTMs: &ttft}
			panic(http.ErrAbortHandler)
		})
	}()
	if recovered != http.ErrAbortHandler || calls != 2 || closed != 2 {
		t.Fatalf("abort cleanup mismatch: panic=%v calls=%d closed=%d", recovered, calls, closed)
	}
	got, ok := p.Metrics.LastFor("test")
	if !ok || len(got.Attempts) != 2 || got.Attempts[0].ErrorCode != "rateLimitExceeded" || got.Attempts[0].StatusCode == nil || *got.Attempts[0].StatusCode != 429 || got.Attempts[1].StatusCode == nil || *got.Attempts[1].StatusCode != 200 {
		t.Fatalf("attempt history lost: %+v", got)
	}
	if got.TTFTMs == nil || *got.TTFTMs < 20 || got.InputTokens != 100 || got.Status != "error" {
		t.Fatalf("preceding duration or final usage lost: %+v", got)
	}
	if p.Metrics.Totals(0, time.Now().Add(time.Minute).Unix()).Requests != 1 || writer.Body.Len() != 0 {
		t.Fatal("replayed or wrote partial failure")
	}
}

func TestGatewayFailoverAbortRecordKeepsOriginalPanicWhenAccountingPanics(t *testing.T) {
	p := newTestProxy(t, 1)
	p.Metrics = &Metrics{} // Invalid isolated sink makes Record panic on a nil map.
	original := errors.New("original private abort")
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		p.gatewayWithAccountFailover(httptest.NewRecorder(), proxyRequest(http.MethodPost, "/v1/responses", `{}`), func(_ http.ResponseWriter, r *http.Request) {
			state := officialFailoverFrom(r.Context())
			defer func() { state.record = &RequestRecord{Account: "test"} }()
			panic(original)
		})
	}()
	if recovered != original {
		t.Fatalf("accounting failure replaced original panic: %v", recovered)
	}
}

func TestGatewayFailoverAbortRecordHandlesGoexitAndMissingRecord(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_record", true: "goexit"}[publish], func(t *testing.T) {
			p := newTestProxy(t, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.gatewayWithAccountFailover(httptest.NewRecorder(), proxyRequest(http.MethodPost, "/v1/responses", `{}`), func(_ http.ResponseWriter, r *http.Request) {
					if publish {
						state := officialFailoverFrom(r.Context())
						defer func() { state.record = &RequestRecord{Account: "test", Status: "ok"} }()
					}
					runtime.Goexit()
				})
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Goexit did not finish")
			}
			got, ok := p.Metrics.LastFor("test")
			if ok != publish || ok && (got.Status != "aborted" || strings.Contains(got.Error, "panic")) {
				t.Fatalf("invented exit reason or lost record: %+v %v", got, ok)
			}
		})
	}
}
