package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func expiredCooldownAccount(t *testing.T, p *Proxy) Account {
	t.Helper()
	if err := p.Store.UpdateAccount("test", func(account *Account) error {
		account.ExpiresAt = time.Now().Add(-time.Hour).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestGatewayRefreshCooldownMergesConcurrentExpiryAnd401Failures(t *testing.T) {
	for _, status := range []int{400, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var exchanges atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"do-not-cache-upstream-body"}`)
			})
			account := expiredCooldownAccount(t, p)
			var group sync.WaitGroup
			for i := range 8 {
				group.Go(func() {
					var err error
					if i%2 == 0 {
						_, err = p.ensureReady(context.Background(), account)
					} else {
						_, err = p.recoverUnauthorized(context.Background(), account, p.Client)
					}
					var failure *gatewayOAuthRefreshError
					if !errors.As(err, &failure) || failure.StatusCode != status || strings.Contains(err.Error(), "do-not-cache") {
						t.Errorf("missing safe OAuth failure: %v", err)
						return
					}
					p.noteAccountError(account, err)
				})
			}
			group.Wait()
			if exchanges.Load() != 1 {
				t.Fatalf("eight waiting requests repeated OAuth %d times", exchanges.Load())
			}
			// A metadata/version edit must not bypass the credential cooldown.
			if _, err := p.Store.UpdateAccountVersion(account.ID, 0, func(a *Account) error {
				a.Name, a.Notes = "renamed", "metadata changed"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.ensureReady(context.Background(), account); err == nil || exchanges.Load() != 1 {
				t.Fatalf("metadata changes reset the credential failure: exchanges=%d err=%v", exchanges.Load(), err)
			}
		})
	}
}

func TestGatewayRefreshCooldownNewCredentialsBypassOldFailure(t *testing.T) {
	var exchanges atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		var request map[string]string
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["refresh_token"] != "newly-imported-refresh" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	})
	before := expiredCooldownAccount(t, p)
	if _, err := p.ensureReady(context.Background(), before); err == nil {
		t.Fatal("initial OAuth failure was accepted")
	}
	raw, _ := json.Marshal(map[string]any{
		"access_token": "newly-imported-access", "refresh_token": "newly-imported-refresh", "chatgpt_account_id": before.AccountID,
		"expires_at": time.Now().Add(-time.Hour).Unix(),
	})
	if _, err := p.Store.ImportDetailed([]ImportItem{{Name: before.Name, Credentials: raw}}, AccountDefaults{}, ImportOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	imported, err := p.Store.Get(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := p.ensureReady(context.Background(), imported)
	if err != nil || updated.AccessToken != "new-access" || updated.RefreshToken != "rotated-refresh" || exchanges.Load() != 2 {
		t.Fatalf("reimport remained blocked by obsolete credentials: exchanges=%d err=%v", exchanges.Load(), err)
	}
	stored, err := p.Store.Get(before.ID)
	if err != nil || stored.RefreshToken != "rotated-refresh" {
		t.Fatal("cooldown interfered with Store CAS persistence")
	}
}

func TestGatewayRefreshCooldownExpiresAndPersistsRTOnlyRotation(t *testing.T) {
	var exchanges atomic.Int32
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if exchanges.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"refresh_token":"rotated-without-new-access"}`)
	})
	account := expiredCooldownAccount(t, p)
	if _, err := p.ensureReady(context.Background(), account); err == nil {
		t.Fatal("expected initial OAuth failure")
	}
	key := gatewayRefreshFailureKey(account, p.TokenURL)
	p.refreshCooldown.mu.Lock()
	entry := p.refreshCooldown.entries[key]
	entry.Until = time.Now().Add(-time.Millisecond).UnixMilli()
	p.refreshCooldown.entries[key] = entry
	p.refreshCooldown.mu.Unlock()
	updated, err := p.ensureReady(context.Background(), account)
	if err != nil || exchanges.Load() != 2 || updated.AccessToken != account.AccessToken || updated.RefreshToken != "rotated-without-new-access" {
		t.Fatalf("expired cooldown lost an RT-only success: exchanges=%d err=%v", exchanges.Load(), err)
	}
	stored, err := p.Store.Get(account.ID)
	if err != nil || stored.RefreshToken != "rotated-without-new-access" {
		t.Fatal("RT-only OAuth rotation was not persisted")
	}
	if _, exists := p.refreshCooldown.get(key, time.Now()); exists {
		t.Fatal("successful exchange kept the previous failure")
	}
}

func TestGatewayRefreshCooldownDoesNotCacheCancellationOrProxyChanges(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errGatewayProxyUnavailable} {
		t.Run(cause.Error(), func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: sourceRefreshRoundTripper(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, cause
			})}
			p := &Proxy{TokenURL: "http://oauth.invalid/token"}
			account := Account{ID: "synthetic", AccessToken: "old-access", RefreshToken: "old-refresh"}
			for range 2 {
				if _, err := p.refreshAccountWithCooldown(context.Background(), client, account, defaultSettings()); !errors.Is(err, cause) {
					t.Fatalf("lost noncacheable cause: %v", err)
				}
			}
			if calls.Load() != 2 || len(p.refreshCooldown.entries) != 0 {
				t.Fatal("canceled or obsolete proxy exchange polluted the shared failure cache")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := p.refreshAccountWithCooldown(ctx, client, account, defaultSettings()); !errors.Is(err, context.Canceled) || calls.Load() != 2 {
				t.Fatal("pre-canceled request started OAuth")
			}
		})
	}
}

func TestGatewayRefreshCooldownHonorsBoundedRetryAfter(t *testing.T) {
	for _, test := range []struct {
		header string
		min    time.Duration
		max    time.Duration
	}{
		{"", 3 * time.Second, 5 * time.Second},
		{"12", 11 * time.Second, 13 * time.Second},
		{"3600", 59 * time.Second, time.Minute},
		{time.Now().Add(20 * time.Second).UTC().Format(http.TimeFormat), 17 * time.Second, 21 * time.Second},
	} {
		t.Run(test.header, func(t *testing.T) {
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Retry-After", test.header)
				w.WriteHeader(http.StatusTooManyRequests)
			})
			account := expiredCooldownAccount(t, p)
			if _, err := p.ensureReady(context.Background(), account); err == nil {
				t.Fatal("expected OAuth 429")
			}
			entry, exists := p.refreshCooldown.get(gatewayRefreshFailureKey(account, p.TokenURL), time.Now())
			remaining := time.Until(time.UnixMilli(entry.Until))
			if !exists || remaining < test.min || remaining > test.max {
				t.Fatalf("Retry-After cooldown = %s, expected [%s,%s]", remaining, test.min, test.max)
			}
		})
	}
}

func TestGatewayRefreshCooldownIsBoundedAndKeyOmitsSecrets(t *testing.T) {
	cache := gatewayRefreshCooldown{}
	now := time.Now()
	base := Account{ID: "one", AccountID: "upstream", AccessToken: "synthetic-secret-access", RefreshToken: "synthetic-secret-refresh", CredentialVersion: 5}
	key := gatewayRefreshFailureKey(base, "http://oauth.invalid")
	if len(key) != 64 || strings.Contains(key, "secret") {
		t.Fatal("cache key exposes credentials")
	}
	metadata := base
	metadata.Name, metadata.Version, metadata.LastError = "renamed", 90, "last attempt failed"
	if gatewayRefreshFailureKey(metadata, "http://oauth.invalid") != key {
		t.Fatal("metadata changes bypass a credential-scoped failure")
	}
	for _, change := range []func(*Account){
		func(a *Account) { a.CredentialVersion++ },
		func(a *Account) { a.AccessToken = "other-access" },
		func(a *Account) { a.RefreshToken = "other-refresh" },
		func(a *Account) { a.ClientID = "other-client" },
	} {
		updated := base
		change(&updated)
		if gatewayRefreshFailureKey(updated, "http://oauth.invalid") == key {
			t.Fatal("new credential state reused an old cooldown")
		}
	}
	for i := range gatewayRefreshFailureLimit + 1 {
		cache.put(fmt.Sprint(i), gatewayRefreshFailure{Kind: "http", StatusCode: 400, Until: now.Add(gatewayRefreshFailureTTL).UnixMilli()}, now)
	}
	if len(cache.entries) != gatewayRefreshFailureLimit {
		t.Fatalf("failure cache is not bounded: %d", len(cache.entries))
	}
	if _, exists := cache.get("0", now); exists {
		t.Fatal("failure cache retained oldest entry beyond its capacity")
	}
	if _, exists := cache.get("1", now.Add(gatewayRefreshFailureTTL+time.Second)); exists {
		t.Fatal("expired failure was reused")
	}
}

func TestGatewayRefreshCooldownDoesNotInventRetryAfterZeroDelay(t *testing.T) {
	for _, header := range []string{"0", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)} {
		t.Run(header, func(t *testing.T) {
			var exchanges atomic.Int32
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Retry-After", header)
				w.WriteHeader(http.StatusTooManyRequests)
			})
			account := expiredCooldownAccount(t, p)
			for range 2 {
				if _, err := p.ensureReady(context.Background(), account); err == nil {
					t.Fatal("expected OAuth 429")
				}
			}
			if exchanges.Load() != 2 || len(p.refreshCooldown.entries) != 0 {
				t.Fatal("explicit zero/past Retry-After was replaced by a default delay")
			}
		})
	}
}

func TestGatewayRefreshCooldownRedisFailureFallsBackLocally(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	p := &Proxy{Store: Store{DB: &Database{Redis: client, Namespace: "synthetic:"}}, TokenURL: server.URL}
	account := Account{ID: "synthetic", AccessToken: "old-access", RefreshToken: "old-refresh"}
	for range 2 {
		if _, err := p.refreshAccountWithCooldown(context.Background(), server.Client(), account, defaultSettings()); err == nil {
			t.Fatal("expected OAuth failure")
		}
	}
	if exchanges.Load() != 1 {
		t.Fatal("Redis outage disabled local failure merging")
	}
}

func TestPGGatewayRefreshCooldownSharesFailureAndReimportClears(t *testing.T) {
	s := accountPGTestStore(t)
	redisDB, _ := gatewayRedisSessionFixture(t)
	s.DB.Redis, s.DB.Namespace = redisDB.Redis, redisDB.Namespace
	account := pgImportForTest(t, s, "same-account", "pg-original-access")
	account.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	if err := s.UpdateTokens(account.ID, account.AccessToken, account); err != nil {
		t.Fatal(err)
	}
	account, err := s.Get(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var exchanges atomic.Int32
	var success atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if !success.Load() {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"never-store-this-body"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"pg-rotated-access","refresh_token":"pg-rotated-refresh","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	p1 := &Proxy{Store: s, Client: server.Client(), TokenURL: server.URL}
	p2 := &Proxy{Store: s, Client: server.Client(), TokenURL: server.URL}
	key := s.DB.Namespace + "gateway:oauth-failure:" + gatewayRefreshFailureKey(account, server.URL)
	t.Cleanup(func() { _ = s.DB.Redis.Del(context.Background(), key).Err() })
	var group sync.WaitGroup
	for i := range 8 {
		group.Go(func() {
			p := p1
			if i%2 == 0 {
				p = p2
			}
			var err error
			if i%3 == 0 {
				_, err = p.recoverUnauthorized(context.Background(), account, p.Client)
			} else {
				_, err = p.ensureReady(context.Background(), account)
			}
			if err == nil {
				t.Error("OAuth failure unexpectedly succeeded")
			}
		})
	}
	group.Wait()
	if exchanges.Load() != 1 {
		t.Fatalf("two proxy instances repeated one credential failure %d times", exchanges.Load())
	}
	raw, err := s.DB.Redis.Get(context.Background(), key).Result()
	if err != nil || strings.Contains(raw, account.AccessToken) || strings.Contains(raw, account.RefreshToken) || strings.Contains(raw, "never-store") {
		t.Fatalf("Redis failure entry unavailable or contains sensitive data: %v", err)
	}
	// An actual replacement acquires a new credential epoch. The import API
	// intentionally treats identical access tokens as duplicate/no-op imports.
	credentials, _ := json.Marshal(map[string]any{"access_token": "pg-reimported-access", "refresh_token": "pg-reimported-refresh", "expires_at": account.ExpiresAt})
	if report, err := s.ImportDetailed([]ImportItem{{Name: account.Name, Credentials: credentials}}, AccountDefaults{}, ImportOptions{Replace: true}); err != nil || report.Replaced != 1 {
		t.Fatalf("explicit credential replacement failed: replaced=%d err=%v", report.Replaced, err)
	}
	imported, err := s.Get(account.ID)
	if err != nil || imported.CredentialVersion == account.CredentialVersion {
		t.Fatalf("reimport did not replace credential epoch: %v", err)
	}
	success.Store(true)
	updated, err := p2.ensureReady(context.Background(), imported)
	if err != nil || exchanges.Load() != 2 || updated.RefreshToken != "pg-rotated-refresh" {
		t.Fatalf("fresh credential epoch remained blocked: exchanges=%d err=%v", exchanges.Load(), err)
	}
	stored, err := s.Get(account.ID)
	if err != nil || stored.RefreshToken != "pg-rotated-refresh" {
		t.Fatal("shared cooldown bypassed successful credential persistence")
	}
}
