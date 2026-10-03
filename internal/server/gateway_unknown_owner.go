package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type gatewayOwnerChangedError struct{}

func (*gatewayOwnerChangedError) Error() string {
	return "upstream account ownership changed or cannot be verified after credential rotation; start a new thread"
}

// CLI 0.155 same_owner requires both the user and workspace. A stored user
// claim alone is not enough to carry a conversation through credential changes.
func gatewayCompleteOwner(account Account) bool {
	return strings.TrimSpace(account.AccountID) != "" && account.Identity != nil && strings.TrimSpace(account.Identity.UserID) != ""
}

// One deterministic, domain-separated digest works across processes, JSON and
// PostgreSQL without persisting a new secret or depending on metadata versions.
// The input is used transiently and never included in diagnostics or the ref.
func gatewayUnknownOwnerCredentials(account Account) string {
	raw, _ := json.Marshal([]string{"codex-agent/unknown-owner-credentials/v1", account.AccessToken, account.RefreshToken, account.ClientID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Existing legacy or credential-qualified bindings for the same static owner
// are never upgraded in place. The client must establish a new thread binding.
func gatewayUnknownBindingForAccount(bound string, account Account) bool {
	return gatewayPriorBindingForAccount(bound, account)
}

func gatewayPriorBindingForAccount(bound string, account Account) bool {
	owner := gatewayAccountOwnerRef(account)
	return bound == owner || strings.HasPrefix(bound, owner+":credentials:") || strings.HasPrefix(bound, owner+":revision:")
}
