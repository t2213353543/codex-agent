package server

import (
	"math/rand"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Native HTTP requests carry full history, including account-bound encrypted
// reasoning/compaction items. Keep a thread on its original account, including
// retries and resume. Redis shares the binding across processes and restarts.
const gatewaySessionTTL = 7 * 24 * time.Hour
const maxGatewaySessions = 16384

type gatewaySessionRoute struct {
	Account string
	Expires time.Time
}

func gatewaySessionKey(r *http.Request, body []byte) string {
	if session := gatewayExplicitSessionKey(r, body); session != "" {
		return session
	}
	return gatewayContentSessionKey(body, r != nil && gatewayUnboundedPayload(r.Context()))
}

// Read client-owned identity before a policy can add or rewrite a cache key.
// Synthesized shared cache prefixes must never create conversation ownership.
func gatewayExplicitSessionKey(r *http.Request, body []byte) string {
	// session-id survives resume and is shared by the root and its descendants,
	// which may reuse encrypted history. Older releases used underscores.
	// Compatible clients also provide local routing aliases. Resolve them with
	// the same scalar/case-folding and Connection rules as forwarded headers,
	// but do not synthesize upstream headers or metadata from these aliases.
	headers := make(http.Header)
	var source http.Header
	if r != nil {
		source = r.Header
	}
	copyGatewayHeaders(headers, source, func(name string) bool {
		switch name {
		case "session-id", "session_id", "thread-id", "conversation_id",
			"x-session-affinity", "x-session-id", "x-opencode-session", "x-conversation-id":
			return true
		}
		return false
	}, func(string) bool { return true })
	// Native identity always wins; compatible conversation headers then take
	// precedence over a cache key, which may change independently each turn.
	for _, header := range []string{
		"session-id", "session_id", "thread-id", "conversation_id",
		"x-session-affinity", "x-session-id", "x-opencode-session", "x-conversation-id",
	} {
		// Only HTTP optional whitespace is framing. Unicode whitespace can
		// survive as header data and must not merge distinct upstream IDs.
		if value := strings.Trim(headers.Get(header), " \t"); value != "" {
			return value
		}
	}
	// Match the exact member used by policy and the upstream protocol. Unknown
	// case variants must neither split a continuation nor create new affinity.
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	var key string
	if json.Unmarshal(payload["prompt_cache_key"], &key) != nil {
		return ""
	}
	return key
}

func gatewayAccountRef(account Account) string {
	ref := gatewayAccountOwnerRef(account)
	if !gatewayCompleteOwner(account) {
		ref += ":credentials:" + gatewayUnknownOwnerCredentials(account)
	}
	if account.OwnerRevision != "" {
		ref += ":revision:" + account.OwnerRevision
	}
	return ref
}

// This static component identifies the account record and available ownership
// claims, not continuity across unknown-owner credential changes.
func gatewayAccountOwnerRef(account Account) string {
	ref := "name:" + account.Name
	if account.ID != "" {
		ref = "id:" + account.ID
	}
	// Re-import keeps the management ID, but may replace the upstream identity.
	// Token refresh must preserve affinity; importing a different ChatGPT
	// account must never receive the old account's encrypted conversation.
	if account.AccountID != "" {
		sum := sha256.Sum256([]byte(account.AccountID))
		ref += ":upstream:" + hex.EncodeToString(sum[:])
	}
	// A ChatGPT workspace can contain multiple users. Known ownership must
	// match both, as in CLI 0.155 auth/change_state.rs; token rotation is not
	// an ownership change. Legacy bindings without this suffix fail closed.
	if account.Identity != nil && strings.TrimSpace(account.Identity.UserID) != "" {
		sum := sha256.Sum256([]byte(account.Identity.UserID))
		ref += ":user:" + hex.EncodeToString(sum[:])
	}
	return ref
}

// sessionAccount atomically installs a proposed account if no binding exists.
// Only hashed key/thread identifiers and account IDs are stored, never tokens
// or conversation content. Redis errors must not cause a silent account switch.
func (p *Proxy) sessionAccount(ctx context.Context, keyID, session, proposed string) (string, error) {
	return p.gatewaySessionAccount(ctx, keyID, session, proposed, false)
}

// Renewal is separate from lookup/claim: a rejected request must not keep an
// unavailable historical owner alive. Only admitted use and active leases may
// extend an existing, exactly matching owner; expired bindings are not revived.
func (p *Proxy) touchGatewaySession(ctx context.Context, keyID, session, expected string) (string, error) {
	return p.gatewaySessionAccount(ctx, keyID, session, expected, true)
}

func (p *Proxy) gatewaySessionAccount(ctx context.Context, keyID, session, proposed string, renew bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	settings, err := p.settingsValue()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err != nil {
		return "", errors.New("session routing settings unavailable")
	}
	ttl := gatewaySessionTTL
	if seconds := settings.StickySessionTTLSeconds; seconds >= 60 && seconds <= 30*24*60*60 {
		ttl = time.Duration(seconds) * time.Second
	}
	// Sub2API keeps inferred content affinity for one hour. Explicit client
	// conversations retain the configured lifetime and existing owner protection.
	if isGatewayContentSessionKey(session) && ttl > time.Hour {
		ttl = time.Hour
	}
	sum := sha256.Sum256([]byte(keyID + "\x00" + session))
	key := hex.EncodeToString(sum[:])
	if db := p.Store.DB; db != nil {
		if db.Redis == nil {
			return "", errors.New("session routing unavailable")
		}
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		key = db.Namespace + "gateway:thread:" + key
		if proposed == "" {
			value, err := db.Redis.Get(readCtx, key).Result()
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if errors.Is(err, redis.Nil) {
				return "", nil
			}
			if err != nil {
				return "", errors.New("session routing unavailable")
			}
			return value, nil
		}
		operation := "claim"
		if renew {
			operation = "renew"
		}
		value, err := db.Redis.Eval(readCtx, `
local account = redis.call('GET', KEYS[1])
if ARGV[3]=='renew' then
 if account==ARGV[1] then redis.call('EXPIRE',KEYS[1],ARGV[2]) end
 return account or ''
end
if not account then
 account=ARGV[1]
 redis.call('SET',KEYS[1],account,'EX',ARGV[2])
end
return account`, []string{key}, proposed, int64(ttl/time.Second), operation).Text()
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err != nil {
			return "", errors.New("session routing unavailable")
		}
		return value, nil
	}
	// File-store compatibility is used by isolated tests and legacy tools.
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now := time.Now()
	if route, ok := p.sessionRoutes[key]; ok && now.Before(route.Expires) {
		if renew && route.Account == proposed {
			route.Expires = now.Add(ttl)
			p.sessionRoutes[key] = route
		}
		return route.Account, nil
	}
	if proposed == "" || renew {
		return "", nil
	}
	if p.sessionRoutes == nil {
		p.sessionRoutes = make(map[string]gatewaySessionRoute)
	}
	if len(p.sessionRoutes) >= maxGatewaySessions {
		for k, route := range p.sessionRoutes {
			if !now.Before(route.Expires) {
				delete(p.sessionRoutes, k)
			}
		}
		if len(p.sessionRoutes) >= maxGatewaySessions && !gatewayUnboundedPayload(ctx) {
			return "", errors.New("session routing capacity reached")
		}
	}
	p.sessionRoutes[key] = gatewaySessionRoute{Account: proposed, Expires: now.Add(ttl)}
	return proposed, nil
}

func (p *Proxy) pickSessionAccount(ctx context.Context, token, keyID, session string) (Account, func(), error) {
	if owner := appServerDelegatedOwner(ctx); owner != "" {
		account, release, err := p.pickAppServerRealtimeOwner(ctx, token, keyID, owner)
		if err != nil {
			return Account{}, nil, err
		}
		if session != "" {
			bound, err := p.sessionAccount(ctx, keyID, session, owner)
			if err != nil || bound != owner {
				release()
				return Account{}, nil, errors.New("conversation owner differs from delegated client identity")
			}
			bound, err = p.touchGatewaySession(ctx, keyID, session, owner)
			if err != nil || bound != owner {
				release()
				if err != nil {
					return Account{}, nil, err
				}
				return Account{}, nil, errors.New("session account binding expired or changed during admission")
			}
		}
		return account, release, nil
	}
	if err := ctx.Err(); err != nil {
		return Account{}, nil, err
	}
	settings, err := p.settingsValue()
	if err != nil {
		return Account{}, nil, errors.New("session routing settings unavailable")
	}
	if session == "" || (!settings.StickySessionsEnabled && p.BPS == nil && !appServerRequiresOwner(ctx)) {
		account, release, _, _, err := p.pickAccount(ctx, token)
		return account, release, err
	}
	bound, err := p.sessionAccount(ctx, keyID, session, "")
	if err != nil {
		return Account{}, nil, err
	}
	if bound == "" {
		account, release, currentKeyID, _, err := p.pickAccount(ctx, token)
		if err != nil {
			// A concurrent first request may have bound the session while
			// account selection was in progress. Respect that binding even
			// if this request found every account busy or unavailable.
			var lookupErr error
			bound, lookupErr = p.sessionAccount(ctx, keyID, session, "")
			if lookupErr != nil {
				return Account{}, nil, lookupErr
			}
			if bound == "" {
				return Account{}, nil, err
			}
		} else {
			// The key can be replaced while an initial request is selecting or
			// refreshing an account. Never claim another key's session namespace.
			if currentKeyID != keyID {
				release()
				return Account{}, nil, errors.New("gateway key is no longer authorized")
			}
			bound, err = p.sessionAccount(ctx, keyID, session, gatewayAccountRef(account))
			if err == nil && bound == gatewayAccountRef(account) {
				return account, release, nil
			}
			release()
			if err != nil {
				return Account{}, nil, err
			}
		}
	}
	capture, _ := ctx.Value(gatewayBoundSelectionCaptureKey{}).(*gatewayBoundSelectionCapture)
	var facts gatewayBoundCandidateSnapshot
	var observed *gatewayBoundCandidateSnapshot
	if capture != nil {
		observed = &facts
	}
	candidates, _, _, err := p.candidatesForContextObserved(ctx, token, observed)
	if err != nil {
		return Account{}, nil, err
	}
	for _, account := range candidates {
		if gatewayAccountRef(account) != bound {
			continue
		}
		selected := account
		account, err = p.gatewayCurrentBoundAccountContext(ctx, account, bound)
		if err != nil {
			var changed *gatewayOwnerChangedError
			if errors.Is(err, errNotFound) || errors.As(err, &changed) {
				return Account{}, nil, gatewayBoundAccountUnavailable(ctx, selected, bound, err)
			}
			return Account{}, nil, err
		}
		if err := p.checkGatewayBoundAccountAccess(ctx, token, keyID, account); err != nil {
			if errors.Is(err, errGatewayAccountNotAllowed) {
				return Account{}, nil, gatewayBoundAccountUnavailable(ctx, account, bound, err)
			}
			return Account{}, nil, err
		}
		if err := p.checkGatewayCooldown(ctx, account); err != nil {
			return Account{}, nil, err
		}
		admission, acquired, admissionErr := p.acquireGatewayAccountContext(ctx, account)
		if admissionErr != nil {
			return Account{}, nil, admissionErr
		}
		if !acquired {
			return Account{}, nil, &gatewayAccountBusyError{session: true}
		}
		release := admission.Release
		ready, err := p.ensureReady(admission.Context, account)
		if err != nil {
			admissionCause := context.Cause(admission.Context)
			release()
			if gatewaySharedAdmissionFailure(admissionCause) {
				return Account{}, nil, admissionCause
			}
			if ctx.Err() != nil {
				return Account{}, nil, ctx.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Account{}, nil, err
			}
			p.noteAccountError(account, err)
			return Account{}, nil, gatewayBoundAccountUnavailable(ctx, account, bound, errors.New("session account unavailable; restore its credentials or start a new thread"))
		}
		if gatewayAccountRef(ready) != bound {
			release()
			return Account{}, nil, gatewayBoundAccountUnavailable(ctx, account, bound, &gatewayOwnerChangedError{})
		}
		if err := p.checkGatewayBoundAccountAccess(ctx, token, keyID, ready); err != nil {
			release()
			if errors.Is(err, errGatewayAccountNotAllowed) {
				return Account{}, nil, gatewayBoundAccountUnavailable(ctx, ready, bound, err)
			}
			return Account{}, nil, err
		}
		if err := p.checkGatewayCooldown(ctx, ready); err != nil {
			release()
			return Account{}, nil, err
		}
		current, err := p.touchGatewaySession(ctx, keyID, session, bound)
		if err != nil || current != bound {
			release()
			if err != nil {
				return Account{}, nil, err
			}
			return Account{}, nil, errors.New("session account binding expired or changed during admission")
		}
		return ready, release, nil
	}
	for _, account := range candidates {
		if gatewayPriorBindingForAccount(bound, account) {
			return Account{}, nil, gatewayBoundAccountUnavailable(ctx, account, bound, &gatewayOwnerChangedError{})
		}
	}
	if capture != nil {
		capture.observation = facts.missing(bound, len(candidates))
	}
	return Account{}, nil, gatewayBoundAccountUnavailable(ctx, Account{}, bound, errors.New("session account is no longer enabled or allowed for this key; start a new thread"))
}

// pickSessionProxy keeps a session on the same enabled outbound proxy across
// processes. Rendezvous hashing is independent of list order and only moves
// sessions whose winning proxy is added or removed. Account affinity remains
// authoritative; changing a proxy never changes the upstream account.

// gatewayCooldownAwait bounds how long one request may hold open waiting for
// a rate-limited account to uncool instead of failing fast. Payment pauses
// (thirty-minute billing stops) and long cooldowns never wait: the request
// keeps its fast 429 with Retry-After.
const (
	// Production cooldowns cluster just above the previous 5s bound (the
	// 4-second default plus concurrent-429 extensions reach 6-8s under load),
	// so a request waiting out a rate limit needs headroom to cover them; the
	// bound stays far below the client's 45-second liveness budget.
	gatewayCooldownAwaitMax     = 15 * time.Second
	gatewayCooldownAwaitRetries = 2
	// Jitter spreads the herd of waiters that would otherwise wake on one
	// cooldown expiry and re-limit each other in the same instant.
	gatewayCooldownAwaitJitter = 1500 * time.Millisecond
)

func awaitGatewayCooldown(ctx context.Context, err error) bool {
	var cooled *gatewayCooldownError
	if !errors.As(err, &cooled) || cooled.payment || ctx.Err() != nil {
		return false
	}
	wait := time.Until(cooled.until)
	if wait <= 0 || wait > gatewayCooldownAwaitMax {
		return false
	}
	if jitter := time.Duration(rand.Int63n(int64(gatewayCooldownAwaitJitter + 1))); jitter > 0 {
		wait += jitter
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

// pickSessionAccountAwait retries sticky selection when every eligible
// account is rate-limited for only a few more seconds: waiting out a short
// cooldown converts a fast 429 into the generation the client asked for,
// without touching saturated accounts or replaying anything.
func (p *Proxy) pickSessionAccountAwait(ctx context.Context, token, keyID, session string) (Account, func(), error) {
	account, release, err := p.pickSessionAccount(ctx, token, keyID, session)
	for range gatewayCooldownAwaitRetries {
		if err == nil || !awaitGatewayCooldown(ctx, err) {
			break
		}
		next, nextRelease, nextErr := p.pickSessionAccount(ctx, token, keyID, session)
		if nextErr == nil {
			return next, nextRelease, nil
		}
		if nextRelease != nil {
			nextRelease()
		}
		account, release, err = next, nil, nextErr
	}
	return account, release, err
}

func (p *Proxy) pickSessionProxy(account Account, keyID, session string) *ProxyEntry {
	settings, err := p.settingsValue()
	if session == "" || (err == nil && !settings.StickySessionsEnabled && p.BPS == nil) {
		return p.pickProxy(account)
	}
	// On a settings read error, keep deterministic routing rather than
	// silently rotating an established session. Account selection fails closed.
	if p.Proxies == nil {
		return nil
	}
	prefix := keyID + "\x00" + session + "\x00" + gatewayAccountRef(account) + "\x00"
	var selected *ProxyEntry
	var best [sha256.Size]byte
	for _, id := range account.Proxies {
		entry, ok := p.Proxies.Get(id)
		if !ok {
			continue
		}
		score := sha256.Sum256([]byte(prefix + id))
		if selected == nil || bytes.Compare(score[:], best[:]) > 0 {
			selected, best = entry, score
		}
	}
	return selected
}
