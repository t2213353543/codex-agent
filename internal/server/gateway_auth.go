package server

import (
	"context"
	"errors"
	"net/http"
)

// recoverUnauthorized reloads or refreshes the account whose bearer token was
// rejected. Call only after an HTTP 401 and before writing any response bytes;
// the caller owns the single retry and must keep the same account and proxy.
func (p *Proxy) recoverUnauthorized(ctx context.Context, rejected Account, refreshClient *http.Client) (Account, error) {
	if err := ctx.Err(); err != nil {
		return rejected, err
	}
	settings, err := p.settingsValue()
	if err != nil {
		return rejected, err
	}
	if !settings.Upstream401RecoveryEnabled {
		return rejected, errors.New("upstream authentication recovery is disabled")
	}
	if p.Store.DB == nil {
		p.refreshMu.Lock()
		defer p.refreshMu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return rejected, err
	}
	ref := rejected.ID
	if ref == "" {
		ref = rejected.Name
	}
	wantIdentity := gatewayAccountRef(rejected)
	updated, err := p.Store.RefreshContext(ctx, ref, rejected.AccessToken, func(current Account) (Account, error) {
		if err := accountRefreshPreflight(ctx); err != nil {
			return current, err
		}
		if gatewayAccountRef(current) != wantIdentity {
			if gatewayAccountOwnerRef(current) == gatewayAccountOwnerRef(rejected) {
				return current, &gatewayOwnerChangedError{}
			}
			return current, errors.New("upstream account changed during authentication recovery")
		}
		if !current.Enabled {
			return current, errors.New("upstream account disabled during authentication recovery")
		}
		// An OAuth response may rotate only the refresh token. Store.Refresh's
		// access-token comparison cannot detect that successful concurrent
		// exchange, so do not spend the replacement RT on another stale 401.
		if current.RefreshToken != rejected.RefreshToken {
			return current, nil
		}
		return p.refreshAccountWithCooldown(ctx, refreshClient, current, settings)
	})
	// Expiry-based selection must see the newly persisted or concurrently
	// replaced snapshot immediately, including an opaque token with no expiry.
	p.invalidateAccounts()
	if err != nil {
		return rejected, err
	}
	// Store.Refresh may return a concurrent replacement without invoking our
	// callback. Never replay encrypted history under a different account.
	if gatewayAccountRef(updated) != wantIdentity {
		if gatewayAccountOwnerRef(updated) == gatewayAccountOwnerRef(rejected) {
			return rejected, &gatewayOwnerChangedError{}
		}
		return rejected, errors.New("upstream account changed during authentication recovery")
	}
	if !updated.Enabled {
		return rejected, errors.New("upstream account disabled during authentication recovery")
	}
	return updated, nil
}
