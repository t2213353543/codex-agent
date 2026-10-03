package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayRoutingAuditWeightedOverflowAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		weights []int
	}{
		{"overflow_negative", []int{math.MaxInt, math.MaxInt}},
		{"overflow_positive", []int{math.MaxInt, math.MaxInt, math.MaxInt}},
		{"defaults", []int{0, -5, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := make([]Account, len(tc.weights))
			for i, weight := range tc.weights {
				accounts[i] = Account{Name: strconv.Itoa(i), Weight: weight}
			}
			counts := make(map[string]int)
			for range 6000 {
				counts[weightedPick(accounts).Name]++
			}
			for _, account := range accounts {
				// Deliberately broad distribution bound: detects lost accounts
				// from overflow without relying on an exact random sequence.
				if counts[account.Name] < 1000 || counts[account.Name] > 4000 {
					t.Fatalf("equal effective weights are not selectable: %v", counts)
				}
			}
		})
	}
	if got := weightedPick(nil); got.Name != "" {
		t.Fatal("empty candidate set returned an account")
	}
}

func TestGatewayRoutingAuditBusyDoesNotInheritOtherAccountCooldown(t *testing.T) {
	p := cooldownNewTestProxy(t, 1)
	if err := p.Store.ImportWith("limited", []byte(`{"access_token":"limited-token"}`), AccountDefaults{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	busy, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	limited, err := p.Store.Get("limited")
	if err != nil {
		t.Fatal(err)
	}
	ctx := withGatewayCooldownModel(t.Context(), "model")
	p.recordGatewayCooldown(ctx, limited, "3600")
	release, admitted := p.acquireGatewayAccount(busy)
	if !admitted {
		t.Fatal("cannot reserve busy fixture")
	}
	defer release()
	_, unexpectedRelease, _, _, err := p.pickAccount(ctx, "test-client-key")
	if unexpectedRelease != nil {
		unexpectedRelease()
		t.Fatal("busy account was admitted")
	}
	var busyErr *gatewayAccountBusyError
	if !errors.As(err, &busyErr) {
		t.Fatalf("busy account inherited long cooldown: %v", err)
	}
	status, retry, recognized := gatewaySelectionFailure(err)
	if status != http.StatusTooManyRequests || retry != "1" || !recognized {
		t.Fatalf("incorrect busy retry: %d %q", status, retry)
	}
	if err := p.checkGatewayCooldown(ctx, busy); err != nil {
		t.Fatalf("busy installed cooldown: %v", err)
	}
	release()
	selected, done, _, _, err := p.pickAccount(ctx, "test-client-key")
	if done != nil {
		defer done()
	}
	if err != nil || selected.ID != busy.ID {
		t.Fatalf("released account remained blocked: %v", err)
	}
}

func TestGatewayRoutingAuditInvalidAdmissionDoesNotAllocate(t *testing.T) {
	p := &Proxy{}
	enableRateLimitCooldownForTest(t, p)
	for _, limit := range []int{-1, 0} {
		if p.acquire("unavailable", limit) {
			t.Fatal("invalid limit admitted work")
		}
	}
	if len(p.admission) != 0 {
		t.Fatal("rejected admission retained empty counters")
	}
}

func TestGatewayRoutingAuditPriorityFallsBackForBusyAndCooling(t *testing.T) {
	p := cooldownNewTestProxy(t, 1)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Priority = 100; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.ImportWith("fallback", []byte(`{"access_token":"fallback"}`), AccountDefaults{Concurrency: 1, Weight: math.MaxInt}); err != nil {
		t.Fatal(err)
	}
	ctx := withGatewayCooldownModel(t.Context(), "model")
	preferred, release, _, _, err := p.pickAccount(ctx, "test-client-key")
	if err != nil || preferred.Name != "test" || release == nil {
		t.Fatalf("weight overrode priority: %v", err)
	}
	defer release()
	checkFallback := func() {
		t.Helper()
		account, done, _, _, err := p.pickAccount(ctx, "test-client-key")
		if done != nil {
			defer done()
		}
		if err != nil || account.Name != "fallback" {
			t.Fatalf("unavailable preferred account blocked fallback: %v", err)
		}
	}
	checkFallback()
	release()
	p.recordGatewayCooldown(ctx, preferred, "30")
	checkFallback()
}

func TestGatewayRoutingAuditCanceledRefreshReleasesAdmission(t *testing.T) {
	p := cooldownNewTestProxy(t, 1)
	if err := p.Store.UpdateAccount("test", func(a *Account) error { a.ExpiresAt = time.Now().Add(-time.Minute).Unix(); return nil }); err != nil {
		t.Fatal(err)
	}
	before, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var refreshes atomic.Int32
	p.TokenURL = "http://cancelled-refresh.invalid/oauth/token"
	p.Client = &http.Client{Transport: sourceRefreshRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		refreshes.Add(1)
		cancel()
		return nil, context.Canceled
	})}
	selected, release, _, _, err := p.pickAccount(ctx, "test-client-key")
	if release != nil {
		release()
	}
	if !errors.Is(err, context.Canceled) || release != nil || selected.ID != "" || refreshes.Load() != 1 {
		t.Fatal("cancelled refresh returned an admitted account or obscured cancellation")
	}
	current, err := p.Store.Get("test")
	if err != nil || current.LastError != before.LastError || current.AccessToken != before.AccessToken || p.inFlight(gatewayAdmissionKey(before)) != 0 {
		t.Fatal("cancelled refresh retained admission or recorded an account failure")
	}
}

func TestGatewayCooldownAuditCancellationAndDeadlineBounds(t *testing.T) {
	p := &Proxy{}
	enableRateLimitCooldownForTest(t, p)
	account := Account{ID: "account"}
	ctx, cancel := context.WithCancel(withGatewayCooldownModel(t.Context(), "model"))
	cancel()
	if err := p.checkGatewayCooldown(ctx, account); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission lost cancellation: %v", err)
	}
	p.recordGatewayCooldown(ctx, account, "30")
	var limited *gatewayCooldownError
	if err := p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), "model"), account); !errors.As(err, &limited) {
		t.Fatalf("downstream cancellation discarded received upstream 429: %v", err)
	}
	bounded := Account{ID: "bounded"}
	err := p.gatewayCooldown(t.Context(), bounded, gatewayCooldownConnection, time.Duration(math.MaxInt64))
	if !errors.As(err, &limited) || time.Until(limited.until) > gatewayCooldownMax {
		t.Fatal("internal caller exceeded the shared TTL bound")
	}
	_, retry, _ := gatewaySelectionFailure(&gatewayCooldownError{until: time.Now().Add(time.Duration(math.MaxInt64))})
	seconds, err := strconv.ParseInt(retry, 10, 64)
	if err != nil || seconds < int64(time.Hour/time.Second) {
		t.Fatal("large Retry-After overflowed to one second")
	}
}

func TestGatewayCooldownAuditWrappedScalarHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, raw, retry string
		limited          bool
	}{
		{"numeric_quota", `{"type":"error","status":429,"headers":{"x-codex-primary-window-minutes":15,"Retry-After":"30"}}`, "30", true},
		{"numeric_retry", `{"type":"error","status":429,"headers":{"retry-after":31}}`, "31", true},
		{"status_code_alias", `{"type":"error","status_code":429,"headers":{"Retry-After":"30"}}`, "30", true},
		{"status_precedence", `{"type":"error","status":503,"status_code":429}`, "", false},
		{"boolean_quota", `{"type":"error","status":429,"headers":{"x-enabled":true,"RETRY-AFTER":"32"}}`, "32", true},
		{"nested_ignored", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}},"headers":{"x-metadata":{"a":1}}}`, "", true},
		{"null_retry", `{"type":"error","status":429,"headers":{"Retry-After":null}}`, "", true},
		{"message_only", `{"type":"error","error":{"message":"rate_limit_exceeded 429 retry in 30 seconds"}}`, "", false},
		{"unrelated_status", `{"type":"error","status":503,"headers":{"Retry-After":30}}`, "30", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, retry, limited := gatewayEventRateLimit([]byte(tc.raw))
			if retry != tc.retry || limited != tc.limited {
				t.Fatalf("parsed %q/%t", retry, limited)
			}
		})
	}
}

func TestGatewayCooldownAuditScopedErrorsCannotPolluteConnection(t *testing.T) {
	for _, tc := range []struct {
		name, lane, response, activeID, completedID, wantModel string
		defaultLane, connection                                bool
	}{
		{name: "unknown_lane", lane: "unknown", activeID: "active"},
		{name: "unknown_lane_known_response", lane: "unknown", response: "active", activeID: "active"},
		{name: "mismatched_response", lane: "active-lane", response: "old", activeID: "active"},
		{name: "unknown_response_without_lane", response: "old", activeID: "active"},
		{name: "completed_response", lane: "active-lane", response: "old", completedID: "old"},
		{name: "matching_lane", lane: "active-lane", response: "active", activeID: "active", wantModel: "active-model"},
		{name: "first_named_failure", lane: "active-lane", response: "first", wantModel: "active-model"},
		{name: "known_response_without_lane", response: "active", activeID: "active", wantModel: "active-model"},
		{name: "first_default_failure", response: "first", defaultLane: true, wantModel: "active-model"},
		{name: "default_active_mismatch", response: "old", activeID: "active", defaultLane: true},
		{name: "genuine_connection_limit", activeID: "active", connection: true},
		{name: "genuine_connection_limit_default_lane", activeID: "active", defaultLane: true, connection: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{}
			enableRateLimitCooldownForTest(t, p)
			account := Account{ID: "account"}
			lane := "active-lane"
			if tc.defaultLane {
				lane = ""
			}
			turns := gatewayWSTurns{proxy: p, pending: map[string][]*gatewayWSTurn{
				lane: {{responseID: tc.activeID, rec: RequestRecord{Model: "active-model"}}},
			}}
			turns.completed.add(tc.completedID)
			raw, err := json.Marshal(map[string]any{"type": "error", "status": 429, "stream_id": tc.lane, "response_id": tc.response, "headers": map[string]any{"Retry-After": 30, "x-window": 15}})
			if err != nil {
				t.Fatal(err)
			}
			turns.observeCooldown(t.Context(), raw, account)
			for _, model := range []string{"active-model", "unrelated-model"} {
				var limited *gatewayCooldownError
				err := p.checkGatewayCooldown(withGatewayCooldownModel(t.Context(), model), account)
				want := tc.connection || model == tc.wantModel
				if errors.As(err, &limited) != want {
					t.Fatalf("model %s cooldown=%v want=%t", model, err, want)
				}
			}
		})
	}
}

func TestGatewayRoutingAuditPinnedProxyAssignmentChanges(t *testing.T) {
	for _, tc := range []struct {
		name              string
		original, changed []string
		wantError         bool
	}{
		{"direct_to_required", nil, []string{"proxy-b"}, true},
		{"required_to_direct", []string{"proxy-a"}, nil, true},
		{"required_to_other", []string{"proxy-a"}, []string{"proxy-b"}, true},
		{"still_allowed", []string{"proxy-a"}, []string{"proxy-b", "proxy-a"}, false},
		{"still_direct", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{Proxies: &ProxyStore{index: map[string]*ProxyEntry{
				"proxy-a": {ID: "proxy-a", Enabled: true}, "proxy-b": {ID: "proxy-b", Enabled: true},
			}}}
			account := Account{ID: "stable", Proxies: tc.original}
			ctx := withGatewayRequestRoute(t.Context(), "key", "")
			first, err := p.gatewayRequestProxy(ctx, account)
			if err != nil {
				t.Fatal(err)
			}
			account.Proxies = tc.changed
			second, err := p.gatewayRequestProxy(ctx, account)
			if tc.wantError {
				if !errors.Is(err, errGatewayProxyUnavailable) {
					t.Fatalf("changed assignment not rejected: %v", err)
				}
			} else if err != nil || second != first {
				t.Fatalf("route changed despite remaining allowed: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(withGatewayRequestRoute(t.Context(), "key", ""))
	cancel()
	if _, err := (&Proxy{}).gatewayRequestProxy(ctx, Account{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled route lookup admitted a direct route")
	}
}

func TestGatewayCooldownRedisAuditServerClockAndTTL(t *testing.T) {
	db, _ := gatewayRedisSessionFixture(t)
	ctx := t.Context()
	key := db.Namespace + "gateway:cooldowns"
	t.Cleanup(func() { _ = db.Redis.Del(context.Background(), key).Err() })
	p := &Proxy{Store: Store{DB: db}}
	enableRateLimitCooldownForTest(t, p)
	account := Account{ID: "server-time"}
	before, err := db.Redis.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	p.recordGatewayCooldown(ctx, account, "3600")
	score, err := db.Redis.ZScore(ctx, key, gatewayCooldownKey(account, gatewayCooldownConnection)).Result()
	if err != nil {
		t.Fatal(err)
	}
	after, err := db.Redis.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	if score < float64(before.Add(time.Hour).UnixMilli()) || score > float64(after.Add(time.Hour).UnixMilli()) {
		t.Fatal("shared deadline was not computed from Redis server time")
	}
	p.recordGatewayCooldown(ctx, Account{ID: "short"}, "1")
	ttl, err := db.Redis.PTTL(ctx, key).Result()
	if err != nil || ttl < gatewayCooldownMax || ttl > gatewayCooldownMax+time.Second {
		t.Fatalf("short record shortened maximum cooldown retention: %v %v", ttl, err)
	}
	still, err := db.Redis.ZScore(ctx, key, gatewayCooldownKey(account, gatewayCooldownConnection)).Result()
	if err != nil || still != score {
		t.Fatal("unrelated short deadline altered long record")
	}
}
