package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Preserve the established machine classification independently of deciding
// whether StatusCode denotes actual HTTP status or logical terminal status.
func TestBPSOAuthRateLimitProvenance(t *testing.T) {
	for _, mode := range []string{"http_429", "sse_http200_failed429"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "http_429" {
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"synthetic quota"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","response":{"id":"failed-quota","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"synthetic quota"},"usage":{"input_tokens":37,"output_tokens":0}}}`+"\n\n")
			})
			// Opaque state blocks cross-account replay without changing cooldown.
			r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":[{"type":"reasoning","encrypted_content":"synthetic-cipher"}]}`)
			w := httptest.NewRecorder()
			p.gatewayBPS(w, r)
			record, ok := p.Metrics.LastFor("test")
			if calls != 1 || !ok || len(record.Attempts) != 1 {
				t.Fatalf("boundary changed: calls=%d record=%+v", calls, record)
			}
			a := record.Attempts[0]
			t.Logf("mode=%s client_http=%d attempt_status=%v attempt_code=%q attempt_source=%q record_error=%q input=%d", mode, w.Code, a.StatusCode, a.ErrorCode, a.ErrorSource, record.Error, record.InputTokens)
			if a.ErrorCode != "rateLimitExceeded" || a.ErrorSource != "upstream" {
				t.Errorf("recognized terminal machine error lost: %+v", a)
			}
			if mode == "http_429" && (a.StatusCode == nil || *a.StatusCode != 429) {
				t.Error("real HTTP429 status lost")
			}
			if mode == "sse_http200_failed429" && (a.StatusCode == nil || *a.StatusCode != 429) {
				t.Error("v3 must preserve the original logical SSE429 status contract")
			}
			observedHTTP := 429
			if mode == "sse_http200_failed429" {
				observedHTTP = 200
			}
			if a.ObservedHTTPStatusCode == nil || *a.ObservedHTTPStatusCode != observedHTTP {
				t.Errorf("observed transport status lost: got=%v want=%d", a.ObservedHTTPStatusCode, observedHTTP)
			}
			if mode == "sse_http200_failed429" && record.InputTokens != 37 {
				t.Error("actual failed usage lost")
			}
		})
	}
}
