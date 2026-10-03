package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

type gatewayAccountFailoverKey struct{}

var errGatewayAccountCandidatesExhausted = errors.New("all eligible accounts have been attempted")

type gatewayBoundAccountUnavailableError struct {
	Account Account
	Owner   string
	Cause   error
}

func (e *gatewayBoundAccountUnavailableError) Error() string { return e.Cause.Error() }
func (e *gatewayBoundAccountUnavailableError) Unwrap() error { return e.Cause }

func gatewayBoundAccountUnavailable(ctx context.Context, account Account, owner string, cause error) error {
	if gatewayAccountFailoverFrom(ctx) == nil {
		return cause
	}
	return &gatewayBoundAccountUnavailableError{Account: account, Owner: owner, Cause: cause}
}

type gatewayAccountFailover struct {
	mu        sync.Mutex
	attempted map[string]bool
}

// Attempts are scoped to one incoming request. Keep the account's immutable
// management identity so credential refresh/rename cannot re-enter the loop.
func withGatewayAccountFailover(ctx context.Context) context.Context {
	if gatewayAccountFailoverFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, gatewayAccountFailoverKey{}, &gatewayAccountFailover{attempted: map[string]bool{}})
}

func gatewayAccountFailoverFrom(ctx context.Context) *gatewayAccountFailover {
	state, _ := ctx.Value(gatewayAccountFailoverKey{}).(*gatewayAccountFailover)
	return state
}

func (s *gatewayAccountFailover) contains(account Account) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempted[gatewayAdmissionKey(account)]
}

func (s *gatewayAccountFailover) claim(account Account) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := gatewayAdmissionKey(account)
	if s.attempted[key] {
		return false
	}
	s.attempted[key] = true
	return true
}

func gatewayExcludeFailedAccount(ctx context.Context, account Account) {
	if state := gatewayAccountFailoverFrom(ctx); state != nil {
		state.claim(account)
	}
}

// Every call uses the existing priority, weight, authorization, cooldown and
// concurrency rules. Callers own release and decide whether replay on a new
// owner is valid; this function never rewrites history or encrypted content.
func (p *Proxy) pickFailoverAccount(ctx context.Context, token, keyID string) (Account, func(), error) {
	if gatewayAccountFailoverFrom(ctx) == nil {
		return Account{}, nil, errors.New("request failover state is missing")
	}
	account, release, actualKey, _, err := p.pickAccount(ctx, token)
	if err != nil {
		return Account{}, nil, err
	}
	if actualKey != keyID {
		release()
		return Account{}, nil, errors.New("gateway key is no longer authorized")
	}
	return account, release, nil
}

// Compare-and-swap a failed owner's sticky binding. A concurrent request which
// has already installed another owner wins; its binding is never overwritten.
// An expired/missing binding can be installed after the caller authorizes the
// selected replacement. The returned owner is authoritative in both cases.
func (p *Proxy) switchSessionAccount(ctx context.Context, keyID, session, expected, replacement string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if keyID == "" || session == "" || replacement == "" {
		return "", false, errors.New("invalid session failover identity")
	}
	settings, err := p.settingsValue()
	if err != nil {
		return "", false, errors.New("session routing settings unavailable")
	}
	ttl := gatewaySessionTTL
	if seconds := settings.StickySessionTTLSeconds; seconds >= 60 && seconds <= 30*24*60*60 {
		ttl = time.Duration(seconds) * time.Second
	}
	if isGatewayContentSessionKey(session) && ttl > time.Hour {
		ttl = time.Hour
	}
	sum := sha256.Sum256([]byte(keyID + "\x00" + session))
	key := hex.EncodeToString(sum[:])
	if db := p.Store.DB; db != nil {
		if db.Redis == nil {
			return "", false, errors.New("session routing unavailable")
		}
		operation, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		actual, err := db.Redis.Eval(operation, `
local current=redis.call('GET',KEYS[1])
if current and current~=ARGV[1] then return current end
redis.call('SET',KEYS[1],ARGV[2],'EX',ARGV[3])
return ARGV[2]`, []string{db.Namespace + "gateway:thread:" + key}, expected, replacement, int64(ttl/time.Second)).Text()
		if err != nil {
			return "", false, errors.New("session routing unavailable")
		}
		return actual, actual == replacement, nil
	}
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	now := time.Now()
	if prior, ok := p.sessionRoutes[key]; ok && now.Before(prior.Expires) && prior.Account != expected {
		return prior.Account, prior.Account == replacement, nil
	}
	if p.sessionRoutes == nil {
		p.sessionRoutes = map[string]gatewaySessionRoute{}
	}
	p.sessionRoutes[key] = gatewaySessionRoute{Account: replacement, Expires: now.Add(ttl)}
	return replacement, true, nil
}
