package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGatewayContentAffinityIgnoresObjectEncodingOnly(t *testing.T) {
	settings := defaultSettings()
	settings.BuiltinPromptEnabled = false
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	requests := []string{
		`{"model":"test","instructions":"中文","tools":[{"type":"function","name":"z","parameters":{"type":"object","properties":{"a":{"type":"number","maximum":9007199254740993},"b":{"type":"string"}}}},{"type":"function","name":"a"}],"reasoning":{"effort":"high","summary":"auto"},"input":"first user turn"}`,
		`{"model":"test","instructions":"\u4e2d\u6587","tools":[{"parameters":{"properties":{"b":{"type":"string"},"a":{"maximum":9007199254740993,"type":"number"}},"type":"object"},"name":"z","type":"function"},{"name":"a","type":"function"}],"reasoning":{"summary":"auto","effort":"high"},"input":"first user turn"}`,
	}
	var firstSession string
	for _, request := range requests {
		prepared, session, err := applyGatewayPolicy(r, []byte(request), "same-api-key", settings)
		if err != nil || session == "" {
			t.Fatalf("policy: session=%q err=%v", session, err)
		}
		if firstSession == "" {
			firstSession = session
		} else if session != firstSession {
			t.Fatal("equivalent object encoding split the local content route")
		}
		var before, after map[string]json.RawMessage
		_ = json.Unmarshal([]byte(request), &before)
		_ = json.Unmarshal(prepared, &after)
		if _, exists := after["prompt_cache_key"]; exists {
			t.Fatal("content affinity generated an upstream cache key")
		}
		for _, field := range []string{"input", "tools", "reasoning"} {
			if string(before[field]) != string(after[field]) {
				t.Fatalf("cache hashing rewrote native %s: %s", field, after[field])
			}
		}
	}
}

func TestGatewayContentAffinityPreservesMeaningAndTenantBoundaries(t *testing.T) {
	settings := defaultSettings()
	settings.BuiltinPromptEnabled = false
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	base := `{"model":"test","instructions":"fixed","tools":[{"name":"z","limit":9007199254740993},{"name":"a"}],"reasoning":{"effort":"high"},"input":"first user"}`
	_, baselineSession, err := applyGatewayPolicy(r, []byte(base), "key-a", settings)
	if err != nil || baselineSession == "" {
		t.Fatalf("baseline content route: %q %v", baselineSession, err)
	}
	cases := []struct {
		name, request, keyID string
		wantSame             bool
	}{
		{"tool order", strings.Replace(base, `[{"name":"z","limit":9007199254740993},{"name":"a"}]`, `[{"name":"a"},{"name":"z","limit":9007199254740993}]`, 1), "key-a", false},
		{"large integer", strings.Replace(base, "9007199254740993", "9007199254740992", 1), "key-a", false},
		{"explicit null outside route seed", strings.TrimSuffix(base, "}") + `,"text":null}`, "key-a", true},
		{"instructions", strings.Replace(base, "fixed", "changed", 1), "key-a", false},
		{"first user", strings.Replace(base, "first user", "other first user", 1), "key-a", false},
		{"tenant isolated at binding", base, "key-b", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, session, err := applyGatewayPolicy(r, []byte(test.request), test.keyID, settings)
			if err != nil {
				t.Fatal(err)
			}
			if session == "" || (session == baselineSession) != test.wantSame {
				t.Fatal("content route lost meaningful prefix precision or hashed an unrelated field")
			}
			if string(got) != test.request {
				t.Fatal("local routing changed native bytes or generated an upstream key")
			}
		})
	}
	// The seed is content-only, but the stored route is always keyed by tenant.
	p := newTestProxy(t, 1)
	for _, keyID := range []string{"key-a", "key-b"} {
		want := "owner-" + keyID
		if got, err := p.sessionAccount(context.Background(), keyID, baselineSession, want); err != nil || got != want {
			t.Fatalf("inferred route crossed tenants: %q %v", got, err)
		}
	}
	if len(p.sessionRoutes) != 2 {
		t.Fatal("content route did not retain independent tenant bindings")
	}
}

func TestGatewayNativeCacheKeyAndControlsRemainOpaqueAcrossSubagents(t *testing.T) {
	settings := defaultSettings()
	settings.BuiltinPromptEnabled = false
	const raw = `{"model":"test","prompt_cache_key":"internal-override:parent-thread","prompt_cache_options":{"mode":"implicit","breakpoints":[2,0],"unknown_state":"opaque"},"input":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"function_call_output","call_id":"call-1","output":"result"}],"tools":[{"name":"z"},{"name":"a"}]}`
	for _, thread := range []string{"root", "child-a", "child-b"} {
		r := httptest.NewRequest(http.MethodPost, "/responses", nil)
		r.Header.Set("User-Agent", "codex_cli_rs/0.155.0")
		r.Header.Set("Session-Id", "root-session")
		r.Header.Set("Thread-Id", thread)
		for _, enabled := range []bool{true, false} {
			settings.PromptCacheKeyEnabled = enabled
			prepared, session, err := applyGatewayPolicy(r, []byte(raw), "key-a", settings)
			if err != nil || session != "root-session" || string(prepared) != raw {
				t.Fatalf("native cache controls/history changed for thread=%s enabled=%v: session=%q err=%v body=%s", thread, enabled, session, err, prepared)
			}
		}
	}
}

func TestStickySessionSettingsAndExpiry(t *testing.T) {
	p := newTestProxy(t, 1)
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.StickySessionTTLSeconds = 60
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, release, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	p.sessionMu.Lock()
	for key, route := range p.sessionRoutes {
		if remaining := time.Until(route.Expires); remaining < 55*time.Second || remaining > time.Minute {
			t.Errorf("configured session TTL was not applied: %s", remaining)
		}
		route.Expires = time.Now().Add(time.Second)
		p.sessionRoutes[key] = route
	}
	p.sessionMu.Unlock()
	if _, err := p.sessionAccount(ctx, "admin", "session", ""); err != nil {
		t.Fatal(err)
	}
	p.sessionMu.Lock()
	for _, route := range p.sessionRoutes {
		if time.Until(route.Expires) > time.Second {
			t.Error("read-only session lookup renewed an unadmitted binding")
		}
	}
	p.sessionMu.Unlock()
	admitted, admittedRelease, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "session")
	if err != nil {
		t.Fatal(err)
	}
	admittedRelease()
	if gatewayAccountRef(admitted) != gatewayAccountRef(first) {
		t.Fatal("admitted renewal changed the session owner")
	}
	p.sessionMu.Lock()
	for _, route := range p.sessionRoutes {
		if time.Until(route.Expires) < 55*time.Second {
			t.Error("authorized admission did not renew its configured TTL")
		}
	}
	p.sessionMu.Unlock()
	if err := p.Store.ImportWith("preferred", []byte(`{"access_token":"other-token"}`), AccountDefaults{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("preferred", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	p.invalidateAccounts()
	settings, _ = p.Settings.Get()
	settings.StickySessionsEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	unpinned, release, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if unpinned.Name != "preferred" {
		t.Fatal("disabled stickiness ignored the account selection policy")
	}
	settings, _ = p.Settings.Get()
	settings.StickySessionsEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	pinned, release, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if pinned.Name != first.Name {
		t.Fatal("temporarily disabling stickiness destroyed the existing binding")
	}
	p.sessionMu.Lock()
	for key, route := range p.sessionRoutes {
		route.Expires = time.Now().Add(-time.Second)
		p.sessionRoutes[key] = route
	}
	p.sessionMu.Unlock()
	expired, release, err := p.pickSessionAccount(ctx, "test-client-key", "admin", "session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if expired.Name != "preferred" {
		t.Fatal("expired session binding prevented a new account selection")
	}
}

func TestStickySessionRechecksKeyAuthorization(t *testing.T) {
	p := newTestProxy(t, 1)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Groups = []string{"original"}; return nil }); err != nil {
		t.Fatal(err)
	}
	key, err := p.Keys.Create("restricted", []string{"original"})
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := p.pickSessionAccount(context.Background(), key.Key, key.ID, "session")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := p.Keys.Update(key.ID, func(k *APIKey) error { k.Groups = []string{"different"}; return nil }); err != nil {
		t.Fatal(err)
	}
	_, release, err = p.pickSessionAccount(context.Background(), key.Key, key.ID, "session")
	if release != nil {
		release()
	}
	if err == nil || !strings.Contains(err.Error(), "no longer enabled or allowed") {
		t.Fatalf("session binding bypassed current key authorization: %v", err)
	}
}

func TestStickySessionRedisFailureDoesNotCreateLocalBinding(t *testing.T) {
	p := newTestProxy(t, 1)
	p.Store.DB = &Database{}
	_, release, err := p.pickSessionAccount(context.Background(), "test-client-key", "admin", "session")
	if release != nil {
		release()
	}
	if err == nil || !strings.Contains(err.Error(), "session routing unavailable") {
		t.Fatalf("missing Redis failed open: %v", err)
	}
	if len(p.sessionRoutes) != 0 || p.inFlight("test") != 0 {
		t.Fatal("failed shared routing created a local binding or leaked admission")
	}
}

func TestStickySessionCredentialFailureDoesNotClaimAccount(t *testing.T) {
	p := newTestProxy(t, 1)
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		a.RefreshToken = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, release, err := p.pickSessionAccount(context.Background(), "test-client-key", "admin", "session")
	if release != nil {
		release()
	}
	if err == nil || len(p.sessionRoutes) != 0 || p.inFlight("test") != 0 {
		t.Fatalf("failed credentials created a binding or leaked admission: err=%v routes=%d", err, len(p.sessionRoutes))
	}
}

func TestGatewayRedisConfigurableSessionRenewal(t *testing.T) {
	db, key := gatewayRedisSessionFixture(t)
	p1, p2 := newTestProxy(t, 1), newTestProxy(t, 1)
	p1.Store.DB, p2.Store.DB = db, db
	for p, ttl := range map[*Proxy]int{p1: 120, p2: 300} {
		settings, err := p.Settings.Get()
		if err != nil {
			t.Fatal(err)
		}
		settings.StickySessionTTLSeconds = ttl
		if err := p.Settings.Set(settings); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assertTTL := func(expected time.Duration) {
		t.Helper()
		ttl, err := db.Redis.TTL(ctx, key).Result()
		if err != nil || ttl > expected || ttl < expected-5*time.Second {
			t.Fatalf("session TTL=%s want=%s: %v", ttl, expected, err)
		}
	}
	if got, err := p1.sessionAccount(ctx, "key", "session", "id:original"); err != nil || got != "id:original" {
		t.Fatalf("initial claim: %q %v", got, err)
	}
	assertTTL(120 * time.Second)
	// Simulate an almost-expired entry without delaying the test or requiring
	// Redis key enumeration / expiry administration permissions.
	if err := db.Redis.Set(ctx, key, "id:original", time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := p2.sessionAccount(ctx, "key", "session", ""); err != nil || got != "id:original" {
		t.Fatalf("cross-instance read renewal: %q %v", got, err)
	}
	assertTTL(300 * time.Second)
	if got, err := p1.sessionAccount(ctx, "key", "session", "id:other"); err != nil || got != "id:original" {
		t.Fatalf("claim switched account: %q %v", got, err)
	}
	assertTTL(120 * time.Second)
}

func TestSessionProxyStableAcrossOrderAndProcesses(t *testing.T) {
	p := newTestProxy(t, 1)
	account := Account{ID: "account", Name: "test"}
	for i := range 3 {
		entry, err := p.Proxies.Create(ProxyEntry{Name: fmt.Sprintf("proxy-%d", i), Type: "http", Host: "localhost", Port: 8000 + i})
		if err != nil {
			t.Fatal(err)
		}
		account.Proxies = append(account.Proxies, entry.ID)
	}
	secondProcess := &Proxy{Settings: p.Settings, Proxies: p.Proxies}
	owners := map[string]string{}
	for i := range 100 {
		session := fmt.Sprintf("session-%d", i)
		owners[session] = p.pickSessionProxy(account, "key", session).ID
	}
	account.Proxies[0], account.Proxies[2] = account.Proxies[2], account.Proxies[0]
	keyIsolated := false
	for session, expected := range owners {
		if got := secondProcess.pickSessionProxy(account, "key", session); got == nil || got.ID != expected {
			t.Fatalf("proxy changed after ordering/process change for %s: %v", session, got)
		}
		if p.pickSessionProxy(account, "different-key", session).ID != expected {
			keyIsolated = true
		}
	}
	if !keyIsolated {
		t.Fatal("proxy hashing ignores gateway key isolation")
	}
	disabledID := account.Proxies[0]
	if _, err := p.Proxies.Update(disabledID, func(e *ProxyEntry) error { e.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	for session, expected := range owners {
		got := p.pickSessionProxy(account, "key", session)
		if got == nil || got.ID == disabledID {
			t.Fatalf("disabled proxy still selected: %v", got)
		}
		if expected != disabledID && got.ID != expected {
			t.Fatal("disabling another session's proxy disrupted a healthy route")
		}
	}
	settings, _ := p.Settings.Get()
	settings.StickySessionsEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	first := p.pickSessionProxy(account, "key", "session")
	second := p.pickSessionProxy(account, "key", "session")
	if first.ID == second.ID {
		t.Fatal("disabled stickiness did not restore proxy rotation")
	}
	settings, _ = p.Settings.Get()
	settings.StickySessionsEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	first = p.pickSessionProxy(account, "key", "")
	second = p.pickSessionProxy(account, "key", "")
	if first.ID == second.ID {
		t.Fatal("requests without a session stopped rotating proxies")
	}
}
