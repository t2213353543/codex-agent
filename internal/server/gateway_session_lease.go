package server

import (
	"context"
	"time"
)

// keepGatewaySession renews account affinity while an HTTP request is active.
// The shortest configurable session TTL is one minute; renew every 20 seconds
// so long-running generations do not lose their binding between requests.
// A routing-store failure or a conflicting owner cancels upstream work instead
// of allowing the request to continue on an account no longer bound to it.
func (p *Proxy) keepGatewaySession(ctx context.Context, keyID, session string, account Account) (context.Context, func()) {
	return p.keepGatewaySessionEvery(ctx, keyID, session, account, 20*time.Second)
}

func (p *Proxy) keepGatewaySessionEvery(ctx context.Context, keyID, session string, account Account, interval time.Duration) (context.Context, func()) {
	if session == "" {
		return ctx, func() {}
	}
	settings, err := p.settingsValue()
	if err == nil && !settings.StickySessionsEnabled && p.BPS == nil && !appServerRequiresOwner(ctx) {
		return ctx, func() {}
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	if err != nil {
		cancel()
		return leaseCtx, func() {}
	}
	expected := gatewayAccountRef(account)
	if bound, err := p.touchGatewaySession(leaseCtx, keyID, session, expected); err != nil || bound != expected {
		cancel()
		return leaseCtx, func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				bound, err := p.touchGatewaySession(leaseCtx, keyID, session, expected)
				if err != nil || bound != expected {
					cancel()
					return
				}
			}
		}
	}()
	return leaseCtx, func() {
		cancel()
		<-done
	}
}
