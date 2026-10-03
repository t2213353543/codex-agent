package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestGatewaySelectionAuthorizationSkipsRevokedCachedCandidate(t *testing.T) {
	for _, change := range []string{"disabled", "groups", "deleted"} {
		for _, mode := range []string{"new_session", "without_session"} {
			t.Run(change+"/"+mode, func(t *testing.T) {
				var requests, originalRequests, alternateRequests atomic.Int32
				p := gatewaySelectionAuthorizationProxy(t, &requests, &originalRequests, &alternateRequests)
				key, account := gatewaySessionAuthorizationFixture(t, p)
				gatewaySelectionAuthorizationWarm(t, p, key, account, 2)
				gatewaySelectionAuthorizationRevoke(t, p, account, change)
				gatewaySessionAuthorizationAssertWarmSnapshot(t, p, account)
				session := ""
				if mode == "new_session" {
					session = "new-selection-session"
				}
				w := gatewaySelectionAuthorizationSend(p, key, session)
				if w.Code != http.StatusOK || requests.Load() != 1 || originalRequests.Load() != 0 || alternateRequests.Load() != 1 {
					t.Errorf("revoked cached candidate was not skipped: status=%d requests=%d original=%d alternate=%d", w.Code, requests.Load(), originalRequests.Load(), alternateRequests.Load())
				}
				if session != "" {
					alternate, err := p.Store.Get("alternate")
					if err != nil {
						t.Fatal(err)
					}
					gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, alternate)
				}
				if p.inFlight(gatewayAdmissionKey(account)) != 0 || p.inFlight("alternate") != 0 {
					t.Fatal("fresh selection retained account admission")
				}
			})
		}
	}
}

func TestGatewaySelectionAuthorizationAllRevokedHasNoDispatchOrBinding(t *testing.T) {
	for _, change := range []string{"disabled", "groups", "deleted"} {
		t.Run(change, func(t *testing.T) {
			var requests, originalRequests, alternateRequests atomic.Int32
			p := gatewaySelectionAuthorizationProxy(t, &requests, &originalRequests, &alternateRequests)
			key, account := gatewaySessionAuthorizationFixture(t, p)
			if err := p.Store.Delete("alternate"); err != nil {
				t.Fatal(err)
			}
			gatewaySelectionAuthorizationWarm(t, p, key, account, 1)
			gatewaySelectionAuthorizationRevoke(t, p, account, change)
			gatewaySessionAuthorizationAssertWarmSnapshot(t, p, account)
			const session = "all-revoked-new-session"
			w := gatewaySelectionAuthorizationSend(p, key, session)
			if w.Code != http.StatusServiceUnavailable || requests.Load() != 0 {
				t.Errorf("all-revoked selection dispatched or returned the wrong status: status=%d requests=%d", w.Code, requests.Load())
			}
			bound, err := p.sessionAccount(t.Context(), key.ID, session, "")
			if err != nil || bound != "" {
				t.Error("failed fresh selection established an account binding")
			}
			if p.inFlight(gatewayAdmissionKey(account)) != 0 {
				t.Fatal("all-revoked selection retained account admission")
			}
		})
	}
}

func TestGatewaySelectionAuthorizationUsesCurrentConcurrency(t *testing.T) {
	p := newTestProxy(t, 100)
	key, account := gatewaySessionAuthorizationFixture(t, p)
	if err := p.Store.Delete("alternate"); err != nil {
		t.Fatal(err)
	}
	gatewaySelectionAuthorizationWarm(t, p, key, account, 1)
	if err := p.Store.UpdateAccount(account.ID, func(a *Account) error {
		a.Concurrency = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gatewaySessionAuthorizationAssertWarmSnapshot(t, p, account)
	p.accountsMu.Lock()
	cachedLimit := p.accounts[0].Concurrency
	p.accountsMu.Unlock()
	if cachedLimit != 100 {
		t.Fatal("fixture did not retain the old scheduler concurrency limit")
	}
	current, err := p.Store.Get(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst, acquired := p.acquireGatewayAccount(current)
	if !acquired {
		t.Fatal("could not occupy the current single concurrency slot")
	}
	defer releaseFirst()
	_, releaseSecond, _, _, err := p.pickAccount(t.Context(), key.Key)
	if releaseSecond != nil {
		defer releaseSecond()
	}
	var busy *gatewayAccountBusyError
	if !errors.As(err, &busy) || releaseSecond != nil || p.inFlight(gatewayAdmissionKey(account)) != 1 {
		t.Errorf("cached concurrency admitted above the current limit: err=%v second_release=%t active=%d", err, releaseSecond != nil, p.inFlight(gatewayAdmissionKey(account)))
	}
}

func TestGatewaySelectionAuthorizationUsesCurrentCredentialsAndOwner(t *testing.T) {
	for _, change := range []string{"same_owner_token", "reimported_owner"} {
		t.Run(change, func(t *testing.T) {
			var requests atomic.Int32
			credentials := make(chan string, 2)
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				credentials <- r.Header.Get("Authorization")
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"selection-current-owner\",\"output\":[]}}\n\n")
			})
			key, original := gatewaySessionAuthorizationFixture(t, p)
			gatewaySelectionAuthorizationWarm(t, p, key, original, 2)
			const oldSession = "previous-owner-binding"
			if _, err := p.sessionAccount(t.Context(), key.ID, oldSession, gatewayAccountRef(original)); err != nil {
				t.Fatal(err)
			}
			if change == "same_owner_token" {
				if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
					a.AccessToken = "synthetic-current-same-owner-token"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				gatewayRevisionImport(t, p.Store, gatewayRevisionCredentials(t, "synthetic-new-selection-owner", "selection-reimport"))
			}
			current, err := p.Store.Get(original.ID)
			if err != nil || current.AccessToken == original.AccessToken {
				t.Fatal("credential replacement did not retain the stable account ID")
			}
			sameOwner := gatewayAccountRef(current) == gatewayAccountRef(original)
			if sameOwner != (change == "same_owner_token") {
				t.Fatal("fixture did not establish the expected owner transition")
			}
			gatewaySessionAuthorizationAssertWarmSnapshot(t, p, original)
			const newSession = "current-unbound-owner"
			w := gatewaySelectionAuthorizationSend(p, key, newSession)
			if w.Code != http.StatusOK || requests.Load() != 1 {
				t.Fatal("new unbound session did not use the current authorized account")
			}
			if got := <-credentials; got != "Bearer "+current.AccessToken {
				t.Fatal("new unbound session sent cached account credentials")
			}
			gatewaySessionAuthorizationAssertBinding(t, p, key.ID, newSession, current)
			if change == "reimported_owner" {
				w = gatewaySelectionAuthorizationSend(p, key, oldSession)
				if w.Code < 400 || requests.Load() != 1 {
					t.Fatal("accepting the current unbound owner weakened the old binding")
				}
				gatewaySessionAuthorizationAssertBinding(t, p, key.ID, oldSession, original)
			}
			if p.inFlight(gatewayAdmissionKey(original)) != 0 || p.inFlight("alternate") != 0 {
				t.Fatal("current-owner selection retained account admission")
			}
		})
	}
}

func TestGatewaySelectionAuthorizationStoreErrorFailsClosed(t *testing.T) {
	var requests, originalRequests, alternateRequests atomic.Int32
	p := gatewaySelectionAuthorizationProxy(t, &requests, &originalRequests, &alternateRequests)
	key, account := gatewaySessionAuthorizationFixture(t, p)
	gatewaySelectionAuthorizationWarm(t, p, key, account, 2)
	original, err := os.ReadFile(p.Store.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(p.Store.Path, original, 0600); err != nil {
			t.Error("could not restore the private synthetic account store")
		}
	})
	if err := os.WriteFile(p.Store.Path, []byte(`{"accounts":`), 0600); err != nil {
		t.Fatal(err)
	}
	gatewaySessionAuthorizationAssertWarmSnapshot(t, p, account)
	const session = "invalid-store-new-session"
	w := gatewaySelectionAuthorizationSend(p, key, session)
	if w.Code != http.StatusServiceUnavailable || requests.Load() != 0 {
		t.Errorf("account store error fell back to cached credentials: status=%d requests=%d", w.Code, requests.Load())
	}
	bound, err := p.sessionAccount(t.Context(), key.ID, session, "")
	if err != nil || bound != "" {
		t.Fatal("account store error established a session binding")
	}
	if p.inFlight(gatewayAdmissionKey(account)) != 0 {
		t.Fatal("account store error retained account admission")
	}
}

func gatewaySelectionAuthorizationProxy(t *testing.T, requests, original, alternate *atomic.Int32) *Proxy {
	t.Helper()
	return proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer test-upstream-token":
			original.Add(1)
		case "Bearer synthetic-group-alternate":
			alternate.Add(1)
		default:
			t.Error("fresh selection sent unexpected synthetic account credentials")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"selection-fixture\",\"output\":[]}}\n\n")
	})
}

func gatewaySelectionAuthorizationWarm(t *testing.T, p *Proxy, key APIKey, account Account, count int) {
	t.Helper()
	candidates, keyID, _, err := p.candidatesFor(key.Key)
	if err != nil || keyID != key.ID || len(candidates) != count {
		t.Fatal("could not warm the ordinary-key candidate snapshot")
	}
	found := false
	for _, candidate := range candidates {
		if candidate.ID == account.ID && candidate.Priority == 10 {
			found = true
		}
	}
	if !found {
		t.Fatal("candidate snapshot omitted the original preferred account")
	}
	gatewaySessionAuthorizationAssertWarmSnapshot(t, p, account)
}

func gatewaySelectionAuthorizationRevoke(t *testing.T, p *Proxy, account Account, change string) {
	t.Helper()
	var err error
	if change == "deleted" {
		err = p.Store.Delete(account.ID)
	} else {
		err = p.Store.UpdateAccount(account.ID, func(a *Account) error {
			if change == "disabled" {
				a.Enabled = false
			} else {
				a.Groups = []string{"group-two"}
			}
			return nil
		})
	}
	if err != nil {
		t.Fatal(err)
	}
}

func gatewaySelectionAuthorizationSend(p *Proxy, key APIKey, session string) *httptest.ResponseRecorder {
	r := proxyRequest(http.MethodPost, "/responses", `{"model":"fixture","stream":false,"input":[]}`)
	r.Header.Set("Authorization", "Bearer "+key.Key)
	if session != "" {
		r.Header.Set("Session-Id", session)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}
