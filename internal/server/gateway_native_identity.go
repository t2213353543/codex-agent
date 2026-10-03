package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

type gatewayNativeAccountScopeKey struct{}

// The selected Account, not caller-supplied JWT claims, defines continuity.
// Known owners survive credential refresh; unknown owners and owner revisions
// follow the existing fail-closed conversation ownership rules. Only an opaque
// digest is retained in the process pool, never bearer tokens or owner labels.
func withGatewayNativeAccountScope(ctx context.Context, account Account) context.Context {
	if account.ID == "" && account.Name == "" && account.AccountID == "" && account.AccessToken == "" && account.RefreshToken == "" && (account.Identity == nil || account.Identity.UserID == "") {
		return ctx
	}
	scope := sha256.Sum256([]byte("codex-agent/native-account-scope/v1\x00" + gatewayAccountRef(account)))
	return context.WithValue(ctx, gatewayNativeAccountScopeKey{}, scope)
}

// Callers outside managed Account dispatch still cannot share authenticated
// infrastructure cookies. Hash their exact authorization/workspace headers;
// do not infer a stable user from an unverified JWT. Truly anonymous requests
// keep the zero scope. HTTP and WS use this same boundary.
func gatewayNativeAccountScope(ctx context.Context, headers http.Header) [32]byte {
	if scope, ok := ctx.Value(gatewayNativeAccountScopeKey{}).([32]byte); ok {
		return scope
	}
	var names []string
	hasAuthorization := false
	for name, values := range headers {
		if strings.EqualFold(name, "Authorization") {
			for _, value := range values {
				hasAuthorization = hasAuthorization || value != ""
			}
		} else if !strings.EqualFold(name, "ChatGPT-Account-Id") {
			continue
		}
		names = append(names, name)
	}
	if !hasAuthorization {
		return [32]byte{}
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := strings.ToLower(names[i]), strings.ToLower(names[j])
		if left == right {
			return names[i] < names[j]
		}
		return left < right
	})
	values := []string{"codex-agent/native-credentials-scope/v1"}
	for _, name := range names {
		for _, value := range headers[name] {
			values = append(values, strings.ToLower(name), value)
		}
	}
	raw, _ := json.Marshal(values)
	return sha256.Sum256(raw)
}
