package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A terminal OAuth grant response must remain remembered after the short
// transient backoff, while the actual same credential snapshot stays current.
func TestGatewayRefreshPermanentGrantDoesNotExpireIntoAnotherExchange(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":"invalid_grant","error_description":"synthetic-private-detail"}`},
		{401, `{"error":{"code":"refresh_token_invalidated","message":"synthetic-private-detail"}}`},
		{401, `{"error":"unknown-oauth-rejection"}`},
	} {
		t.Run(fmt.Sprint(tc.status)+"/"+tc.body[:10], func(t *testing.T) {
			var exchanges atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.recoverUnauthorized(context.Background(), account, p.Client); err == nil || strings.Contains(err.Error(), "synthetic-private-detail") {
				t.Fatalf("missing redacted initial grant failure: %v", err)
			}
			// Simulate expiration of every transient entry, without sleeping.
			p.refreshCooldown.mu.Lock()
			for key, failure := range p.refreshCooldown.entries {
				failure.Until = time.Now().Add(-time.Second).UnixMilli()
				p.refreshCooldown.entries[key] = failure
			}
			p.refreshCooldown.mu.Unlock()
			if _, err := p.recoverUnauthorized(context.Background(), account, p.Client); err == nil {
				t.Fatal("same permanently rejected credential became usable")
			}
			if exchanges.Load() != 1 {
				t.Fatalf("permanent grant failure reused the same RT after transient TTL: OAuth exchanges=%d, want 1", exchanges.Load())
			}
		})
	}
}

func TestGatewayRefreshPermanentClassificationMatchesGrantResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"bad_request_invalid_grant", 400, `{"error":"INVALID_GRANT"}`, true},
		{"nested_revoked", 400, `{"error":{"code":"refresh_token_invalidated"}}`, true},
		{"expired_500", 500, `{"error":"refresh_token_expired"}`, true},
		{"reused_429", 429, `{"error":"refresh_token_reused"}`, true},
		{"top_expired", 403, `{"code":"refresh_token_expired"}`, true},
		{"reused", 409, `{"error":"refresh_token_reused"}`, true},
		{"grant_401_even_malformed", 401, `not json`, true},
		{"invalid_grant_500", 500, `{"error":"invalid_grant"}`, false},
		{"token_revoked_not_refresh_code", 400, `{"error":"token_revoked"}`, false},
		{"message_is_not_code", 400, `{"error":{"message":"refresh_token_invalidated"}}`, false},
		{"malformed", 400, `{"error":"invalid_grant"} {}`, false},
		{"null_error_fallback", 400, `{"error":null,"code":"invalid_grant"}`, true},
		{"object_missing_code_fallback", 400, `{"error":{},"code":"invalid_grant"}`, true},
		{"wrongtype_error_fallback", 400, `{"error":123,"code":"invalid_grant"}`, true},
		{"empty_error_no_fallback", 400, `{"error":"","code":"invalid_grant"}`, false},
		{"nested_unknown_no_fallback", 400, `{"error":{"code":"unknown"},"code":"invalid_grant"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.refreshAccountWithCooldown(context.Background(), p.Client, account, defaultSettings())
			var failure *gatewayOAuthRefreshError
			if !errors.As(err, &failure) || failure.Permanent != tc.permanent || calls.Load() != 1 {
				t.Fatalf("grant classification differs: error=%v terminal=%v calls=%d", err, failure, calls.Load())
			}
			if tc.permanent && strings.Contains(err.Error(), "retry later") {
				t.Fatal("permanent failure directed caller to retry later")
			}
		})
	}
}

func TestGatewayRefreshPermanentPrunesDeletedOwnersWithoutLosingNewerFailures(t *testing.T) {
	p := newTestProxy(t, 1)
	a, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	key, snapshot := gatewayPermanentRefreshKeys(a, p.TokenURL)
	p.refreshCooldown.rememberPermanent(key, snapshot, 400)
	watermark := p.refreshCooldown.permanentWatermark()
	added := Account{ID: "synthetic-added-after-list-start", AccessToken: "synthetic-at", RefreshToken: "synthetic-rt"}
	addedKey, addedSnapshot := gatewayPermanentRefreshKeys(added, p.TokenURL)
	p.refreshCooldown.rememberPermanent(addedKey, addedSnapshot, 401)
	p.refreshCooldown.prunePermanentOwners(nil, watermark)
	if p.refreshCooldown.permanentFailure(addedKey, addedSnapshot) == nil {
		t.Fatal("old list removed new account's permanent rejection")
	}
	if p.refreshCooldown.permanentFailure(key, snapshot) != nil {
		t.Fatal("absent owner was not pruned")
	}
	// Present owners survive even when the list contains old credentials.
	p.refreshCooldown.rememberPermanent(key, "newer-snapshot", 401)
	p.refreshCooldown.prunePermanentOwners([]Account{a}, p.refreshCooldown.permanentWatermark())
	if p.refreshCooldown.permanentFailure(key, "newer-snapshot") == nil {
		t.Fatal("stale credential list removed current failure")
	}
	// The actual snapshot hook prunes after a confirmed delete and fresh reload.
	if err := p.Store.DeleteVersion(a.ID, a.Version); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	if _, err := p.accountsSnapshot(); err != nil {
		t.Fatal(err)
	}
	if len(p.refreshCooldown.permanent) != 0 {
		t.Fatal("deleted account failures remain after authoritative reload")
	}
}

func TestGatewayRefreshPermanentSnapshotChangeAndMetadataIsolation(t *testing.T) {
	for _, field := range []string{"access", "refresh", "client", "endpoint"} {
		t.Run(field, func(t *testing.T) {
			var exchanges atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			})
			a, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = p.refreshAccountWithCooldown(context.Background(), p.Client, a, defaultSettings())
			a.Name, a.Notes, a.Version, a.CredentialVersion, a.OwnerRevision = "renamed", "metadata", 999, 999, "changed-local-revision"
			_, _ = p.refreshAccountWithCooldown(context.Background(), p.Client, a, defaultSettings())
			if exchanges.Load() != 1 {
				t.Fatal("metadata edit bypassed rejected credential snapshot")
			}
			switch field {
			case "access":
				a.AccessToken = "synthetic-new-access"
			case "refresh":
				a.RefreshToken = "synthetic-new-refresh"
			case "client":
				a.ClientID = "synthetic-new-client"
			case "endpoint":
				p.TokenURL += "/new-route"
			}
			_, _ = p.refreshAccountWithCooldown(context.Background(), p.Client, a, defaultSettings())
			if exchanges.Load() != 2 {
				t.Fatal("new credential snapshot remained blocked")
			}
			if len(p.refreshCooldown.permanent) != 1 {
				t.Fatal("retained historical credential slots")
			}
			raw, _ := json.Marshal(p.refreshCooldown.permanent)
			for _, secret := range []string{a.AccessToken, a.RefreshToken, a.ClientID, p.TokenURL, "invalid_grant"} {
				if secret != "" && strings.Contains(string(raw), secret) {
					t.Fatal("permanent memory retained private values")
				}
			}
		})
	}
}

func TestGatewayRefreshPermanentMemoryDoesNotSurviveNewManager(t *testing.T) {
	var exchanges atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		w.WriteHeader(401)
	})
	a, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.refreshAccountWithCooldown(context.Background(), p.Client, a, defaultSettings())
	fresh := &Proxy{TokenURL: p.TokenURL}
	_, _ = fresh.refreshAccountWithCooldown(context.Background(), p.Client, a, defaultSettings())
	if exchanges.Load() != 2 {
		t.Fatal("unexpected persistent terminal-state scope")
	}
}

func TestGatewayRefreshPermanentGrantAfterCallerLeavesStillRemembered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var exchanges atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		cancel()
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	})
	a, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.refreshAccountWithCooldown(ctx, p.Client, a, defaultSettings())
	_, _ = p.refreshAccountWithCooldown(context.Background(), p.Client, a, defaultSettings())
	if exchanges.Load() != 1 {
		t.Fatal("client cancellation erased observed permanent grant rejection")
	}
}

func TestGatewayRefreshPermanentConcurrentAndStale401CannotClearCurrentFailure(t *testing.T) {
	var exchanges atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	})
	old, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount(old.ID, func(a *Account) error {
		a.AccessToken = "synthetic-current-access"
		a.RefreshToken = "synthetic-current-refresh"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := p.Store.Get(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := p.recoverUnauthorized(context.Background(), current, p.Client); err == nil {
				t.Error("permanent failure accepted")
			}
		})
	}
	group.Wait()
	if exchanges.Load() != 1 {
		t.Fatalf("concurrent permanent grants=%d", exchanges.Load())
	}
	updated, err := p.recoverUnauthorized(context.Background(), old, p.Client)
	if err != nil || updated.AccessToken != current.AccessToken {
		t.Fatalf("stale 401 did not reload current snapshot: %v", err)
	}
	_, err = p.recoverUnauthorized(context.Background(), current, p.Client)
	if err == nil || exchanges.Load() != 1 {
		t.Fatal("stale 401 erased newer snapshot rejection")
	}
}
