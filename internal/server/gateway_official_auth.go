package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type officialAuthRecoveryKey struct{}
type officialAuthRecovery func(context.Context, Account) (Account, error)
type officialAuthCurrentKey struct{}
type officialAuthCurrent func(context.Context, Account) (Account, error)

func officialAuthenticationUnavailable(err error) error {
	return &officialProtocolError{message: "official authentication unavailable: " + boundedGatewayMetadata(err.Error(), 1024), status: 401, code: "authentication_unavailable", source: "upstream", cause: err}
}

func (p *Proxy) officialAuthContext(ctx context.Context, token, key string) context.Context {
	callback := officialAuthRecovery(func(ctx context.Context, rejected Account) (Account, error) {
		if err := p.authorizeGatewayRetry(ctx, token, key, rejected); err != nil {
			return rejected, err
		}
		proxy, err := p.gatewayRequestProxy(ctx, rejected)
		if err != nil {
			return rejected, err
		}
		updated, err := p.recoverUnauthorized(ctx, rejected, p.gatewayRequestClient(ctx, proxy, gatewayClientHTTP))
		if err != nil {
			return rejected, err
		}
		if err := p.authorizeGatewayRetry(ctx, token, key, updated); err != nil {
			return rejected, err
		}
		return updated, nil
	})
	current := officialAuthCurrent(func(ctx context.Context, original Account) (Account, error) {
		if err := p.authorizeGatewayRetry(ctx, token, key, original); err != nil {
			return original, err
		}
		return p.gatewayCurrentSelectionAccount(ctx, original)
	})
	return context.WithValue(context.WithValue(ctx, officialAuthRecoveryKey{}, callback), officialAuthCurrentKey{}, current)
}

func (p *officialRPC) recoverAuth(ctx context.Context) (Account, error) {
	p.authMu.Lock()
	defer p.authMu.Unlock()
	callback, ok := ctx.Value(officialAuthRecoveryKey{}).(officialAuthRecovery)
	if !ok {
		return p.authAccount, errors.New("official authentication refresh is not configured")
	}
	updated, err := callback(ctx, p.authAccount)
	if err != nil {
		return p.authAccount, err
	}
	if updated.AccountID != p.authAccount.AccountID || gatewayAccountRef(updated) != gatewayAccountRef(p.authAccount) || updated.AccessToken == "" {
		return p.authAccount, errors.New("official authentication refresh changed account identity")
	}
	p.authAccount = updated
	return updated, nil
}

func (p *officialRPC) setAuthContext(ctx context.Context) {
	if _, ok := ctx.Value(officialAuthRecoveryKey{}).(officialAuthRecovery); !ok {
		return
	}
	p.mu.Lock()
	p.authContext = ctx
	p.mu.Unlock()
}

// The official process can request external-token renewal while another RPC
// (including initial model/list) is still outstanding. Handle this on the read
// boundary so it never waits for a tool/event consumer or leaks to a WS client.
func (p *officialRPC) handleAuthRequest(raw json.RawMessage) bool {
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Reason   string `json:"reason"`
			Previous string `json:"previousAccountId"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &message) != nil || message.Method != "account/chatgptAuthTokens/refresh" || len(message.ID) == 0 {
		return false
	}
	p.mu.Lock()
	active := p.authContext
	p.mu.Unlock()
	received := time.Now()
	go func() {
		p.authMu.Lock()
		defer p.authMu.Unlock()
		failure := func(err error) {
			p.mu.Lock()
			p.authFailure = officialAuthenticationUnavailable(err)
			p.mu.Unlock()
			_ = p.send(map[string]any{"id": message.ID, "error": map[string]any{"code": -32000, "message": boundedGatewayMetadata(err.Error(), 1024)}})
			select {
			case p.wake <- struct{}{}:
			default:
			}
		}
		if message.Params.Reason != "unauthorized" {
			failure(errors.New("unsupported official authentication refresh reason"))
			return
		}
		if active == nil {
			failure(errors.New("official authentication refresh is not configured"))
			return
		}
		callback, ok := active.Value(officialAuthRecoveryKey{}).(officialAuthRecovery)
		if !ok {
			failure(errors.New("official authentication refresh is not configured"))
			return
		}
		account := p.authAccount
		if message.Params.Previous != "" && message.Params.Previous != account.AccountID {
			failure(errors.New("official authentication refresh account mismatch"))
			return
		}
		ctx, stop := context.WithDeadline(active, received.Add(9*time.Second))
		defer stop()
		if err := ctx.Err(); err != nil {
			failure(err)
			return
		}
		updated, err := callback(ctx, account)
		if err != nil {
			failure(err)
			return
		}
		if err := ctx.Err(); err != nil {
			failure(err)
			return
		}
		if updated.AccountID != account.AccountID || gatewayAccountRef(updated) != gatewayAccountRef(account) || updated.AccessToken == "" {
			failure(errors.New("official authentication refresh changed account identity"))
			return
		}
		p.authAccount = updated
		p.mu.Lock()
		p.authFailure = nil
		p.mu.Unlock()
		_ = p.send(map[string]any{"id": message.ID, "result": map[string]any{"accessToken": updated.AccessToken, "chatgptAccountId": updated.AccountID, "chatgptPlanType": nil}})
	}()
	return true
}
