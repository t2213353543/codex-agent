package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

func gatewayUnknownOwnerFixture(t *testing.T, p *Proxy) Account {
	t.Helper()
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Identity = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	a, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGatewayUnknownOwnerReferenceTracksCredentialsOnly(t *testing.T) {
	base := Account{ID: "fixture-id", Name: "fixture-name", AccountID: "fixture-workspace", AccessToken: "PRIVATE_ACCESS", RefreshToken: "PRIVATE_REFRESH", ClientID: "fixture-client"}
	ref := gatewayAccountRef(base)
	if !strings.Contains(ref, ":credentials:") || strings.Contains(ref, "PRIVATE_") || strings.Contains(ref, "fixture-workspace") {
		t.Fatal("unknown reference missing opaque credential generation or leaked material")
	}
	for _, mutate := range []func(*Account){
		func(a *Account) { a.AccessToken = "changed-access" }, func(a *Account) { a.RefreshToken = "changed-refresh" }, func(a *Account) { a.ClientID = "changed-client" },
	} {
		changed := base
		mutate(&changed)
		if gatewayAccountRef(changed) == ref {
			t.Fatal("unknown credentials changed without invalidating ownership")
		}
	}
	stable := base
	stable.Name = "renamed"
	stable.Version = 99
	stable.CredentialVersion = 100
	stable.ExpiresAt = 12345
	stable.LastError = "fixture"
	stable.Notes = "metadata"
	if gatewayAccountRef(stable) != ref {
		t.Fatal("unchanged credentials changed owner through metadata")
	}
	encoded, err := json.Marshal(stable)
	if err != nil {
		t.Fatal(err)
	}
	var reopened Account
	if json.Unmarshal(encoded, &reopened) != nil || gatewayAccountRef(reopened) != ref {
		t.Fatal("JSON restart or lost transient credential version changed owner")
	}
	for _, test := range []Account{
		{ID: base.ID, AccessToken: base.AccessToken, RefreshToken: base.RefreshToken, ClientID: base.ClientID},
		{ID: base.ID, AccessToken: base.AccessToken, RefreshToken: base.RefreshToken, ClientID: base.ClientID, Identity: &managedAccountIdentity{UserID: "known-user-without-workspace"}},
		{ID: base.ID, AccountID: " ", AccessToken: base.AccessToken, RefreshToken: base.RefreshToken, ClientID: base.ClientID, Identity: &managedAccountIdentity{UserID: "known-user"}},
	} {
		changed := test
		changed.AccessToken = "new"
		if gatewayCompleteOwner(test) || gatewayAccountRef(test) == gatewayAccountRef(changed) {
			t.Fatal("incomplete identity was treated as proven owner")
		}
	}
	known := base
	known.Identity = &managedAccountIdentity{UserID: "known-user", WorkspaceID: base.AccountID}
	changed := known
	changed.AccessToken = "known-new"
	changed.RefreshToken = "known-rotated"
	changed.ClientID = "known-client"
	if !gatewayCompleteOwner(known) || gatewayAccountRef(known) != gatewayAccountRef(changed) || strings.Contains(gatewayAccountRef(known), ":credentials:") {
		t.Fatal("complete known owner changed during credential rotation")
	}
}

func TestGatewayUnknownOwnerHTTPRotationRejectsOldThreadAndKeepsNewThread(t *testing.T) {
	for _, field := range []string{"access_token", "refresh_token", "client_id"} {
		t.Run(field, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
			})
			before := gatewayUnknownOwnerFixture(t, p)
			send := func(session string) int {
				r := proxyRequest("POST", "/responses", `{"model":"fixture","input":[{"type":"reasoning","encrypted_content":"fixture-old-history"}]}`)
				r.Header.Set("Session-Id", session)
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				return w.Code
			}
			if send("old-thread") != 200 {
				t.Fatal("cannot establish unknown-owner thread")
			}
			if err := p.Store.UpdateAccount(before.ID, func(a *Account) error {
				switch field {
				case "access_token":
					a.AccessToken = "different-access"
				case "refresh_token":
					a.RefreshToken = "different-refresh"
				case "client_id":
					a.ClientID = "different-client"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			if status := send("old-thread"); status != 409 || calls.Load() != 1 {
				t.Fatalf("old thread forwarded or unclear owner status: status=%d calls=%d", status, calls.Load())
			}
			if _, err := p.authorizedGatewayWSAccount("test-client-key", "admin", before); err == nil {
				t.Fatal("existing WS accepted unknown-owner credential replacement")
			}
			if send("new-thread") != 200 || calls.Load() != 2 {
				t.Fatal("new unknown-owner thread cannot use replaced credentials")
			}
			bound, err := p.sessionAccount(t.Context(), "admin", "old-thread", "")
			if err != nil || bound != gatewayAccountRef(before) {
				t.Fatal("old binding was migrated rather than invalidated")
			}
		})
	}
}

func TestGatewayUnknownOwnerExpiryRefreshPersistsBeforeOldThreadConflict(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint("existing=", existing), func(t *testing.T) {
			var refreshes, inference atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					_, _ = io.WriteString(w, `{"access_token":"expiry-new-access","refresh_token":"expiry-new-refresh","expires_in":3600}`)
					return
				}
				inference.Add(1)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
			})
			before := gatewayUnknownOwnerFixture(t, p)
			if existing {
				if _, err := p.sessionAccount(t.Context(), "admin", "expiry-thread", gatewayAccountRef(before)); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Store.UpdateAccount(before.ID, func(a *Account) error { a.ExpiresAt = time.Now().Add(-time.Hour).Unix(); return nil }); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			send := func(session string) int {
				r := proxyRequest("POST", "/responses", `{"model":"fixture","stream":true}`)
				r.Header.Set("Session-Id", session)
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				return w.Code
			}
			want := 200
			if existing {
				want = 409
			}
			if status := send("expiry-thread"); status != want {
				t.Fatalf("expiry owner transition status=%d want=%d", status, want)
			}
			stored, err := p.Store.Get(before.ID)
			if err != nil || stored.AccessToken != "expiry-new-access" || stored.RefreshToken != "expiry-new-refresh" || refreshes.Load() != 1 {
				t.Fatal("owner conflict prevented rotated credentials persistence")
			}
			wantInference := int32(1)
			if existing {
				wantInference = 0
			}
			if inference.Load() != wantInference {
				t.Fatal("old history crossed unknown-owner expiry transition")
			}
			if send("fresh-thread") != 200 || refreshes.Load() != 1 {
				t.Fatal("new thread attempted another rotation or failed")
			}
		})
	}
}

func TestGatewayUnknownOwner401RefreshPersistsWithoutReplay(t *testing.T) {
	for _, refreshOnly := range []bool{false, true} {
		t.Run(fmt.Sprint("refresh-only=", refreshOnly), func(t *testing.T) {
			var inference, refreshes atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					if refreshOnly {
						_, _ = io.WriteString(w, `{"refresh_token":"rotated-rt-only"}`)
					} else {
						_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`)
					}
					return
				}
				inference.Add(1)
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":{"message":"fixture-expired"}}`)
			})
			before := gatewayUnknownOwnerFixture(t, p)
			w := httptest.NewRecorder()
			r := proxyRequest("POST", "/responses", `{"model":"fixture","stream":true}`)
			r.Header.Set("Session-Id", "401-old-thread")
			p.ServeHTTP(w, r)
			if w.Code != 409 || inference.Load() != 1 || refreshes.Load() != 1 {
				t.Fatalf("unknown-owner recovery replayed or hid conflict: status=%d requests=%d refresh=%d", w.Code, inference.Load(), refreshes.Load())
			}
			stored, err := p.Store.Get(before.ID)
			wantRT := "rotated-refresh"
			if refreshOnly {
				wantRT = "rotated-rt-only"
			}
			if err != nil || stored.RefreshToken != wantRT || gatewayAccountRef(stored) == gatewayAccountRef(before) {
				t.Fatal("unknown ownership conflict lost actual rotated RT")
			}
		})
	}
}

func TestGatewayUnknownOwnerLegacyBindingFailsClosed(t *testing.T) {
	p := newTestProxy(t, 1)
	a := gatewayUnknownOwnerFixture(t, p)
	legacy := gatewayAccountOwnerRef(a)
	if _, err := p.sessionAccount(t.Context(), "admin", "legacy-unknown-thread", legacy); err != nil {
		t.Fatal(err)
	}
	_, release, err := p.pickSessionAccount(t.Context(), "test-client-key", "admin", "legacy-unknown-thread")
	if release != nil {
		release()
	}
	var changed *gatewayOwnerChangedError
	if !errors.As(err, &changed) {
		t.Fatal("legacy unknown binding without credential generation was silently accepted")
	}
}

func TestGatewayUnknownOwnerWebSocket401PersistsAndRejectsBeforeCreate(t *testing.T) {
	for _, headerSession := range []bool{true, false} {
		t.Run(fmt.Sprint("header-session=", headerSession), func(t *testing.T) {
			var upgrades, refreshes atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					refreshes.Add(1)
					_, _ = io.WriteString(w, `{"access_token":"ws-unknown-new","refresh_token":"ws-unknown-rt"}`)
					return
				}
				upgrades.Add(1)
				w.WriteHeader(401)
			})
			before := gatewayUnknownOwnerFixture(t, p)
			enableGatewayWS(t, p, "")
			server := httptest.NewServer(p)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			headers := http.Header{"Authorization": {"Bearer test-client-key"}}
			if headerSession {
				headers.Set("Session-Id", "unknown-ws-thread")
			}
			conn, response, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
			if headerSession {
				if conn != nil {
					defer conn.CloseNow()
				}
				if err == nil || response == nil || response.StatusCode != 409 {
					t.Fatal("rejected unknown-owner WS handshake did not expose 409")
				}
				if response.Body != nil {
					_ = response.Body.Close()
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer conn.CloseNow()
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"fixture","prompt_cache_key":"unknown-ws-thread","input":[]}`)); err != nil {
					t.Fatal(err)
				}
				_, raw, err := conn.Read(ctx)
				var event struct {
					Type   string `json:"type"`
					Status int    `json:"status"`
				}
				if err != nil || json.Unmarshal(raw, &event) != nil || event.Type != "error" || event.Status != 409 {
					t.Fatal("body-routed unknown-owner WS turn did not expose 409")
				}
			}
			stored, err := p.Store.Get(before.ID)
			if err != nil || stored.RefreshToken != "ws-unknown-rt" || stored.AccessToken != "ws-unknown-new" || upgrades.Load() != 1 || refreshes.Load() != 1 {
				t.Fatal("unknown-owner WS refresh lost tokens or retried upgrade")
			}
			if p.inFlight("test") != 0 {
				t.Fatal("rejected owner transition leaked admission")
			}
		})
	}
}

func TestGatewayUnknownOwnerInitialRefreshKeepsSelectedProxy(t *testing.T) {
	for _, sticky := range []bool{false, true} {
		t.Run(fmt.Sprint("sticky=", sticky), func(t *testing.T) {
			p := newTestProxy(t, 1)
			before := gatewayUnknownOwnerFixture(t, p)
			p.BaseURL, p.TokenURL = "http://fixture-route.invalid", "http://fixture-route.invalid/oauth/token"
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.StickySessionsEnabled = sticky
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			var calls [2]atomic.Int32
			var ids []string
			for index := range 2 {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls[index].Add(1)
					if r.URL.Path == "/oauth/token" {
						_, _ = io.WriteString(w, `{"access_token":"proxy-new","refresh_token":"proxy-new-rt","expires_in":3600}`)
						return
					}
					if r.Header.Get("Authorization") != "Bearer proxy-new" {
						t.Error("stale token reached inference")
					}
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
				}))
				t.Cleanup(upstream.Close)
				entry := poolProxyEntry(t, p, upstream.URL)
				ids = append(ids, entry.ID)
			}
			if err := p.Store.UpdateAccount(before.ID, func(a *Account) error { a.ExpiresAt = time.Now().Add(-time.Hour).Unix(); a.Proxies = ids; return nil }); err != nil {
				t.Fatal(err)
			}
			p.invalidateAccounts()
			r := proxyRequest("POST", "/responses", `{"model":"fixture","stream":true}`)
			r.Header.Set("Session-Id", "new-proxy-thread")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != 200 || !((calls[0].Load() == 2 && calls[1].Load() == 0) || (calls[0].Load() == 0 && calls[1].Load() == 2)) {
				t.Fatal("unknown-owner refresh and initial inference switched configured proxy")
			}
			stored, err := p.Store.Get(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gatewayAccountRef(stored) == gatewayAccountRef(before) || gatewayAccountOwnerRef(stored) != gatewayAccountOwnerRef(before) {
				t.Fatal("fixture did not cross fullref while preserving static route identity")
			}
		})
	}
}

func TestPGGatewayUnknownOwnerCredentialReplacementAndRestart(t *testing.T) {
	p1, p2, token := adminKeyGatewayPGProxies(t)
	redisDB, _ := gatewayRedisSessionFixture(t)
	p1.Store.DB.Redis, p1.Store.DB.Namespace = redisDB.Redis, redisDB.Namespace
	const session = "unknown-owner-pg-thread"
	key := fmt.Sprintf("%sgateway:thread:%x", redisDB.Namespace, sha256.Sum256([]byte("admin\x00"+session)))
	t.Cleanup(func() { _ = redisDB.Redis.Del(context.Background(), key).Err() })
	a := pgImportForTest(t, p1.Store, "unknown-owner", "initial-opaque-token")
	if gatewayCompleteOwner(a) {
		t.Fatal("PG fixture unexpectedly has a complete owner")
	}
	first, release, err := p1.pickSessionAccount(t.Context(), token, "admin", session)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := p1.Store.UpdateAccount(a.ID, func(a *Account) error { a.Notes = "metadata-only"; return nil }); err != nil {
		t.Fatal(err)
	}
	p2.invalidateAccounts()
	unchanged, release, err := p2.pickSessionAccount(t.Context(), token, "admin", session)
	if err != nil {
		t.Fatal("unknown unchanged credentials broke across process metadata edit")
	}
	release()
	if gatewayAccountRef(unchanged) != gatewayAccountRef(first) {
		t.Fatal("PG metadata/CV contaminated credential identity")
	}
	current, err := p1.Store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	incoming := current
	incoming.AccessToken = "replacement-opaque-token"
	incoming.RefreshToken = "replacement-opaque-refresh"
	if _, err := p1.Store.reauthorizeManagedAccount(t.Context(), current.ID, current.Version, incoming, false); err != nil {
		t.Fatal(err)
	}
	// A fresh process object must derive exactly the same new credential digest
	// from persisted state while refusing the old Redis conversation binding.
	restarted := &Proxy{Store: p2.Store, Settings: p2.Settings, AdminKey: token}
	_, release, err = restarted.pickSessionAccount(t.Context(), token, "admin", session)
	if release != nil {
		release()
	}
	var changed *gatewayOwnerChangedError
	if !errors.As(err, &changed) {
		t.Fatal("restarted instance reused old unknown-owner Redis binding")
	}
	stored, err := p1.Store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != incoming.RefreshToken || gatewayAccountRef(stored) == gatewayAccountRef(first) {
		t.Fatal("replacement lost credentials or identity generation")
	}
	if bound, err := restarted.sessionAccount(t.Context(), "admin", session, ""); err != nil || bound != gatewayAccountRef(first) {
		t.Fatal("restart migrated encrypted history")
	}
	if _, err := restarted.authorizedGatewayWSAccount(token, "admin", first); err == nil {
		t.Fatal("WS authorization after restart accepted replaced unknown owner")
	}
}
