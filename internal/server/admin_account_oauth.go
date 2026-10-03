package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const managedOAuthRedirect = "http://localhost:1455/auth/callback"
const managedOAuthNamespace = "account-oauth"

type managedOAuthSession struct {
	Owner          string          `json:"owner"`
	Method         string          `json:"method"`
	Phase          string          `json:"phase"`
	State          string          `json:"state,omitempty"`
	Verifier       string          `json:"verifier,omitempty"`
	ProxyID        string          `json:"proxy_id,omitempty"`
	TargetID       string          `json:"target_id,omitempty"`
	TargetVersion  int64           `json:"target_version,omitempty"`
	Credentials    json.RawMessage `json:"credentials,omitempty"`
	AppliedID      string          `json:"applied_id,omitempty"`
	AppliedVersion int64           `json:"applied_version,omitempty"`
}
type managedOAuthDestination struct {
	ProxyID  string `json:"proxy_id,omitempty"`
	TargetID string `json:"target_id,omitempty"`
	Version  int64  `json:"version,omitempty"`
}

func managedOAuthOwner(r *http.Request) string {
	auth, _ := r.Context().Value(adminAuthenticationContextKey{}).(adminAuthentication)
	if auth.CookieToken != "" {
		return adminKeyHash("cookie:" + auth.CookieToken)
	}
	if auth.BearerHash != "" {
		return "bearer:" + auth.BearerHash
	}
	return ""
}
func managedOAuthRandom() string {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(value[:])
}

func (p *Proxy) validateManagedOAuthDestination(input managedOAuthDestination) error {
	if input.TargetID != "" {
		if input.Version < 1 {
			return errInvalid
		}
		a, err := p.Store.GetMetadata(input.TargetID)
		if err != nil {
			return err
		}
		if a.ID != input.TargetID {
			return errNotFound
		}
		if a.Version != input.Version {
			return errVersionConflict
		}
	} else if input.Version != 0 {
		return errInvalid
	}
	if input.ProxyID != "" {
		entry, ok := p.Proxies.Get(input.ProxyID)
		if !ok {
			return errNotFound
		}
		if !entry.Enabled {
			return errInvalid
		}
	}
	return nil
}
func decodeManagedOAuth(entry *managementStateEntry, owner string) (managedOAuthSession, error) {
	var state managedOAuthSession
	if entry.Version == 0 || entry.ExpiresAt <= time.Now().Unix() {
		return state, errNotFound
	}
	if err := json.Unmarshal(entry.Payload, &state); err != nil {
		return state, err
	}
	if owner == "" || subtle.ConstantTimeCompare([]byte(state.Owner), []byte(owner)) != 1 {
		return state, errNotFound
	}
	return state, nil
}
func (p *Proxy) newManagedOAuth(ctx context.Context, owner, method string, destination managedOAuthDestination) (string, managementStateEntry, managedOAuthSession, error) {
	var entry managementStateEntry
	state := managedOAuthSession{Owner: owner, Method: method, Phase: "pending", ProxyID: destination.ProxyID, TargetID: destination.TargetID, TargetVersion: destination.Version}
	if owner == "" {
		return "", entry, state, errInvalid
	}
	if err := p.validateManagedOAuthDestination(destination); err != nil {
		return "", entry, state, err
	}
	if method == "oauth" {
		state.State = managedOAuthRandom()
		state.Verifier = managedOAuthRandom()
	}
	id := managedOAuthRandom()
	err := p.managementState().Update(ctx, managedOAuthNamespace, id, func(row *managementStateEntry) error {
		if row.Version != 0 {
			return errConflict
		}
		raw, err := json.Marshal(state)
		row.Payload = raw
		row.ExpiresAt = time.Now().Add(30 * time.Minute).Unix()
		entry = *row
		return err
	})
	return id, entry, state, err
}
func (p *Proxy) oauthEndpoint(path string) (string, error) {
	endpoint := p.TokenURL
	if endpoint == "" {
		endpoint = oauthURL
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errInvalid
	}
	u.Path = path
	u.RawPath = ""
	return u.String(), nil
}

func (p *Proxy) handleManagedOAuthStart(w http.ResponseWriter, r *http.Request) {
	var input managedOAuthDestination
	if err := readJSONLimit(w, r, &input, 16<<10); err != nil {
		adminError(w, err)
		return
	}
	endpoint, err := p.oauthEndpoint("/oauth/authorize")
	if err != nil {
		p.storeError(w, err)
		return
	}
	id, entry, state, err := p.newManagedOAuth(r.Context(), managedOAuthOwner(r), "oauth", input)
	if err != nil {
		p.storeError(w, err)
		return
	}
	challenge := sha256.Sum256([]byte(state.Verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {oauthClientID}, "redirect_uri": {managedOAuthRedirect}, "scope": {"openid profile email offline_access api.connectors.read api.connectors.invoke"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "state": {state.State}, "originator": {clientOriginator}}
	writeJSON(w, map[string]any{"session_id": id, "phase": "pending", "auth_url": endpoint + "?" + q.Encode(), "redirect_uri": managedOAuthRedirect, "expires_at": entry.ExpiresAt})
}

func managedOAuthCallback(code, state string) (string, string, error) {
	code = strings.TrimSpace(code)
	if strings.Contains(code, "://") {
		u, err := url.Parse(code)
		if err != nil || u.Scheme != "http" || u.Host != "localhost:1455" || u.Path != "/auth/callback" || u.User != nil || u.Fragment != "" {
			return "", "", errInvalid
		}
		q := u.Query()
		if len(q["code"]) != 1 || len(q["state"]) != 1 || q.Get("error") != "" {
			return "", "", errInvalid
		}
		if state != "" && state != q.Get("state") {
			return "", "", errInvalid
		}
		state = q.Get("state")
		code = q.Get("code")
	}
	if code == "" || len(code) > 8192 || state == "" || len(state) > 256 {
		return "", "", errInvalid
	}
	return code, state, nil
}

func (p *Proxy) claimManagedOAuth(ctx context.Context, id, owner, method, stateValue string) (managedOAuthSession, error) {
	var claimed managedOAuthSession
	err := p.managementState().Update(ctx, managedOAuthNamespace, id, func(entry *managementStateEntry) error {
		state, err := decodeManagedOAuth(entry, owner)
		if err != nil {
			return err
		}
		if state.Method != method || state.Phase != "pending" {
			return errConflict
		}
		if method == "oauth" && subtle.ConstantTimeCompare([]byte(state.State), []byte(stateValue)) != 1 {
			return errInvalid
		}
		state.Phase = "exchanging"
		claimed = state
		entry.Payload, err = json.Marshal(state)
		return err
	})
	return claimed, err
}

func canonicalManagedCredentials(a Account) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"access_token": a.AccessToken, "refresh_token": a.RefreshToken, "account_id": a.AccountID, "client_id": a.ClientID, "expires_at": a.ExpiresAt, "identity": a.Identity})
	return raw
}
func (p *Proxy) finishManagedOAuth(ctx context.Context, id, owner string, a Account, exchangeErr error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return p.managementState().Update(ctx, managedOAuthNamespace, id, func(entry *managementStateEntry) error {
		state, err := decodeManagedOAuth(entry, owner)
		if err != nil {
			return err
		}
		if state.Phase != "exchanging" {
			return errConflict
		}
		state.State = ""
		state.Verifier = ""
		state.Credentials = nil
		if exchangeErr != nil {
			state.Phase = "failed"
		} else {
			state.Phase = "ready"
			state.Credentials = canonicalManagedCredentials(a)
		}
		entry.Payload, err = json.Marshal(state)
		return err
	})
}

func (p *Proxy) managedOAuthRequest(ctx context.Context, state managedOAuthSession, body io.Reader, contentType string) (Account, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	account := Account{}
	if state.ProxyID != "" {
		account.Proxies = []string{state.ProxyID}
	} else if state.TargetID != "" {
		a, err := p.Store.GetMetadata(state.TargetID)
		if err != nil {
			return Account{}, err
		}
		account.Proxies = a.Proxies
	}
	client, err := p.managedAccountClient(account)
	if err != nil {
		return Account{}, errors.New("configured OAuth proxy is unavailable")
	}
	endpoint := p.TokenURL
	if endpoint == "" {
		endpoint = oauthURL
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return Account{}, errInvalid
	}
	settings, err := p.settingsValue()
	if err != nil {
		return Account{}, errors.New("cannot load OAuth request settings")
	}
	request = request.WithContext(withGatewayNativeAccountScope(request.Context(), account))
	// This private helper has exactly two callers: authorization-code form
	// exchange (official raw auth client) and JSON refresh-token import.
	if err = applyGatewayOAuthHeaders(request.Header, settings, contentType == "application/x-www-form-urlencoded"); err != nil {
		return Account{}, errors.New("cannot load OAuth request settings")
	}
	request.Header.Set("Content-Type", contentType)
	response, err := client.Do(request)
	if err != nil {
		return Account{}, errors.New("OAuth exchange connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return Account{}, fmt.Errorf("OAuth exchange returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return Account{}, errors.New("invalid OAuth response")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return Account{}, errors.New("invalid OAuth response")
	}
	if expiry, ok := object["expires_in"]; ok {
		var seconds int64
		if json.Unmarshal(expiry, &seconds) != nil || seconds < 0 || seconds > 315360000 {
			return Account{}, errors.New("invalid OAuth expiry")
		}
		if seconds > 0 {
			object["expires_at"], _ = json.Marshal(time.Now().Unix() + seconds)
		}
	}
	raw, _ = json.Marshal(object)
	a, err := parseAccount("oauth", raw)
	if err != nil {
		return Account{}, errors.New("OAuth response has no usable access token")
	}
	return a, nil
}

func (p *Proxy) handleManagedOAuthExchange(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
	}
	if err := readJSONLimit(w, r, &input, 32<<10); err != nil {
		adminError(w, err)
		return
	}
	code, stateValue, err := managedOAuthCallback(input.Code, input.State)
	if err != nil {
		adminError(w, errors.New("supply the authorization code and matching state or the full localhost callback URL"))
		return
	}
	owner := managedOAuthOwner(r)
	state, err := p.claimManagedOAuth(r.Context(), input.SessionID, owner, "oauth", stateValue)
	if err != nil {
		p.storeError(w, err)
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {oauthClientID}, "code": {code}, "redirect_uri": {managedOAuthRedirect}, "code_verifier": {state.Verifier}}
	a, exchangeErr := p.managedOAuthRequest(r.Context(), state, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	a.ClientID = oauthClientID
	if err := p.finishManagedOAuth(r.Context(), input.SessionID, owner, a, exchangeErr); err != nil {
		p.storeError(w, err)
		return
	}
	if exchangeErr != nil {
		adminAPIError(w, 502, "oauth_exchange_failed", exchangeErr.Error())
		return
	}
	p.writeManagedOAuthStatus(w, r, input.SessionID)
}

func parseManagedSessionCredentials(raw json.RawMessage) (Account, error) {
	object, err := accountObject(raw)
	if err != nil {
		return Account{}, errInvalid
	}
	root := object
	if nested := object["tokens"]; len(nested) > 0 {
		object, err = accountObject(nested)
		if err != nil {
			return Account{}, errInvalid
		}
	}
	for from, to := range map[string]string{"accessToken": "access_token", "refreshToken": "refresh_token", "idToken": "id_token", "accountId": "account_id", "chatgptAccountId": "chatgpt_account_id", "clientId": "client_id", "expiresAt": "expires_at"} {
		if object[to] == nil && object[from] != nil {
			object[to] = object[from]
		}
	}
	if len(root["agent_identity"]) > 0 {
		object["agent_identity"] = root["agent_identity"]
	}
	if object["account_id"] == nil {
		var account map[string]json.RawMessage
		if json.Unmarshal(root["account"], &account) == nil {
			object["account_id"] = account["id"]
		}
	}
	// Session cookies and the browser session expiry are not OAuth credentials.
	for _, key := range []string{"session_token", "sessionToken", "expires"} {
		delete(object, key)
	}
	encoded, _ := json.Marshal(object)
	a, err := parseAccount("session", encoded)
	if err != nil {
		return Account{}, errors.New("session JSON must contain a usable access_token or accessToken")
	}
	return a, nil
}

func (p *Proxy) managedPATAccount(ctx context.Context, state managedOAuthSession, token string) (Account, error) {
	if !strings.HasPrefix(token, "at-") || len(token) > 128<<10 || strings.ContainsAny(token, " \r\n\t") {
		return Account{}, errors.New("Codex PAT must start with at- and contain no whitespace")
	}
	a := Account{AccessToken: token}
	if state.ProxyID != "" {
		a.Proxies = []string{state.ProxyID}
	} else if state.TargetID != "" {
		existing, err := p.Store.GetMetadata(state.TargetID)
		if err != nil {
			return Account{}, err
		}
		a.Proxies = existing.Proxies
	}
	client, err := p.managedAccountClient(a)
	if err != nil {
		return Account{}, errors.New("configured PAT proxy is unavailable")
	}
	endpoint, err := p.oauthEndpoint("/api/accounts/v1/user-auth-credential/whoami")
	if err != nil {
		return Account{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err = p.managedUpstreamHeaders(request, a); err != nil {
		return Account{}, errors.New("cannot load PAT request settings")
	}
	response, err := client.Do(request)
	if err != nil {
		return Account{}, errors.New("PAT validation connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return Account{}, fmt.Errorf("PAT validation returned HTTP %d", response.StatusCode)
	}
	var whoami struct {
		AccountID string `json:"chatgpt_account_id"`
		UserID    string `json:"chatgpt_user_id"`
		PlanType  string `json:"chatgpt_plan_type"`
		FedRAMP   *bool  `json:"chatgpt_account_is_fedramp"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&whoami) != nil || whoami.AccountID == "" || whoami.UserID == "" || whoami.PlanType == "" || whoami.FedRAMP == nil {
		return Account{}, errors.New("invalid PAT identity response")
	}
	if *whoami.FedRAMP {
		return Account{}, errors.New("this gateway does not support the account's FedRAMP routing")
	}
	a.AccountID = whoami.AccountID
	return a, nil
}

func (p *Proxy) handleManagedOAuthImport(w http.ResponseWriter, r *http.Request) {
	var input struct {
		managedOAuthDestination
		Method       string          `json:"method"`
		RefreshToken string          `json:"refresh_token"`
		ClientID     string          `json:"client_id"`
		AccessToken  string          `json:"access_token"`
		Credentials  json.RawMessage `json:"credentials"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	if !accountHas([]string{"refresh_token", "mobile_refresh_token", "session", "pat"}, input.Method) {
		adminError(w, errors.New("unsupported Codex authorization import method"))
		return
	}
	var incoming Account
	var err error
	if input.Method == "session" {
		incoming, err = parseManagedSessionCredentials(input.Credentials)
	}
	if input.Method == "refresh_token" || input.Method == "mobile_refresh_token" {
		if input.RefreshToken == "" || len(input.RefreshToken) > 128<<10 || strings.ContainsAny(input.RefreshToken, "\r\n") {
			err = errors.New("a refresh token is required")
		}
		if input.Method == "mobile_refresh_token" && input.ClientID == "" {
			err = errors.New("mobile refresh tokens require their original client_id")
		}
		if len(input.ClientID) > 256 || strings.ContainsAny(input.ClientID, "\r\n") {
			err = errInvalid
		}
	}
	if input.Method == "pat" && (!strings.HasPrefix(input.AccessToken, "at-") || len(input.AccessToken) > 128<<10) {
		err = errors.New("a Codex at- personal access token is required")
	}
	if err != nil {
		adminError(w, err)
		return
	}
	owner := managedOAuthOwner(r)
	id, _, _, err := p.newManagedOAuth(r.Context(), owner, input.Method, input.managedOAuthDestination)
	if err != nil {
		p.storeError(w, err)
		return
	}
	state, err := p.claimManagedOAuth(r.Context(), id, owner, input.Method, "")
	if err != nil {
		p.storeError(w, err)
		return
	}
	if input.Method == "refresh_token" || input.Method == "mobile_refresh_token" {
		clientID := input.ClientID
		if clientID == "" {
			clientID = oauthClientID
		}
		payload, _ := json.Marshal(map[string]string{"grant_type": "refresh_token", "refresh_token": input.RefreshToken, "client_id": clientID})
		incoming, err = p.managedOAuthRequest(r.Context(), state, bytes.NewReader(payload), "application/json")
		if incoming.RefreshToken == "" {
			incoming.RefreshToken = input.RefreshToken
		}
		incoming.ClientID = clientID
	} else if input.Method == "pat" {
		incoming, err = p.managedPATAccount(r.Context(), state, input.AccessToken)
	}
	if saveErr := p.finishManagedOAuth(r.Context(), id, owner, incoming, err); saveErr != nil {
		p.storeError(w, saveErr)
		return
	}
	if err != nil {
		adminAPIError(w, 502, "oauth_import_failed", err.Error())
		return
	}
	p.writeManagedOAuthStatus(w, r, id)
}

func (p *Proxy) handleManagedOAuthGet(w http.ResponseWriter, r *http.Request) {
	p.writeManagedOAuthStatus(w, r, r.PathValue("id"))
}
func (p *Proxy) writeManagedOAuthStatus(w http.ResponseWriter, r *http.Request, id string) {
	entry, err := p.managementState().Get(r.Context(), managedOAuthNamespace, id)
	if err != nil {
		p.storeError(w, err)
		return
	}
	state, err := decodeManagedOAuth(&entry, managedOAuthOwner(r))
	if err != nil {
		p.storeError(w, err)
		return
	}
	result := map[string]any{"session_id": id, "phase": state.Phase, "method": state.Method, "expires_at": entry.ExpiresAt, "target_id": state.TargetID, "has_refresh_token": false, "expires_at_token": int64(0), "identity_change": false}
	if state.Phase == "ready" {
		a, err := parseAccount("staged", state.Credentials)
		if err != nil {
			p.storeError(w, err)
			return
		}
		result["has_refresh_token"] = a.RefreshToken != ""
		result["expires_at_token"] = a.ExpiresAt
		if state.TargetID != "" {
			current, err := p.Store.Get(state.TargetID)
			if err != nil {
				p.storeError(w, err)
				return
			}
			result["identity_change"] = managedIdentityChanged(current, a)
			result["target_changed"] = current.Version != state.TargetVersion
		}
	}
	if state.Phase == "applied" {
		result["id"] = state.AppliedID
		result["version"] = state.AppliedVersion
	}
	writeJSON(w, result)
}

func (p *Proxy) handleManagedOAuthCancel(w http.ResponseWriter, r *http.Request) {
	err := p.managementState().Update(r.Context(), managedOAuthNamespace, r.PathValue("id"), func(entry *managementStateEntry) error {
		state, err := decodeManagedOAuth(entry, managedOAuthOwner(r))
		if err != nil {
			return err
		}
		if state.Phase == "exchanging" {
			return errConflict
		}
		state.Phase = "canceled"
		state.Credentials = nil
		state.Verifier = ""
		state.State = ""
		entry.Payload, err = json.Marshal(state)
		return err
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

type managedOAuthApplyInput struct {
	SessionID           string          `json:"session_id"`
	Name                string          `json:"name"`
	Defaults            AccountDefaults `json:"defaults"`
	AllowIdentityChange bool            `json:"allow_identity_change"`
	Notes               *string         `json:"notes"`
	Tags                *[]string       `json:"tags"`
	Enabled             *bool           `json:"enabled"`
}

func (p *Proxy) handleManagedOAuthApply(w http.ResponseWriter, r *http.Request) {
	var input managedOAuthApplyInput
	if err := readJSONLimit(w, r, &input, 32<<10); err != nil {
		adminError(w, err)
		return
	}
	result, err := p.applyManagedOAuth(r.Context(), managedOAuthOwner(r), input)
	if errors.Is(err, errManagedIdentityChange) {
		adminAPIError(w, 409, "identity_change_requires_confirmation", err.Error())
		return
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	p.invalidateAccounts()
	writeJSON(w, map[string]any{"ok": true, "id": result.ID, "version": result.Version})
}

// applyManagedOAuth commits account credentials and the staged-session
// acknowledgement together. In PostgreSQL every read/write in the callback
// uses the one transaction; even a one-connection pool can make progress.
func (p *Proxy) applyManagedOAuth(parent context.Context, owner string, input managedOAuthApplyInput) (Account, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	var result Account
	var invalidateDatabase bool
	apply := func(tx pgx.Tx, entry *managementStateEntry) error {
		state, err := decodeManagedOAuth(entry, owner)
		if err != nil {
			return err
		}
		if state.Phase == "applied" {
			result = Account{ID: state.AppliedID, Version: state.AppliedVersion}
			return nil
		}
		if state.Phase != "ready" {
			return errConflict
		}
		incoming, err := parseAccount("staged", state.Credentials)
		if err != nil {
			return err
		}
		if state.TargetID != "" {
			if input.Notes != nil || input.Tags != nil || input.Enabled != nil {
				return errInvalid
			}
			var current Account
			var e error
			if tx != nil {
				current, e = p.Store.pgLockManagedCredentialAccount(ctx, tx, state.TargetID)
			} else {
				current, e = p.Store.Get(state.TargetID)
			}
			if e != nil {
				return e
			}
			if current.ID != state.TargetID {
				return errNotFound
			}
			// Recover a prior successful account write whose state acknowledgement
			// failed. Never replay a second OAuth exchange or overwrite new data.
			if current.Version != state.TargetVersion && current.AccessToken == incoming.AccessToken && current.RefreshToken == incoming.RefreshToken && current.AccountID == incoming.AccountID {
				result = accountMetadata(current)
			} else {
				if tx != nil {
					result, err = p.Store.pgReauthorizeManagedAccountFrom(ctx, tx, current, state.TargetID, state.TargetVersion, incoming, input.AllowIdentityChange)
					invalidateDatabase = err == nil
				} else {
					result, err = p.Store.reauthorizeManagedAccount(ctx, state.TargetID, state.TargetVersion, incoming, input.AllowIdentityChange)
				}
				if err != nil {
					return err
				}
			}
		} else {
			if validAccountName(input.Name) != nil {
				return fmt.Errorf("%w: name must contain 1 to 100 bytes without control characters", errInvalid)
			}
			if err := validateImportDefaults(input.Defaults); err != nil {
				return err
			}
			if tx != nil {
				groups := uniqueStrings(input.Defaults.Groups)
				var count int
				if err := tx.QueryRow(ctx, "SELECT count(*) FROM groups WHERE id=ANY($1::text[])", groups).Scan(&count); err != nil {
					return err
				}
				if count != len(groups) {
					return fmt.Errorf("%w: unknown group", errInvalid)
				}
			} else {
				groups, e := p.Groups.List()
				if e != nil {
					return e
				}
				validGroups := map[string]bool{}
				for _, g := range groups {
					validGroups[g.ID] = true
				}
				for _, id := range input.Defaults.Groups {
					if !validGroups[id] {
						return fmt.Errorf("%w: unknown group", errInvalid)
					}
				}
			}
			for _, id := range input.Defaults.Proxies {
				if _, ok := p.Proxies.Get(id); !ok {
					return fmt.Errorf("%w: unknown proxy", errInvalid)
				}
			}
			var createCredentials map[string]any
			if json.Unmarshal(state.Credentials, &createCredentials) != nil {
				return errInvalid
			}
			if input.Notes != nil {
				if len(*input.Notes) > 8192 {
					return errInvalid
				}
				createCredentials["notes"] = *input.Notes
			}
			if input.Tags != nil {
				if len(*input.Tags) > 50 {
					return errInvalid
				}
				for _, tag := range *input.Tags {
					if len(tag) > 100 {
						return errInvalid
					}
				}
				createCredentials["tags"] = *input.Tags
			}
			if input.Enabled != nil {
				createCredentials["enabled"] = *input.Enabled
			}
			createRaw, _ := json.Marshal(createCredentials)
			items := []ImportItem{{Name: input.Name, Credentials: createRaw}}
			var report ImportReport
			var e error
			if tx != nil {
				var parsed []Account
				parsed, report, e = prepareImportContext(ctx, items, input.Defaults)
				if e == nil {
					report, e = p.Store.pgImportDetailedTx(ctx, tx, parsed, report, ImportOptions{})
					invalidateDatabase = e == nil
				}
			} else {
				report, e = p.Store.ImportDetailedContext(ctx, items, input.Defaults, ImportOptions{})
			}
			if e != nil {
				return e
			}
			if report.Failed > 0 {
				return errConflict
			}
			if len(report.Results) != 1 {
				return errors.New("invalid account save result")
			}
			if tx != nil {
				result, err = p.Store.pgGetFrom(ctx, tx, report.Results[0].ID, false)
			} else {
				result, err = p.Store.GetMetadataContext(ctx, report.Results[0].ID)
			}
			if err != nil {
				return err
			}
		}
		state.Phase = "applied"
		state.AppliedID = result.ID
		state.AppliedVersion = result.Version
		state.Credentials = nil
		entry.Payload, err = json.Marshal(state)
		return err
	}
	if p.Store.DB == nil {
		err := p.managementState().Update(ctx, managedOAuthNamespace, input.SessionID, func(entry *managementStateEntry) error {
			return apply(nil, entry)
		})
		return result, err
	}
	tx, err := p.Store.DB.Pool.Begin(ctx)
	if err != nil {
		return Account{}, safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	if err = p.managementState().updatePGTx(ctx, tx, managedOAuthNamespace, input.SessionID, "", func(entry *managementStateEntry) error {
		return apply(tx, entry)
	}); err != nil {
		return Account{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Account{}, safeDBError(err)
	}
	if invalidateDatabase {
		p.Store.DB.invalidate()
	}
	return result, nil
}
