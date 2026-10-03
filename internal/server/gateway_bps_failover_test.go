package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBPSAccountFailoverPreservesPriorityAndStopsAfterOutput(t *testing.T) {
	for _, mode := range []string{"invalid-token", "bound-disabled", "exhausted", "after-output", "encrypted-state"} {
		t.Run(mode, func(t *testing.T) {
			var calls []string
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				identity := r.Header.Get("Authorization")
				calls = append(calls, identity)
				if identity == "Bearer test-upstream-token" || mode == "exhausted" {
					if mode == "after-output" {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"VISIBLE\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"failed\",\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n")
						return
					}
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"error":{"code":"unauthorized"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "replacement", `[]`))
			})
			settings, _ := p.Settings.Get()
			settings.Upstream401RecoveryEnabled = false
			settings.AutomaticTokenRefreshEnabled = false
			_ = p.Settings.Set(settings)
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "replacement", "expires_at": time.Now().Add(time.Hour).Unix()})
			if err := p.Store.ImportWith("second", raw, AccountDefaults{Concurrency: 1, Priority: 0}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			if mode == "bound-disabled" {
				a, _ := p.Store.Get("test")
				_, _ = p.sessionAccount(t.Context(), "admin", "failover-session", gatewayAccountRef(a))
				_ = p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil })
				p.invalidateAccounts()
			}
			input := `[{"role":"user","content":"synthetic"}]`
			if mode == "encrypted-state" {
				input = `[{"type":"reasoning","encrypted_content":"opaque"},{"role":"user","content":"synthetic"}]`
			}
			r := proxyRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":%s}`, mode == "after-output", input))
			r.Header.Set("Session-Id", "failover-session")
			w := httptest.NewRecorder()
			p.gatewayBPS(w, r)
			want := 2
			if mode == "after-output" || mode == "encrypted-state" || mode == "bound-disabled" {
				want = 1
			}
			if len(calls) != want {
				t.Fatalf("wrong account attempts: %v", calls)
			}
			if mode == "exhausted" || mode == "encrypted-state" {
				if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "upstream_authentication_unavailable") {
					t.Fatalf("terminal account error lost: %d", w.Code)
				}
				lastAccount := "second"
				if mode == "encrypted-state" {
					lastAccount = "test"
				}
				record, ok := p.Metrics.LastFor(lastAccount)
				if !ok || len(record.Attempts) != want {
					t.Fatal("upstream authentication ledger missing")
				}
				for _, attempt := range record.Attempts {
					if attempt.StatusCode == nil || *attempt.StatusCode != 401 || attempt.ObservedHTTPStatusCode == nil || *attempt.ObservedHTTPStatusCode != 401 {
						t.Fatal("actual upstream 401 changed")
					}
				}
			} else if mode == "after-output" {
				if !strings.Contains(w.Body.String(), "VISIBLE") {
					t.Fatal("visible output lost")
				}
			} else if w.Code != 200 || calls[len(calls)-1] != "Bearer replacement" {
				t.Fatalf("replacement not used: %d %v %s", w.Code, calls, w.Body.String())
			}
			if p.inFlight("test") != 0 || p.inFlight("second") != 0 {
				t.Fatal("failover retained concurrency slot")
			}
		})
	}
}
