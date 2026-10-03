package server

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestSourceEnsureReadyRejectsReplacedIdentity(t *testing.T) {
	for _, change := range []string{"same bearer", "new bearer", "disabled"} {
		t.Run(change, func(t *testing.T) {
			p := proxyFixture(t, func(http.ResponseWriter, *http.Request) {
				t.Error("replacement or disabled account was refreshed")
			})
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			selected, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				if change == "disabled" {
					a.Enabled = false
				} else {
					a.AccountID = "replacement-identity"
				}
				if change == "new bearer" {
					a.AccessToken = "replacement-access"
				}
				// Even a non-expiring replacement must not bypass the identity guard.
				a.ExpiresAt = 0
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			ready, err := p.ensureReady(context.Background(), selected)
			if err == nil || gatewayAccountRef(ready) != gatewayAccountRef(selected) {
				t.Fatal("expiry refresh returned the replacement account to the old session")
			}
		})
	}
}

func TestSourceEnsureReadyInvalidatesExpiredSnapshot(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"renewed-access","refresh_token":"renewed-refresh","expires_in":3600}`, "test-account", "fixture-user"))
	})
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.ExpiresAt = time.Now().Add(-time.Hour).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := p.accountsSnapshot()
	if err != nil || len(before) != 1 {
		t.Fatal("cannot load test account snapshot")
	}
	ready, err := p.ensureReady(context.Background(), before[0])
	if err != nil || ready.AccessToken != "renewed-access" || gatewayAccountRef(ready) != gatewayAccountRef(before[0]) {
		t.Fatalf("same-identity refresh failed: %v", err)
	}
	after, err := p.accountsSnapshot()
	if err != nil || len(after) != 1 || after[0].AccessToken != "renewed-access" || after[0].ExpiresAt <= before[0].ExpiresAt {
		t.Fatal("expiry refresh left the rejected bearer in the account cache")
	}
}

func TestSourceRefreshPersistsRefreshTokenOnlyRotation(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"refresh_token":"rotation-without-access","expires_in":3600}`)
	})
	before, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.recoverUnauthorized(context.Background(), before, p.Client)
	if err != nil {
		t.Fatalf("optional access_token caused rotating credentials to be lost: %v", err)
	}
	after, err := p.Store.Get("test")
	if err != nil || after.RefreshToken != "rotation-without-access" || after.AccessToken != before.AccessToken || after.ExpiresAt != before.ExpiresAt {
		t.Fatal("refresh-only response changed the old access token/expiry or failed to save rotation")
	}
}
