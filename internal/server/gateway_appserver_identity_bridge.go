package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const appServerClientIdentityPath = "/v1/user-auth-credential/whoami"
const appServerClientIdentitySession = "appserver-delegated-client-identity-v1"

type appServerDelegatedOwnerKey struct{}

// The official PAT metadata protocol delegates the gateway key to one actual
// account identity. No upstream token is returned or written on the client.
func (p *Proxy) gatewayAppServerClientIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		apiError(w, 405, "method not allowed")
		return
	}
	token := bearerToken(r)
	key, _, _, err := p.resolveGatewayKeyContext(r.Context(), token)
	if err != nil {
		apiError(w, 503, "gateway key authorization unavailable")
		return
	}
	if key == "" {
		apiError(w, 401, "invalid API key")
		return
	}
	ctx := withAppServerRequiredOwner(withGatewayRequestRoute(r.Context(), key, appServerClientIdentitySession))
	defer stopGatewayRequestRoute(ctx)
	account, identity, release, err := p.pickAppServerClientIdentity(ctx, token, key)
	if err != nil {
		var metadata *appServerIdentityMetadataError
		if errors.As(err, &metadata) {
			apiError(w, metadata.status, metadata.Error())
			return
		}
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	if err = p.authorizeGatewayRetry(ctx, token, key, account); err != nil {
		apiError(w, gatewayTransportFailureStatus(err), "account authorization changed")
		return
	}
	if owner, err := p.sessionAccount(ctx, key, appServerClientIdentitySession, ""); err != nil || owner != gatewayAccountRef(account) {
		apiError(w, http.StatusConflict, "delegated identity changed during hydration")
		return
	}
	// Email is optional in the official schema. Avoid disclosing an account's
	// personal address to downstream gateway clients who need only routing.
	writeJSON(w, identity)
}

type appServerIdentityMetadataError struct {
	status  int
	message string
}

func (e *appServerIdentityMetadataError) Error() string { return e.message }

func appServerClientIdentityMetadata(account Account) (map[string]any, error) {
	if account.Identity == nil || account.Identity.UserID == "" || account.AccountID == "" || account.Identity.PlanType == "" {
		return nil, &appServerIdentityMetadataError{503, "account identity metadata unavailable"}
	}
	if account.Identity.WorkspaceID != "" && account.Identity.WorkspaceID != account.AccountID {
		return nil, &appServerIdentityMetadataError{409, "account identity metadata conflicts with its route"}
	}
	fedramp := false
	var auth map[string]json.RawMessage
	_ = json.Unmarshal(managedIdentityJWT(account.AccessToken)["https://api.openai.com/auth"], &auth)
	if raw, ok := auth["chatgpt_account_is_fedramp"]; ok {
		if json.Unmarshal(raw, &fedramp) != nil {
			return nil, &appServerIdentityMetadataError{503, "invalid account residency metadata"}
		}
	}
	return map[string]any{"chatgpt_user_id": account.Identity.UserID, "chatgpt_account_id": account.AccountID, "chatgpt_plan_type": account.Identity.PlanType, "chatgpt_account_is_fedramp": fedramp}, nil
}

// Hydration binds only a validated identity. A failed first metadata read must
// not poison the key's login with an account the client could never identify.
// Explicit whoami may replace an unavailable former login using CAS; existing
// conversation bindings are untouched and reject a different owner afterwards.
func (p *Proxy) pickAppServerClientIdentity(ctx context.Context, token, key string) (Account, map[string]any, func(), error) {
	owner, err := p.sessionAccount(ctx, key, appServerClientIdentitySession, "")
	if err != nil {
		return Account{}, nil, nil, err
	}
	var metadataError error
	if owner != "" {
		account, release, selectErr := p.pickAppServerRealtimeOwner(ctx, token, key, owner)
		if selectErr == nil {
			identity, identityErr := appServerClientIdentityMetadata(account)
			if identityErr == nil {
				return account, identity, release, nil
			}
			metadataError = identityErr
			release()
		} else {
			var busy *gatewayAccountBusyError
			var cooldown *gatewayCooldownError
			if ctx.Err() != nil || gatewaySharedAdmissionFailure(selectErr) || errors.As(selectErr, &busy) || errors.As(selectErr, &cooldown) {
				return Account{}, nil, nil, selectErr
			}
		}
	}
	selection := withGatewayAccountFailover(ctx)
	for {
		account, release, actualKey, _, selectErr := p.pickAccount(selection, token)
		if selectErr != nil {
			if errors.Is(selectErr, errGatewayAccountCandidatesExhausted) && metadataError != nil {
				selectErr = metadataError
			}
			return Account{}, nil, nil, selectErr
		}
		if actualKey != key {
			release()
			return Account{}, nil, nil, errors.New("gateway key is no longer authorized")
		}
		identity, identityErr := appServerClientIdentityMetadata(account)
		if identityErr != nil {
			metadataError = identityErr
			release()
			continue
		}
		if err := p.authorizeGatewayRetry(ctx, token, key, account); err != nil {
			release()
			return Account{}, nil, nil, err
		}
		ref := gatewayAccountRef(account)
		actual, _, bindErr := p.switchSessionAccount(ctx, key, appServerClientIdentitySession, owner, ref)
		if bindErr != nil {
			release()
			return Account{}, nil, nil, bindErr
		}
		if actual != ref {
			release()
			return Account{}, nil, nil, errors.New("delegated identity changed during hydration")
		}
		return account, identity, release, nil
	}
}

func (p *Proxy) appServerBindDelegatedRequest(r *http.Request) (*http.Request, error) {
	claimed := r.Header.Get("ChatGPT-Account-Id")
	key, _, _, err := p.resolveGatewayKeyContext(r.Context(), bearerToken(r))
	if err != nil {
		return nil, err
	}
	if key == "" {
		return r, nil
	}
	owner, err := p.sessionAccount(r.Context(), key, appServerClientIdentitySession, "")
	if err != nil {
		return nil, err
	}
	// Ordinary provider clients have no delegated identity. Their historical
	// routing behavior is unchanged; the header alone grants no new authority.
	if owner == "" {
		return r, nil
	}
	accounts, _, _, err := p.candidatesForContext(r.Context(), bearerToken(r))
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if gatewayAccountRef(a) == owner {
			if claimed != "" && a.AccountID != claimed {
				return nil, errors.New("client account identity changed; reload delegated login")
			}
			ctx := context.WithValue(withAppServerRequiredOwner(r.Context()), appServerDelegatedOwnerKey{}, owner)
			return r.WithContext(ctx), nil
		}
	}
	return nil, errors.New("delegated client account is unavailable")
}

func appServerDelegatedOwner(ctx context.Context) string {
	owner, _ := ctx.Value(appServerDelegatedOwnerKey{}).(string)
	return owner
}

// Backend read endpoints belong to the selected delegated identity, never an
// arbitrary pool account. Paths are explicit; no supplied URL becomes a target.
func appServerBackendReadPath(path string) bool {
	switch path {
	case "/backend-api/wham/settings/user", "/backend-api/wham/config/bundle", "/backend-api/wham/accounts/check", "/backend-api/wham/profiles/me", "/backend-api/wham/usage", "/backend-api/wham/rate-limit-reset-credits", "/backend-api/wham/tasks/list",
		"/backend-api/wham/workspace-messages", "/backend-api/plugins/featured", "/backend-api/plugins/export/curated", "/backend-api/ps/plugins/suggested/codex", "/backend-api/ps/plugins/installed", "/backend-api/ps/plugins/list", "/backend-api/ps/plugins/search", "/backend-api/ps/plugins/workspace/shared", "/backend-api/ps/plugins/workspace/created", "/backend-api/connectors/directory/list", "/backend-api/connectors/directory/list_workspace":
		return true
	}
	return false
}

// Dynamic identifiers are still allowlisted by shape. They are never joined
// to an arbitrary client URL or forwarded as a host/path escape.
func appServerBackendDynamicPath(path string) (bool, string) {
	if strings.HasPrefix(path, "/backend-api/wham/tasks/") {
		parts := strings.Split(strings.TrimPrefix(path, "/backend-api/wham/tasks/"), "/")
		if len(parts) == 1 && validAppServerOpaqueID(parts[0]) {
			return true, http.MethodGet
		}
		if len(parts) == 4 && validAppServerOpaqueID(parts[0]) && parts[1] == "turns" && validAppServerOpaqueID(parts[2]) && parts[3] == "sibling_turns" {
			return true, http.MethodGet
		}
	}
	if strings.HasPrefix(path, "/backend-api/ps/plugins/") {
		parts := strings.Split(strings.TrimPrefix(path, "/backend-api/ps/plugins/"), "/")
		if len(parts) == 1 && validAppServerOpaqueID(parts[0]) {
			return true, http.MethodGet
		}
		if len(parts) == 2 && validAppServerOpaqueID(parts[0]) && (parts[1] == "install" || parts[1] == "uninstall") {
			return true, http.MethodPost
		}
		if len(parts) == 2 && validAppServerOpaqueID(parts[0]) && parts[1] == "shares" {
			return true, http.MethodPut
		}
	}
	if path == "/backend-api/public/plugins/workspace" || path == "/backend-api/public/plugins/workspace/upload-url" {
		return true, http.MethodPost
	}
	if strings.HasPrefix(path, "/backend-api/public/plugins/workspace/") {
		parts := strings.Split(strings.TrimPrefix(path, "/backend-api/public/plugins/workspace/"), "/")
		if len(parts) == 1 && validAppServerOpaqueID(parts[0]) {
			if strings.EqualFold(parts[0], "upload-url") {
				return false, ""
			}
			return true, http.MethodPost
		}
	}
	return false, ""
}

func validAppServerOpaqueID(value string) bool {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "/\\?#%") {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func appServerModelAlias(path string) string {
	if !strings.HasPrefix(path, "/backend-api/codex/") {
		return ""
	}
	tail := strings.TrimPrefix(path, "/backend-api/codex")
	if tail == "/responses" || tail == "/responses/compact" || tail == "/models" || tail == "/guardian" || tail == "/guardian-classifier" || appServerAuxiliaryPath(tail) || appServerHistoryPath(tail) || appServerRealtimePath("/v1"+tail) {
		return "/v1" + tail
	}
	return ""
}
