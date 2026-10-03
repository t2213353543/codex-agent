package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type managedCRSProxy struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}
type managedCRSRow struct {
	SourceID        string `json:"source_id"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	Priority        int    `json:"priority"`
	HasRefreshToken bool   `json:"has_refresh_token"`
	HasProxy        bool   `json:"has_proxy"`
	TargetID        string `json:"target_id,omitempty"`
	TargetVersion   int64  `json:"target_version,omitempty"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
}
type managedCRSAccount struct {
	Row         managedCRSRow    `json:"row"`
	Notes       string           `json:"notes"`
	Credentials json.RawMessage  `json:"credentials"`
	Proxy       *managedCRSProxy `json:"proxy,omitempty"`
}
type managedCRSResult struct {
	ProxyID  string `json:"proxy_id,omitempty"`
	SourceID string `json:"source_id"`
	ID       string `json:"id,omitempty"`
	Version  int64  `json:"version,omitempty"`
	Status   string `json:"status"`
	Code     string `json:"code,omitempty"`
}
type managedCRSPreview struct {
	Owner       string              `json:"owner"`
	Source      string              `json:"source"`
	Accounts    []managedCRSAccount `json:"accounts"`
	Phase       string              `json:"phase"`
	RequestHash string              `json:"request_hash,omitempty"`
	Results     []managedCRSResult  `json:"results,omitempty"`
}
type managedCRSLink struct {
	ID string `json:"id"`
}

func managedCRSHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
func managedCRSLinkKey(source, id string) string { return managedCRSHash(source + "\x00" + id) }

func (p *Proxy) handleManagedCRSPreview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		BaseURL           string `json:"base_url"`
		Username          string `json:"username"`
		Password          string `json:"password"`
		ProxyID           string `json:"proxy_id"`
		AllowInsecureHTTP bool   `json:"allow_insecure_http"`
	}
	if err := readJSONLimit(w, r, &input, 32<<10); err != nil {
		adminError(w, err)
		return
	}
	u, err := url.Parse(strings.TrimSpace(input.BaseURL))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && (u.Scheme != "http" || !input.AllowInsecureHTTP) || input.Username == "" || input.Password == "" || len(input.Username) > 512 || len(input.Password) > 8192 {
		adminError(w, errors.New("valid CRS base URL and credentials required; HTTP requires explicit allow_insecure_http"))
		return
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	source := u.String()
	a := Account{}
	if input.ProxyID != "" {
		a.Proxies = []string{input.ProxyID}
	}
	client, err := p.managedAccountClient(a)
	if err != nil {
		adminAPIError(w, 400, "proxy_unavailable", "configured CRS proxy unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	call := func(method, path string, body any, token string, limit int64) ([]byte, error) {
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, source+path, reader)
		if err != nil {
			return nil, errInvalid
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, errors.New("CRS connection failed")
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, errors.New("CRS rejected request")
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil || int64(len(raw)) > limit {
			return nil, errors.New("CRS response invalid or too large")
		}
		return raw, nil
	}
	raw, err := call("POST", "/web/auth/login", map[string]string{"username": input.Username, "password": input.Password}, "", 1<<20)
	var login struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
	}
	if err != nil || json.Unmarshal(raw, &login) != nil || !login.Success || login.Token == "" {
		adminAPIError(w, 502, "crs_login_failed", "CRS authentication failed")
		return
	}
	raw, err = call("GET", "/admin/sync/export-accounts?include_secrets=true", nil, login.Token, 16<<20)
	var exported struct {
		Success bool `json:"success"`
		Data    struct {
			Accounts []struct {
				ID          string           `json:"id"`
				Name        string           `json:"name"`
				Description string           `json:"description"`
				AuthType    string           `json:"authType"`
				IsActive    bool             `json:"isActive"`
				Priority    int              `json:"priority"`
				Credentials json.RawMessage  `json:"credentials"`
				Proxy       *managedCRSProxy `json:"proxy"`
			} `json:"openaiOAuthAccounts"`
		} `json:"data"`
	}
	if err != nil || json.Unmarshal(raw, &exported) != nil || !exported.Success || exported.Data.Accounts == nil {
		adminAPIError(w, 502, "crs_export_failed", "CRS did not return a valid OpenAI OAuth account export")
		return
	}
	if len(exported.Data.Accounts) > 10000 {
		adminAPIError(w, 413, "crs_export_too_large", "CRS preview supports at most 10000 accounts")
		return
	}
	preview := managedCRSPreview{Owner: managedOAuthOwner(r), Source: source, Phase: "ready", Accounts: []managedCRSAccount{}}
	rows := []managedCRSRow{}
	seen := map[string]bool{}
	// Resolve every source→local link and the linked accounts' metadata in two
	// batched reads instead of two queries per exported account.
	linkKeys := make([]string, 0, len(exported.Data.Accounts))
	for _, item := range exported.Data.Accounts {
		if item.AuthType != "" && item.AuthType != "oauth" {
			continue
		}
		linkKeys = append(linkKeys, managedCRSLinkKey(source, item.ID))
	}
	links, err := p.managedCRSLinks(ctx, linkKeys)
	if err != nil {
		p.storeError(w, err)
		return
	}
	targetIDs := make([]string, 0, len(links))
	seenTargets := map[string]bool{}
	for _, link := range links {
		if link.ID != "" && !seenTargets[link.ID] {
			seenTargets[link.ID] = true
			targetIDs = append(targetIDs, link.ID)
		}
	}
	targets, err := p.Store.metadataByIDsContext(ctx, targetIDs)
	if err != nil {
		p.storeError(w, err)
		return
	}
	for _, item := range exported.Data.Accounts {
		if item.AuthType != "" && item.AuthType != "oauth" {
			continue
		}
		row := managedCRSRow{SourceID: item.ID, Name: item.Name, Enabled: item.IsActive, Priority: item.Priority, HasProxy: item.Proxy != nil, Status: "ready"}
		account, err := parseManagedSessionCredentials(item.Credentials)
		if err != nil || item.ID == "" || len(item.ID) > 256 || seen[item.ID] || validAccountName(item.Name) != nil || len(item.Description) > 8192 || item.Priority < 0 || item.Priority > 1000000000 {
			row.Status = "invalid"
			row.Error = "invalid source account metadata or OAuth credentials"
		}
		seen[item.ID] = true
		row.HasRefreshToken = account.RefreshToken != ""
		if link, ok := links[managedCRSLinkKey(source, item.ID)]; ok {
			if local, ok := targets[link.ID]; ok {
				row.TargetID = local.ID
				row.TargetVersion = local.Version
			}
		}
		preview.Accounts = append(preview.Accounts, managedCRSAccount{Row: row, Notes: item.Description, Credentials: canonicalManagedCredentials(account), Proxy: item.Proxy})
		rows = append(rows, row)
	}
	id := managedOAuthRandom()
	expires := time.Now().Add(15 * time.Minute).Unix()
	err = p.managementState().Update(ctx, "crs-preview", id, func(entry *managementStateEntry) error {
		var err error
		entry.Payload, err = json.Marshal(preview)
		if len(entry.Payload) > 8<<20 {
			return errAdminBodyTooLarge
		}
		entry.ExpiresAt = expires
		return err
	})
	if err != nil {
		if errors.Is(err, errAdminBodyTooLarge) {
			adminError(w, err)
		} else {
			p.storeError(w, err)
		}
		return
	}
	writeJSON(w, map[string]any{"preview_id": id, "expires_at": expires, "source": source, "accounts": rows, "total": len(rows), "scope": "openai_oauth_only"})
}

func (p *Proxy) managedCRSProxyID(ctx context.Context, source string, remote *managedCRSProxy) (string, error) {
	if remote == nil {
		return "", nil
	}
	entry := ProxyEntry{Name: "CRS-" + managedCRSHash(source + "\x00" + remote.Host)[:12], Type: remote.Protocol, Host: remote.Host, Port: remote.Port, Username: remote.Username, Password: remote.Password, Enabled: true}
	if entry.Type == "socks5h" {
		entry.Type = "socks5"
	}
	if validProxy(entry) != nil {
		return "", errInvalid
	}
	matching, err := p.managedCRSMatchingProxy(ctx, entry)
	if err != nil {
		return "", err
	}
	if matching.ID != "" {
		return matching.ID, nil
	}
	created, err := p.Proxies.Create(entry)
	return created.ID, err
}

// managedCRSMatchingProxy finds an enabled proxy with the same endpoint,
// username and password. The endpoint and username narrow the candidates by
// metadata first; only those candidates have their password decrypted — a CRS
// sync must not decrypt the whole proxy table per account.
func (p *Proxy) managedCRSMatchingProxy(ctx context.Context, entry ProxyEntry) (ProxyEntry, error) {
	matches := func(value ProxyEntry) bool {
		return value.Enabled && value.Type == entry.Type && value.Host == entry.Host && value.Port == entry.Port && value.Username == entry.Username
	}
	if p.Store.DB == nil {
		existing, err := p.Proxies.List()
		if err != nil {
			return ProxyEntry{}, err
		}
		for _, value := range existing {
			if matches(value) && value.Password == entry.Password {
				return value, nil
			}
		}
		return ProxyEntry{}, nil
	}
	candidates, err := p.Proxies.managedMetadata()
	if err != nil {
		return ProxyEntry{}, err
	}
	wanted := map[string]struct{}{}
	for _, value := range candidates {
		if matches(value) {
			wanted[value.ID] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return ProxyEntry{}, nil
	}
	decrypted, err := p.Proxies.managedSelectedCredentials(ctx, wanted)
	if err != nil {
		return ProxyEntry{}, err
	}
	for _, value := range decrypted {
		if matches(value) && value.Password == entry.Password {
			return value, nil
		}
	}
	return ProxyEntry{}, nil
}

func (p *Proxy) syncManagedCRSAccount(ctx context.Context, source string, item managedCRSAccount, syncProxies, allowIdentity bool, resolveProxy func(context.Context, string, *managedCRSProxy) (string, error)) managedCRSResult {
	result := managedCRSResult{SourceID: item.Row.SourceID, Status: "failed"}
	if item.Row.Status != "ready" {
		result.Code = "invalid_source_account"
		return result
	}
	incoming, err := parseAccount(item.Row.Name, item.Credentials)
	if err != nil {
		result.Code = "invalid_credentials"
		return result
	}
	// Proxy resources are independent management writes and remain visible if
	// the account transaction fails. Credentials and source identity commit once.
	if item.Row.TargetID == "" && syncProxies && item.Proxy != nil {
		resolve := resolveProxy
		if resolve == nil {
			resolve = p.managedCRSProxyID
		}
		result.ProxyID, err = resolve(ctx, source, item.Proxy)
		if err != nil {
			result.Code = "proxy_storage_error"
			return result
		}
	}
	result.ID, result.Version, result.Status, err = p.commitManagedCRSAccount(ctx, source, item, incoming, result.ProxyID, allowIdentity)
	if err != nil {
		result.Status = "failed"
		result.ID, result.Version = "", 0
		switch {
		case errors.Is(err, errVersionConflict):
			result.Code = "version_conflict"
		case errors.Is(err, errManagedIdentityChange):
			result.Code = "identity_change_requires_confirmation"
		case errors.Is(err, errNotFound):
			result.Code = "target_deleted"
		case errors.Is(err, errConflict):
			result.Code = "account_conflict"
		case errors.Is(err, errInvalid):
			result.Code = "invalid_source_account"
		default:
			result.Code = "storage_error"
		}
	}
	return result
}

func (p *Proxy) handleManagedCRSSync(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PreviewID     string   `json:"preview_id"`
		Selected      []string `json:"selected_account_ids"`
		SyncProxies   bool     `json:"sync_proxies"`
		AllowIdentity bool     `json:"allow_identity_change"`
	}
	if err := readJSONLimit(w, r, &input, 64<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.PreviewID == "" || len(input.Selected) < 1 || len(input.Selected) > 100 {
		adminError(w, errors.New("select 1 to 100 CRS accounts"))
		return
	}
	sort.Strings(input.Selected)
	for i, id := range input.Selected {
		if id == "" || i > 0 && id == input.Selected[i-1] {
			adminError(w, errInvalid)
			return
		}
	}
	raw, _ := json.Marshal(input)
	requestHash := managedCRSHash(string(raw))
	owner := managedOAuthOwner(r)
	var preview managedCRSPreview
	replay := false
	err := p.managementState().Update(r.Context(), "crs-preview", input.PreviewID, func(entry *managementStateEntry) error {
		if entry.Version == 0 || entry.ExpiresAt <= time.Now().Unix() {
			return errNotFound
		}
		if json.Unmarshal(entry.Payload, &preview) != nil {
			return errInvalid
		}
		if owner == "" || preview.Owner != owner {
			return errNotFound
		}
		if preview.Phase != "ready" {
			if preview.Phase == "finished" && preview.RequestHash == requestHash {
				replay = true
				return nil
			}
			return errConflict
		}
		available := map[string]bool{}
		for _, item := range preview.Accounts {
			available[item.Row.SourceID] = true
		}
		for _, id := range input.Selected {
			if !available[id] {
				return errInvalid
			}
		}
		preview.Phase = "running"
		preview.RequestHash = requestHash
		encoded, err := json.Marshal(preview)
		entry.Payload = encoded
		return err
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	if !replay {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 90*time.Second)
		defer cancel()
		wanted := map[string]bool{}
		for _, id := range input.Selected {
			wanted[id] = true
		}
		// Selected accounts usually share a handful of CRS proxies; resolve
		// each distinct proxy once instead of rescanning per account.
		proxyIDs := map[string]string{}
		resolveProxy := func(ctx context.Context, source string, remote *managedCRSProxy) (string, error) {
			if remote == nil {
				return "", nil
			}
			fingerprint := remote.Protocol + "\x00" + remote.Host + "\x00" + strconv.Itoa(remote.Port) + "\x00" + remote.Username + "\x00" + remote.Password
			if id, ok := proxyIDs[fingerprint]; ok {
				return id, nil
			}
			id, err := p.managedCRSProxyID(ctx, source, remote)
			if err == nil {
				proxyIDs[fingerprint] = id
			}
			return id, err
		}
		preview.Results = []managedCRSResult{}
		for _, item := range preview.Accounts {
			if wanted[item.Row.SourceID] {
				preview.Results = append(preview.Results, p.syncManagedCRSAccount(ctx, preview.Source, item, input.SyncProxies, input.AllowIdentity, resolveProxy))
			}
		}
		preview.Phase = "finished"
		preview.Accounts = nil
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer persistCancel()
		if err := p.managementState().Update(persistCtx, "crs-preview", input.PreviewID, func(entry *managementStateEntry) error {
			var err error
			entry.Payload, err = json.Marshal(preview)
			return err
		}); err != nil {
			p.storeError(w, err)
			return
		}
		p.invalidateAccounts()
	}
	succeeded := 0
	for _, result := range preview.Results {
		if result.Status != "failed" {
			succeeded++
		}
	}
	writeJSON(w, map[string]any{"results": preview.Results, "succeeded": succeeded, "failed": len(preview.Results) - succeeded})
}
