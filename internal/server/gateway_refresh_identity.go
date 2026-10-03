package server

import (
	"encoding/json"
	"strings"
	"time"
)

func gatewayRefreshIdentityPresent(raw json.RawMessage) bool {
	return len(raw) != 0 && strings.TrimSpace(string(raw)) != "null"
}

// A successful OAuth response may already have rotated RT. Identity metadata
// cannot turn that success into an error that discards the replacement tokens.
// Rebuild ownership from this credential snapshot instead; an unknown or changed
// owner advances the stored revision and rejects the old session after commit.
func rebuildGatewayRefreshIdentity(account Account, idToken json.RawMessage) Account {
	unknown := &managedAccountIdentity{
		WorkspaceID: account.AccountID, Source: "oauth_refresh_identity_unavailable", CollectedAt: time.Now().Unix(),
	}
	credentials := map[string]json.RawMessage{}
	if gatewayRefreshIdentityPresent(idToken) {
		var token string
		if json.Unmarshal(idToken, &token) != nil || managedIdentityJWT(token) == nil {
			account.Identity = unknown
			return account
		}
		// An explicitly returned but unusable ID token must not be silently
		// ignored in favor of a retained or newly returned access token.
		identity, err := parseManagedIdentity(nil, nil, Account{AccessToken: token, AccountID: account.AccountID})
		if err != nil || identity == nil || strings.TrimSpace(identity.UserID) == "" {
			account.Identity = unknown
			return account
		}
		credentials["id_token"] = idToken
	}
	// Do not seed this parse with account.Identity: that is the prior token's
	// metadata, may be verified, and must not conceal changed JWT user claims.
	identity, err := parseManagedIdentity(nil, credentials, Account{AccessToken: account.AccessToken, AccountID: account.AccountID})
	if err != nil || identity == nil || strings.TrimSpace(identity.UserID) == "" {
		if err != nil {
			unknown.Source = "oauth_refresh_identity_conflict"
		}
		account.Identity = unknown
		return account
	}
	identity.Verified = false
	account.Identity = identity
	if account.AccountID == "" {
		account.AccountID = identity.WorkspaceID
	}
	return account
}
