package server

// Session D (2026-09-30): a request whose only eligible accounts are cooling
// for a few more seconds waits out that cooldown instead of failing fast;
// payment pauses and long cooldowns keep the immediate 429.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestAwaitGatewayCooldownWaitsShortRemainder(t *testing.T) {
	err := &gatewayCooldownError{until: time.Now().Add(300 * time.Millisecond)}
	start := time.Now()
	if !awaitGatewayCooldown(t.Context(), err) {
		t.Fatal("short cooldown remainder must be awaited")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("await returned before the cooldown expired: %s", elapsed)
	}
}

func TestAwaitGatewayCooldownRejectsLongAndExpired(t *testing.T) {
	long := &gatewayCooldownError{until: time.Now().Add(gatewayCooldownAwaitMax + time.Minute)}
	if awaitGatewayCooldown(t.Context(), long) {
		t.Fatal("long cooldown must fail fast with Retry-After")
	}
	expired := &gatewayCooldownError{until: time.Now().Add(-time.Second)}
	if awaitGatewayCooldown(t.Context(), expired) {
		t.Fatal("expired cooldown must not wait")
	}
}

func TestAwaitGatewayCooldownNeverWaitsPayment(t *testing.T) {
	payment := &gatewayCooldownError{until: time.Now().Add(time.Second), payment: true}
	if awaitGatewayCooldown(t.Context(), payment) {
		t.Fatal("billing pauses must keep failing fast")
	}
}

func TestAwaitGatewayCooldownHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	err := &gatewayCooldownError{until: time.Now().Add(2 * time.Second)}
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if awaitGatewayCooldown(ctx, err) {
		t.Fatal("cancelled context must not report an awaited cooldown")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancel did not interrupt the wait: %s", elapsed)
	}
}

func TestAwaitGatewayCooldownIgnoresOtherErrors(t *testing.T) {
	if awaitGatewayCooldown(t.Context(), errors.New("unrelated")) {
		t.Fatal("non-cooldown errors must not be awaited")
	}
	if awaitGatewayCooldown(t.Context(), nil) {
		t.Fatal("nil error must not be awaited")
	}
}

// End to end: the only eligible account is rate-limited for a fraction of a
// second; selection waits out the cooldown and admits it instead of 429ing.
func TestPickSessionAccountAwaitConvertsShortCooldown(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.RateLimitCooldownEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.gatewayCooldown(t.Context(), account, gatewayCooldownScope(t.Context()), 700*time.Millisecond); !errors.As(err, new(*gatewayCooldownError)) {
		t.Fatalf("record cooldown: %v", err)
	}
	start := time.Now()
	selected, release, err := p.pickSessionAccountAwait(t.Context(), p.AdminKey, "admin", "")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("short cooldown must be awaited, got %v", err)
	}
	if release != nil {
		release()
	}
	if selected.ID != account.ID {
		t.Fatalf("selected %s, want %s", selected.ID, account.ID)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("selection did not wait out the cooldown: %s", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("wait exceeded its bound: %s", elapsed)
	}
}

// A long rate-limit cooldown keeps the immediate failure; nothing waits.
func TestPickSessionAccountAwaitFailsFastOnLongCooldown(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.RateLimitCooldownEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.gatewayCooldown(t.Context(), account, gatewayCooldownScope(t.Context()), gatewayCooldownAwaitMax+time.Minute); !errors.As(err, new(*gatewayCooldownError)) {
		t.Fatalf("record cooldown: %v", err)
	}
	start := time.Now()
	_, _, err = p.pickSessionAccountAwait(t.Context(), p.AdminKey, "admin", "")
	if !errors.As(err, new(*gatewayCooldownError)) {
		t.Fatalf("long cooldown must surface its fast 429, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("long cooldown must not wait")
	}
}
