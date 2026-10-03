package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// Renaming a management row or rotating its credentials must not create a new
// concurrency budget while requests admitted under its former name are alive.
func gatewayAdmissionKey(account Account) string {
	if account.ID != "" {
		return "id:" + account.ID
	}
	if account.AccountID != "" {
		sum := sha256.Sum256([]byte(account.AccountID))
		return "upstream:" + hex.EncodeToString(sum[:])
	}
	return "name:" + account.Name
}

func (p *Proxy) acquireGatewayAccount(account Account) (func(), bool) {
	if p.Store.DB != nil {
		// Compatibility callers hold/release a slot only. Production dispatch
		// uses the context-aware API and propagates admission errors explicitly.
		lease, admitted, err := p.acquireGatewayAccountContext(context.Background(), account)
		if err != nil || !admitted {
			return nil, false
		}
		return lease.Release, true
	}
	key := gatewayAdmissionKey(account)
	if !p.acquire(key, account.effectiveConcurrency()) {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { p.release(key) }) }, true
}
