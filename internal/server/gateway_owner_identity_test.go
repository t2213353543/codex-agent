package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func setGatewayOwnerFixture(t *testing.T, p *Proxy, user string) Account {
	t.Helper()
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.Identity = &managedAccountIdentity{UserID: user, WorkspaceID: a.AccountID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	a, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGatewayOwnerIdentityRefKnownUserAndStableCredentials(t *testing.T) {
	a := Account{ID: "stable-fixture-id", Name: "display", AccountID: "fixture-workspace", AccessToken: "initial-token",
		Identity: &managedAccountIdentity{UserID: "fixture-user-a", WorkspaceID: "fixture-workspace"}}
	ref := gatewayAccountRef(a)
	if strings.Contains(ref, "fixture-user-a") || strings.Contains(ref, "fixture-workspace") || strings.Contains(ref, "initial-token") || !strings.Contains(ref, ":user:") {
		t.Fatal("binding retained raw identity or omitted user identity")
	}
	changed := a
	changed.Name, changed.AccessToken, changed.RefreshToken = "renamed", "rotated-token", "rotated-refresh"
	changed.CredentialVersion, changed.Version = 99, 100
	changed.Identity = &managedAccountIdentity{UserID: a.Identity.UserID, WorkspaceID: a.AccountID, Email: "changed@example.invalid", PlanType: "different"}
	if gatewayAccountRef(changed) != ref {
		t.Fatal("non-owner change invalidated known account affinity")
	}
	for _, identity := range []*managedAccountIdentity{nil, {}, {UserID: " "}, {UserID: "fixture-user-b", WorkspaceID: a.AccountID}} {
		changed.Identity = identity
		if gatewayAccountRef(changed) == ref {
			t.Fatal("known user change or removal retained old owner")
		}
	}
	unknown := a
	unknown.Identity = nil
	unknownRef := gatewayAccountRef(unknown)
	unknown.AccessToken, unknown.RefreshToken, unknown.CredentialVersion = "unknown-rotated", "unknown-refresh", 101
	if gatewayAccountRef(unknown) == unknownRef {
		t.Fatal("unknown user credential rotation retained unproven ownership")
	}
}

func TestGatewayOwnerIdentityHTTPRejectsUserChangeAndRemoval(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint("remove=", remove), func(t *testing.T) {
			var requests atomic.Int32
			const result = `{"id":"owner-fixture","output":[]}`
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+result+"}\n\n")
			})
			original := setGatewayOwnerFixture(t, p, "fixture-user-a")
			send := func(session string) *httptest.ResponseRecorder {
				r := proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":false,"input":[{"type":"reasoning","encrypted_content":"fixture-history"}]}`)
				r.Header.Set("Session-Id", session)
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				return w
			}
			if w := send("existing-owner-thread"); w.Code != 200 || w.Body.String() != result {
				t.Fatal("could not establish known-owner session")
			}
			if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
				if remove {
					a.Identity = nil
				} else {
					a.Identity = &managedAccountIdentity{UserID: "fixture-user-b", WorkspaceID: a.AccountID}
				}
				a.AccessToken = "replacement-token"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			if w := send("existing-owner-thread"); w.Code != 503 || requests.Load() != 1 {
				t.Fatal("old encrypted thread was forwarded to changed or unknown owner")
			}
			bound, err := p.sessionAccount(t.Context(), "admin", "existing-owner-thread", "")
			if err != nil || bound != gatewayAccountRef(original) {
				t.Fatal("old binding was silently migrated")
			}
			if w := send("new-owner-thread"); w.Code != 200 || requests.Load() != 2 {
				t.Fatal("new owner's independent thread was incorrectly blocked")
			}
		})
	}
}

func TestGatewayOwnerIdentityLegacyKnownUserBindingFailsClosed(t *testing.T) {
	p := newTestProxy(t, 1)
	known := setGatewayOwnerFixture(t, p, "fixture-known-user")
	legacy := known
	legacy.Identity = nil
	oldRef := gatewayAccountRef(legacy)
	if _, err := p.sessionAccount(t.Context(), "admin", "pre-upgrade-thread", oldRef); err != nil {
		t.Fatal(err)
	}
	_, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", "pre-upgrade-thread")
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("legacy binding without user proof was upgraded into known-user ownership")
	}
	bound, err := p.sessionAccount(t.Context(), "admin", "pre-upgrade-thread", gatewayAccountRef(known))
	if err != nil || bound != oldRef {
		t.Fatal("legacy encrypted history binding was migrated")
	}
}

func TestGatewayOwnerIdentityWebSocketRejectsChangedUser(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint("remove=", remove), func(t *testing.T) {
			var turns atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					if _, _, err := conn.Read(r.Context()); err != nil {
						return
					}
					turns.Add(1)
					if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"owner-turn","status":"completed","output":[]}}`)); err != nil {
						return
					}
				}
			})
			original := setGatewayOwnerFixture(t, p, "fixture-user-a")
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			conn, _ := gatewayWSDial(t, server.URL, "owner-ws-thread")
			defer conn.CloseNow()
			send := func() []byte {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
					t.Fatal(err)
				}
				return gatewayWSRead(t, conn)
			}
			if !strings.Contains(string(send()), "response.completed") {
				t.Fatal("initial WS turn did not complete")
			}
			waitGatewayWSReleased(t, p)
			if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
				if remove {
					a.Identity = nil
				} else {
					a.Identity = &managedAccountIdentity{UserID: "fixture-user-b", WorkspaceID: a.AccountID}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			if _, err := p.authorizedGatewayWSAccount("test-client-key", "admin", original); err == nil {
				t.Fatal("WS authorization accepted changed user on same account ID/workspace")
			}
			var rejected struct {
				Type   string `json:"type"`
				Status int    `json:"status"`
			}
			if json.Unmarshal(send(), &rejected) != nil || rejected.Type != "error" || rejected.Status != http.StatusForbidden || turns.Load() != 1 {
				t.Fatal("second WS turn reached previous owner's socket after replacement")
			}
			waitGatewayWSReleased(t, p)
		})
	}
}

func TestGatewayOwnerIdentitySameUserRotationKeepsSessionAndWSAuthorization(t *testing.T) {
	p := newTestProxy(t, 1)
	original := setGatewayOwnerFixture(t, p, "fixture-user-a")
	first, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", "stable-owner-thread")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := p.Store.UpdateAccount(original.ID, func(a *Account) error {
		a.AccessToken, a.RefreshToken = "synthetic-new-token", "synthetic-new-refresh"
		a.Version++
		a.CredentialVersion++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	rotated, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", "stable-owner-thread")
	if err != nil {
		t.Fatal("same-user credential rotation broke sticky session")
	}
	release()
	if rotated.AccessToken == first.AccessToken || gatewayAccountRef(rotated) != gatewayAccountRef(first) {
		t.Fatal("rotation fixture or ownership invariants failed")
	}
	if _, err := p.authorizedGatewayWSAccount("test-client-key", "admin", original); err != nil {
		t.Fatal("same-owner replacement invalidated existing WS authorization")
	}
}

func TestPGGatewayOwnerIdentityReplacementRejectsSharedSession(t *testing.T) {
	p1, p2, token := adminKeyGatewayPGProxies(t)
	redisDB, _ := gatewayRedisSessionFixture(t)
	p1.Store.DB.Redis, p1.Store.DB.Namespace = redisDB.Redis, redisDB.Namespace
	const session = "owner-replacement-thread"
	threadKey := fmt.Sprintf("%sgateway:thread:%x", redisDB.Namespace, sha256.Sum256([]byte("admin\x00"+session)))
	t.Cleanup(func() { _ = redisDB.Redis.Del(context.Background(), threadKey).Err() })
	a := pgImportForTest(t, p1.Store, "owner-account", "synthetic-first-token")
	incoming := a
	incoming.AccountID = "fixture-shared-workspace"
	incoming.Identity = &managedAccountIdentity{UserID: "fixture-user-a", WorkspaceID: incoming.AccountID}
	incoming.AccessToken = "synthetic-known-user-token"
	if _, err := p1.Store.reauthorizeManagedAccount(t.Context(), a.ID, a.Version, incoming, true); err != nil {
		t.Fatal(err)
	}
	first, release, err := p1.pickSessionAccount(t.Context(), token, "admin", session)
	if err != nil {
		t.Fatal(err)
	}
	release()
	// The same-owner CAS changes credentials/version without invalidating the
	// Redis owner binding, while the next CAS deliberately changes only user.
	current, err := p1.Store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	incoming = current
	incoming.AccessToken = "synthetic-same-owner-rotation"
	if _, err := p1.Store.reauthorizeManagedAccount(t.Context(), current.ID, current.Version, incoming, false); err != nil {
		t.Fatal(err)
	}
	p2.invalidateAccounts()
	rotated, release, err := p2.pickSessionAccount(t.Context(), token, "admin", session)
	if err != nil {
		t.Fatal("same owner credential CAS broke shared session")
	}
	release()
	if gatewayAccountRef(rotated) != gatewayAccountRef(first) {
		t.Fatal("same owner CAS changed reference")
	}
	current, err = p1.Store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	incoming = current
	incoming.AccessToken = "synthetic-other-owner-token"
	incoming.Identity = &managedAccountIdentity{UserID: "fixture-user-b", WorkspaceID: current.AccountID}
	if _, err := p1.Store.reauthorizeManagedAccount(t.Context(), current.ID, current.Version, incoming, true); err != nil {
		t.Fatal(err)
	}
	p2.invalidateAccounts()
	_, release, err = p2.pickSessionAccount(t.Context(), token, "admin", session)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("same workspace user replacement reused another owner's Redis thread")
	}
	if _, err := p2.authorizedGatewayWSAccount(token, "admin", first); err == nil {
		t.Fatal("shared WS authorization accepted another user")
	}
	if bound, err := p2.sessionAccount(t.Context(), "admin", session, ""); err != nil || bound != gatewayAccountRef(first) {
		t.Fatal("shared owner binding silently migrated")
	}
}
