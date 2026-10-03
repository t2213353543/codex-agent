package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewaySelectionAuthorizationExcludedCandidatePreservesBusyStatus(t *testing.T) {
	for _, mutation := range []string{"disabled", "groups", "deleted"} {
		t.Run(mutation, func(t *testing.T) {
			p := newTestProxy(t, 100)
			key, original := gatewaySessionAuthorizationFixture(t, p)
			gatewaySelectionAuthorizationWarm(t, p, key, original, 2)
			gatewaySelectionAuthorizationRevoke(t, p, original, mutation)
			if err := p.Store.UpdateAccount("alternate", func(account *Account) error {
				account.Concurrency = 1
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			alternate, err := p.Store.Get("alternate")
			if err != nil {
				t.Fatal(err)
			}
			release, acquired := p.acquireGatewayAccount(alternate)
			if !acquired {
				t.Fatal("cannot occupy the sole eligible account")
			}
			defer release()
			_, unexpectedRelease, _, _, err := p.pickAccount(t.Context(), key.Key)
			if unexpectedRelease != nil {
				unexpectedRelease()
			}
			var busy *gatewayAccountBusyError
			if !errors.As(err, &busy) || p.inFlight(gatewayAdmissionKey(alternate)) != 1 || p.inFlight(gatewayAdmissionKey(original)) != 0 {
				t.Fatal("an excluded account became an OAuth failure or bypassed the current concurrency limit")
			}
		})
	}
}

func TestGatewaySelectionAuthorizationCancellationPrecedesSelection(t *testing.T) {
	p := newTestProxy(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.Store.Path = ""
	_, release, _, _, err := p.pickAccount(ctx, "test-client-key")
	if release != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled selection read unavailable storage or admitted an account")
	}
}

func TestGatewaySelectionAuthorizationRechecksKeyAfterRefresh(t *testing.T) {
	for _, mutation := range []string{"disabled", "groups"} {
		t.Run(mutation, func(t *testing.T) {
			var inference, refreshes atomic.Int32
			var p *Proxy
			var key APIKey
			p = proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					if _, err := p.Keys.Update(key.ID, func(current *APIKey) error {
						if mutation == "disabled" {
							current.Enabled = false
						} else {
							current.Groups = []string{"unavailable-group"}
						}
						return nil
					}); err != nil {
						t.Error(err)
						http.Error(w, "synthetic key update failed", 500)
						return
					}
					_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"unbound-rotated-access","refresh_token":"unbound-rotated-refresh","expires_in":3600}`, "test-account", "fixture-user"))
					return
				}
				inference.Add(1)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"unexpected\",\"status\":\"completed\",\"output\":[]}}\n\n")
			})
			var account Account
			key, account = gatewaySessionAuthorizationFixture(t, p)
			if err := p.Store.UpdateAccount(account.ID, func(current *Account) error {
				current.ExpiresAt = time.Now().Add(-time.Minute).Unix()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			const session = "unbound-key-change-during-refresh"
			r := proxyRequest(http.MethodPost, "/responses", `{"model":"fixture","stream":false,"input":[]}`)
			r.Header.Set("Authorization", "Bearer "+key.Key)
			r.Header.Set("Session-Id", session)
			response := httptest.NewRecorder()
			p.ServeHTTP(response, r)
			if response.Code != http.StatusServiceUnavailable || inference.Load() != 0 || refreshes.Load() != 1 {
				t.Fatalf("unbound request bypassed refresh-time key revocation: status=%d inference=%d refresh=%d", response.Code, inference.Load(), refreshes.Load())
			}
			current, err := p.Store.Get(account.ID)
			if err != nil || current.AccessToken != "unbound-rotated-access" || current.RefreshToken != "unbound-rotated-refresh" ||
				current.LastError != account.LastError || gatewayAccountRef(current) != gatewayAccountRef(account) {
				t.Fatal("authorization rejection lost refreshed credentials or became an account refresh failure")
			}
			bound, err := p.sessionAccount(t.Context(), key.ID, session, "")
			if err != nil || bound != "" || p.inFlight(account.ID) != 0 || p.inFlight("alternate") != 0 {
				t.Fatal("rejected fresh selection retained a session binding or admission")
			}
		})
	}
}
