package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayPaymentStructuredEvidence(t *testing.T) {
	for _, tc := range []struct {
		raw            string
		terminal, want bool
	}{
		{`{"error":{"status":402}}`, false, true},
		{`{"detail":{"code":"deactivated_workspace"}}`, false, true},
		{`{"type":"response.failed","response":{"error":{"code":"payment_required"}}}`, true, true},
		{`{"type":"error","status_code":402}`, true, true},
		{`{"type":"response.completed","error":{"status":402}}`, true, false},
		{`{"type":"response.failed","error":{"message":"request text says 402"}}`, true, false},
		{`{"type":"error","error":{"status":400,"code":"content_policy_violation"}}`, true, false},
		{`{"Type":"error","status":402}`, true, false},
	} {
		fields, err := gatewayWSParseFields([]byte(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := gatewayPaymentFailureFields(fields, tc.terminal); got != tc.want {
			t.Fatalf("%s: got=%t want=%t", tc.raw, got, tc.want)
		}
	}
}

func TestBPSPaymentFailureFallsBackAndSkipsAccountConcurrently(t *testing.T) {
	for _, mode := range []string{"http402", "typed_body", "sse402"} {
		t.Run(mode, func(t *testing.T) {
			var rejected, succeeded atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/responses/access" {
					w.WriteHeader(503)
					return
				}
				if r.Header.Get("Authorization") == "Bearer test-upstream-token" {
					rejected.Add(1)
					switch mode {
					case "http402":
						w.WriteHeader(402)
						_, _ = io.WriteString(w, `{"error":{"message":"payment unavailable"}}`)
					case "typed_body":
						w.WriteHeader(403)
						_, _ = io.WriteString(w, `{"detail":{"code":"deactivated_workspace"}}`)
					case "sse402":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"payment-response\",\"status\":\"failed\",\"error\":{\"status\":402,\"message\":\"payment unavailable\"}}}\n\n")
					}
					return
				}
				succeeded.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "payment-replacement", "[]"))
			})
			settings, _ := p.Settings.Get()
			settings.RateLimitCooldownEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; a.Concurrency = 50; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := p.Store.ImportWith("fallback", []byte(`{"access_token":"fallback-payment-token"}`), AccountDefaults{Concurrency: 50}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			request := func() *httptest.ResponseRecorder {
				r := proxyRequest("POST", "/v1/responses", `{"model":"gpt-6-astra","stream":false,"input":[{"role":"user","content":"payment fallback fixture"}]}`)
				out := httptest.NewRecorder()
				p.gatewayBPS(out, r)
				return out
			}
			if out := request(); out.Code != 200 {
				t.Fatalf("first payment rejection did not use fallback: %d %s", out.Code, out.Body.String())
			}
			var wg sync.WaitGroup
			for range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if out := request(); out.Code != 200 {
						t.Errorf("parallel followup failed: %d %s", out.Code, out.Body.String())
					}
				}()
			}
			wg.Wait()
			if rejected.Load() != 1 || succeeded.Load() != 17 {
				t.Fatalf("unavailable account was repeatedly dispatched: rejected=%d succeeded=%d", rejected.Load(), succeeded.Load())
			}
			a, err := p.Store.Get("test")
			if err != nil || !a.Enabled || a.CredentialStatus != gatewayPaymentStatus || accountManagementStatus(a, "", time.Now().Unix()) != "error" {
				t.Fatalf("payment state not visible/recoverable: %+v %v", a, err)
			}
			if p.inFlight("test") != 0 || p.inFlight("fallback") != 0 {
				t.Fatal("payment failover leaked admission")
			}
		})
	}
}

func TestGatewayPaymentExpiryReplacementAndIdentity(t *testing.T) {
	p, _ := webTestSetup(t)
	a := managedTestAccount(t, p, "payment-state", nil)
	p.recordGatewayPayment(t.Context(), a)
	stored, err := p.Store.Get(a.ID)
	if err != nil || p.checkGatewayCooldown(t.Context(), stored) == nil {
		t.Fatalf("payment not blocked: %v", err)
	}
	// A fresh Proxy observes the persisted state, independent of local memory.
	restarted := &Proxy{Store: p.Store, Settings: p.Settings}
	if restarted.checkGatewayCooldown(t.Context(), stored) == nil {
		t.Fatal("restart lost payment state")
	}
	if err := p.Store.UpdateTokens(stored.ID, stored.AccessToken, Account{AccessToken: "payment-refreshed-token"}); err != nil {
		t.Fatal(err)
	}
	stored, _ = p.Store.Get(a.ID)
	if stored.CredentialStatus != gatewayPaymentStatus || restarted.checkGatewayCooldown(t.Context(), stored) == nil {
		t.Fatal("OAuth token rotation incorrectly cleared billing rejection")
	}
	if err := p.Store.UpdateAccount(a.ID, func(a *Account) error {
		a.LastCheckedAt = time.Now().Add(-gatewayPaymentCooldown - time.Second).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stored, _ = p.Store.Get(a.ID)
	if err := restarted.checkGatewayCooldown(t.Context(), stored); err != nil {
		t.Fatal("expired payment state did not recover:", err)
	}
	if accountManagementStatus(stored, "ok", time.Now().Unix()) != "ok" {
		t.Fatal("expired payment state concealed a later successful observation")
	}
	raw, _ := json.Marshal(map[string]any{"access_token": "replacement-payment-token"})
	if _, err := p.Store.ImportDetailed([]ImportItem{{Name: a.Name, Credentials: raw}}, AccountDefaults{}, ImportOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	p.recordGatewayPayment(context.Background(), a) // Late response for replaced credential.
	stored, _ = p.Store.Get(a.ID)
	if stored.CredentialStatus == gatewayPaymentStatus || p.checkGatewayCooldown(t.Context(), stored) != nil {
		t.Fatal("late payment error poisoned replacement credentials")
	}
}

func TestPGGatewayPaymentCredentialEpochAndRestart(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "pg-payment", nil)
	p.recordGatewayPayment(t.Context(), a)
	stored, err := p.Store.Get(a.ID)
	if err != nil || stored.CredentialStatus != gatewayPaymentStatus {
		t.Fatalf("PG payment was not persisted: %v", err)
	}
	restarted := &Proxy{Store: p.Store, Settings: p.Settings}
	if restarted.checkGatewayCooldown(t.Context(), stored) == nil {
		t.Fatal("PG restart lost payment state")
	}
	// This process still holds the account selected before the other process
	// observed a402. The final dispatch boundary must read authoritative state.
	req := httptest.NewRequest("GET", "https://synthetic.invalid/responses", nil)
	called := false
	_, dispatchErr := restarted.doAccountRequest(nil, req, a, func(*http.Client, *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	}, nil)
	var blocked *gatewayCooldownError
	if called || !errors.As(dispatchErr, &blocked) || !blocked.payment {
		t.Fatalf("stale cross-process lease dispatched after payment rejection: called=%t err=%v", called, dispatchErr)
	}
	refreshed, err := p.Store.Refresh(a.ID, a.AccessToken, func(Account) (Account, error) {
		return Account{AccessToken: "pg-payment-rotated", RefreshToken: "pg-payment-rotated-rt"}, nil
	})
	if err != nil || refreshed.CredentialStatus != gatewayPaymentStatus || refreshed.LastCheckedAt != stored.LastCheckedAt || restarted.checkGatewayCooldown(t.Context(), refreshed) == nil {
		t.Fatalf("OAuth refresh cleared or extended workspace billing state: %v", err)
	}
	_, err = p.Store.ImportDetailed([]ImportItem{{Name: a.Name, Credentials: json.RawMessage(fmt.Sprintf(`{"access_token":%q}`, "pg-payment-new"))}}, AccountDefaults{}, ImportOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	p.recordGatewayPayment(t.Context(), a)
	stored, err = p.Store.Get(a.ID)
	if err != nil || stored.CredentialStatus == gatewayPaymentStatus || p.checkGatewayCooldown(t.Context(), stored) != nil {
		t.Fatalf("PG stale credential quarantined replacement: %v", err)
	}
}

func TestPGGatewayPaymentConcurrentMetadataCAS(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	a := managedTestAccount(t, p, "payment-concurrent-cas", nil)
	if _, err := p.Store.UpdateAccountVersion(a.ID, a.Version, func(a *Account) error { a.Notes = "concurrent metadata"; return nil }); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); p.recordGatewayPayment(t.Context(), a) }()
	}
	wg.Wait()
	current, err := p.Store.Get(a.ID)
	if err != nil || current.CredentialStatus != gatewayPaymentStatus || current.Notes != "concurrent metadata" {
		t.Fatalf("payment CAS lost concurrent metadata or quarantine: %v", err)
	}
	if current.Version != a.Version+2 {
		t.Fatalf("concurrent identical rejections rewrote account repeatedly: got=%d want=%d", current.Version, a.Version+2)
	}
}

func TestGatewayPaymentStaleRotationGoHTTP(t *testing.T) {
	exerciseGatewayPaymentStaleRotationGoHTTP(t, webTestSetup)
}

func TestPGGatewayPaymentStaleRotationGoHTTP(t *testing.T) {
	exerciseGatewayPaymentStaleRotationGoHTTP(t, adminPGTestProxy)
}

func exerciseGatewayPaymentStaleRotationGoHTTP(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	for _, payment := range []bool{false, true} {
		t.Run(fmt.Sprintf("payment_%t", payment), func(t *testing.T) {
			writer, _ := setup(t)
			settings, err := writer.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.Upstream401RecoveryEnabled = true
			if err := writer.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			raw := gatewayKnownOwnerRefreshFixture(`{"access_token":"stale-payment-before","refresh_token":"stale-payment-rt"}`, "payment-workspace", "payment-user")
			if err := writer.Store.ImportWith("stale-payment-http", []byte(raw), AccountDefaults{}); err != nil {
				t.Fatal(err)
			}
			selected, err := writer.Store.Get("stale-payment-http")
			if err != nil {
				t.Fatal(err)
			}
			// The reader already holds its lease and old credential snapshot.
			reader := &Proxy{Store: writer.Store, Settings: writer.Settings}
			if reader.nativeHTTPFor(t.Context()) != nil {
				t.Fatal("fixture must exercise plain Go HTTP without a native guard")
			}
			if payment {
				writer.recordGatewayPayment(t.Context(), selected)
			}
			rotated := selected
			rotated.AccessToken = "stale-payment-after"
			if err := writer.Store.UpdateTokens(selected.ID, selected.AccessToken, rotated); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") == "Bearer stale-payment-before" {
					w.WriteHeader(401)
					return
				}
				if r.Header.Get("Authorization") != "Bearer stale-payment-after" {
					t.Error("unexpected credential dispatched")
				}
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			defer upstream.Close()
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, upstream.URL+"/responses", nil)
			req.Header.Set("Authorization", "Bearer "+selected.AccessToken)
			response, err := reader.doAccountRequest(upstream.Client(), req, selected, doHTTPAccountRequest, func(current Account) error {
				if current.AccessToken != rotated.AccessToken || gatewayAccountRef(current) != gatewayAccountRef(selected) {
					return errors.New("recovery changed owner or failed to reuse rotated token")
				}
				return nil
			})
			if response != nil {
				defer response.Body.Close()
			}
			if payment {
				var blocked *gatewayCooldownError
				if response != nil || calls.Load() != 0 || !errors.As(err, &blocked) || !blocked.payment {
					t.Fatalf("old epoch bypassed cross-process payment: calls=%d response=%v err=%v", calls.Load(), response, err)
				}
			} else if err != nil || response == nil || response.StatusCode != 200 || calls.Load() != 2 {
				t.Fatalf("normal same-owner rotation stopped using existing 401 recovery: calls=%d response=%v err=%v", calls.Load(), response, err)
			}
		})
	}
}
