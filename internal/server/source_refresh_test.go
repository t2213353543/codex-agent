package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Codex 0.154.0 login/src/auth/manager.rs RefreshResponse has no expires_in
// field. Successful refresh responses must retain the rotated credential pair.
func TestSourceRefreshPersistsRotationWithoutExpiresIn(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Unix()
	claims, err := json.Marshal(map[string]any{"exp": expiry})
	if err != nil {
		t.Fatal(err)
	}
	jwt := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
	for _, test := range []struct {
		name, access string
		wantExpiry   int64
	}{
		{"JWT expiry", jwt, expiry},
		{"opaque access token", "opaque-refreshed-access", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{
					"access_token": test.access, "refresh_token": "rotated-without-expiry",
					"id_token": managedIdentityTestJWT("test-account", "fixture-user"),
				})
			})
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.ExpiresAt = time.Now().Add(-time.Minute).Unix()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			updated, err := p.ensureReady(context.Background(), before)
			if err != nil {
				t.Fatalf("native refresh contract rejected: %v", err)
			}
			stored, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			for _, account := range []Account{updated, stored} {
				if account.AccessToken != test.access || account.RefreshToken != "rotated-without-expiry" || account.ExpiresAt != test.wantExpiry {
					t.Fatalf("refresh lost rotated tokens or expiry: access=%t refresh=%t expiry=%d", account.AccessToken == test.access, account.RefreshToken == "rotated-without-expiry", account.ExpiresAt)
				}
				if account.ID != before.ID || account.AccountID != before.AccountID {
					t.Fatal("refresh changed account identity")
				}
			}
		})
	}
}

func TestSource401RecoveryRefreshesOnceAndInvalidatesCache(t *testing.T) {
	var refreshes atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, gatewayKnownOwnerRefreshFixture(`{"access_token":"after-401","refresh_token":"rotated-after-401"}`, "test-account", "fixture-user"))
	})
	accounts, err := p.accountsSnapshot()
	if err != nil || len(accounts) != 1 {
		t.Fatalf("account snapshot: count=%d err=%v", len(accounts), err)
	}
	rejected := accounts[0] // The token's recorded expiry is still in the future.
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			updated, err := p.recoverUnauthorized(context.Background(), rejected, p.Client)
			if err != nil {
				t.Error(err)
				return
			}
			if updated.AccessToken != "after-401" || updated.RefreshToken != "rotated-after-401" || gatewayAccountRef(updated) != gatewayAccountRef(rejected) {
				t.Error("401 recovery did not return the original account's refreshed tokens")
			}
		})
	}
	workers.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refresh token used %d times for the same rejected bearer", refreshes.Load())
	}
	accounts, err = p.accountsSnapshot()
	if err != nil || len(accounts) != 1 || accounts[0].AccessToken != "after-401" || accounts[0].ExpiresAt != 0 {
		t.Fatalf("recovery left the expired snapshot cached: count=%d err=%v", len(accounts), err)
	}
}

func TestSource401RecoveryReloadsConcurrentCredentials(t *testing.T) {
	p := proxyFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("concurrently refreshed credentials triggered another OAuth exchange")
	})
	rejected, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.AccessToken, a.RefreshToken = "concurrent-access", "concurrent-refresh"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := p.recoverUnauthorized(context.Background(), rejected, p.Client)
	if err != nil || updated.AccessToken != "concurrent-access" || updated.RefreshToken != "concurrent-refresh" {
		t.Fatalf("concurrent credential reload failed: %v", err)
	}
}

func TestSource401RecoveryRejectsConcurrentIdentityReplacement(t *testing.T) {
	for _, replaceToken := range []bool{false, true} {
		t.Run(map[bool]string{false: "same bearer", true: "new bearer"}[replaceToken], func(t *testing.T) {
			p := proxyFixture(t, func(http.ResponseWriter, *http.Request) {
				t.Error("replacement account was refreshed during recovery of another identity")
			})
			rejected, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Store.UpdateAccount("test", func(a *Account) error {
				a.AccountID = "replacement-chatgpt-account"
				if replaceToken {
					a.AccessToken = "replacement-access"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.recoverUnauthorized(context.Background(), rejected, p.Client); err == nil {
				t.Fatal("401 recovery silently switched upstream identities")
			}
		})
	}
}

func TestSource401RecoveryDoesNotStartAfterCancellation(t *testing.T) {
	p := proxyFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("canceled request started a new OAuth exchange")
	})
	rejected, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.recoverUnauthorized(ctx, rejected, p.Client); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled recovery error = %v", err)
	}
}

type sourceRefreshRoundTripper func(*http.Request) (*http.Response, error)

func (f sourceRefreshRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestSource401RecoveryUsesSelectedClientAndFingerprint(t *testing.T) {
	p := newTestProxy(t, 1)
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.CodexFingerprintEnabled = true
	settings.CodexFingerprintProfile = "linux"
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	profile, _ := codexFingerprintProfile("linux")
	p.Client = &http.Client{Transport: sourceRefreshRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("401 recovery bypassed its selected outbound client")
		return nil, errors.New("unexpected direct OAuth request")
	})}
	var used atomic.Bool
	selected := &http.Client{Transport: sourceRefreshRoundTripper(func(r *http.Request) (*http.Response, error) {
		used.Store(true)
		if r.Header.Get("User-Agent") != profile.UserAgent || r.Header.Get("Originator") != profile.Originator {
			t.Error("401 recovery ignored the configured application identity")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(gatewayKnownOwnerRefreshFixture(`{"access_token":"selected-client-access","refresh_token":"selected-client-refresh"}`, "test-account", "fixture-user")))}, nil
	})}
	rejected, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := p.recoverUnauthorized(context.Background(), rejected, selected)
	if err != nil || !used.Load() || updated.AccessToken != "selected-client-access" {
		t.Fatalf("selected refresh client was not used successfully: used=%v err=%v", used.Load(), err)
	}
}
