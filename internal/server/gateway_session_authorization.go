package server

import (
	"context"
	"errors"
)

var errGatewayAccountNotAllowed = errors.New("session account is no longer enabled or allowed for this key")

// Recheck the freshly loaded account against this instance's current key
// authorization. Scheduling-cache membership is not an authorization decision.
func (p *Proxy) checkGatewayBoundAccountAccess(ctx context.Context, token, keyID string, account Account) error {
	return p.checkGatewayAccountAccess(ctx, token, keyID, account)
}

func (p *Proxy) checkGatewayAccountAccess(ctx context.Context, token, keyID string, account Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	currentKeyID, _, key, err := p.resolveGatewayKeyContext(ctx, token)
	if err != nil {
		return err
	}
	if currentKeyID == "" || currentKeyID != keyID {
		return errors.New("gateway key is no longer authorized")
	}
	if !account.Enabled {
		return errGatewayAccountNotAllowed
	}
	if keyID == "admin" {
		return nil
	}
	if key == nil || key.ID != keyID {
		return errors.New("gateway key is no longer authorized")
	}
	for _, allowed := range key.Groups {
		for _, group := range account.Groups {
			if allowed == group {
				return nil
			}
		}
	}
	return errGatewayAccountNotAllowed
}

// Unbound selection may use the current owner. Bound sessions instead use
// gatewayCurrentBoundAccount and must still match their existing owner ref.
func (p *Proxy) gatewayCurrentSelectionAccount(ctx context.Context, account Account) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if p.Store.DB != nil {
		ref := account.ID
		if ref == "" {
			ref = account.Name
		}
		current, err := p.Store.pgGetContext(ctx, ref, true)
		if err == nil && account.ID != "" && current.ID != account.ID {
			return Account{}, errNotFound
		}
		return current, err
	}
	if account.ID != "" {
		return p.Store.getAccountByID(account.ID)
	}
	return p.Store.Get(account.Name)
}
