package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Diagnostics only: the routing behavior is already covered by B's matrix.
// Reproduce a missing sticky owner without any real upstream generation.
func TestCollabSessionOwnerLocalAttemptClassification(t *testing.T) {
	for _, mode := range []string{"opaque-blocks-replay", "plaintext-no-candidate"} {
		t.Run(mode, func(t *testing.T) {
			var upstream atomic.Int32
			p := bpsGatewayFixture(t, func(http.ResponseWriter, *http.Request) { upstream.Add(1) })
			owner, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			const session = "synthetic-missing-owner-diagnostic"
			if _, err := p.sessionAccount(t.Context(), "admin", session, gatewayAccountRef(owner)); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			input := `[{"type":"message","role":"user","content":"synthetic fixture"}]`
			if mode == "opaque-blocks-replay" {
				input = `[{"type":"reasoning","encrypted_content":"synthetic-cipher"},{"type":"message","role":"user","content":"synthetic fixture"}]`
			}
			request := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":`+input+`}`)
			request.Header.Set("Session-Id", session)
			writer := httptest.NewRecorder()
			p.gatewayBPS(writer, request)
			if writer.Code != http.StatusServiceUnavailable || upstream.Load() != 0 || !strings.Contains(writer.Body.String(), "session account is no longer enabled or allowed") {
				t.Fatalf("diagnostic fixture boundary changed: status=%d actual_upstream_calls=%d", writer.Code, upstream.Load())
			}
			p.Metrics.mu.Lock()
			count := p.Metrics.recentAt
			record := p.Metrics.recent[0]
			p.Metrics.mu.Unlock()
			if count != 1 || p.Metrics.Totals(0, time.Now().Add(time.Minute).Unix()).Requests != 1 {
				t.Fatal("expected one request-level observation")
			}
			if record.ErrorSource != "gateway" || record.ErrorPhase != "account_selection" {
				t.Errorf("local selection source wrong: source=%q phase=%q", record.ErrorSource, record.ErrorPhase)
			}
			if len(record.Attempts) != 0 {
				t.Errorf("invented generation attempt for zero-dispatch local selection: %+v", record.Attempts)
			}
			for _, attempt := range record.Attempts {
				if attempt.ErrorSource == "upstream" || attempt.ErrorCode == "app_server_error" {
					t.Errorf("local owner failure mislabeled as upstream/app_server_error: %+v", attempt)
				}
			}
			if record.Account != "" || record.SentModel != "" || record.UpstreamPath != "" || record.InputTokens != 0 || record.OutputTokens != 0 {
				t.Fatal("local admission failure fabricated dispatch metadata")
			}
			t.Logf("mode=%s status=%d actual_upstream_calls=%d record_source=%s record_phase=%s recorded_attempts=%d", mode, writer.Code, upstream.Load(), record.ErrorSource, record.ErrorPhase, len(record.Attempts))
		})
	}
}

func TestCollabSessionOwnerLocalAttemptReplacementKeepsOnlyRealGeneration(t *testing.T) {
	var calls atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "real-replacement", "[]"))
	})
	owner, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	const session = "synthetic-local-then-real"
	if _, err := p.sessionAccount(t.Context(), "admin", session, gatewayAccountRef(owner)); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"access_token": "synthetic-replacement", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("second", raw, AccountDefaults{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	r := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":"synthetic plaintext"}`)
	r.Header.Set("Session-Id", session)
	w := httptest.NewRecorder()
	p.gatewayBPS(w, r)
	record, ok := p.Metrics.LastFor("second")
	if w.Code != 200 || calls.Load() != 1 || !ok || record.Status != "ok" || len(record.Attempts) != 1 {
		t.Fatalf("local rejection contaminated actual replacement accounting: status=%d calls=%d record=%+v", w.Code, calls.Load(), record)
	}
	if record.Attempts[0].StatusCode == nil || *record.Attempts[0].StatusCode != 200 || record.Attempts[0].ErrorCode != "" {
		t.Fatalf("real successful attempt mislabeled: %+v", record.Attempts)
	}
	if p.Metrics.Totals(0, time.Now().Add(time.Minute).Unix()).Requests != 1 {
		t.Fatal("replacement request counted more than once")
	}
}
