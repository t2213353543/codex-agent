package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (p *Proxy) managedDisabledPrivacyIDs(ctx context.Context, now int64) ([]string, error) {
	entries, err := p.managementState().List(ctx, "account-upstream")
	if err != nil {
		return nil, err
	}
	candidates := map[string]managedUpstreamSnapshot{}
	ids := []string{}
	for id, entry := range entries {
		var values map[string]managedUpstreamSnapshot
		if json.Unmarshal(entry.Payload, &values) != nil {
			return nil, errInvalid
		}
		snapshot, ok := values["privacy"]
		if ok && snapshot.OK && !snapshot.Stale && now-snapshot.CheckedAt <= 300 {
			candidates[id] = snapshot
			ids = append(ids, id)
		}
	}
	matched := []string{}
	if len(ids) == 0 {
		return matched, nil
	}
	if p.Store.DB != nil {
		rows, err := p.Store.DB.Pool.Query(ctx, "SELECT account_id,credential_version FROM account_credentials WHERE account_id=ANY($1::text[])", ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var version int64
			if err = rows.Scan(&id, &version); err != nil {
				return nil, err
			}
			if candidates[id].CredentialVersion == version {
				matched = append(matched, id)
			}
		}
		return matched, rows.Err()
	}
	_, accounts, err := p.Store.ListMetadata()
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if snapshot, ok := candidates[a.ID]; ok && snapshot.CredentialVersion == a.CredentialVersion {
			matched = append(matched, a.ID)
		}
	}
	return matched, nil
}

type managedAccountIdentity struct {
	WorkspaceID    string `json:"workspace_id,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	UserID         string `json:"user_id,omitempty"`
	Email          string `json:"email,omitempty"`
	PlanType       string `json:"plan_type,omitempty"`
	AgentRuntimeID string `json:"agent_runtime_id,omitempty"`
	StableKey      string `json:"stable_key,omitempty"`
	Source         string `json:"source"`
	CollectedAt    int64  `json:"collected_at"`
	Verified       bool   `json:"verified"`
}

func managedIdentityJWT(token string) map[string]json.RawMessage {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) > 128<<10 {
		return nil
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil {
		return nil
	}
	return result
}
func parseManagedIdentity(root, credentials map[string]json.RawMessage, a Account) (*managedAccountIdentity, error) {
	identity := managedAccountIdentity{WorkspaceID: a.AccountID, Source: "import_metadata", CollectedAt: time.Now().Unix()}
	read := func(object map[string]json.RawMessage, key string) string {
		var value string
		_ = json.Unmarshal(object[key], &value)
		return value
	}
	if raw := credentials["identity"]; len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &identity) != nil {
			return nil, errInvalid
		}
		identity.Verified = false
	}
	for _, token := range []string{a.AccessToken, read(credentials, "id_token")} {
		claims := managedIdentityJWT(token)
		if claims == nil {
			continue
		}
		var auth map[string]json.RawMessage
		_ = json.Unmarshal(claims["https://api.openai.com/auth"], &auth)
		for _, row := range []struct {
			key    string
			target *string
		}{{"chatgpt_account_id", &identity.WorkspaceID}, {"chatgpt_user_id", &identity.UserID}, {"chatgpt_plan_type", &identity.PlanType}} {
			value := read(auth, row.key)
			// Match Codex's chatgpt_user_id.or(user_id). A workspace can
			// contain multiple users, so the fallback must participate in
			// the same identity conflict check as the primary claim.
			if row.key == "chatgpt_user_id" && (len(auth[row.key]) == 0 || strings.TrimSpace(string(auth[row.key])) == "null") {
				value = read(auth, "user_id")
			}
			if value != "" {
				if row.key == "chatgpt_account_id" && a.AccountID != "" {
					continue
				}
				if *row.target != "" && *row.target != value && row.key != "chatgpt_plan_type" {
					return nil, errors.New("conflicting OAuth identity claims")
				}
				*row.target = value
			}
		}
		if identity.Email == "" {
			identity.Email = read(claims, "email")
		}
		identity.Source = "jwt_claims_unverified"
	}
	for _, key := range []string{"workspace_id"} {
		if value := read(credentials, key); value != "" && identity.WorkspaceID == "" {
			identity.WorkspaceID = value
		}
	}
	if identity.UserID == "" {
		identity.UserID = read(credentials, "chatgpt_user_id")
	}
	if identity.UserID == "" {
		identity.UserID = read(credentials, "user_id")
	}
	if identity.OrganizationID == "" {
		identity.OrganizationID = read(credentials, "organization_id")
	}
	if identity.Email == "" {
		identity.Email = read(credentials, "email")
	}
	if identity.PlanType == "" {
		identity.PlanType = read(credentials, "plan_type")
	}
	if raw := root["agent_identity"]; len(raw) > 0 && string(raw) != "null" {
		var record map[string]json.RawMessage
		var token string
		if json.Unmarshal(raw, &token) == nil {
			record = managedIdentityJWT(token)
		} else {
			_ = json.Unmarshal(raw, &record)
		}
		if record == nil {
			return nil, errors.New("invalid agent_identity record or JWT")
		}
		workspace, user := read(record, "account_id"), read(record, "chatgpt_user_id")
		if workspace == "" || user == "" || read(record, "agent_runtime_id") == "" || read(record, "agent_private_key") == "" {
			return nil, errors.New("incomplete agent_identity record")
		}
		if identity.WorkspaceID != "" && identity.WorkspaceID != workspace || identity.UserID != "" && identity.UserID != user {
			return nil, errors.New("agent_identity does not match OAuth workspace/user")
		}
		identity.WorkspaceID = workspace
		identity.UserID = user
		identity.AgentRuntimeID = read(record, "agent_runtime_id")
		identity.Source = "agent_identity_metadata_unverified"
		if identity.Email == "" {
			identity.Email = read(record, "email")
		}
		if identity.PlanType == "" {
			identity.PlanType = read(record, "plan_type")
		}
	}
	for _, value := range []string{identity.WorkspaceID, identity.OrganizationID, identity.UserID, identity.Email, identity.PlanType, identity.AgentRuntimeID} {
		if len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errInvalid
		}
	}
	identity.StableKey = ""
	if identity.WorkspaceID != "" && identity.UserID != "" {
		identity.StableKey = managedCRSHash(identity.WorkspaceID + "\x00" + identity.UserID)
	}
	return &identity, nil
}
func managedIdentityForAccount(a Account) *managedAccountIdentity {
	if a.Identity != nil {
		return a.Identity
	}
	identity, _ := parseManagedIdentity(nil, nil, a)
	if identity != nil {
		identity.CollectedAt = 0
	}
	return identity
}
func managedIdentityChanged(current, incoming Account) bool {
	if current.AccountID != "" && current.AccountID != incoming.AccountID {
		return true
	}
	old, next := managedIdentityForAccount(current), managedIdentityForAccount(incoming)
	return old != nil && old.UserID != "" && (next == nil || next.UserID != old.UserID)
}
func (p *Proxy) handleManagedAccountMetadata(w http.ResponseWriter, r *http.Request) {
	a, err := p.Store.Get(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	if a.ID != r.PathValue("name") {
		p.storeError(w, errNotFound)
		return
	}
	upstream := map[string]any{}
	privacy := map[string]any{"status": "unknown", "source": "no_snapshot", "collected_at": int64(0), "stale": true}
	entry, err := p.managementState().Get(r.Context(), "account-upstream", a.ID)
	if err != nil && !errors.Is(err, errNotFound) {
		p.storeError(w, err)
		return
	}
	if err == nil {
		var snapshots map[string]managedUpstreamSnapshot
		if json.Unmarshal(entry.Payload, &snapshots) != nil {
			p.storeError(w, errInvalid)
			return
		}
		for kind, snapshot := range snapshots {
			version := a.CredentialVersion
			stale := snapshot.Stale || snapshot.CredentialVersion != version || time.Now().Unix()-snapshot.CheckedAt > 300
			upstream[kind] = map[string]any{"source": snapshot.Source, "collected_at": snapshot.CheckedAt, "stale": stale, "ok": snapshot.OK}
			if kind == "privacy" {
				status := "unknown"
				if snapshot.OK && !stale {
					status = "disabled"
				}
				privacy = map[string]any{"status": status, "source": snapshot.Source, "collected_at": snapshot.CheckedAt, "stale": stale}
			}
		}
	}
	writeJSON(w, map[string]any{"id": a.ID, "version": a.Version, "identity": managedIdentityForAccount(a), "privacy": privacy, "upstream": upstream})
}
func (p *Proxy) handleManagedReauthorizeBatch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Items []struct {
			ID                  string          `json:"id"`
			Version             int64           `json:"version"`
			Credentials         json.RawMessage `json:"credentials"`
			AllowIdentityChange bool            `json:"allow_identity_change"`
		} `json:"items"`
	}
	if err := readJSONLimit(w, r, &input, 8<<20); err != nil {
		adminError(w, err)
		return
	}
	items := []managedAccountItem{}
	for _, item := range input.Items {
		items = append(items, managedAccountItem{ID: item.ID, Version: item.Version})
	}
	if err := validateManagedItems(items, 10); err != nil {
		adminError(w, err)
		return
	}
	results := []map[string]any{}
	succeeded := 0
	for _, item := range input.Items {
		result := map[string]any{"id": item.ID, "ok": false}
		incoming, err := parseAccount("reauthorize", item.Credentials)
		var saved Account
		if err == nil {
			saved, err = p.Store.reauthorizeManagedAccount(r.Context(), item.ID, item.Version, incoming, item.AllowIdentityChange)
		}
		if err != nil {
			code := managedAccountErrorCode(err)
			if errors.Is(err, errManagedIdentityChange) {
				code = "identity_change_requires_confirmation"
			}
			result["code"] = code
			result["error"] = "account credentials could not be replaced"
		} else {
			result["ok"] = true
			result["version"] = saved.Version
			succeeded++
		}
		results = append(results, result)
	}
	if succeeded > 0 {
		p.invalidateAccounts()
	}
	writeJSON(w, map[string]any{"results": results, "succeeded": succeeded, "failed": len(results) - succeeded})
}
