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

func gatewayOwnerCacheRefresh(t *testing.T, store Store, previous Account) Account {
	t.Helper()
	fresh, err := parseAccount("test", gatewayRevisionCredentials(t, "cache-owner-a", "same-owner-refreshed"))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.Refresh(previous.ID, previous.AccessToken, func(account Account) (Account, error) {
		account.AccessToken, account.RefreshToken, account.ExpiresAt = fresh.AccessToken, fresh.RefreshToken, fresh.ExpiresAt
		return account, nil
	})
	if err != nil || gatewayAccountRef(updated) != gatewayAccountRef(previous) || updated.AccessToken == previous.AccessToken {
		t.Fatalf("same-owner refresh fixture changed ownership or failed to rotate credentials: %v", err)
	}
	return updated
}

func gatewayOwnerCacheAssertWarm(t *testing.T, p *Proxy, original Account) {
	t.Helper()
	p.accountsMu.Lock()
	defer p.accountsMu.Unlock()
	if p.accountsAt.IsZero() || time.Since(p.accountsAt) >= 5*time.Second {
		t.Fatal("fixture no longer exercises the live account-cache window")
	}
	for _, cached := range p.accounts {
		if cached.ID == original.ID {
			if gatewayAccountRef(cached) != gatewayAccountRef(original) || cached.ExpiresAt <= time.Now().Add(time.Minute).Unix() {
				t.Fatal("fixture cache is not the original unexpired owner snapshot")
			}
			return
		}
	}
	t.Fatal("fixture did not warm the selected account cache")
}

func TestGatewayOwnerCacheHTTPRejectsABAWithoutInvalidation(t *testing.T) {
	var inference, refreshes atomic.Int32
	var expectedAuthorization atomic.Value
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			refreshes.Add(1)
			http.Error(w, "unexpected expiry refresh", http.StatusBadGateway)
			return
		}
		inference.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") != expectedAuthorization.Load().(string) {
			t.Error("warm account cache sent superseded access credentials")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"cache-owner\",\"output\":[]}}\n\n")
	})
	originalRaw := gatewayRevisionCredentials(t, "cache-owner-a", "initial")
	original := gatewayRevisionImport(t, p.Store, originalRaw)
	expectedAuthorization.Store("Bearer " + original.AccessToken)
	const session = "owner-cache-http-thread"
	send := func() *httptest.ResponseRecorder {
		r := proxyRequest(http.MethodPost, "/responses", `{"model":"test","stream":false,"input":[{"type":"reasoning","encrypted_content":"synthetic-existing-history"}]}`)
		r.Header.Set("Session-Id", session)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	if response := send(); response.Code != http.StatusOK || inference.Load() != 1 {
		t.Fatalf("cannot establish warm HTTP owner binding: HTTP %d", response.Code)
	}
	gatewayOwnerCacheAssertWarm(t, p, original)
	rotated := gatewayOwnerCacheRefresh(t, p.Store, original)
	expectedAuthorization.Store("Bearer " + rotated.AccessToken)
	if response := send(); response.Code != http.StatusOK || inference.Load() != 2 {
		t.Fatalf("same-owner refresh broke existing warm-cache thread: HTTP %d", response.Code)
	}
	// Both central writes happen inside the existing five-second cache window;
	// no invalidation or synthetic cache expiry may mask the owner recheck.
	_ = gatewayRevisionImport(t, p.Store, gatewayRevisionCredentials(t, "cache-owner-b", "replacement"))
	restored := gatewayRevisionImport(t, p.Store, originalRaw)
	if restored.AccessToken != original.AccessToken || restored.OwnerRevision == original.OwnerRevision {
		t.Fatal("ABA fixture did not restore credentials with a fresh persisted owner revision")
	}
	gatewayOwnerCacheAssertWarm(t, p, original)
	response := send()
	if response.Code != http.StatusConflict && response.Code != http.StatusServiceUnavailable || inference.Load() != 2 || refreshes.Load() != 0 {
		t.Fatalf("cached HTTP owner bypassed ABA replacement: HTTP=%d inference=%d refresh=%d", response.Code, inference.Load(), refreshes.Load())
	}
	if bound, err := p.sessionAccount(t.Context(), "admin", session, ""); err != nil || bound != gatewayAccountRef(original) {
		t.Fatal("warm-cache rejection migrated the original HTTP history binding")
	}
}

func TestGatewayOwnerCacheConnectedWebSocketRejectsABAWithoutInvalidation(t *testing.T) {
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
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"cache-ws-owner-%d","status":"completed","output":[]}}`, turn))); err != nil {
				return
			}
		}
	})
	originalRaw := gatewayRevisionCredentials(t, "cache-owner-a", "initial")
	original := gatewayRevisionImport(t, p.Store, originalRaw)
	enableGatewayWS(t, p, "")
	server := httptest.NewServer(p)
	defer server.Close()
	const session = "owner-cache-ws-thread"
	conn, _ := gatewayWSDial(t, server.URL, session)
	defer conn.CloseNow()
	send := func() []byte {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
			t.Fatal(err)
		}
		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	if !strings.Contains(string(send()), "response.completed") {
		t.Fatal("cannot establish warm WS owner binding")
	}
	waitGatewayWSReleased(t, p)
	gatewayOwnerCacheAssertWarm(t, p, original)
	_ = gatewayOwnerCacheRefresh(t, p.Store, original)
	if !strings.Contains(string(send()), "response.completed") || turns.Load() != 2 {
		t.Fatal("same-owner rotation broke the connected WS")
	}
	waitGatewayWSReleased(t, p)
	_ = gatewayRevisionImport(t, p.Store, gatewayRevisionCredentials(t, "cache-owner-b", "replacement"))
	restored := gatewayRevisionImport(t, p.Store, originalRaw)
	if restored.OwnerRevision == original.OwnerRevision {
		t.Fatal("WS fixture did not advance persistent owner revision")
	}
	gatewayOwnerCacheAssertWarm(t, p, original)
	var rejection struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if json.Unmarshal(send(), &rejection) != nil || rejection.Type != "error" || rejection.Status != http.StatusForbidden || turns.Load() != 2 || handshakes.Load() != 1 {
		t.Fatalf("cached connected WS bypassed owner replacement: status=%d turns=%d handshakes=%d", rejection.Status, turns.Load(), handshakes.Load())
	}
	waitGatewayWSReleased(t, p)
	if bound, err := p.sessionAccount(t.Context(), "admin", session, ""); err != nil || bound != gatewayAccountRef(original) {
		t.Fatal("warm-cache rejection migrated the original WS history binding")
	}
}

func TestPGGatewayOwnerCacheReauthorizeABAWithoutInvalidation(t *testing.T) {
	p1, p2, token := adminKeyGatewayPGProxies(t)
	redisDB, _ := gatewayRedisSessionFixture(t)
	p1.Store.DB.Redis, p1.Store.DB.Namespace = redisDB.Redis, redisDB.Namespace
	originalRaw := gatewayRevisionCredentials(t, "cache-owner-a", "initial")
	original := gatewayRevisionImport(t, p1.Store, originalRaw)
	const session = "owner-cache-pg-thread"
	key := fmt.Sprintf("%sgateway:thread:%x", redisDB.Namespace, sha256.Sum256([]byte("admin\x00"+session)))
	t.Cleanup(func() { _ = redisDB.Redis.Del(context.Background(), key).Err() })
	_, release, err := p2.pickSessionAccount(t.Context(), token, "admin", session)
	if err != nil {
		t.Fatal("could not warm shared owner account cache")
	}
	release()
	gatewayOwnerCacheAssertWarm(t, p2, original)
	reauthorize := func(raw []byte, changeOwner bool) Account {
		current, err := p1.Store.Get(original.ID)
		if err != nil {
			t.Fatal(err)
		}
		incoming, err := parseAccount("test", raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p1.Store.reauthorizeManagedAccount(t.Context(), current.ID, current.Version, incoming, changeOwner); err != nil {
			t.Fatal("synthetic cached owner CAS failed")
		}
		updated, err := p1.Store.Get(original.ID)
		if err != nil {
			t.Fatal(err)
		}
		return updated
	}
	rotated := reauthorize(gatewayRevisionCredentials(t, "cache-owner-a", "rotated"), false)
	ready, release, err := p2.pickSessionAccount(t.Context(), token, "admin", session)
	if err != nil {
		t.Fatal("same-owner CAS broke shared cached conversation")
	}
	release()
	if ready.AccessToken != rotated.AccessToken || gatewayAccountRef(ready) != gatewayAccountRef(original) {
		t.Error("shared warm cache failed to load same-owner fresh credentials")
	}
	_ = reauthorize(gatewayRevisionCredentials(t, "cache-owner-b", "replacement"), true)
	restored := reauthorize(originalRaw, true)
	if restored.OwnerRevision == original.OwnerRevision || restored.AccessToken != original.AccessToken {
		t.Fatal("PG cached owner fixture did not perform exact-credential ABA")
	}
	gatewayOwnerCacheAssertWarm(t, p2, original)
	_, release, err = p2.pickSessionAccount(t.Context(), token, "admin", session)
	if release != nil {
		release()
	}
	if err == nil {
		t.Error("stale shared account cache revived the original PG owner")
	}
	if _, err := p2.authorizedGatewayWSAccount(token, "admin", original); err == nil {
		t.Error("stale shared account cache authorized the original WS owner")
	}
	if bound, err := p2.sessionAccount(t.Context(), "admin", session, ""); err != nil || bound != gatewayAccountRef(original) {
		t.Fatal("PG cache rejection rewrote the old Redis binding")
	}
}
