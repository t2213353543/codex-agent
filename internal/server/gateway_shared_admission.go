package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Private candidate: callers MUST carry Context into refresh and transport,
// and defer Release. A Redis store failure is not account saturation.
type gatewayAccountAdmission struct {
	Context context.Context
	release func()
}

func (l *gatewayAccountAdmission) Release() {
	if l != nil && l.release != nil {
		l.release()
	}
}

type gatewaySharedAdmissionError struct{ phase string }

func (e *gatewaySharedAdmissionError) Error() string { return "shared account admission " + e.phase }

type gatewaySharedAdmissionPolicy struct{ ttl, renew, operation, cancelMargin time.Duration }

var gatewaySharedAdmissionDefaults = gatewaySharedAdmissionPolicy{ttl: 30 * time.Second, renew: 8 * time.Second, operation: time.Second, cancelMargin: 5 * time.Second}

// Redis TIME supplies every score. Tokens are per acquisition, not process IDs:
// an old release/renew can never delete or revive another generation's lease.
const gatewaySharedAdmissionAcquire = `
if #KEYS>1 then
  local fence=redis.call('GET',KEYS[2])
  if fence and ARGV[4]~='' and fence==ARGV[4] then
    redis.call('DEL',KEYS[2])
  elseif fence then return -2 end
end
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
redis.call('ZREMRANGEBYSCORE',KEYS[1],'-inf',now)
local existing=redis.call('ZSCORE',KEYS[1],ARGV[1])
if existing then return 1 end
if redis.call('ZCARD',KEYS[1])>=tonumber(ARGV[2]) then return 0 end
redis.call('ZADD',KEYS[1],now+tonumber(ARGV[3]),ARGV[1])
local last=redis.call('ZRANGE',KEYS[1],-1,-1,'WITHSCORES')
redis.call('PEXPIREAT',KEYS[1],tonumber(last[2])+tonumber(ARGV[3]))
return 1`
const gatewaySharedAdmissionRenew = `
local t=redis.call('TIME')
local now=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)
local expires=redis.call('ZSCORE',KEYS[1],ARGV[1])
if not expires or tonumber(expires)<=now then return 0 end
redis.call('ZADD',KEYS[1],'XX',now+tonumber(ARGV[2]),ARGV[1])
local last=redis.call('ZRANGE',KEYS[1],-1,-1,'WITHSCORES')
redis.call('PEXPIREAT',KEYS[1],tonumber(last[2])+tonumber(ARGV[2]))
return 1`
const gatewaySharedAdmissionRelease = `
local n=redis.call('ZREM',KEYS[1],ARGV[1])
if redis.call('ZCARD',KEYS[1])==0 then redis.call('DEL',KEYS[1]) end
return n`

func gatewaySharedAdmissionKey(namespace string, account Account) string {
	sum := sha256.Sum256([]byte(gatewayAdmissionKey(account)))
	return namespace + "gateway:admission:v1:{" + hex.EncodeToString(sum[:]) + "}"
}

func (p *Proxy) acquireGatewayAccountContext(ctx context.Context, account Account) (*gatewayAccountAdmission, bool, error) {
	return p.acquireGatewayAccountWithPolicy(ctx, account, gatewaySharedAdmissionDefaults)
}

func (p *Proxy) acquireGatewayAccountWithPolicy(ctx context.Context, account Account, policy gatewaySharedAdmissionPolicy) (*gatewayAccountAdmission, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if p.Store.DB == nil {
		release, ok := p.acquireGatewayAccount(account)
		if !ok {
			return nil, false, nil
		}
		return &gatewayAccountAdmission{Context: ctx, release: release}, true, nil
	}
	db := p.Store.DB
	if db.Redis == nil || db.Namespace == "" || account.ID == "" {
		return nil, false, &gatewaySharedAdmissionError{phase: "unavailable"}
	}
	if policy.operation <= 0 || policy.renew <= 0 || policy.cancelMargin <= policy.operation || policy.ttl <= policy.renew+policy.cancelMargin {
		return nil, false, &gatewaySharedAdmissionError{phase: "configuration invalid"}
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, false, &gatewaySharedAdmissionError{phase: "unavailable"}
	}
	token := hex.EncodeToString(nonce[:])
	key := gatewaySharedAdmissionKey(db.Namespace, account)
	guardCtx, cancelGuard := context.WithTimeout(ctx, policy.operation)
	unlockDrain, guardErr := p.lockGatewayAdmissionDrain(guardCtx, account)
	if guardErr != nil {
		cancelGuard()
		return nil, false, guardErr
	}
	defer cancelGuard()
	defer unlockDrain()
	drainState := ""
	if db.Pool != nil {
		// Under the shared account lock, an absent PG drain also proves that
		// this exact orphan fence is safe to remove. A command may arrive at
		// Redis after its caller times out and releases the PG lock: it MUST
		// NOT delete a newer drain token installed after that timeout.
		var err error
		drainState, err = db.Redis.WithTimeout(policy.operation).Get(guardCtx, accountDrainKey(db.Namespace, account)).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, false, &gatewaySharedAdmissionError{phase: "unavailable"}
		}
	}
	// WithTimeout shares the existing pool; never Close this clone. Lua is
	// idempotent for a token, including an acquire whose reply was lost. The
	// local expiry watchdog remains independent of a blocked Redis command.
	client := db.Redis.WithTimeout(policy.operation)
	cleanup := func() {
		operation, stop := context.WithTimeout(context.WithoutCancel(ctx), policy.operation)
		defer stop()
		_ = client.Eval(operation, gatewaySharedAdmissionRelease, []string{key}, token).Err()
	}
	started := time.Now()
	operation, stop := context.WithTimeout(guardCtx, policy.operation)
	admitted, err := client.Eval(operation, gatewaySharedAdmissionAcquire, []string{key, accountDrainKey(db.Namespace, account)}, token, account.effectiveConcurrency(), policy.ttl.Milliseconds(), drainState).Int()
	stop()
	unlockDrain()
	cancelGuard()
	if err != nil {
		cleanup() // Own-token only; TTL still bounds an ambiguous/unavailable cleanup.
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, &gatewaySharedAdmissionError{phase: "unavailable"}
	}
	if admitted == 0 {
		return nil, false, nil
	}
	if admitted == -2 {
		return nil, false, errAccountDraining
	}
	if admitted != 1 {
		cleanup()
		return nil, false, &gatewaySharedAdmissionError{phase: "unavailable"}
	}
	if ctx.Err() != nil {
		cleanup()
		return nil, false, ctx.Err()
	}
	if time.Since(started) >= policy.ttl-policy.cancelMargin {
		cleanup()
		return nil, false, &gatewaySharedAdmissionError{phase: "lease expired"}
	}
	// Redis is authoritative; local counters remain a process observation and
	// a conservative bound if an expired request has not unwound yet.
	localKey := gatewayAdmissionKey(account)
	if !p.acquire(localKey, account.effectiveConcurrency()) {
		cleanup()
		return nil, false, nil
	}
	leaseCtx, cancel := context.WithCancelCause(ctx)
	var stateMu sync.Mutex
	released := false
	failLease := func(cause error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		if released {
			return
		}
		cancel(cause)
		if route, _ := ctx.Value(gatewayRequestRouteKey{}).(*gatewayRequestRoute); route != nil && route.admissionCancel != nil {
			route.admissionCancel(cause)
		}
	}
	var deadlineMu sync.Mutex
	mustCancelAt := started.Add(policy.ttl - policy.cancelMargin)
	watchdog := time.NewTimer(max(time.Duration(0), time.Until(mustCancelAt)))
	expiryDone := make(chan struct{})
	go func() {
		defer close(expiryDone)
		defer watchdog.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-watchdog.C:
			}
			deadlineMu.Lock()
			if delay := time.Until(mustCancelAt); delay > 0 {
				watchdog.Reset(delay)
			} else {
				failLease(&gatewaySharedAdmissionError{phase: "lease expired"})
			}
			deadlineMu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer watchdog.Stop()
		ticker := time.NewTicker(policy.renew)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
			}
			began := time.Now()
			op, cancelOp := context.WithTimeout(leaseCtx, policy.operation)
			renewed, err := client.Eval(op, gatewaySharedAdmissionRenew, []string{key}, token, policy.ttl.Milliseconds()).Int()
			cancelOp()
			if leaseCtx.Err() != nil {
				return
			}
			if err != nil || renewed != 1 {
				failLease(&gatewaySharedAdmissionError{phase: "lease lost"})
				return
			}
			deadlineMu.Lock()
			if leaseCtx.Err() == nil {
				mustCancelAt = began.Add(policy.ttl - policy.cancelMargin)
				watchdog.Reset(max(time.Duration(0), time.Until(mustCancelAt)))
			}
			deadlineMu.Unlock()
		}
	}()
	var once sync.Once
	return &gatewayAccountAdmission{Context: leaseCtx, release: func() {
		once.Do(func() {
			stateMu.Lock()
			released = true
			cancel(context.Canceled)
			stateMu.Unlock()
			watchdog.Stop()
			<-done
			<-expiryDone
			cleanup()
			p.release(localKey)
		})
	}}, true, nil
}

// Proposed selection integration: return this error unchanged to the handler,
// never turn it into a busy candidate or errGatewayAccountCandidatesExhausted.
func gatewaySharedAdmissionFailure(err error) bool {
	var failure *gatewaySharedAdmissionError
	return errors.As(err, &failure)
}

// Internal admission loss is not a client disconnect or upstream rejection.
// Keep any observed terminal usage and already-delivered TTFT unchanged.
func observeGatewayAdmissionLoss(record *RequestRecord, ctx context.Context) {
	if record == nil || ctx == nil {
		return
	}
	if cause := context.Cause(ctx); gatewaySharedAdmissionFailure(cause) {
		record.Status, record.Error = "error", cause.Error()
		record.ErrorSource, record.ErrorPhase = "gateway", "account_admission"
	}
}
