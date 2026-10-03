package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type sub2DataPayload struct {
	Type       string                       `json:"type,omitempty"`
	Version    int                          `json:"version,omitempty"`
	ExportedAt string                       `json:"exported_at,omitempty"`
	Proxies    []map[string]json.RawMessage `json:"proxies"`
	Accounts   []map[string]json.RawMessage `json:"accounts"`
}

func sub2ProxyKey(px ProxyEntry) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", px.Type, px.Host, px.Port, px.Username, px.Password)
}

func (p *Proxy) sub2ExportData(w http.ResponseWriter, r *http.Request) {
	includeProxies := true
	if value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("include_proxies"))); value != "" {
		switch value {
		case "true", "1", "yes", "on":
		case "false", "0", "no", "off":
			includeProxies = false
		default:
			compatError(w, r, sub2Invalid("invalid include_proxies"))
			return
		}
	}
	selected := map[int64]bool{}
	for _, value := range r.URL.Query()["ids"] {
		for _, part := range strings.Split(value, ",") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil || id <= 0 {
				compatError(w, r, sub2Invalid("invalid account ID"))
				return
			}
			selected[id] = true
		}
	}
	var rows []adminCompatAccount
	var err error
	if len(selected) > 0 {
		// Explicit IDs are an export selection, not a post-filter. Resolve the
		// numeric compatibility IDs first, then hydrate only the selected rows
		// and their credentials in PostgreSQL. This avoids decrypting every
		// account when a caller exports one account from a large catalog.
		rows, err = p.sub2SelectedAccounts(r.Context(), r, selected)
	} else {
		rows, err = p.sub2FilteredAccounts(r.Context(), r, true)
	}
	if err != nil {
		compatError(w, r, err)
		return
	}
	var proxies []ProxyEntry
	if includeProxies {
		wanted := make(map[string]struct{})
		for _, row := range rows {
			if len(row.Account.Proxies) > 0 {
				wanted[row.Account.Proxies[0]] = struct{}{}
			}
		}
		if len(selected) > 0 {
			proxies, err = p.Proxies.managedSelectedCredentials(r.Context(), wanted)
		} else {
			proxies, err = p.Proxies.List()
		}
		if err != nil {
			compatError(w, r, err)
			return
		}
	}
	byID := map[string]ProxyEntry{}
	for _, px := range proxies {
		byID[px.ID] = px
	}
	data := sub2DataPayload{ExportedAt: time.Now().UTC().Format(time.RFC3339), Proxies: []map[string]json.RawMessage{}, Accounts: []map[string]json.RawMessage{}}
	used := map[string]bool{}
	for _, row := range rows {
		if len(selected) > 0 && !selected[compatNumericID(row.Account.ID)] {
			continue
		}
		dto := sub2AccountDTO(row)
		account := map[string]json.RawMessage{}
		for _, key := range []string{"name", "notes", "platform", "type", "extra", "concurrency", "priority", "rate_multiplier", "expires_at", "auto_pause_on_expired"} {
			account[key] = sub2Raw(dto[key])
		}
		account["credentials"] = sub2Raw(sub2AccountCredentials(row, true))
		if includeProxies && len(row.Account.Proxies) > 0 {
			if px, ok := byID[row.Account.Proxies[0]]; ok {
				account["proxy_key"] = sub2Raw(sub2ProxyKey(px))
				used[px.ID] = true
			}
		}
		data.Accounts = append(data.Accounts, account)
	}
	if len(selected) > 0 && len(data.Accounts) != len(selected) {
		compatError(w, r, errNotFound)
		return
	}
	for _, px := range proxies {
		if !used[px.ID] {
			continue
		}
		dto := sub2ProxyDTO(px, true)
		item := map[string]json.RawMessage{}
		for _, key := range []string{"name", "protocol", "host", "port", "username", "password", "status"} {
			item[key] = sub2Raw(dto[key])
		}
		item["proxy_key"] = sub2Raw(sub2ProxyKey(px))
		data.Proxies = append(data.Proxies, item)
	}
	sub2Success(w, data)
}

func (p *Proxy) sub2ImportData(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Data                 sub2DataPayload `json:"data"`
		SkipDefaultGroupBind *bool           `json:"skip_default_group_bind"`
	}
	if err := compatReadJSON(w, r, &input, 16<<20); err != nil {
		compatError(w, r, err)
		return
	}
	data := input.Data
	if (data.Type != "" && data.Type != "sub2api-data" && data.Type != "sub2api-bundle") || (data.Version != 0 && data.Version != 1) || data.Proxies == nil || data.Accounts == nil {
		compatError(w, r, sub2Invalid("invalid data header; proxies and accounts arrays are required"))
		return
	}
	if len(data.Accounts)+len(data.Proxies) > 10000 {
		compatError(w, r, sub2Invalid("data contains too many rows"))
		return
	}
	if input.SkipDefaultGroupBind != nil && !*input.SkipDefaultGroupBind {
		compatError(w, r, sub2Invalid("this gateway has no implicit default group; bind group_ids explicitly after import"))
		return
	}
	proxies, err := p.Proxies.List()
	if err != nil {
		compatError(w, r, err)
		return
	}
	byKey := map[string]ProxyEntry{}
	for _, px := range proxies {
		byKey[sub2ProxyKey(px)] = px
	}
	counts := map[string]any{"proxy_created": 0, "proxy_reused": 0, "proxy_failed": 0, "account_created": 0, "account_failed": 0}
	issues := []map[string]string{}
	addError := func(kind, name, key string, err error) {
		counts[kind+"_failed"] = counts[kind+"_failed"].(int) + 1
		issues = append(issues, map[string]string{"kind": kind, "name": name, "proxy_key": key, "message": err.Error()})
	}
	for _, item := range data.Proxies {
		if err := r.Context().Err(); err != nil {
			compatError(w, r, err)
			return
		}
		name := sub2String(item, "name", "imported-proxy")
		key := sub2String(item, "proxy_key", "")
		px := ProxyEntry{Name: name, Enabled: true}
		var rowErr error
		if rowErr = sub2ValidateResourceSettings(item, false); rowErr == nil {
			for field, target := range map[string]*string{"protocol": &px.Type, "host": &px.Host, "username": &px.Username, "password": &px.Password} {
				if rowErr = sub2Field(item, field, target); rowErr != nil {
					break
				}
			}
		}
		if rowErr == nil {
			rowErr = sub2Field(item, "port", &px.Port)
		}
		status := sub2String(item, "status", "active")
		if status != "active" && status != "inactive" {
			rowErr = sub2Invalid("invalid proxy status")
		}
		px.Enabled = status == "active"
		if rowErr == nil {
			rowErr = validProxy(px)
		}
		if key == "" {
			key = sub2ProxyKey(px)
		}
		if rowErr != nil {
			addError("proxy", name, key, rowErr)
			continue
		}
		if existing, ok := byKey[key]; ok {
			if existing.Enabled != px.Enabled {
				existing, rowErr = p.Proxies.Update(existing.ID, func(current *ProxyEntry) error { current.Enabled = px.Enabled; return nil })
			}
			if rowErr != nil {
				addError("proxy", name, key, rowErr)
				continue
			}
			byKey[key] = existing
			counts["proxy_reused"] = counts["proxy_reused"].(int) + 1
			continue
		}
		if !px.Enabled {
			addError("proxy", name, key, sub2Invalid("inactive proxy import requires an existing proxy"))
			continue
		}
		px, rowErr = p.Proxies.Create(px)
		if rowErr != nil {
			addError("proxy", name, key, rowErr)
			continue
		}
		byKey[key] = px
		counts["proxy_created"] = counts["proxy_created"].(int) + 1
	}
	for _, item := range data.Accounts {
		if err := r.Context().Err(); err != nil {
			compatError(w, r, err)
			return
		}
		name, key := sub2String(item, "name", ""), sub2String(item, "proxy_key", "")
		if key != "" {
			px, ok := byKey[key]
			if !ok {
				addError("account", name, key, sub2Invalid("proxy_key not found"))
				continue
			}
			item["proxy_id"] = sub2Raw(compatNumericID(px.ID))
		}
		delete(item, "group_ids") // Portable Sub2API data deliberately excludes local bindings.
		if _, err := p.sub2ApplyAccount(r.Context(), "", item); err != nil {
			addError("account", name, key, err)
			continue
		}
		counts["account_created"] = counts["account_created"].(int) + 1
	}
	if len(issues) > 0 {
		counts["errors"] = issues
	}
	p.invalidateAccounts()
	sub2Success(w, counts)
}
