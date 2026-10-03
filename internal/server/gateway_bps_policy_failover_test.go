package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const bpsObservedPolicyEnvelope = `{"error":{"message":"403: This request was blocked by our usage policy.","type":"server_error","param":null,"code":null}}`

func TestBPSAccountRejectionMatchesOnlyObservedEnvelope(t *testing.T) {
	if !bpsObservedAccountRejection(403, []byte(bpsObservedPolicyEnvelope)) {
		t.Fatal("observed envelope not recognized")
	}
	for _, raw := range []string{
		strings.Replace(bpsObservedPolicyEnvelope, `"server_error"`, `"policy_error"`, 1),
		strings.Replace(bpsObservedPolicyEnvelope, `"code":null`, `"code":"content_policy_violation"`, 1),
		strings.Replace(bpsObservedPolicyEnvelope, `"param":null`, `"param":"input"`, 1),
		strings.Replace(bpsObservedPolicyEnvelope, `403: This request was blocked by our usage policy.`, `Content was blocked by policy.`, 1),
		`{"detail":"403: This request was blocked by our usage policy."}`,
		`{"error":{"message":"403: This request was blocked by our usage policy.","type":"server_error"}}`,
	} {
		if bpsObservedAccountRejection(403, []byte(raw)) {
			t.Fatalf("unobserved policy condition accepted: %s", raw)
		}
	}
	if bpsObservedAccountRejection(400, []byte(bpsObservedPolicyEnvelope)) {
		t.Fatal("non403 reclassified")
	}
	status, code, source := officialFailureHTTP(&bpsAccountRejectedError{}, 0)
	if status != 403 || code != "upstream_account_rejected" || source != "upstream" {
		t.Fatal("account rejection provenance lost")
	}
}

func TestBPSAccountPolicyFailoverPriorityExhaustionAndOpaqueHistory(t *testing.T) {
	for _, mode := range []string{"replacement", "all-rejected", "content-policy", "encrypted-history"} {
		t.Run(mode, func(t *testing.T) {
			var calls []string
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/responses/access" {
					w.WriteHeader(503)
					return
				}
				identity := r.Header.Get("Authorization")
				calls = append(calls, identity)
				if identity == "Bearer test-upstream-token" || mode == "all-rejected" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(403)
					body := bpsObservedPolicyEnvelope
					if mode == "content-policy" {
						body = strings.Replace(body, `"code":null`, `"code":"content_policy_violation"`, 1)
					}
					_, _ = io.WriteString(w, body)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "policy-replacement", "[]"))
			})
			_ = p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; a.Weight = 1; a.Concurrency = 50; return nil })
			for _, candidate := range []struct {
				name, token      string
				priority, weight int
			}{{"next", "next-token", 50, 1}, {"last", "last-token", 0, 100}} {
				raw, _ := json.Marshal(map[string]any{"access_token": candidate.token, "expires_at": time.Now().Add(time.Hour).Unix()})
				if err := p.Store.ImportWith(candidate.name, raw, AccountDefaults{Concurrency: 50, Priority: candidate.priority, Weight: candidate.weight}); err != nil {
					t.Fatal(err)
				}
			}
			p.invalidateAccounts()
			input := `[{"role":"user","content":"same short request"}]`
			if mode == "encrypted-history" {
				input = `[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"same short request"}]`
			}
			req := proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":`+input+`}`)
			req.Header.Set("Session-Id", "policy-failover-fixture")
			out := httptest.NewRecorder()
			p.gatewayBPS(out, req)
			want := []string{"Bearer test-upstream-token", "Bearer next-token"}
			status := 200
			if mode == "all-rejected" {
				want = append(want, "Bearer last-token")
				status = 403
			}
			if mode == "content-policy" || mode == "encrypted-history" {
				want = want[:1]
				status = 403
			}
			if !reflect.DeepEqual(calls, want) || out.Code != status {
				t.Fatalf("attempts=%v status=%d want=%v/%d", calls, out.Code, want, status)
			}
			for _, name := range []string{"test", "next", "last"} {
				account, err := p.Store.Get(name)
				if err != nil || account.Concurrency != 50 || p.inFlight(name) != 0 {
					t.Fatal("account concurrency mutated/leaked")
				}
			}
			if mode == "replacement" {
				record, ok := p.Metrics.LastFor("next")
				if !ok || len(record.Attempts) < 2 || record.Attempts[0].ErrorCode != "upstream_account_rejected" {
					t.Fatal("retry provenance absent")
				}
			}
		})
	}
}
