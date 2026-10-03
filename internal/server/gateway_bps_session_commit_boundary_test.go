package server

import (
	"bytes"
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

// These are complete gateway requests over a loopback upstream. The upstream
// commits a different session owner before its valid terminal is processed.
// No fabricated failure text or direct RequestRecord mutation is used.
func TestBPSSessionCommitReviewBufferedAndStructured(t *testing.T) {
	for _, mode := range []string{"json", "structured_json", "structured_sse", "compact_json"} {
		for _, conflict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/conflict_%t", mode, conflict), func(t *testing.T) {
				var p *Proxy
				var calls atomic.Int32
				const session = "commit-review-session"
				const responseID = "resp_commit_review"
				var oldOwner string
				p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if conflict {
						actual, changed, err := p.switchSessionAccount(r.Context(), "admin", session, oldOwner, "synthetic-foreign-winner")
						if err != nil || !changed || actual != "synthetic-foreign-winner" {
							t.Error("synthetic concurrent winner was not installed")
						}
					}
					if strings.HasPrefix(mode, "structured") {
						bpsStructuredUpstreamReply(t, w, r, responseID, `{"number":9007199254740993}`)
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					usage := map[string]any{"input_tokens": 100, "output_tokens": 10, "total_tokens": 110, "input_tokens_details": map[string]int{"cached_tokens": 80, "cache_write_tokens": 7}}
					output := []any{map[string]any{"type": "message", "id": "msg_commit_review", "role": "assistant", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": "PRIVATE_COMMIT_REVIEW_OUTPUT"}}}}
					if mode == "compact_json" {
						output = []any{map[string]string{"type": "compaction", "encrypted_content": "PRIVATE_COMMIT_REVIEW_CIPHER"}}
					}
					response := map[string]any{"id": responseID, "model": "gpt-6-astra", "status": "completed", "output": output, "usage": usage}
					if mode == "compact_json" {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"resp_commit_review","model":"gpt-6-astra","status":"in_progress","output":[]}}`+"\n\n")
					// JSON consumers observe but have not received this real upstream delta.
					_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","response_id":"resp_commit_review","delta":"PRIVATE_COMMIT_REVIEW_OUTPUT"}`+"\n\n")
					terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
					_, _ = fmt.Fprintf(w, "data: %s\n\n", terminal)
				})
				credentials, _ := json.Marshal(map[string]any{"access_token": "replacement-token", "chatgpt_account_id": "replacement-workspace", "expires_at": time.Now().Add(time.Hour).Unix()})
				if err := p.Store.ImportWith("replacement", credentials, AccountDefaults{Concurrency: 2, Priority: 10, Weight: 1}); err != nil {
					t.Fatal(err)
				}
				if err := p.Store.UpdateAccount("replacement", func(a *Account) error {
					a.Identity = &managedAccountIdentity{UserID: "replacement-user", WorkspaceID: "replacement-workspace"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				old, _ := p.Store.Get("test")
				oldOwner = gatewayAccountRef(old)
				if _, err := p.sessionAccount(t.Context(), "admin", session, oldOwner); err != nil {
					t.Fatal(err)
				}
				if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
					t.Fatal(err)
				}
				p.invalidateAccounts()
				path := "/v1/responses"
				body := `{"model":"gpt-6-astra","stream":false,"input":"synthetic plaintext"}`
				if strings.HasPrefix(mode, "structured") {
					body = fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"synthetic plaintext","text":%s}`, mode == "structured_sse", bpsStructuredTestText)
				}
				if mode == "compact_json" {
					path = "/v1/responses/compact"
					body = `{"model":"gpt-6-astra","input":[{"role":"user","content":"synthetic plaintext"}]}`
				}
				req := proxyRequest(http.MethodPost, path, body)
				req.Header.Set("Session-Id", session)
				out := httptest.NewRecorder()
				p.gatewayBPS(out, req)
				rec, found := p.Metrics.LastFor("replacement")
				if !found {
					t.Fatalf("request observation missing: status=%d body=%s", out.Code, out.Body)
				}
				wire, _ := json.Marshal(rec)
				t.Logf("mode=%s conflict=%t public_http=%d network_posts=%d record=%s", mode, conflict, out.Code, calls.Load(), wire)
				if calls.Load() != 1 || len(rec.Attempts) != 1 || rec.ObservationSchemaVersion != 2 {
					t.Error("one real generation was not observed exactly once")
				}
				if rec.Attempts[0].ObservedHTTPStatusCode == nil || *rec.Attempts[0].ObservedHTTPStatusCode != 200 {
					t.Error("true upstream HTTP 200 lost")
				}
				if rec.InputTokens != 100 || rec.OutputTokens != 10 || rec.CachedTokens != 80 || rec.UsageKnown == nil || !*rec.UsageKnown || rec.CachedTokensKnown == nil || !*rec.CachedTokensKnown || rec.CacheWriteTokens == nil {
					t.Error("actual terminal usage/cache observations lost")
				}
				wantWrite := int64(7)
				if strings.HasPrefix(mode, "structured") {
					wantWrite = 0
				}
				if rec.CacheWriteTokens != nil && *rec.CacheWriteTokens != wantWrite {
					t.Error("cache read and write counters were conflated")
				}
				if rec.ReturnedModel != "gpt-6-astra" || rec.FinalUpstreamErrorCode != "" || rec.FinalUpstreamErrorSource != "" {
					t.Error("returned model lost or gateway rejection mislabeled upstream")
				}
				if conflict {
					if rec.Status != "error" || rec.ErrorSource != "gateway" || rec.ErrorPhase != "session_binding" || rec.TTFTMs != nil || rec.FirstOutputKind != "" {
						t.Error("undelivered binding conflict misclassified")
					}
					if bytes.Contains(out.Body.Bytes(), []byte("PRIVATE_COMMIT_REVIEW")) || strings.Contains(out.Body.String(), "9007199254740993") || strings.Contains(out.Body.String(), "emit_structured_") || strings.Contains(out.Body.String(), "response.completed") {
						t.Error("rejected result escaped to client")
					}
					if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", responseID); err == nil {
						t.Error("rejected result persisted as continuable history")
					}
					wantHTTP := 502
					if mode == "structured_sse" {
						wantHTTP = 200
						if !strings.Contains(out.Body.String(), `"type":"response.failed"`) || !strings.Contains(out.Body.String(), `"output":[]`) {
							t.Error("structured rejection terminal missing or leaked output")
						}
					}
					if out.Code != wantHTTP || rec.HTTPCode != wantHTTP {
						t.Errorf("public HTTP changed by outer finalization: actual=%d record=%d want=%d", out.Code, rec.HTTPCode, wantHTTP)
					}
					bound, _ := p.sessionAccount(t.Context(), "admin", session, "")
					if bound != "synthetic-foreign-winner" {
						t.Error("failed result rewrote winning session owner")
					}
				} else {
					if out.Code != 200 || rec.Status != "ok" {
						t.Error("successful control was rejected")
					}
					if mode != "compact_json" && rec.TTFTMs == nil {
						t.Error("delivered output lost real TTFT")
					}
					if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", responseID); err != nil {
						t.Error("successful control history missing")
					}
				}
			})
		}
	}
}

func TestBPSSessionCommitReviewNativeCompactAuthorizationRevoked(t *testing.T) {
	var p *Proxy
	var calls atomic.Int32
	p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
			t.Error(err)
		}
		p.invalidateAccounts()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_compact_revoked","model":"gpt-6-astra","status":"completed","output":[{"type":"compaction","encrypted_content":"PRIVATE_REVOKED_CIPHER"}],"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":7}}}`)
	})
	req := proxyRequest(http.MethodPost, "/v1/responses/compact", `{"model":"gpt-6-astra","input":[{"role":"user","content":"synthetic"}]}`)
	req.Header.Set("Session-Id", "compact-revoked")
	out := httptest.NewRecorder()
	p.gatewayBPS(out, req)
	rec, found := p.Metrics.LastFor("test")
	wire, _ := json.Marshal(rec)
	t.Logf("mode=compact_authorization_revoked public_http=%d network_posts=%d record=%s", out.Code, calls.Load(), wire)
	if !found || out.Code != http.StatusForbidden || rec.HTTPCode != http.StatusForbidden || rec.Status != "error" || rec.ErrorSource != "gateway" || rec.ErrorPhase != "authorization" || !strings.Contains(rec.Error, "no longer authorized") {
		t.Errorf("non-commit completion failure became success or forged binding classification: %+v", rec)
	}
	if rec.InputTokens != 100 || rec.OutputTokens != 10 || rec.CachedTokens != 80 || rec.CacheWriteTokens == nil || *rec.CacheWriteTokens != 7 || rec.UsageKnown == nil || !*rec.UsageKnown || rec.ReturnedModel != "gpt-6-astra" {
		t.Error("real completed upstream usage was lost after local authorization rejection")
	}
	if calls.Load() != 1 || len(rec.Attempts) != 1 || rec.FinalUpstreamErrorCode != "" || rec.FinalUpstreamErrorSource != "" {
		t.Error("local completion failure replayed generation or invented upstream error")
	}
	if bytes.Contains(out.Body.Bytes(), []byte("PRIVATE_REVOKED_CIPHER")) || rec.TTFTMs != nil {
		t.Error("rejected compaction leaked output or delivered TTFT")
	}
	if _, err := p.bpsLoadHistory(withGatewayUnboundedPayload(t.Context(), true), "admin", "resp_compact_revoked"); err == nil {
		t.Error("rejected compaction persisted history")
	}
}
