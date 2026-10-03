package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Existing cooldown-focused tests opt into the old policy explicitly. General
// proxy fixtures retain the new production default of no rate-limit cooldown.
func enableRateLimitCooldownForTest(t *testing.T, p *Proxy) {
	t.Helper()
	if p.Settings == nil {
		p.Settings = NewSettingsStore(filepath.Join(t.TempDir(), "cooldown-settings.json"))
	}
	settings, err := p.Settings.Latest()
	if err != nil {
		t.Fatal(err)
	}
	settings.RateLimitCooldownEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
}
func cooldownProxyFixture(t *testing.T, handler http.HandlerFunc) *Proxy {
	t.Helper()
	p := proxyFixture(t, handler)
	enableRateLimitCooldownForTest(t, p)
	return p
}
func cooldownNewTestProxy(t *testing.T, concurrency int) *Proxy {
	t.Helper()
	p := newTestProxy(t, concurrency)
	enableRateLimitCooldownForTest(t, p)
	return p
}

func TestRateLimitCooldownDefaultsAndAdminPersistence(t *testing.T) {
	for _, document := range []string{`{"version":2,"admin_api":true}`, `{"version":2,"rate_limit_cooldown_enabled":false}`} {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(document), 0600); err != nil {
			t.Fatal(err)
		}
		settings, err := NewSettingsStore(path).Get()
		if err != nil || settings.RateLimitCooldownEnabled {
			t.Fatal("legacy unset/false enabled cooldown")
		}
	}
	p, handler := webTestSetup(t)
	verifyRateLimitCooldownAdminPersistence(t, p, handler)
}
func TestPGRateLimitCooldownAdminPersistence(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	verifyRateLimitCooldownAdminPersistence(t, p, handler)
}
func verifyRateLimitCooldownAdminPersistence(t *testing.T, p *Proxy, handler http.Handler) {
	t.Helper()
	initial, err := p.Settings.Get()
	if err != nil || initial.RateLimitCooldownEnabled {
		t.Fatal("fresh default enabled rate-limit cooldown")
	}
	post := func(body string, want int) {
		t.Helper()
		w := webTestRequest(handler, http.MethodPost, "/api/settings", body, "Bearer "+webTestKey)
		if w.Code != want {
			t.Fatalf("settings update status=%d want=%d", w.Code, want)
		}
	}
	body := fmt.Sprintf(`{"settings_version":%d,"rate_limit_cooldown_enabled":true}`, initial.Version)
	post(body, 200)
	post(body, 409)
	for _, bad := range []string{`{"rate_limit_cooldown_enabled":"true"}`, `{"rate_limit_cooldown_enabled":true,"regenerate":true}`} {
		post(bad, 400)
	}
	post(`{"websocket_enabled":true}`, 200)
	var reloaded Settings
	if p.Store.DB != nil {
		reloaded, err = NewPGSettingsStore(p.Store.DB).Get()
	} else {
		reloaded, err = NewSettingsStore(p.Settings.store.path).Get()
	}
	if err != nil || !reloaded.RateLimitCooldownEnabled {
		t.Fatal("scope-only/legacy partial update lost cooldown policy")
	}
	w := webTestRequest(handler, http.MethodGet, "/api/settings", "", "Bearer "+webTestKey)
	var got Settings
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got != reloaded {
		t.Fatal("GET omitted cooldown policy")
	}
	post(`{"rate_limit_cooldown_enabled":false}`, 200)
	got, err = p.Settings.Latest()
	if err != nil || got.RateLimitCooldownEnabled {
		t.Fatal("cooldown policy could not disable")
	}
}

func TestRateLimitCooldownDisabledBypassesExistingAndPreservesTTL(t *testing.T) {
	p := newTestProxy(t, 1)
	a, _ := p.Store.Get("test")
	ctx := withGatewayCooldownModel(t.Context(), "model")
	p.recordGatewayCooldown(ctx, a, "30")
	if len(p.cooldowns) != 0 || p.checkGatewayCooldown(ctx, a) != nil {
		t.Fatal("default recorded or applied cooldown")
	}
	enableRateLimitCooldownForTest(t, p)
	p.recordGatewayCooldown(ctx, a, "30")
	var limited *gatewayCooldownError
	if !errors.As(p.checkGatewayCooldown(ctx, a), &limited) {
		t.Fatal("enabled policy did not record")
	}
	deadline := limited.until
	settings, _ := p.Settings.Get()
	settings.RateLimitCooldownEnabled = false
	_ = p.Settings.Set(settings)
	if p.checkGatewayCooldown(ctx, a) != nil {
		t.Fatal("disabled policy blocked existing cooldown")
	}
	p.recordGatewayCooldown(ctx, a, "3600")
	if len(p.cooldowns) != 1 {
		t.Fatal("disabling removed existing shared-scope state")
	}
	enableRateLimitCooldownForTest(t, p)
	if !errors.As(p.checkGatewayCooldown(ctx, a), &limited) || !limited.until.Equal(deadline) {
		t.Fatal("re-enable lost/extended existing deadline")
	}
	p.cooldownMu.Lock()
	for key := range p.cooldowns {
		p.cooldowns[key] = time.Now().Add(-time.Second)
	}
	p.cooldownMu.Unlock()
	if p.checkGatewayCooldown(ctx, a) != nil {
		t.Fatal("expired original cooldown still blocked")
	}
	settings, _ = p.Settings.Get()
	settings.RateLimitCooldownEnabled = false
	_ = p.Settings.Set(settings)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if !errors.Is(p.checkGatewayCooldown(canceled, a), context.Canceled) {
		t.Fatal("disabled cooldown swallowed cancellation")
	}
}

func TestRateLimitCooldownDisabledAvoidsRedisAndSettingsFailureIs503(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer rdb.Close()
	rdb.AddHook(cooldownRedisFailureHook{})
	p := newTestProxy(t, 1)
	p.Store.DB = &Database{Redis: rdb, Namespace: "synthetic:"}
	if err := p.checkGatewayCooldown(t.Context(), Account{ID: "fixture"}); err != nil {
		t.Fatal("disabled cooldown read Redis")
	}
	p.Settings = NewSettingsStore(filepath.Join(t.TempDir(), "missing", "settings.json"))
	if err := os.MkdirAll(p.Settings.store.path, 0700); err != nil {
		t.Fatal(err)
	}
	err := p.checkGatewayCooldown(t.Context(), Account{ID: "fixture"})
	status, retry, recognized := gatewaySelectionFailure(err)
	if !recognized || status != 503 || retry != "" {
		t.Fatal("settings outage became cooldown or failed open")
	}
	p.recordGatewayCooldown(t.Context(), Account{ID: "fixture"}, "30")
	if len(p.cooldowns) != 0 {
		t.Fatal("settings read failure wrote local cooldown")
	}
}

func TestRateLimitCooldownDisabledPreservesReal429AndSSEFailures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if !stream {
					w.Header().Set("Retry-After", "30")
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"limited\",\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n")
			})
			for range 2 {
				w := cooldownRequest(p, "model", "same")
				want := 429
				if stream {
					want = 200
				}
				if w.Code != want {
					t.Fatalf("upstream failure changed status=%d", w.Code)
				}
				record, ok := p.Metrics.LastFor("test")
				if !ok || record.Status != "error" {
					t.Fatal("quota failure disappeared from actual records")
				}
			}
			if calls != 2 || len(p.cooldowns) != 0 {
				t.Fatal("request-local quota failure cooled later request")
			}
		})
	}
}

func TestRateLimitCooldownDisabledStillFailsOverCurrentQuotaRequest(t *testing.T) {
	for _, mode := range []string{"http429", "sse"} {
		t.Run(mode, func(t *testing.T) {
			p, calls := teamCRateFixture(t, mode, true)
			settings, err := p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			settings.RateLimitCooldownEnabled = false
			if err := p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				out := teamCRateRequest(t, p, false, fmt.Sprintf("quota-no-cooldown-%d", i))
				if out.Code != http.StatusOK {
					t.Fatalf("current request did not fail over: HTTP=%d", out.Code)
				}
				record := teamCAssertLimitedAttempt(t, p, "second", 2)
				if record.Status != "ok" || len(p.cooldowns) != 0 {
					t.Fatal("disabled cooldown erased failover or recorded future account exclusion")
				}
			}
			if calls.count() != 4 {
				t.Fatal("subsequent independent request skipped priority account despite disabled cooldown")
			}
		})
	}
}

func TestRateLimitCooldownRedisDisableDoesNotDeleteSharedDeadline(t *testing.T) {
	db, _ := gatewayRedisSessionFixture(t)
	p := &Proxy{Store: Store{DB: db}}
	enableRateLimitCooldownForTest(t, p)
	key := db.Namespace + "gateway:cooldowns"
	t.Cleanup(func() { _ = db.Redis.Del(context.Background(), key).Err() })
	a := Account{ID: "scope", AccessToken: "synthetic"}
	ctx := withGatewayCooldownModel(t.Context(), "model")
	p.recordGatewayCooldown(ctx, a, "30")
	before, err := db.Redis.ZRangeWithScores(ctx, key, 0, -1).Result()
	if err != nil || len(before) != 1 {
		t.Fatal("shared cooldown not recorded")
	}
	s, _ := p.Settings.Get()
	s.RateLimitCooldownEnabled = false
	_ = p.Settings.Set(s)
	if p.checkGatewayCooldown(ctx, a) != nil {
		t.Fatal("disabled Redis cooldown still blocked")
	}
	p.recordGatewayCooldown(ctx, a, "3600")
	after, err := db.Redis.ZRangeWithScores(ctx, key, 0, -1).Result()
	if err != nil || len(after) != 1 || after[0] != before[0] {
		t.Fatal("disabled policy mutated shared deadline")
	}
	enableRateLimitCooldownForTest(t, p)
	var limited *gatewayCooldownError
	if !errors.As(p.checkGatewayCooldown(ctx, a), &limited) {
		t.Fatal("re-enable ignored unexpired shared cooldown")
	}
}
