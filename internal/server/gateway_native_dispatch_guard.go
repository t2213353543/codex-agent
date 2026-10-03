package server

import (
	"context"
	"errors"
)

type gatewayNativeDispatchGuardKey struct{}
type gatewayNativeDispatchGuard func(context.Context) error
type gatewayNativeDispatchAuthorizerKey struct{}
type gatewayNativeDispatchAuthorizer func(context.Context, Account) error

// Store caller authorization separately from the selected account. Each
// original/recovery attempt binds its current snapshot, and the transport's
// derived deadline governs the final authoritative storage reads.
func (p *Proxy) withGatewayNativeDispatchAuthorization(ctx context.Context, token, keyID string) context.Context {
	if p.nativeHTTPFor(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, gatewayNativeDispatchAuthorizerKey{}, gatewayNativeDispatchAuthorizer(func(dispatchCtx context.Context, account Account) error {
		return p.authorizeGatewayRetry(dispatchCtx, token, keyID, account)
	}))
}

// This marker is returned only by the final guard before any IPC head write.
// Preserve the typed authorization/ownership cause without claiming a send.
type gatewayNativeDispatchUnsentError struct{ cause error }

func (e *gatewayNativeDispatchUnsentError) Error() string { return e.cause.Error() }
func (e *gatewayNativeDispatchUnsentError) Unwrap() error { return e.cause }

// Account admission can precede native body buffering, cold helper startup and
// local IPC dialing. Carry the request's authoritative authorization check to
// the final pre-write boundary; never replace its account, bearer or route.
func withGatewayNativeDispatchGuard(ctx context.Context, guard gatewayNativeDispatchGuard) context.Context {
	return context.WithValue(ctx, gatewayNativeDispatchGuardKey{}, guard)
}

func checkGatewayNativeDispatch(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	guard, _ := ctx.Value(gatewayNativeDispatchGuardKey{}).(gatewayNativeDispatchGuard)
	if guard == nil {
		return nil
	}
	err := guard(ctx)
	if contextErr := ctx.Err(); contextErr != nil {
		return &gatewayNativeDispatchUnsentError{cause: contextErr}
	}
	var authorization *gatewayRecoveryAuthorizationError
	if errors.As(err, &authorization) {
		err = &gatewayRecoveryAuthorizationError{status: authorization.status, beforeNativeDispatch: true}
	}
	if err != nil {
		return &gatewayNativeDispatchUnsentError{cause: err}
	}
	return nil
}
