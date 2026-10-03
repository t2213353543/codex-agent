package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGatewaySessionAuthorizationHTTPUsesCurrentAccountGroups(t *testing.T) {
	for _, change := range []string{"account_groups", "account_disabled", "key_groups", "unchanged", "remaining_overlap"} {
		t.Run(change, func(t *testing.T) { gatewaySessionAuthorizationHTTP(t, change) })
	}
}

func gatewaySessionAuthorizationHTTP(t *testing.T, change string) {
	t.Helper()
	var requests atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"group-fixture\",\"output\":[]}}\n\n")
	})
	key, account := gatewaySessionAuthorizationFixture(t, p)
	const session = "group-bound-http"
	send := func() *httptest.ResponseRecorder {
		r := proxyRequest(http.MethodPost, "/responses", `{"model":"fixture","stream":false,"input":[]}`)
		r.Header.Set("Authorization", "Bearer "+key.Key)
		r.Header.Set("Session-Id", session)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	if w := send(); w.Code != http.StatusOK || requests.Load() != 1 {
		t.Fatal("could not establish an ordinary-key group-bound HTTP session")
	}
	gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
	gatewaySessionAuthorizationChange(t, p, key, account, change)
	gatewaySessionAuthorizationAssertWarmSnapshot(t, p, account)
	w := send()
	if change == "unchanged" || change == "remaining_overlap" {
		if w.Code != http.StatusOK || requests.Load() != 2 {
			t.Errorf("unchanged group authorization did not preserve the session: status=%d requests=%d", w.Code, requests.Load())
		}
	} else if w.Code < 400 || requests.Load() != 1 {
		t.Errorf("warm account-group cache bypassed current authorization: status=%d upstream_requests=%d", w.Code, requests.Load())
	}
	gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
	if p.inFlight(account.ID) != 0 || p.inFlight("alternate") != 0 {
		t.Fatal("denied group authorization retained account admission")
	}
	if change == "account_groups" {
		gatewaySessionAuthorizationRestoreGroup(t, p, account)
		if restored := send(); restored.Code != http.StatusOK || requests.Load() != 2 {
			t.Fatal("restoring group access did not resume the original HTTP binding")
		}
		gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
	}
}

func TestGatewaySessionAuthorizationWebSocketUsesCurrentAccountGroups(t *testing.T) {
	for _, change := range []string{"account_groups", "account_disabled", "key_groups", "unchanged", "remaining_overlap"} {
		t.Run(change, func(t *testing.T) {
			var handshakes, turns atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				handshakes.Add(1)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
					turn := turns.Add(1)
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"group-turn-%d","status":"completed","output":[]}}`, turn))); err != nil {
						return
					}
				}
			})
			key, account := gatewaySessionAuthorizationFixture(t, p)
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			const session = "group-bound-websocket"
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			headers := http.Header{"Authorization": {"Bearer " + key.Key}, "Session-Id": {session}}
			conn, _, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatal("could not connect ordinary-key group-bound WebSocket")
			}
			defer conn.CloseNow()
			send := func() (string, int) {
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"fixture","input":[]}`)); err != nil {
					t.Fatal(err)
				}
				var frame struct {
					Type   string `json:"type"`
					Status int    `json:"status"`
				}
				if err := json.Unmarshal(gatewayWSRead(t, conn), &frame); err != nil {
					t.Fatal(err)
				}
				return frame.Type, frame.Status
			}
			if kind, _ := send(); kind != "response.completed" || turns.Load() != 1 {
				t.Fatal("could not complete initial ordinary-key WebSocket turn")
			}
			waitGatewayWSReleased(t, p)
			gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
			gatewaySessionAuthorizationChange(t, p, key, account, change)
			gatewaySessionAuthorizationAssertWarmSnapshot(t, p, account)
			kind, status := send()
			if change == "unchanged" || change == "remaining_overlap" {
				if kind != "response.completed" || turns.Load() != 2 {
					t.Errorf("unchanged WebSocket authorization rejected the next turn: type=%s turns=%d", kind, turns.Load())
				}
			} else if kind != "error" || status != http.StatusForbidden || turns.Load() != 1 {
				t.Errorf("warm account-group cache bypassed WebSocket authorization: type=%s status=%d upstream_turns=%d", kind, status, turns.Load())
			}
			if handshakes.Load() != 1 {
				t.Error("authorization change opened another upstream connection")
			}
			waitGatewayWSReleased(t, p)
			gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
			if p.inFlight("alternate") != 0 {
				t.Fatal("authorization failure admitted an alternate account")
			}
			if change == "account_groups" {
				gatewaySessionAuthorizationRestoreGroup(t, p, account)
				_ = conn.CloseNow()
				conn, _, err = websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
				if err != nil {
					t.Fatal("restored group could not reconnect the original WebSocket session")
				}
				defer conn.CloseNow()
				if kind, _ := send(); kind != "response.completed" || turns.Load() != 2 || handshakes.Load() != 2 {
					t.Fatal("restoring group access did not resume the original WebSocket binding")
				}
				waitGatewayWSReleased(t, p)
				gatewaySessionAuthorizationAssertBinding(t, p, key.ID, session, account)
			}
		})
	}
}

func gatewaySessionAuthorizationChange(t *testing.T, p *Proxy, key APIKey, account Account, change string) {
	t.Helper()
	var err error
	switch change {
	case "account_groups":
		err = p.Store.UpdateAccount(account.ID, func(a *Account) error {
			a.Groups = []string{"group-two"}
			return nil
		})
	case "remaining_overlap":
		err = p.Store.UpdateAccount(account.ID, func(a *Account) error {
			a.Groups = []string{"group-two", "group-one"}
			return nil
		})
	case "account_disabled":
		err = p.Store.UpdateAccount(account.ID, func(a *Account) error {
			a.Enabled = false
			return nil
		})
	case "key_groups":
		_, err = p.Keys.Update(key.ID, func(k *APIKey) error {
			k.Groups = []string{"group-two"}
			return nil
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	current, err := p.Store.Get(account.ID)
	if err != nil || gatewayAccountRef(current) != gatewayAccountRef(account) {
		t.Fatal("authorization-only change unexpectedly replaced the account owner")
	}
	if change == "account_groups" && !slices.Equal(current.Groups, []string{"group-two"}) {
		t.Fatal("account group revocation did not persist")
	}
}

func TestGatewaySessionAuthorizationCurrentAccountBoundary(t *testing.T) {
	t.Run("admin_across_groups", func(t *testing.T) {
		p := newTestProxy(t, 1)
		_, account := gatewaySessionAuthorizationFixture(t, p)
		const session = "admin-group-independent"
		selected, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", session)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if selected.ID != account.ID {
			t.Fatal("admin fixture bound a different account")
		}
		gatewaySessionAuthorizationChange(t, p, APIKey{}, account, "account_groups")
		gatewaySessionAuthorizationAssertWarmSnapshot(t, p, account)
		selected, release, err = p.pickSessionAccount(t.Context(), "test-client-key", "admin", session)
		if release != nil {
			release()
		}
		if err != nil || selected.ID != account.ID || !slices.Equal(selected.Groups, []string{"group-two"}) {
			t.Fatal("account group changes incorrectly revoked administrator binding")
		}
		if current, err := p.authorizedGatewayWSAccount("test-client-key", "admin", account); err != nil || current.ID != account.ID {
			t.Fatal("account group changes incorrectly revoked administrator WebSocket access")
		}
		gatewaySessionAuthorizationAssertBinding(t, p, "admin", session, account)
	})
	t.Run("returned_ready_account", func(t *testing.T) {
		p := newTestProxy(t, 1)
		key, account := gatewaySessionAuthorizationFixture(t, p)
		if err := p.checkGatewayBoundAccountAccess(t.Context(), key.Key, key.ID, account); err != nil {
			t.Fatal("initial current account was not authorized")
		}
		// This is the helper's post-refresh input contract, not a fabricated
		// OAuth response claiming it can change management group membership.
		ready := account
		ready.Groups = []string{"group-two"}
		if err := p.checkGatewayBoundAccountAccess(t.Context(), key.Key, key.ID, ready); err == nil {
			t.Fatal("returned ready account reused the earlier group authorization")
		}
		ready.Groups = []string{"group-two", "group-one"}
		if err := p.checkGatewayBoundAccountAccess(t.Context(), key.Key, key.ID, ready); err != nil {
			t.Fatal("remaining group overlap on the returned ready account was rejected")
		}
	})
	t.Run("cancelled_context", func(t *testing.T) {
		p := newTestProxy(t, 1)
		key, account := gatewaySessionAuthorizationFixture(t, p)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := p.checkGatewayBoundAccountAccess(ctx, key.Key, key.ID, account); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled authorization continued through the key lookup: %v", err)
		}
	})
}

func gatewaySessionAuthorizationRestoreGroup(t *testing.T, p *Proxy, account Account) {
	t.Helper()
	if err := p.Store.UpdateAccount(account.ID, func(a *Account) error {
		a.Groups = []string{"group-one"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func gatewaySessionAuthorizationFixture(t *testing.T, p *Proxy) (APIKey, Account) {
	t.Helper()
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.Groups, a.Priority = []string{"group-one"}, 10
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.ImportWith("alternate", []byte(`{"access_token":"synthetic-group-alternate","chatgpt_account_id":"synthetic-alternate-workspace","expires_at":4102444800}`), AccountDefaults{Groups: []string{"group-one"}, Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("ordinary-group-key", []string{"group-one"})
	if err != nil {
		t.Fatal(err)
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.StickySessionsEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	return key, account
}

func gatewaySessionAuthorizationAssertWarmSnapshot(t *testing.T, p *Proxy, original Account) {
	t.Helper()
	p.accountsMu.Lock()
	defer p.accountsMu.Unlock()
	if p.accountsAt.IsZero() || time.Since(p.accountsAt) >= 5*time.Second {
		t.Fatal("fixture no longer has a warm scheduler account snapshot")
	}
	for _, cached := range p.accounts {
		if cached.ID == original.ID {
			if !cached.Enabled || !slices.Equal(cached.Groups, []string{"group-one"}) {
				t.Fatal("fixture accidentally invalidated the original account authorization cache")
			}
			return
		}
	}
	t.Fatal("warm scheduler snapshot omitted the bound account")
}

func gatewaySessionAuthorizationAssertBinding(t *testing.T, p *Proxy, keyID, session string, account Account) {
	t.Helper()
	bound, err := p.sessionAccount(t.Context(), keyID, session, "")
	if err != nil || bound != gatewayAccountRef(account) {
		t.Fatal("session binding changed to a different account after group authorization changed")
	}
}
