package server

import "context"

// Carry only the persisted credential generation at the actual attempt boundary.
// Never resolve a mutable display name or read newer credentials while logging
// an older request that finished after a reauthorization.
type gatewayRequestCredentials struct {
	id, name string
	version  int64
}

type gatewayRequestCredentialsKey struct{}

func withGatewayRequestCredentials(ctx context.Context, account Account) context.Context {

	return context.WithValue(ctx, gatewayRequestCredentialsKey{}, gatewayRequestCredentials{
		id: account.ID, name: account.Name, version: account.CredentialVersion,
	})
}

func observeGatewayRequestCredentials(rec *RequestRecord, ctx context.Context) {
	if value, ok := ctx.Value(gatewayRequestCredentialsKey{}).(gatewayRequestCredentials); ok {
		rec.AccountID, rec.AccountNameSnapshot = value.id, value.name
		rec.AccountCredentialVersion = value.version
	}
}

func (t *gatewayWSTurns) observeConnectionCredentials(streamID string, account Account) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if queue := t.pending[streamID]; len(queue) > 0 {
		rec := &queue[len(queue)-1].rec
		rec.AccountID = account.ID
		rec.AccountCredentialVersion = account.CredentialVersion
	}
}
