package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// One real loopback response and real session CAS conflict. The first usable
// text arrives inside a failed terminal, after the typed quota was observed.
// Retaining the true upstream failure is valid; it must not override a later
// explicit gateway/session_binding failure in the management category.
func produceFinalFailureSessionConflict(t *testing.T) RequestRecord {
	t.Helper()
	var p *Proxy
	var calls atomic.Int32
	var originalOwner string
	const session = "synthetic-failed-terminal-cas"
	p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		actual, changed, err := p.switchSessionAccount(r.Context(), "admin", session, originalOwner, "synthetic-foreign-winner")
		if err != nil || !changed || actual != "synthetic-foreign-winner" {
			t.Error("synthetic concurrent owner not installed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"response.failed","response":{"id":"synthetic-failed-cas-response","model":"gpt-6-astra","status":"failed","output":[{"type":"message","id":"synthetic-message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"SYNTHETIC_NOT_DELIVERED"}]}],"error":{"code":null,"type":"rate_limit_error","message":"synthetic quota"},"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":80}}}}`+"\n\n")
	})
	credentials, _ := json.Marshal(map[string]any{"access_token": "synthetic-replacement-token", "chatgpt_account_id": "synthetic-workspace", "expires_at": time.Now().Add(time.Hour).Unix()})
	if err := p.Store.ImportWith("replacement", credentials, AccountDefaults{Concurrency: 2, Priority: 10, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("replacement", func(a *Account) error {
		a.Identity = &managedAccountIdentity{UserID: "synthetic-user", WorkspaceID: "synthetic-workspace"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	originalOwner = gatewayAccountRef(original)
	if _, err := p.sessionAccount(t.Context(), "admin", session, originalOwner); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	req := proxyRequest(http.MethodPost, "/v1/responses", `{"model":"gpt-6-astra","stream":true,"input":"synthetic plaintext"}`)
	req.Header.Set("Session-Id", session)
	out := httptest.NewRecorder()
	var recovered any
	func() { defer func() { recovered = recover() }(); p.gatewayBPS(out, req) }()
	rec, found := p.Metrics.LastFor("replacement")
	if !found {
		t.Fatalf("logical record absent: panic=%v posts=%d", recovered, calls.Load())
	}
	category := managedFailureCategory(rec)
	observation, _ := json.Marshal(map[string]any{
		"posts": calls.Load(), "attempts": len(rec.Attempts), "panic_abort_handler": recovered == http.ErrAbortHandler,
		"status": rec.Status, "http_code": rec.HTTPCode, "error_source": rec.ErrorSource, "error_phase": rec.ErrorPhase,
		"final_code": rec.FinalUpstreamErrorCode, "final_source": rec.FinalUpstreamErrorSource, "category": category,
		"visible_bytes": out.Body.Len(), "ttft_cleared": rec.TTFTMs == nil, "first_kind_empty": rec.FirstOutputKind == "",
		"is_cas_cause": strings.Contains(rec.Error, "session account changed"),
	})
	t.Logf("BOUNDARY_RESULT %s", observation)
	if recovered != http.ErrAbortHandler || calls.Load() != 1 || len(rec.Attempts) != 1 || out.Body.Len() != 0 || rec.Status != "error" || rec.ErrorSource != "gateway" || rec.ErrorPhase != "session_binding" || !strings.Contains(rec.Error, "session account changed") {
		t.Fatalf("proposed intersection not reached; inspect BOUNDARY_RESULT")
	}
	return rec
}

func TestBPSFinalClassificationFailedTerminalCAS(t *testing.T) {
	record := produceFinalFailureSessionConflict(t)
	if got := managedFailureCategory(record); got != "unknown" {
		t.Fatalf("local session conflict classified %s", got)
	}
}

func TestPGBPSFinalClassificationSessionConflict(t *testing.T) {
	metrics := newTestPGMetrics(t, newMetricsTestDB(t))
	record := produceFinalFailureSessionConflict(t)
	record.EventID = "final-failure-session-conflict"
	record.ID = 0
	metrics.Record(record)
	page, err := metrics.managedLogs(t.Context(), managedQuery(t, "error_category=unknown&exact_total=true"))
	if err != nil || len(page.Items) != 1 || page.Items[0].ErrorCategory != "unknown" || page.Items[0].FinalUpstreamErrorCode != "rateLimitExceeded" || page.Items[0].ErrorSource != "gateway" {
		t.Fatalf("local category/upstream provenance not preserved: %v", err)
	}
	stats, err := metrics.managedLogStats(t.Context(), managedQuery(t, "error_category=rate_limit"))
	if err != nil || stats.Requests != 0 {
		t.Fatalf("local conflict counted as upstream quota: %v", err)
	}
}
