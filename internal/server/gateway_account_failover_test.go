package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestGatewayRequestFailoverPreservesPriorityWeightsAndExhaustion(t *testing.T) {
	p := newTestProxy(t, 1)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name             string
		priority, weight int
	}{{"same-light", 10, 1}, {"same-heavy", 10, 9}, {"lower", 0, 1}} {
		raw, _ := json.Marshal(map[string]any{"access_token": "synthetic-" + item.name, "expires_at": time.Now().Add(time.Hour).Unix()})
		if err := p.Store.ImportWith(item.name, raw, AccountDefaults{Concurrency: 1, Priority: item.priority, Weight: item.weight}); err != nil {
			t.Fatal(err)
		}
	}
	p.invalidateAccounts()
	ctx := withGatewayAccountFailover(t.Context())
	var order []string
	for i := 0; i < 4; i++ {
		account, release, err := p.pickFailoverAccount(ctx, "test-client-key", "admin")
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, account.Name)
		release()
		gatewayExcludeFailedAccount(ctx, account)
	}
	if order[0] != "test" || order[3] != "lower" || order[1] == order[2] {
		t.Fatalf("priority order changed: %v", order)
	}
	if _, release, err := p.pickFailoverAccount(ctx, "test-client-key", "admin"); !errors.Is(err, errGatewayAccountCandidatesExhausted) || release != nil {
		t.Fatal("exhausted accounts were retried")
	}
	// Compare each selected account against the unchanged weighted rendezvous
	// primitive for many deterministic seeds, without flaky random sampling.
	candidates, _, _, _ := p.candidatesForContext(t.Context(), "test-client-key")
	level := []Account{}
	for _, a := range candidates {
		if a.Priority == 10 {
			level = append(level, a)
		}
	}
	hits := map[string]int{}
	for i := 0; i < 128; i++ {
		var seed [32]byte
		seed[0] = byte(i)
		ctx := context.WithValue(withGatewayAccountFailover(t.Context()), gatewayCacheAffinityContextKey{}, gatewayCacheAffinity{seed: seed})
		for _, a := range candidates {
			if a.Priority == 100 {
				gatewayExcludeFailedAccount(ctx, a)
			}
		}
		want := gatewayCacheAffinityPick(ctx, level)
		got, release, err := p.pickFailoverAccount(ctx, "test-client-key", "admin")
		if err != nil {
			t.Fatal(err)
		}
		release()
		if got.ID != want.ID {
			t.Fatal("failover changed same-priority weights")
		}
		hits[got.Name]++
	}
	if hits["same-heavy"] <= hits["same-light"] {
		t.Fatalf("weights were ignored: %v", hits)
	}
}

func TestGatewayRequestFailoverSkipsFailedRefreshAndBusyAccount(t *testing.T) {
	refreshes := 0
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		refreshes++
		http.Error(w, `{"error":"invalid_grant"}`, 400)
	})
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; a.ExpiresAt = time.Now().Add(-time.Hour).Unix(); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"busy", "healthy"} {
		raw, _ := json.Marshal(map[string]any{"access_token": "synthetic-" + name, "expires_at": time.Now().Add(time.Hour).Unix()})
		if err := p.Store.ImportWith(name, raw, AccountDefaults{Concurrency: 1, Priority: 10, Weight: 1}); err != nil {
			t.Fatal(err)
		}
	}
	busy, _ := p.Store.Get("busy")
	releaseBusy, ok := p.acquireGatewayAccount(busy)
	if !ok {
		t.Fatal("fixture admission")
	}
	defer releaseBusy()
	p.invalidateAccounts()
	ctx := withGatewayAccountFailover(t.Context())
	got, release, err := p.pickFailoverAccount(ctx, "test-client-key", "admin")
	if err != nil || got.Name != "healthy" {
		t.Fatalf("failed to skip unavailable accounts: %s %v", got.Name, err)
	}
	release()
	if refreshes != 1 {
		t.Fatalf("refresh attempts=%d", refreshes)
	}
	remaining, remainingRelease, selectionErr := p.pickFailoverAccount(ctx, "test-client-key", "admin")
	var busyError *gatewayAccountBusyError
	// Equal-priority weighting can select the healthy account before probing
	// the busy one. Its first admission check must remain a busy error, not be
	// disguised as exhaustion merely because this request already failed auth.
	if remaining.ID != "" || remainingRelease != nil || (!errors.Is(selectionErr, errGatewayAccountCandidatesExhausted) && !errors.As(selectionErr, &busyError)) {
		t.Fatalf("unavailable accounts retried: account=%q err=%v", remaining.Name, selectionErr)
	}
	if _, _, err := p.pickFailoverAccount(ctx, "test-client-key", "admin"); !errors.Is(err, errGatewayAccountCandidatesExhausted) {
		t.Fatalf("fully visited account pool was not exhausted: %v", err)
	}
	if refreshes != 1 {
		t.Fatal("failed refresh repeated within request")
	}
}

func TestGatewayFailoverExhaustionDoesNotMaskSelectionErrors(t *testing.T) {
	p := newTestProxy(t, 1)
	if _, _, err := p.pickFailoverAccount(withGatewayAccountFailover(t.Context()), "invalid-key", "admin"); err == nil || errors.Is(err, errGatewayAccountCandidatesExhausted) {
		t.Fatalf("invalid key masked as exhaustion: %v", err)
	}
	account, _ := p.Store.Get("test")
	release, ok := p.acquireGatewayAccount(account)
	if !ok {
		t.Fatal("fixture account unavailable")
	}
	if _, _, err := p.pickFailoverAccount(withGatewayAccountFailover(t.Context()), "test-client-key", "admin"); err == nil || errors.Is(err, errGatewayAccountCandidatesExhausted) {
		t.Fatalf("concurrency masked as exhaustion: %v", err)
	}
	release()
	broken := newTestProxy(t, 1)
	if err := os.WriteFile(broken.Store.Path, []byte(`invalid stored JSON`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := broken.pickFailoverAccount(withGatewayAccountFailover(t.Context()), "test-client-key", "admin"); err == nil || errors.Is(err, errGatewayAccountCandidatesExhausted) {
		t.Fatalf("storage failure masked as exhaustion: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := p.pickFailoverAccount(withGatewayAccountFailover(ctx), "test-client-key", "admin"); !errors.Is(err, context.Canceled) || errors.Is(err, errGatewayAccountCandidatesExhausted) {
		t.Fatalf("cancellation masked as exhaustion: %v", err)
	}
}

func TestGatewaySessionFailoverCompareAndSwap(t *testing.T) {
	p := newTestProxy(t, 1)
	ctx := t.Context()
	if bound, err := p.sessionAccount(ctx, "key", "session", "failed"); err != nil || bound != "failed" {
		t.Fatal("initial binding")
	}
	actual, switched, err := p.switchSessionAccount(ctx, "key", "session", "failed", "healthy")
	if err != nil || !switched || actual != "healthy" {
		t.Fatalf("CAS failed: %q %v %v", actual, switched, err)
	}
	actual, switched, err = p.switchSessionAccount(ctx, "key", "session", "failed", "other")
	if err != nil || switched || actual != "healthy" {
		t.Fatal("stale failover overwrote concurrent binding")
	}
	if bound, err := p.sessionAccount(ctx, "key", "session", ""); err != nil || bound != "healthy" {
		t.Fatal("binding not retained")
	}
	if bound, err := p.sessionAccount(ctx, "different-key", "session", ""); err != nil || bound != "" {
		t.Fatal("binding escaped key isolation")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := p.switchSessionAccount(cancelled, "key", "session", "healthy", "bad"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled CAS accepted")
	}
}

func TestGatewayBoundUnavailableIsTypedOnlyForFailover(t *testing.T) {
	p := newTestProxy(t, 1)
	account, _ := p.Store.Get("test")
	owner := gatewayAccountRef(account)
	if _, err := p.sessionAccount(t.Context(), "admin", "bound", owner); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	_, _, ordinaryErr := p.pickSessionAccount(t.Context(), "test-client-key", "admin", "bound")
	var unavailable *gatewayBoundAccountUnavailableError
	if ordinaryErr == nil || errors.As(ordinaryErr, &unavailable) {
		t.Fatal("ordinary sticky behavior changed")
	}
	_, _, failoverErr := p.pickSessionAccount(withGatewayAccountFailover(t.Context()), "test-client-key", "admin", "bound")
	if !errors.As(failoverErr, &unavailable) || unavailable.Owner != owner {
		t.Fatalf("unavailable owner was not preserved: %v", failoverErr)
	}
}

func TestOfficialRedisSessionFailoverCompareAndSwap(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	ctx := t.Context()
	// Track the exact hashed session key for fixture cleanup.
	const keyID, session = "synthetic-key", "synthetic-failover-session"
	key := f.namespace + "gateway:thread:" + bpsHistoryKey(keyID, session)
	f.track(key)
	if bound, err := p.sessionAccount(ctx, keyID, session, "failed"); err != nil || bound != "failed" {
		t.Fatal("Redis initial binding", err)
	}
	actual, switched, err := p.switchSessionAccount(ctx, keyID, session, "failed", "healthy")
	if err != nil || !switched || actual != "healthy" {
		t.Fatal("Redis CAS failed", err)
	}
	actual, switched, err = p.switchSessionAccount(ctx, keyID, session, "failed", "another")
	if err != nil || switched || actual != "healthy" {
		t.Fatal("Redis stale CAS overwrote selected account")
	}
	if ttl, err := p.Store.DB.Redis.TTL(ctx, key).Result(); err != nil || ttl <= 0 {
		t.Fatal(fmt.Sprintf("Redis binding TTL: %v %v", ttl, err))
	}
}
