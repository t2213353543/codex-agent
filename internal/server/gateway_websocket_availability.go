package server

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Natural traffic shows accounts whose upstream answers the preferred
// WebSocket handshake with a deterministic unsupported-transport status while
// sibling accounts upgrade successfully. Each such answer is one wasted dial
// round trip before the HTTP fallback runs. Availability is therefore
// remembered per account, outbound proxy and dialled endpoint: after
// consecutive pre-generation failures that pairing skips the WebSocket dial
// entirely until the cooldown expires, and any established exchange resets
// the memory.
//
// A refused-with-429 handshake is a different, shorter-lived signal: the
// upstream connection limit is capacity, not transport support, and it
// recovers on the order of a minute. Such refusals trip a brief dial backoff
// on the same pairing while the request itself keeps its HTTP fallback.
//
// Scope decisions kept deliberate:
//   - Refusable handshakes (401/403 policy answers) carry no signal; the
//     existing recovery and cooldown paths own them, which is also why the
//     key needs no credential epoch — unavailable statuses describe
//     route/transport support, not credentials.
//   - The proxy is part of the key, including its host/port fingerprint: the
//     same account may traverse a different egress whose upstream path
//     supports the upgrade, and an operator editing a proxy must not inherit
//     refusals observed on the old route. The endpoint is part of the key for
//     the same reason: a /responses refusal says nothing about /guardian.
//   - After a cooldown expires the entry is dropped and the next dial is a
//     probe. Concurrent requests may overlap probes in that instant; that is
//     safe because both outcomes just feed observe* again, and a failing
//     probe re-trips after the threshold, not before it. The handshake-limit
//     backoff keeps a threshold-minus-one count on expiry so one more refused
//     probe re-trips without a fresh climb.
//   - The map is bounded by the account/proxy/endpoint universe: entries
//     leave on success, cooldown expiry or age, a lazy sweep reclaims
//     expired state at the size bound, and a still-full map refuses new keys
//     rather than growing without bound.

const (
	gatewayWSAvailabilityThreshold = 3
	gatewayWSAvailabilityCooldown  = 10 * time.Minute
	gatewayWSHandshakeLimitBackoff = time.Minute
	gatewayWSAvailabilitySweepAt   = 1024
)

type gatewayWSAccountKey struct{}

// gatewayWSAccountRoute carries the selected account's identity and
// credential epoch to the dial boundary without widening signatures. The
// epoch lets transport-level reuse (sockets) stay scoped to one token
// generation even though availability itself is credential-independent.
type gatewayWSAccountRoute struct {
	ID                string
	CredentialVersion int64
}

type gatewayWSAvailability struct {
	mu      sync.Mutex
	entries map[string]*gatewayWSAvailabilityEntry
}

type gatewayWSAvailabilityEntry struct {
	consecutive        int
	blockedTill        time.Time
	consecutiveLimited int
	limitedTill        time.Time
	lastObserved       time.Time
}

func gatewayWSAccountFrom(ctx context.Context) gatewayWSAccountRoute {
	route, _ := ctx.Value(gatewayWSAccountKey{}).(gatewayWSAccountRoute)
	return route
}

// gatewayWSAvailabilityKey scopes one availability memory to the account, the
// outbound proxy actually used for the dial (nil means direct egress) and the
// dialled endpoint. An empty account ID yields a key starting with NUL, which
// observe*/blocked/dialLimited ignore.
func gatewayWSAvailabilityKey(accountID string, proxy *ProxyEntry, endpoint string) string {
	proxyID := "direct"
	if proxy != nil && proxy.ID != "" {
		proxyID = proxy.ID + "@" + proxy.Host + ":" + strconv.Itoa(proxy.Port)
	}
	return accountID + "\x00" + proxyID + "\x00" + endpoint
}

func gatewayWSAvailabilityKeyTracked(key string) bool {
	return key != "" && key[0] != '\x00'
}

// wsDialSkipped reports whether the preferred WebSocket dial for one
// account/egress/endpoint pairing is currently known-wasted: transport
// unavailable, or handshake-rate-limited within the short backoff. Callers
// keep their shared HTTP fallback.
func (p *Proxy) wsDialSkipped(accountID string, proxy *ProxyEntry, endpoint string) bool {
	key := gatewayWSAvailabilityKey(accountID, proxy, endpoint)
	return p.wsAvailability.blocked(key) || p.wsAvailability.dialLimited(key)
}

func (v *gatewayWSAvailability) blocked(key string) bool {
	if !gatewayWSAvailabilityKeyTracked(key) {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	entry, ok := v.entries[key]
	if !ok {
		return false
	}
	if entry.blockedTill.IsZero() {
		// Still accumulating consecutive refusals; keep the count.
		return false
	}
	if entry.blockedTill.After(time.Now()) {
		return true
	}
	// The cooldown expired; the next dial is the probe that re-checks
	// availability, so the stale entry leaves instead of blocking forever.
	// Any handshake-limit backoff on this entry is necessarily older than the
	// ten-minute cooldown and worthless by now.
	delete(v.entries, key)
	return false
}

// dialLimited reports whether a recent burst of handshake-level rate limiting
// still rules out the dial for this pairing. Unlike transport availability
// this backoff is short, and an expired backoff leaves a threshold-minus-one
// count so a single refused probe re-trips it.
func (v *gatewayWSAvailability) dialLimited(key string) bool {
	if !gatewayWSAvailabilityKeyTracked(key) {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	entry, ok := v.entries[key]
	if !ok || entry.limitedTill.IsZero() {
		return false
	}
	if entry.limitedTill.After(time.Now()) {
		return true
	}
	entry.limitedTill = time.Time{}
	entry.consecutiveLimited = gatewayWSAvailabilityThreshold - 1
	return false
}

// sweepLocked reclaims state that no longer enforces anything: every till has
// expired and nothing was observed on the pairing for a whole cooldown.
// Callers hold mu.
func (v *gatewayWSAvailability) sweepLocked(now time.Time) {
	for key, entry := range v.entries {
		if entry.blockedTill.After(now) || entry.limitedTill.After(now) {
			continue
		}
		if entry.lastObserved.IsZero() || now.Sub(entry.lastObserved) > gatewayWSAvailabilityCooldown {
			delete(v.entries, key)
		}
	}
}

// trackLocked returns whether the key may keep availability state, creating
// the entry when the map has room. Past the size bound expired state is
// swept first; a still-full map refuses new keys instead of growing.
// Callers hold mu.
func (v *gatewayWSAvailability) trackLocked(key string, now time.Time) bool {
	if v.entries == nil {
		v.entries = map[string]*gatewayWSAvailabilityEntry{}
	}
	if v.entries[key] == nil {
		if len(v.entries) >= gatewayWSAvailabilitySweepAt {
			v.sweepLocked(now)
			if len(v.entries) >= gatewayWSAvailabilitySweepAt {
				return false
			}
		}
		v.entries[key] = &gatewayWSAvailabilityEntry{}
	}
	return true
}

func (v *gatewayWSAvailability) observeUnavailable(key string) {
	if !gatewayWSAvailabilityKeyTracked(key) {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if !v.trackLocked(key, now) {
		return
	}
	entry := v.entries[key]
	entry.consecutive++
	entry.lastObserved = now
	if entry.consecutive >= gatewayWSAvailabilityThreshold {
		entry.blockedTill = now.Add(gatewayWSAvailabilityCooldown)
	}
}

// observeHandshakeLimited records one handshake refused with 429: capacity at
// the connection level, recovered on a short clock, so the pairing pauses its
// dials briefly instead of paying the refused handshake on every request.
func (v *gatewayWSAvailability) observeHandshakeLimited(key string) {
	if !gatewayWSAvailabilityKeyTracked(key) {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if !v.trackLocked(key, now) {
		return
	}
	entry := v.entries[key]
	entry.consecutiveLimited++
	entry.lastObserved = now
	if entry.consecutiveLimited >= gatewayWSAvailabilityThreshold {
		entry.limitedTill = now.Add(gatewayWSHandshakeLimitBackoff)
	}
}

func (v *gatewayWSAvailability) observeEstablished(key string) {
	if !gatewayWSAvailabilityKeyTracked(key) {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.entries, key)
}
