package server

import (
	"context"
	"errors"
	"net/http"
)

type gatewayRecoveryAuthorizationError struct {
	status               int
	beforeNativeDispatch bool
}

func (e *gatewayRecoveryAuthorizationError) Error() string {
	if e.beforeNativeDispatch {
		if e.status == http.StatusForbidden {
			return "gateway request is no longer authorized before native dispatch"
		}
		return "gateway authorization unavailable before native dispatch"
	}
	if e.status == http.StatusForbidden {
		return "gateway request is no longer authorized after authentication recovery"
	}
	return "gateway authorization unavailable after authentication recovery"
}

// Recovery persists rotating credentials independently. This later checkpoint
// only decides whether the original caller may retry on that same account.
func (p *Proxy) authorizeGatewayRetry(ctx context.Context, token, keyID string, recovered Account) error {
	current, err := p.gatewayCurrentSelectionAccount(ctx, recovered)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errNotFound) {
		return &gatewayRecoveryAuthorizationError{status: http.StatusForbidden}
	}
	if err != nil {
		return &gatewayRecoveryAuthorizationError{status: http.StatusServiceUnavailable}
	}
	if gatewayAccountRef(current) != gatewayAccountRef(recovered) {
		return &gatewayOwnerChangedError{}
	}
	err = p.checkGatewayAccountAccess(ctx, token, keyID, current)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errGatewayKeyStoreUnavailable) {
		return &gatewayRecoveryAuthorizationError{status: http.StatusServiceUnavailable}
	}
	if err != nil {
		return &gatewayRecoveryAuthorizationError{status: http.StatusForbidden}
	}
	if err := p.checkGatewayPayment(current); err != nil {
		return gatewayBoundAccountUnavailable(ctx, current, gatewayAccountRef(current), err)
	}
	return nil
}
