package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type collabStructuredFailOnceWriter struct {
	header   http.Header
	writes   int
	accepted bytes.Buffer
}

func (w *collabStructuredFailOnceWriter) Header() http.Header { return w.header }
func (w *collabStructuredFailOnceWriter) WriteHeader(int)     {}
func (w *collabStructuredFailOnceWriter) Flush()              {}
func (w *collabStructuredFailOnceWriter) Write(raw []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		return 0, errors.New("synthetic downstream write failure private-marker")
	}
	return w.accepted.Write(raw)
}

func TestCollabStructuredWriteErrorNotValidationFailure(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		bpsStructuredUpstreamReply(t, w, r, "resp_write_failure", `{"number":9007199254740993}`)
	})
	w := &collabStructuredFailOnceWriter{header: make(http.Header)}
	var aborted bool
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					aborted = true
				} else {
					panic(recovered)
				}
			}
		}()
		p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":"synthetic","text":`+bpsStructuredTestText+`}`))
	}()
	record, ok := p.Metrics.LastFor("test")
	t.Logf("calls=%d writes=%d aborted=%t bytes_after_failure=%d record=%t source=%s wire=%s", calls.Load(), w.writes, aborted, w.accepted.Len(), ok, record.ErrorSource, w.accepted.String())
	if w.writes != 1 || w.accepted.Len() != 0 || !aborted {
		t.Error("downstream I/O failure was retried as a synthetic validation terminal")
	}
	if strings.Contains(w.accepted.String(), "response_validation_failed") || strings.Contains(w.accepted.String(), "private-marker") {
		t.Error("transport error was mislabeled or exposed as validation failure")
	}
	if calls.Load() != 1 || !ok || record.InputTokens != 100 || record.CachedTokens != 80 {
		t.Error("generation repeated or real usage lost")
	}
}

func TestCollabStructuredIncompleteTransportNotValidationFailure(t *testing.T) {
	for _, mode := range []string{"eof", "truncated", "cancelled", "upstream_failed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				switch mode {
				case "cancelled":
					cancel()
					return
				case "eof":
					_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"partial\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
				case "truncated":
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":")
				case "upstream_failed":
					_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"real_failed\",\"status\":\"failed\",\"output\":[],\"error\":{\"code\":\"server_error\",\"message\":\"real synthetic upstream failure\"},\"usage\":{\"input_tokens\":37}}}\n\n")
				}
			})
			w := httptest.NewRecorder()
			aborted := false
			func() {
				defer func() {
					if got := recover(); got != nil {
						if got == http.ErrAbortHandler {
							aborted = true
						} else {
							panic(got)
						}
					}
				}()
				r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":"synthetic","text":`+bpsStructuredTestText+`}`).WithContext(ctx)
				p.ServeHTTP(w, r)
			}()
			t.Logf("mode=%s calls=%d status=%d abort=%t body=%s", mode, calls.Load(), w.Code, aborted, w.Body.String())
			if strings.Contains(w.Body.String(), "response_validation_failed") || strings.Contains(w.Body.String(), "response.completed") {
				t.Error("non-validation failure became fabricated validation/success terminal")
			}
			if mode == "upstream_failed" && (!strings.Contains(w.Body.String(), "server_error") || !strings.Contains(w.Body.String(), "real_failed")) {
				t.Error("real upstream failure changed")
			}
			if calls.Load() != 1 {
				t.Fatal("request retried")
			}
		})
	}
}
