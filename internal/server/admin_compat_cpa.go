package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const cpaFileLimit = 2 << 20

func (p *Proxy) registerCPACompatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v0/management/auth-files", p.handleCPAList)
	mux.HandleFunc("POST /v0/management/auth-files", p.handleCPAUpload)
	mux.HandleFunc("DELETE /v0/management/auth-files", p.handleCPADelete)
	mux.HandleFunc("GET /v0/management/auth-files/download", p.handleCPADownload)
	mux.HandleFunc("PATCH /v0/management/auth-files/status", p.handleCPAStatus)
	mux.HandleFunc("PATCH /v0/management/auth-files/fields", p.handleCPAFields)
	mux.HandleFunc("GET /v0/management/auth-files/models", p.handleCPAModels)
	mux.HandleFunc("POST /v0/management/api-call", p.handleCPAAPICall)
}

func cpaReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func cpaInvalid(message string) error { return fmt.Errorf("%w: %s", errInvalid, message) }

func cpaRaw(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func cpaString(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return strings.TrimSpace(value)
}

func cpaClone(fields map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		result[key] = bytes.Clone(value)
	}
	return result
}

func cpaValidFileName(name string) bool {
	return name != "" && len(name) <= 240 && strings.HasSuffix(name, ".json") &&
		!strings.ContainsAny(name, `/\\:`) && strings.IndexFunc(name, unicode.IsControl) < 0 && strings.TrimSpace(name) == name
}

func (p *Proxy) cpaFind(ctx context.Context, name, index string) (adminCompatAccount, error) {
	name, index = strings.TrimSpace(name), strings.TrimSpace(index)
	if name != "" {
		id, err := p.compatResolveAccount(ctx, name)
		if err != nil {
			return adminCompatAccount{}, err
		}
		if index != "" && compatAuthIndex(id) != index {
			return adminCompatAccount{}, errNotFound
		}
		return p.compatGetAccount(ctx, id)
	}
	if index == "" {
		return adminCompatAccount{}, cpaInvalid("name or auth_index is required")
	}
	// Resolve the auth index from a bounded id scan; the catalogue ordering
	// keeps the first-match semantics of the previous full projection.
	ids, err := p.compatAuthIndexTargets(ctx, index)
	if err != nil {
		return adminCompatAccount{}, err
	}
	if len(ids) == 0 {
		return adminCompatAccount{}, errNotFound
	}
	return p.compatGetAccount(ctx, ids[0])
}

func (p *Proxy) cpaProxyURL(a Account) string {
	if p.Proxies == nil || len(a.Proxies) == 0 {
		return ""
	}
	for _, id := range a.Proxies {
		entry, err := p.Proxies.GetForAdmin(id)
		if err != nil {
			continue
		}
		u := url.URL{Scheme: entry.Type, Host: fmt.Sprintf("%s:%d", entry.Host, entry.Port)}
		if strings.Contains(entry.Host, ":") {
			u.Host = fmt.Sprintf("[%s]:%d", entry.Host, entry.Port)
		}
		if entry.Username != "" || entry.Password != "" {
			u.User = url.UserPassword(entry.Username, entry.Password)
		}
		return u.String()
	}
	return ""
}

func (p *Proxy) cpaListEntry(row adminCompatAccount, proxyURLs map[string]string, websockets bool) map[string]any {
	a := row.Account
	if a.AccountID == "" && a.Identity != nil {
		a.AccountID = a.Identity.WorkspaceID
	}
	email := cpaString(row.Record.CPA, "email")
	if a.Identity != nil && a.Identity.Email != "" {
		email = a.Identity.Email
	}
	status, message := "active", ""
	needsReauth := a.CredentialStatus == "invalid" || a.CredentialStatus == "expired" || a.CredentialStatus == "revoked"
	if a.ExpiresAt > 0 && a.ExpiresAt <= time.Now().Unix() && !a.HasRefreshToken {
		needsReauth = true
	}
	if !a.Enabled {
		status, message = "disabled", "disabled via management API"
	} else if needsReauth {
		status, message = "error", "credential_requires_reauthorization"
	}
	entry := map[string]any{
		"id": a.ID, "auth_index": compatAuthIndex(a.ID), "name": compatFileName(a, row.Record),
		"type": "codex", "provider": "codex", "label": a.Name, "email": email, "account": email,
		"account_type": "oauth", "account_id": a.AccountID, "status": status, "status_message": message,
		"disabled": !a.Enabled, "unavailable": needsReauth, "needs_reauth": needsReauth,
		"runtime_only": false, "source": "file", "size": 0, "priority": a.Priority, "weight": a.effectiveWeight(),
		"created_at": time.Unix(a.CreatedAt, 0).UTC(), "updated_at": time.Unix(a.UpdatedAt, 0).UTC(),
		"modtime": time.Unix(a.UpdatedAt, 0).UTC(), "websockets": websockets,
	}
	if a.Notes != "" {
		entry["note"] = a.Notes
	}
	for _, id := range a.Proxies {
		if proxyURL := proxyURLs[id]; proxyURL != "" {
			entry["proxy_url"] = proxyURL
			break
		}
	}
	claims := map[string]any{}
	if a.AccountID != "" {
		claims["chatgpt_account_id"] = a.AccountID
	}
	if a.Identity != nil && a.Identity.PlanType != "" {
		claims["plan_type"] = a.Identity.PlanType
	}
	if len(claims) > 0 {
		entry["id_token"] = claims
	}
	return entry
}

// cpaFilterTargetIDs resolves the bounded candidate set for a filtered CPA
// listing. The filter only accepts account IDs and auth-file names, never bare
// display names, so candidates come from the id table, the display-name form
// behind an implicit .json file name, and the preserved-name reverse link.
func (p *Proxy) cpaFilterTargetIDs(ctx context.Context, name, index string) ([]string, error) {
	if index != "" {
		return p.compatAuthIndexTargets(ctx, index)
	}
	candidates := map[string]struct{}{}
	if metadata, err := p.Store.metadataByIDsContext(ctx, []string{name}); err == nil {
		if _, ok := metadata[name]; ok {
			candidates[name] = struct{}{}
		}
	} else {
		return nil, err
	}
	named, err := p.compatNameCandidates(ctx, name, false)
	if err != nil {
		return nil, err
	}
	for id := range named {
		candidates[id] = struct{}{}
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	return ids, nil
}

func (p *Proxy) handleCPAList(w http.ResponseWriter, r *http.Request) {
	name, index := strings.TrimSpace(r.URL.Query().Get("name")), strings.TrimSpace(r.URL.Query().Get("auth_index"))
	var accounts []adminCompatAccount
	if p.Store.DB != nil && (name != "" || index != "") {
		// A filtered listing is a bounded lookup: hydrate metadata, records
		// and credentials for the matching rows only instead of decrypting the
		// whole catalogue. Corrupt unrelated documents stop failing it.
		ids, err := p.cpaFilterTargetIDs(r.Context(), name, index)
		if err != nil {
			compatError(w, r, err)
			return
		}
		if len(ids) == 0 {
			accounts = []adminCompatAccount{}
		} else {
			rows, err := p.compatAccountsByIDs(r.Context(), ids, true)
			if errors.Is(err, errNotFound) {
				accounts = []adminCompatAccount{}
			} else if err != nil {
				compatError(w, r, err)
				return
			} else {
				accounts = rows
			}
		}
		selected := accounts[:0]
		for _, row := range accounts {
			if name != "" && name != row.Account.ID && name != compatFileName(row.Account, row.Record) {
				continue
			}
			if index != "" && index != compatAuthIndex(row.Account.ID) {
				continue
			}
			selected = append(selected, row)
		}
		accounts = selected
	} else {
		var err error
		accounts, err = p.compatSelectAccounts(r.Context(), true, func(rows []adminCompatAccount) []adminCompatAccount {
			selected := rows[:0]
			for _, row := range rows {
				if name != "" && name != row.Account.ID && name != compatFileName(row.Account, row.Record) {
					continue
				}
				if index != "" && index != compatAuthIndex(row.Account.ID) {
					continue
				}
				selected = append(selected, row)
			}
			return selected
		})
		if err != nil {
			compatError(w, r, err)
			return
		}
	}
	settings, err := p.settingsValue()
	if err != nil {
		compatError(w, r, err)
		return
	}
	proxyURLs := map[string]string{}
	if p.Proxies != nil {
		wanted := make(map[string]struct{})
		for _, account := range accounts {
			for _, id := range account.Account.Proxies {
				wanted[id] = struct{}{}
			}
		}
		proxies, err := p.Proxies.managedSelectedMetadata(r.Context(), wanted)
		if err != nil {
			compatError(w, r, err)
			return
		}
		for _, proxy := range proxies {
			host := proxy.Host
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			proxyURLs[proxy.ID] = (&url.URL{Scheme: proxy.Type, Host: host + ":" + strconv.Itoa(proxy.Port)}).String()
		}
	}
	files := make([]map[string]any, 0, len(accounts))
	for _, row := range accounts {
		files = append(files, p.cpaListEntry(row, proxyURLs, settings.WebsocketEnabled))
	}
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i]["name"].(string)) < strings.ToLower(files[j]["name"].(string))
	})
	cpaReply(w, http.StatusOK, map[string]any{"files": files})
}

// Current credential values always win over preserved source metadata.
func (p *Proxy) cpaDownload(row adminCompatAccount) map[string]json.RawMessage {
	fields := cpaClone(row.Record.CPA)
	a := row.Account
	for _, key := range []string{"tokens", "credentials", "token", "Token", "accessToken", "refreshToken", "session_access_token", "sessionAccessToken", "expiresAt", "expiry", "expire", "expires"} {
		delete(fields, key)
	}
	fields["type"] = cpaRaw("codex")
	fields["access_token"] = cpaRaw(a.AccessToken)
	fields["refresh_token"] = cpaRaw(a.RefreshToken)
	fields["account_id"] = cpaRaw(a.AccountID)
	fields["chatgpt_account_id"] = cpaRaw(a.AccountID)
	fields["client_id"] = cpaRaw(a.ClientID)
	fields["disabled"] = cpaRaw(!a.Enabled)
	fields["priority"] = cpaRaw(a.Priority)
	fields["weight"] = cpaRaw(a.effectiveWeight())
	fields["concurrency"] = cpaRaw(a.effectiveConcurrency())
	fields["quota"] = cpaRaw(a.Quota)
	fields["window_days"] = cpaRaw(a.effectiveWindowDays())
	fields["note"] = cpaRaw(a.Notes)
	fields["proxy_url"] = cpaRaw(p.cpaProxyURL(a))
	fields["expires_at"] = cpaRaw(a.ExpiresAt)
	fields["expired"] = cpaRaw("")
	if a.ExpiresAt > 0 {
		fields["expired"] = cpaRaw(time.Unix(a.ExpiresAt, 0).UTC().Format(time.RFC3339))
	}
	if a.Identity != nil && a.Identity.Email != "" {
		fields["email"] = cpaRaw(a.Identity.Email)
	}
	return fields
}

func (p *Proxy) handleCPADownload(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if !cpaValidFileName(name) {
		compatError(w, r, cpaInvalid("invalid name"))
		return
	}
	row, err := p.cpaFind(r.Context(), name, "")
	if err != nil {
		compatError(w, r, err)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	cpaReply(w, http.StatusOK, p.cpaDownload(row))
}

func cpaPrepareImport(name string, fields map[string]json.RawMessage) (Account, error) {
	if !cpaValidFileName(name) {
		return Account{}, cpaInvalid("name must be a safe .json filename")
	}
	if fields == nil {
		return Account{}, cpaInvalid("auth file must be a JSON object")
	}
	if _, err := accountString(fields, "type"); err != nil {
		return Account{}, cpaInvalid("type must be a string")
	}
	if provider := strings.ToLower(cpaString(fields, "provider")); provider != "" && provider != "codex" {
		return Account{}, cpaInvalid("unsupported provider")
	}
	if kind := strings.ToLower(cpaString(fields, "type")); kind != "codex" && kind != "oauth" && kind != "" {
		return Account{}, cpaInvalid("unsupported provider: only codex OAuth accounts are supported")
	}
	native := cpaClone(fields)
	native["type"] = cpaRaw("oauth")
	for _, key := range []string{"tokens", "credentials", "token", "Token"} {
		if len(native[key]) != 0 {
			return Account{}, cpaInvalid("wrapped auth files are unsupported; upload a codex auth object")
		}
	}
	if cpaString(native, "access_token") == "" {
		for _, key := range []string{"accessToken", "session_access_token", "sessionAccessToken"} {
			if value := cpaString(native, key); value != "" {
				native["access_token"] = cpaRaw(value)
				break
			}
		}
	}
	if len(native["expires_at"]) == 0 {
		for _, key := range []string{"expired", "expire", "expiresAt", "expiry", "expires"} {
			raw := bytes.TrimSpace(native[key])
			if len(raw) > 0 && string(raw) != `""` && string(raw) != "false" && string(raw) != "null" {
				native["expires_at"] = raw
				break
			}
		}
	}
	if raw, ok := native["disabled"]; ok {
		var disabled bool
		if string(raw) == "null" || json.Unmarshal(raw, &disabled) != nil {
			return Account{}, cpaInvalid("disabled must be a boolean")
		}
		native["enabled"] = cpaRaw(!disabled)
	}
	if raw, ok := native["priority"]; ok && len(raw) > 0 && raw[0] == '"' {
		priority, err := strconv.ParseInt(cpaString(native, "priority"), 10, 32)
		if err != nil {
			return Account{}, cpaInvalid("priority must be an integer")
		}
		native["priority"] = cpaRaw(priority)
	}
	if raw, ok := native["note"]; ok {
		native["notes"] = raw
	}
	if headers, err := cpaCustomHeaders(native["headers"]); err != nil {
		return Account{}, err
	} else if len(headers) > 0 {
		return Account{}, cpaInvalid("per-account custom headers are unsupported")
	}
	for _, key := range []string{"prefix", "models", "model_mapping", "excluded_models", "excluded-models", "group_ids", "source_ip", "sourceIp", "source-ip", "max_concurrent_requests", "rpm_limit", "rpd_limit", "tpm_limit"} {
		raw := bytes.TrimSpace(native[key])
		if len(raw) > 0 && string(raw) != `""` && string(raw) != "null" && string(raw) != "[]" && string(raw) != "{}" {
			return Account{}, cpaInvalid("unsupported per-account field: " + key)
		}
	}
	if raw, ok := native["websockets"]; ok {
		var enabled bool
		if json.Unmarshal(raw, &enabled) != nil || string(raw) == "null" {
			return Account{}, cpaInvalid("websockets must be a boolean")
		}
	}
	accountName := name[:len(name)-5]
	if len(accountName) > 100 {
		accountName = "CPA-" + compatAuthIndex(name)
	}
	a, err := parseAccount(accountName, cpaRaw(native))
	if err != nil {
		return Account{}, cpaInvalid("invalid codex credentials or account fields")
	}
	a = applyAccountDefaults(a, AccountDefaults{})
	normalizeAccount(&a)
	return a, nil
}

func cpaCustomHeaders(raw json.RawMessage) (map[string]string, error) {
	result := map[string]string{}
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return result, nil
	}
	if json.Unmarshal(raw, &result) != nil || len(result) > 64 {
		return nil, cpaInvalid("headers must be an object of at most 64 strings")
	}
	seen := map[string]bool{}
	for name, value := range result {
		folded := strings.ToLower(name)
		if seen[folded] {
			return nil, cpaInvalid("duplicate custom header name")
		}
		seen[folded] = true
		if len(name) == 0 || len(name) > 256 || len(value) > 8192 || strings.IndexFunc(value, func(c rune) bool { return c == '\r' || c == '\n' || c == 0 }) >= 0 {
			return nil, cpaInvalid("invalid custom header")
		}
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
				return nil, cpaInvalid("invalid custom header name")
			}
		}
	}
	return result, nil
}

func (p *Proxy) cpaBindProxy(raw json.RawMessage) ([]string, error) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return nil, cpaInvalid("proxy_url must be a string")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return nil, cpaInvalid("proxy_url must be an http or socks5 proxy URL")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		if u.Port() == "" && u.Scheme == "http" {
			port = 80
		} else {
			return nil, cpaInvalid("proxy_url requires a valid port")
		}
	}
	remote := &managedCRSProxy{Protocol: u.Scheme, Host: u.Hostname(), Port: port}
	if u.User != nil {
		remote.Username = u.User.Username()
		remote.Password, _ = u.User.Password()
	}
	if p.Proxies == nil {
		return nil, cpaInvalid("proxy store unavailable")
	}
	entries, err := p.Proxies.List()
	if err != nil {
		return nil, err
	}
	if remote.Protocol == "socks5h" {
		remote.Protocol = "socks5"
	}
	for _, entry := range entries {
		if entry.Enabled && entry.Type == remote.Protocol && entry.Host == remote.Host && entry.Port == remote.Port && entry.Username == remote.Username && entry.Password == remote.Password {
			return []string{entry.ID}, nil
		}
	}
	return nil, cpaInvalid("proxy_url must match an enabled proxy configured in this system")
}

func cpaPreservedMetadata(fields map[string]json.RawMessage) map[string]json.RawMessage {
	result := cpaClone(fields)
	for _, key := range []string{"access_token", "refresh_token", "accessToken", "refreshToken", "session_access_token", "sessionAccessToken", "account_id", "client_id", "expired", "expires_at", "expiresAt", "expiry", "expire", "expires", "disabled", "priority", "weight", "proxy_url", "notes", "note", "tokens", "credentials", "token", "Token"} {
		delete(result, key)
	}
	return result
}

func (p *Proxy) cpaImport(ctx context.Context, name string, fields map[string]json.RawMessage) error {
	if err := p.cpaValidateRuntime(fields); err != nil {
		return err
	}
	incoming, err := cpaPrepareImport(name, fields)
	if err != nil {
		return err
	}
	var existing adminCompatAccount
	id, err := p.compatResolveAccount(ctx, name)
	if err == nil {
		existing, err = p.compatGetAccount(ctx, id)
	}
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	if errors.Is(err, errNotFound) {
		id = ""
	}
	var proxies []string
	if raw, ok := fields["proxy_url"]; ok {
		proxies, err = p.cpaBindProxy(raw)
		if err != nil {
			return err
		}
	}
	_, err = p.compatMutateAccount(ctx, id, existing.Account.Version, func(a *Account, record *adminCompatRecord) error {
		if id == "" {
			*a = incoming
		} else {
			cpaRetainIdentityObservation(a, &incoming)
			a.AccessToken, a.RefreshToken, a.AccountID, a.ClientID, a.ExpiresAt, a.Identity = incoming.AccessToken, incoming.RefreshToken, incoming.AccountID, incoming.ClientID, incoming.ExpiresAt, incoming.Identity
			a.CredentialStatus, a.LastError, a.LastCheckedAt = "unverified", "", 0
		}
		if _, ok := fields["disabled"]; ok {
			a.Enabled = incoming.Enabled
		}
		if _, ok := fields["priority"]; ok {
			a.Priority = incoming.Priority
		}
		if _, ok := fields["weight"]; ok {
			a.Weight = incoming.Weight
		}
		if _, ok := fields["concurrency"]; ok {
			a.Concurrency = incoming.Concurrency
		}
		if _, ok := fields["quota"]; ok {
			a.Quota = incoming.Quota
		}
		if _, ok := fields["window_days"]; ok {
			a.WindowDays = incoming.WindowDays
		}
		if _, ok := fields["note"]; ok {
			a.Notes = incoming.Notes
		}
		if _, ok := fields["proxy_url"]; ok {
			a.Proxies = proxies
		}
		record.FileName, record.CPA = name, cpaPreservedMetadata(fields)
		return nil
	})
	return err
}

func (p *Proxy) handleCPAUpload(w http.ResponseWriter, r *http.Request) {
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType != "multipart/form-data" {
		var fields map[string]json.RawMessage
		if err := compatReadJSON(w, r, &fields, cpaFileLimit); err != nil {
			compatError(w, r, err)
			return
		}
		if err := cpaImportDefaults(fields, r.URL.Query().Get("default_websockets")); err != nil {
			compatError(w, r, err)
			return
		}
		if err := p.cpaImport(r.Context(), strings.TrimSpace(r.URL.Query().Get("name")), fields); err != nil {
			compatError(w, r, err)
			return
		}
		cpaReply(w, 200, map[string]string{"status": "ok"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, adminBodyLimit)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			compatError(w, r, errAdminBodyTooLarge)
		} else {
			compatError(w, r, cpaInvalid("invalid multipart form"))
		}
		return
	}
	defer r.MultipartForm.RemoveAll()
	defaults := r.URL.Query().Get("default_websockets")
	if values := r.MultipartForm.Value["default_websockets"]; len(values) > 0 {
		defaults = values[len(values)-1]
	}
	if err := cpaImportDefaults(map[string]json.RawMessage{}, defaults); err != nil {
		compatError(w, r, err)
		return
	}
	names := make([]string, 0, len(r.MultipartForm.File))
	count := 0
	for key, files := range r.MultipartForm.File {
		names = append(names, key)
		count += len(files)
	}
	if count == 0 || count > 1000 {
		compatError(w, r, cpaInvalid("expected 1 to 1000 auth files"))
		return
	}
	sort.Strings(names)
	uploaded, failed := []string{}, []map[string]string{}
	for _, key := range names {
		for _, file := range r.MultipartForm.File[key] {
			var fields map[string]json.RawMessage
			var err error
			if file.Size > cpaFileLimit {
				err = errAdminBodyTooLarge
			} else {
				reader, openErr := file.Open()
				err = openErr
				if err == nil {
					raw, readErr := io.ReadAll(io.LimitReader(reader, cpaFileLimit+1))
					_ = reader.Close()
					if readErr != nil || len(raw) > cpaFileLimit || compatValidateJSON(raw) != nil || json.Unmarshal(raw, &fields) != nil || fields == nil {
						err = cpaInvalid("invalid auth JSON")
					}
				}
			}
			if err == nil {
				err = cpaImportDefaults(fields, defaults)
			}
			if err == nil {
				err = p.cpaImport(r.Context(), file.Filename, fields)
			}
			if err != nil {
				if count == 1 {
					compatError(w, r, err)
					return
				}
				failed = append(failed, map[string]string{"name": file.Filename, "error": cpaActionError(err)})
			} else {
				uploaded = append(uploaded, file.Filename)
			}
		}
	}
	if len(failed) > 0 {
		cpaReply(w, 207, map[string]any{"status": "partial", "uploaded": len(uploaded), "files": uploaded, "failed": failed})
	} else if count == 1 {
		cpaReply(w, 200, map[string]string{"status": "ok"})
	} else {
		cpaReply(w, 200, map[string]any{"status": "ok", "uploaded": len(uploaded), "files": uploaded})
	}
}

func (p *Proxy) cpaValidateRuntime(fields map[string]json.RawMessage) error {
	if raw, ok := fields["websockets"]; ok {
		var enabled bool
		if json.Unmarshal(raw, &enabled) != nil || string(bytes.TrimSpace(raw)) == "null" {
			return cpaInvalid("websockets must be a boolean")
		}
		settings, err := p.settingsValue()
		if err != nil {
			return err
		}
		if enabled != settings.WebsocketEnabled {
			return cpaInvalid("per-account websockets must match the global gateway setting")
		}
	}
	return nil
}

func cpaActionError(err error) string {
	switch {
	case errors.Is(err, errNotFound):
		return "auth file not found"
	case errors.Is(err, errVersionConflict), errors.Is(err, errConflict), errors.Is(err, ErrTokenConflict):
		return "auth file changed; reload and retry"
	case errors.Is(err, errAdminBodyTooLarge):
		return "auth file is too large"
	case errors.Is(err, errInvalid):
		return "invalid or unsupported auth file fields"
	default:
		return "auth file operation failed"
	}
}

func cpaImportDefaults(fields map[string]json.RawMessage, raw string) error {
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return cpaInvalid("default_websockets must be a boolean")
	}
	if _, ok := fields["websockets"]; !ok {
		fields["websockets"] = cpaRaw(value)
	}
	return nil
}

func (p *Proxy) handleCPADelete(w http.ResponseWriter, r *http.Request) {
	names := []string{}
	preloaded := map[string]adminCompatAccount{}
	all := r.URL.Query().Get("all")
	if all == "true" || all == "1" || all == "*" {
		// Only IDs, display names and preserved file names drive a delete-all;
		// the reference projection skips full metadata and credentials.
		rows, err := p.compatAccountReferences(r.Context())
		if err != nil {
			compatError(w, r, err)
			return
		}
		for _, row := range rows {
			names = append(names, row.Account.ID)
			preloaded[row.Account.ID] = row
		}
	} else {
		names = r.URL.Query()["name"]
		if len(names) == 0 {
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
			if err != nil {
				compatError(w, r, errAdminBodyTooLarge)
				return
			}
			raw = bytes.TrimSpace(raw)
			if len(raw) > 0 && raw[0] == '[' {
				err = json.Unmarshal(raw, &names)
			} else if len(raw) > 0 {
				var body struct {
					Name  string   `json:"name"`
					Names []string `json:"names"`
				}
				err = json.Unmarshal(raw, &body)
				names = append(body.Names, body.Name)
			}
			if err != nil {
				compatError(w, r, cpaInvalid("invalid request body"))
				return
			}
		}
	}
	unique := []string{}
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	if len(unique) > 10000 || len(unique) == 0 && all != "true" && all != "1" && all != "*" {
		compatError(w, r, cpaInvalid("invalid name"))
		return
	}
	deleted, failed := []string{}, []map[string]string{}
	for _, name := range unique {
		row, cached := preloaded[name]
		var err error
		if !cached {
			row, err = p.cpaFind(r.Context(), name, "")
		}
		if err == nil {
			// Deletion is defined by the resolved account ID, which is never
			// reused. A version captured before the delete only turns benign
			// concurrent updates (refresh, lease tick) into false 409 reports.
			err = p.compatDeleteAccount(r.Context(), row.Account.ID, 0)
		}
		if err != nil {
			if len(unique) == 1 && all == "" {
				compatError(w, r, err)
				return
			}
			failed = append(failed, map[string]string{"name": name, "error": cpaActionError(err)})
		} else {
			deleted = append(deleted, compatFileName(row.Account, row.Record))
		}
	}
	if len(failed) > 0 {
		cpaReply(w, 207, map[string]any{"status": "partial", "deleted": len(deleted), "files": deleted, "failed": failed})
	} else if len(unique) == 1 && all == "" {
		cpaReply(w, 200, map[string]string{"status": "ok"})
	} else {
		cpaReply(w, 200, map[string]any{"status": "ok", "deleted": len(deleted), "files": deleted})
	}
}

// A management-only edit must not advance the credential epoch merely because
// parsing the same identity collected a new local timestamp.
func cpaRetainIdentityObservation(previous *Account, incoming *Account) {
	if previous.Identity == nil || incoming.Identity == nil {
		return
	}
	before, after := *previous.Identity, *incoming.Identity
	after.CollectedAt = before.CollectedAt
	after.Verified = before.Verified
	after.Source = before.Source
	if before == after {
		incoming.Identity = previous.Identity
	}
}

func (p *Proxy) handleCPAStatus(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		Disabled  *bool  `json:"disabled"`
	}
	if err := compatReadJSON(w, r, &input, 16<<10); err != nil {
		compatError(w, r, err)
		return
	}
	if strings.TrimSpace(input.Name) == "" || input.Disabled == nil {
		compatError(w, r, cpaInvalid("name and disabled are required"))
		return
	}
	row, err := p.cpaFind(r.Context(), input.Name, input.AuthIndex)
	if err == nil {
		_, err = p.compatMutateAccount(r.Context(), row.Account.ID, row.Account.Version, func(a *Account, _ *adminCompatRecord) error {
			a.Enabled = !*input.Disabled
			if a.Enabled {
				a.LastError = ""
			}
			return nil
		})
	}
	if err != nil {
		compatError(w, r, err)
		return
	}
	cpaReply(w, 200, map[string]any{"status": "ok", "disabled": *input.Disabled})
}
