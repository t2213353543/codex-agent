package server

// Session D (2026-09-30): per-account WebSocket availability regression tests.
// Natural production traffic shows accounts whose upstream answers the
// preferred WebSocket handshake with deterministic 404s while sibling accounts
// upgrade; those accounts must stop paying a wasted dial before the HTTP
// fallback, without affecting accounts whose handshakes succeed.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func contextWithWSAccount(parent context.Context, id string) context.Context {
	return context.WithValue(parent, gatewayWSAccountKey{}, gatewayWSAccountRoute{ID: id})
}

func TestWSAvailabilityTripsAfterConsecutiveRefusals(t *testing.T) {
	var v gatewayWSAvailability
	id := "ws-blocked-account"
	if v.blocked(id) {
		t.Fatal("fresh account must not be blocked")
	}
	v.observeUnavailable(id)
	v.observeUnavailable(id)
	if v.blocked(id) {
		t.Fatal("threshold is three consecutive refusals")
	}
	v.observeUnavailable(id)
	if !v.blocked(id) {
		t.Fatal("third consecutive refusal must trip the cooldown")
	}
}

func TestWSAvailabilityIndependentPerAccount(t *testing.T) {
	var v gatewayWSAvailability
	for i := 0; i < gatewayWSAvailabilityThreshold+2; i++ {
		v.observeUnavailable("account-a\x00direct")
	}
	if !v.blocked("account-a\x00direct") {
		t.Fatal("account-a should be blocked")
	}
	if v.blocked("account-b\x00direct") {
		t.Fatal("account-b inherits no failure from account-a")
	}
}

// A refused handshake through one egress must not block another proxy path
// for the same account.
func TestWSAvailabilityScopesToProxy(t *testing.T) {
	var v gatewayWSAvailability
	direct := gatewayWSAvailabilityKey("shared-account", nil, "/responses")
	viaProxy := gatewayWSAvailabilityKey("shared-account", &ProxyEntry{ID: "px-1"}, "/responses")
	if direct == viaProxy {
		t.Fatal("proxy key must differ from direct egress")
	}
	for i := 0; i < gatewayWSAvailabilityThreshold; i++ {
		v.observeUnavailable(direct)
	}
	if !v.blocked(direct) {
		t.Fatal("direct egress should be blocked after refusals")
	}
	if v.blocked(viaProxy) {
		t.Fatal("refusals on direct egress must not block a distinct proxy")
	}
}

func TestWSAvailabilityEstablishedResets(t *testing.T) {
	var v gatewayWSAvailability
	key := "ws-recovered-account\x00direct"
	v.observeUnavailable(key)
	v.observeUnavailable(key)
	v.observeEstablished(key)
	v.observeUnavailable(key)
	v.observeUnavailable(key)
	if v.blocked(key) {
		t.Fatal("a successful upgrade must reset the consecutive count")
	}
}

func TestWSAvailabilityCooldownExpiryAllowsProbe(t *testing.T) {
	var v gatewayWSAvailability
	key := "ws-expiry-account\x00direct"
	for i := 0; i < gatewayWSAvailabilityThreshold; i++ {
		v.observeUnavailable(key)
	}
	if !v.blocked(key) {
		t.Fatal("precondition: account blocked")
	}
	v.mu.Lock()
	entry := v.entries[key]
	entry.blockedTill = time.Now().Add(-time.Second)
	v.mu.Unlock()
	if v.blocked(key) {
		t.Fatal("expired cooldown must allow the next dial as a probe")
	}
	// The probe failed again; the cooldown returns.
	v.observeUnavailable(key)
	if v.blocked(key) {
		t.Fatal("probe failure alone must not block before the threshold")
	}
}

// Consecutive counting must survive availability checks: reading state
// without a trip may not reset the counter (natural traffic interleaves
// blocked() probes between refusals).
func TestWSAvailabilityChecksDoNotResetConsecutiveCount(t *testing.T) {
	var v gatewayWSAvailability
	key := "ws-interleaved\x00direct"
	v.observeUnavailable(key)
	_ = v.blocked(key)
	v.observeUnavailable(key)
	_ = v.blocked(key)
	v.observeUnavailable(key)
	if !v.blocked(key) {
		t.Fatal("interleaved checks must not reset the consecutive count")
	}
}

// The entry map stays bounded: once large, recording sweeps expired cooldowns
// instead of accumulating stale state forever.
func TestWSAvailabilitySweepsExpiredEntries(t *testing.T) {
	var v gatewayWSAvailability
	v.entries = map[string]*gatewayWSAvailabilityEntry{}
	for i := 0; i < gatewayWSAvailabilitySweepAt; i++ {
		key := "stale-" + strconv.Itoa(i) + "\x00direct"
		v.entries[key] = &gatewayWSAvailabilityEntry{consecutive: gatewayWSAvailabilityThreshold, blockedTill: time.Now().Add(-time.Minute)}
	}
	live := "live-account\x00direct"
	v.observeUnavailable(live)
	if len(v.entries) != 1 {
		t.Fatalf("expired entries must be swept at the size bound: %d remain", len(v.entries))
	}
	if v.entries[live] == nil {
		t.Fatal("the fresh observation must survive the sweep")
	}
}

func TestWSAvailabilityEmptyAccountIgnored(t *testing.T) {
	var v gatewayWSAvailability
	v.observeUnavailable(gatewayWSAvailabilityKey("", nil, "/responses"))
	if v.blocked(gatewayWSAvailabilityKey("", nil, "/responses")) {
		t.Fatal("empty account ID never participates")
	}
}

func TestWSAvailabilityConcurrentAccess(t *testing.T) {
	var v gatewayWSAvailability
	key := "ws-concurrent-account\x00direct"
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				v.observeUnavailable(key)
			} else {
				v.observeEstablished(key)
			}
			_ = v.blocked(key)
		}(i)
	}
	wg.Wait()
}

// A refused 404 handshake followed by the HTTP fallback is the natural-traffic
// failure this component exists to short-circuit.
func TestBPSWebSocketRefusedHandshakeTripsAndSkips(t *testing.T) {
	refusals := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refusals++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	p := newTestProxy(t, 1)
	accountID := "ws-404-account"
	key := gatewayWSAvailabilityKey(accountID, nil, "/responses")
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/responses", nil)
	req.Header.Set("Authorization", "Bearer synthetic-availability-404")
	for i := 0; i < gatewayWSAvailabilityThreshold; i++ {
		req := req.Clone(contextWithWSAccount(req.Context(), accountID))
		response, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), defaultSettings())
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if !errors.Is(err, errPreferredWebSocketUnavailable) {
			t.Fatalf("refusal %d: expected unavailable fallback signal, got %v", i, err)
		}
	}
	if refusals != gatewayWSAvailabilityThreshold {
		t.Fatalf("each pre-block dial must reach the upstream once: %d", refusals)
	}
	if !p.wsAvailability.blocked(key) {
		t.Fatal("three refusals must block the account")
	}
	blocked, err := p.bpsWebSocketRequest(req.Clone(contextWithWSAccount(req.Context(), accountID)), nil, []byte(`{"model":"fixture","input":[]}`), defaultSettings())
	if !errors.Is(err, errPreferredWebSocketUnavailable) {
		t.Fatalf("blocked dial: expected unavailable signal, got %v", err)
	}
	if blocked != nil {
		t.Fatal("blocked dial must not produce a response")
	}
	if refusals != gatewayWSAvailabilityThreshold {
		t.Fatalf("blocked dial must not reach the upstream: %d", refusals)
	}
}

// A refusable handshake (401 policy answer) carries no availability signal.
func TestBPSWebSocketPolicyRefusalDoesNotTrip(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	p := newTestProxy(t, 1)
	accountID := "ws-401-account"
	key := gatewayWSAvailabilityKey(accountID, nil, "/responses")
	for i := 0; i < gatewayWSAvailabilityThreshold+2; i++ {
		req, _ := http.NewRequestWithContext(contextWithWSAccount(t.Context(), accountID), http.MethodPost, server.URL+"/responses", nil)
		req.Header.Set("Authorization", "Bearer synthetic-availability-401")
		response, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), defaultSettings())
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("policy refusal must keep its HTTP answer: %v %v", response, err)
		}
	}
	if p.wsAvailability.blocked(key) {
		t.Fatal("policy refusals must not trip the availability cooldown")
	}
}

// Natural traffic (2026-09-30, post connection-reuse deploy) shows accounts
// whose every preferred-WS dial is refused with 429 at the handshake while the
// HTTP fallback succeeds: ~100 wasted dials per minute. Three consecutive
// refusals must pause the pairing's dials briefly, and any established
// exchange lifts the pause.
func TestWSHandshakeLimitBackoffTripsAndEstablishedResets(t *testing.T) {
	var v gatewayWSAvailability
	key := "ws-429-account\x00direct\x00/responses"
	v.observeHandshakeLimited(key)
	v.observeHandshakeLimited(key)
	if v.dialLimited(key) {
		t.Fatal("threshold is three consecutive handshake refusals")
	}
	v.observeHandshakeLimited(key)
	if !v.dialLimited(key) {
		t.Fatal("third consecutive handshake refusal must trip the backoff")
	}
	v.observeEstablished(key)
	if v.dialLimited(key) {
		t.Fatal("an established exchange must lift the dial backoff")
	}
}

// A limited pairing keeps serving HTTP: the transport-support cooldown and
// the handshake-limit backoff stay independent states on one entry.
func TestWSHandshakeLimitIndependentFromAvailability(t *testing.T) {
	var v gatewayWSAvailability
	key := "ws-mixed-account\x00direct\x00/responses"
	for i := 0; i < gatewayWSAvailabilityThreshold; i++ {
		v.observeHandshakeLimited(key)
	}
	if !v.dialLimited(key) {
		t.Fatal("precondition: dial backoff active")
	}
	if v.blocked(key) {
		t.Fatal("handshake 429s must not trip the transport-support cooldown")
	}
	for i := 0; i < gatewayWSAvailabilityThreshold; i++ {
		v.observeUnavailable(key)
	}
	if !v.blocked(key) {
		t.Fatal("precondition: availability cooldown active")
	}
}

// An expired backoff leaves a threshold-minus-one count so a single refused
// probe re-trips it without a fresh climb, mirroring probe semantics.
func TestWSHandshakeLimitExpirySingleRefusalRetrips(t *testing.T) {
	var v gatewayWSAvailability
	key := "ws-429-expiry\x00direct\x00/responses"
	for i := 0; i < gatewayWSAvailabilityThreshold; i++ {
		v.observeHandshakeLimited(key)
	}
	v.mu.Lock()
	entry := v.entries[key]
	entry.limitedTill = time.Now().Add(-time.Second)
	v.mu.Unlock()
	if v.dialLimited(key) {
		t.Fatal("expired backoff must allow the next dial as a probe")
	}
	v.observeHandshakeLimited(key)
	if !v.dialLimited(key) {
		t.Fatal("one refused probe after expiry must re-trip the backoff")
	}
}

// The pairing key must separate proxies whose operator edited host/port (an
// edited route must not inherit refusals from the old one) and separate the
// dialled endpoints (a /responses refusal says nothing about /guardian).
func TestWSAvailabilityKeyScopesToFingerprintAndEndpoint(t *testing.T) {
	base := &ProxyEntry{ID: "px-1", Host: "10.0.0.1", Port: 8080}
	edited := &ProxyEntry{ID: "px-1", Host: "10.0.0.2", Port: 8080}
	if gatewayWSAvailabilityKey("acct", base, "/responses") == gatewayWSAvailabilityKey("acct", edited, "/responses") {
		t.Fatal("editing a proxy's host must not inherit the old route's refusals")
	}
	if gatewayWSAvailabilityKey("acct", base, "/responses") == gatewayWSAvailabilityKey("acct", base, "/guardian") {
		t.Fatal("refusals must stay scoped to the dialled endpoint")
	}
}

// The entry map is a hard bound: a map already full of live state refuses new
// keys instead of growing past the sweep threshold.
func TestWSAvailabilityRefusesNewKeysWhenFull(t *testing.T) {
	var v gatewayWSAvailability
	v.entries = map[string]*gatewayWSAvailabilityEntry{}
	now := time.Now()
	for i := 0; i < gatewayWSAvailabilitySweepAt; i++ {
		v.entries["live-"+strconv.Itoa(i)+"\x00direct\x00/responses"] = &gatewayWSAvailabilityEntry{consecutive: 1, lastObserved: now}
	}
	v.observeUnavailable("fresh-account\x00direct\x00/responses")
	if len(v.entries) != gatewayWSAvailabilitySweepAt {
		t.Fatalf("a full live map must refuse new keys: %d entries", len(v.entries))
	}
	if v.entries["fresh-account\x00direct\x00/responses"] != nil {
		t.Fatal("the refused key must not gain state")
	}
	if v.blocked("live-0\x00direct\x00/responses") {
		t.Fatal("existing live state must be preserved")
	}
}

// Counter-only entries (never tripped a till) age out after a full cooldown
// instead of accumulating forever.
func TestWSAvailabilitySweepsAgedCounterEntries(t *testing.T) {
	var v gatewayWSAvailability
	now := time.Now()
	v.entries = map[string]*gatewayWSAvailabilityEntry{
		"aged\x00direct\x00/responses":  {consecutive: 1, lastObserved: now.Add(-gatewayWSAvailabilityCooldown - time.Minute)},
		"fresh\x00direct\x00/responses": {consecutive: 1, lastObserved: now},
	}
	v.sweepLocked(now)
	if v.entries["aged\x00direct\x00/responses"] != nil {
		t.Fatal("an idle counter entry must age out at the sweep")
	}
	if v.entries["fresh\x00direct\x00/responses"] == nil {
		t.Fatal("a recently observed counter entry must survive")
	}
}

// End to end over the real dial path: three 429 handshakes pause the dials,
// the request keeps its 429 answer for HTTP recovery, and the pause expires
// back to probe semantics.
func TestBPSWebSocketHandshakeRateLimitedPausesDials(t *testing.T) {
	refusals := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refusals++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	p := newTestProxy(t, 1)
	accountID := "ws-429-dial-account"
	key := gatewayWSAvailabilityKey(accountID, nil, "/responses")
	for i := 0; i < gatewayWSAvailabilityThreshold; i++ {
		req, _ := http.NewRequestWithContext(contextWithWSAccount(t.Context(), accountID), http.MethodPost, server.URL+"/responses", nil)
		req.Header.Set("Authorization", "Bearer synthetic-availability-429")
		response, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), defaultSettings())
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err != nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("handshake %d: the 429 answer must keep flowing to HTTP recovery: %v %v", i, response, err)
		}
	}
	if refusals != gatewayWSAvailabilityThreshold {
		t.Fatalf("each pre-pause dial must reach the upstream once: %d", refusals)
	}
	if !p.wsAvailability.dialLimited(key) {
		t.Fatal("three refused handshakes must pause the pairing's dials")
	}
	req, _ := http.NewRequestWithContext(contextWithWSAccount(t.Context(), accountID), http.MethodPost, server.URL+"/responses", nil)
	req.Header.Set("Authorization", "Bearer synthetic-availability-429")
	paused, err := p.bpsWebSocketRequest(req, nil, []byte(`{"model":"fixture","input":[]}`), defaultSettings())
	if !errors.Is(err, errPreferredWebSocketUnavailable) {
		t.Fatalf("paused dial: expected unavailable signal, got %v", err)
	}
	if paused != nil {
		t.Fatal("paused dial must not produce a response")
	}
	if refusals != gatewayWSAvailabilityThreshold {
		t.Fatalf("paused dial must not reach the upstream: %d", refusals)
	}
}
