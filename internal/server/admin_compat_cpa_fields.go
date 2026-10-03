package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

func cpaSetMetadataPath(fields map[string]json.RawMessage, key string, value json.RawMessage) error {
	parts := strings.Split(key, ".")
	if len(parts) > 8 {
		return cpaInvalid("metadata path is too deep")
	}
	for _, part := range parts {
		if part == "" || part != strings.TrimSpace(part) {
			return cpaInvalid("invalid metadata field path")
		}
	}
	if len(parts) == 1 {
		fields[key] = bytes.Clone(value)
		return nil
	}
	root := map[string]json.RawMessage{}
	if raw := fields[parts[0]]; len(raw) > 0 {
		if json.Unmarshal(raw, &root) != nil || root == nil {
			root = map[string]json.RawMessage{}
		}
	}
	if err := cpaSetMetadataPath(root, strings.Join(parts[1:], "."), value); err != nil {
		return err
	}
	fields[parts[0]] = cpaRaw(root)
	return nil
}

func (p *Proxy) handleCPAFields(w http.ResponseWriter, r *http.Request) {
	var input map[string]json.RawMessage
	if err := compatReadJSON(w, r, &input, cpaFileLimit); err != nil {
		compatError(w, r, err)
		return
	}
	name := cpaString(input, "name")
	if name == "" {
		compatError(w, r, cpaInvalid("name is required"))
		return
	}
	delete(input, "name")
	if len(input) == 0 {
		compatError(w, r, cpaInvalid("no fields to update"))
		return
	}
	row, err := p.cpaFind(r.Context(), name, "")
	if err != nil {
		compatError(w, r, err)
		return
	}
	fields := p.cpaDownload(row)
	for key, value := range input {
		root := strings.Split(key, ".")[0]
		if root == "auth_index" || root == "id" || root == "type" || root == "provider" || root == "enabled" || root == "auth" || root == "credentials" || root == "tokens" {
			compatError(w, r, cpaInvalid("unsupported field mutation: "+root))
			return
		}
		// Credential fields are atomic values, never arbitrary nested objects.
		if strings.Contains(key, ".") {
			switch root {
			case "access_token", "refresh_token", "account_id", "client_id", "expires_at", "expired", "disabled", "priority", "proxy_url", "websockets", "headers", "prefix", "weight", "models", "group_ids":
				compatError(w, r, cpaInvalid("field does not support nested updates: "+root))
				return
			}
		}
		if err = cpaSetMetadataPath(fields, key, value); err != nil {
			compatError(w, r, err)
			return
		}
	}
	// expired is an alias; do not let the existing canonical expires_at shadow it.
	if _, ok := input["expired"]; ok {
		if _, explicit := input["expires_at"]; !explicit {
			delete(fields, "expires_at")
		}
	}
	if err := p.cpaValidateRuntime(fields); err != nil {
		compatError(w, r, err)
		return
	}
	incoming, err := cpaPrepareImport(compatFileName(row.Account, row.Record), fields)
	if err != nil {
		compatError(w, r, err)
		return
	}
	var proxies []string
	if raw, ok := input["proxy_url"]; ok {
		proxies, err = p.cpaBindProxy(raw)
		if err != nil {
			compatError(w, r, err)
			return
		}
	}
	_, err = p.compatMutateAccount(r.Context(), row.Account.ID, row.Account.Version, func(a *Account, record *adminCompatRecord) error {
		cpaRetainIdentityObservation(a, &incoming)
		a.AccessToken, a.RefreshToken, a.AccountID, a.ClientID, a.ExpiresAt, a.Identity = incoming.AccessToken, incoming.RefreshToken, incoming.AccountID, incoming.ClientID, incoming.ExpiresAt, incoming.Identity
		a.Enabled, a.Priority, a.Weight, a.Notes = incoming.Enabled, incoming.Priority, incoming.Weight, incoming.Notes
		a.Concurrency, a.Quota, a.WindowDays = incoming.Concurrency, incoming.Quota, incoming.WindowDays
		if _, ok := input["proxy_url"]; ok {
			a.Proxies = proxies
		}
		record.CPA = cpaPreservedMetadata(fields)
		return nil
	})
	if err != nil {
		compatError(w, r, err)
		return
	}
	cpaReply(w, 200, map[string]string{"status": "ok"})
}
