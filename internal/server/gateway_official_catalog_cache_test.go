package server

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOfficialCatalogCacheCoalescesAndCopies(t *testing.T) {
	var cache officialCatalogCache
	defer cache.close()
	var calls atomic.Int32
	started, finish := make(chan struct{}), make(chan struct{})
	fetch := func(context.Context) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-finish
		return []byte("directory"), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _, err := cache.get(t.Context(), [32]byte{1}, fetch)
			if err != nil || string(raw) != "directory" {
				t.Errorf("directory read failed: %q %v", raw, err)
			}
			if len(raw) > 0 {
				raw[0] = 'X'
			}
		}()
	}
	<-started
	close(finish)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("directory was fetched %d times", calls.Load())
	}
	raw, hit, err := cache.get(t.Context(), [32]byte{1}, fetch)
	if err != nil || !hit || string(raw) != "directory" {
		t.Fatalf("returned copy modified shared directory: %q %v %v", raw, hit, err)
	}
}

func TestOfficialCatalogCacheIsolationExpiryAndErrors(t *testing.T) {
	var cache officialCatalogCache
	defer cache.close()
	a := Account{AccessToken: "account-token", AccountID: "account-one"}
	keys := [][32]byte{officialCatalogKey(a, nil)}
	a.AccessToken = "replacement-token"
	keys = append(keys, officialCatalogKey(a, nil))
	a.CredentialVersion++
	keys = append(keys, officialCatalogKey(a, nil))
	a.AccountID = "account-two"
	keys = append(keys, officialCatalogKey(a, nil))
	keys = append(keys, officialCatalogKey(a, &ProxyEntry{Host: "127.0.0.1", Port: 8080}))
	seen := map[[32]byte]bool{}
	for _, key := range keys {
		if seen[key] {
			t.Fatal("different account, credential or proxy shared a model directory")
		}
		seen[key] = true
	}
	count := 0
	fetch := func(context.Context) ([]byte, error) { count++; return []byte{byte(count)}, nil }
	for _, key := range keys {
		if _, hit, err := cache.get(t.Context(), key, fetch); err != nil || hit {
			t.Fatalf("new isolated key read: hit=%v err=%v", hit, err)
		}
	}
	cache.mu.Lock()
	entry := cache.entries[keys[0]]
	entry.expires = time.Now().Add(-time.Second)
	cache.entries[keys[0]] = entry
	cache.mu.Unlock()
	if _, hit, err := cache.get(t.Context(), keys[0], fetch); err != nil || hit || count != len(keys)+1 {
		t.Fatal("expired directory was not fetched again")
	}
	failure := errors.New("directory fetch failed")
	key := [32]byte{88}
	if _, _, err := cache.get(t.Context(), key, func(context.Context) ([]byte, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatal("upstream error was hidden")
	}
	if _, hit, err := cache.get(t.Context(), key, fetch); err != nil || hit {
		t.Fatal("failed directory fetch was cached")
	}
}

func TestOfficialCatalogCacheCancellationAndClose(t *testing.T) {
	var cache officialCatalogCache
	started, leaderDone := make(chan struct{}), make(chan error, 1)
	leaderCtx, cancel := context.WithCancel(t.Context())
	go func() {
		_, _, err := cache.get(leaderCtx, [32]byte{1}, func(ctx context.Context) ([]byte, error) { close(started); <-ctx.Done(); return nil, ctx.Err() })
		leaderDone <- err
	}()
	<-started
	waitCtx, stopWait := context.WithCancel(t.Context())
	waitDone := make(chan error, 1)
	go func() {
		_, _, err := cache.get(waitCtx, [32]byte{1}, func(context.Context) ([]byte, error) { t.Error("waiter repeated active fetch"); return nil, nil })
		waitDone <- err
	}()
	stopWait()
	select {
	case err := <-waitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("waiting request ignored cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter stayed blocked on an unrelated request")
	}
	cancel()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatal("fetch did not return caller cancellation")
	}
	if _, _, err := cache.get(t.Context(), [32]byte{1}, func(context.Context) ([]byte, error) { return []byte("next"), nil }); err != nil {
		t.Fatal("canceled fetch poisoned future requests")
	}
	started = make(chan struct{})
	go func() {
		_, _, err := cache.get(t.Context(), [32]byte{2}, func(ctx context.Context) ([]byte, error) { close(started); <-ctx.Done(); return nil, ctx.Err() })
		leaderDone <- err
	}()
	<-started
	cache.close()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatal("closing runtime failed to cancel pending directory fetch")
	}
	if len(cache.entries) != 0 || cache.bytes != 0 {
		t.Fatal("closing runtime retained model directories")
	}
	if _, _, err := cache.get(t.Context(), [32]byte{1}, nil); err == nil {
		t.Fatal("closed cache accepted requests")
	}
}

func TestOfficialCatalogCacheStaleMissingModelRefreshes(t *testing.T) {
	p, account := officialManagedFixture(t, false)
	key := officialCatalogKey(account, nil)
	stale := []byte(`{"models":[{"slug":"previous-model"}]}`)
	if _, _, err := p.Official.catalog.get(t.Context(), key, func(context.Context) ([]byte, error) { return stale, nil }); err != nil {
		t.Fatal(err)
	}
	cache, catalog, err := p.Official.clientToolModelCatalog(t.Context(), account, nil, "model-a")
	if err != nil || !bytes.Contains(catalog, []byte(`"model-a"`)) || bytes.Equal(cache, stale) {
		t.Fatalf("stale account directory rejected newly available model: %v", err)
	}
	if _, _, err := p.Official.clientToolModelCatalog(t.Context(), account, nil, "model-does-not-exist"); err == nil {
		t.Fatal("unavailable model was invented after refresh")
	}
}

func TestOfficialCatalogCacheBoundedCountAndBytes(t *testing.T) {
	var cache officialCatalogCache
	defer cache.close()
	for i := 0; i < officialCatalogMaxEntries+3; i++ {
		if _, _, err := cache.get(t.Context(), [32]byte{byte(i)}, func(context.Context) ([]byte, error) { return []byte("value"), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) > officialCatalogMaxEntries || cache.bytes != len(cache.entries)*len("value") {
		t.Fatal("directory count or byte bookkeeping exceeded bounds")
	}
	for _, key := range [][32]byte{{101}, {102}} {
		if _, _, err := cache.get(t.Context(), key, func(context.Context) ([]byte, error) {
			return bytes.Repeat([]byte{1}, officialCatalogMaxBytes/2+1), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if cache.bytes > officialCatalogMaxBytes || len(cache.entries) != 1 {
		t.Fatal("large directories exceeded memory budget")
	}
}

func TestOfficialCatalogCacheRemovesSecondBootstrapOnNewSessions(t *testing.T) {
	p, account := officialManagedFixture(t, false)
	command := p.Official.command
	var processes atomic.Int32
	p.Official.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		processes.Add(1)
		return command(ctx, name, args...)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	first, err := p.Official.startClientToolRPC(ctx, account, nil, "model-a")
	if err != nil {
		t.Fatal(err)
	}
	first.close()
	if processes.Load() != 2 {
		t.Fatalf("first session needs bootstrap + runtime; spawned %d", processes.Load())
	}
	second, err := p.Official.startClientToolRPC(ctx, account, nil, "model-b")
	if err != nil {
		t.Fatal(err)
	}
	second.close()
	if processes.Load() != 3 {
		t.Fatalf("second session repeated bootstrap; total spawned %d", processes.Load())
	}
	account.AccessToken = "SENSITIVE_FIXTURE_TOKEN_REFRESHED"
	account.CredentialVersion++
	third, err := p.Official.startClientToolRPC(ctx, account, nil, "model-a")
	if err != nil {
		t.Fatal(err)
	}
	third.close()
	if processes.Load() != 5 {
		t.Fatal("credential replacement reused prior credential directory")
	}
}
