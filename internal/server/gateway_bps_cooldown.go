package server

import (
	"context"
	"errors"
)

// Only a definite account/model cooldown permits trying another owner. Shared
// store errors and cancellation fail closed at this local pre-dispatch gate.
func bpsResolvedCooldownFailure(ctx context.Context, account Account, err error) error {
	var limited *gatewayCooldownError
	if errors.As(err, &limited) {
		return gatewayBoundAccountUnavailable(ctx, account, gatewayAccountRef(account), err)
	}
	return err
}
