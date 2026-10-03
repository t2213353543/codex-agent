package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
)

// Affinity must check authoritative ownership, not the five-second scheduling
// cache. Otherwise a completed replacement can revive an old cached session.
func (p *Proxy) gatewayCurrentBoundAccount(account Account, expected string) (Account, error) {
	return p.gatewayCurrentBoundAccountContext(context.Background(), account, expected)
}

func (p *Proxy) gatewayCurrentBoundAccountContext(ctx context.Context, account Account, expected string) (Account, error) {
	current, err := p.gatewayCurrentSelectionAccount(ctx, account)
	// Storage sanitizes database errors. Preserve the caller's cancellation
	// without confusing it with an unavailable or replaced account.
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if err != nil {
		return Account{}, err
	}
	if gatewayAccountRef(current) != expected || !current.Enabled {
		p.invalidateAccounts()
		return Account{}, &gatewayOwnerChangedError{}
	}
	return current, nil
}

// The revision is a persisted, opaque incarnation, not a client-controlled
// credential field. Fresh random values also protect delete/legacy re-create.
func advanceGatewayOwnerRevision(previous *Account, updated *Account) error {
	if previous != nil && sameGatewayOwnerRevision(*previous, *updated) {
		updated.OwnerRevision = previous.OwnerRevision
		return nil
	}
	var revision [16]byte
	if _, err := rand.Read(revision[:]); err != nil {
		return errors.New("cannot create account ownership revision")
	}
	updated.OwnerRevision = hex.EncodeToString(revision[:])
	return nil
}

func sameGatewayOwnerRevision(previous, updated Account) bool {
	if gatewayAccountOwnerRef(previous) != gatewayAccountOwnerRef(updated) {
		return false
	}
	if gatewayCompleteOwner(previous) && gatewayCompleteOwner(updated) {
		return true
	}
	return previous.AccessToken == updated.AccessToken && previous.RefreshToken == updated.RefreshToken && previous.ClientID == updated.ClientID
}

func validGatewayOwnerRevision(value string) bool {
	if value == "" {
		return true
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16 && value == hex.EncodeToString(decoded)
}
