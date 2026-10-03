package server

import (
	"context"
	"errors"
	"fmt"
)

// This marker is reserved for cancellation before committing to an OAuth
// exchange. An actual exchange failure must still use the normal failure path.
var errAccountRefreshNotStarted = errors.New("credential refresh not started")

func accountRefreshPreflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", errAccountRefreshNotStarted, err)
	}
	return nil
}

// RefreshContext makes PostgreSQL refresh admission cancellable. Once the
// callback is admitted, storage must retain its independent commit budget so a
// rotated token can be saved even when the original caller leaves. File-store
// lock semantics remain unchanged; its callback still checks the caller.
func (s Store) RefreshContext(ctx context.Context, ref, previous string, refresh func(Account) (Account, error)) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if s.DB != nil {
		return s.pgRefreshContext(ctx, ref, previous, refresh)
	}
	return s.Refresh(ref, previous, refresh)
}
