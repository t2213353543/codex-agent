package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

func sub2GroupDTO(g Group) map[string]any {
	return map[string]any{"id": compatNumericID(g.ID), "name": g.Name, "description": g.Description, "platform": "openai", "status": "active", "rate_multiplier": 1, "created_at": sub2Timestamp(g.CreatedAt), "updated_at": sub2Timestamp(g.UpdatedAt)}
}
func sub2ProxyDTO(px ProxyEntry, secrets bool) map[string]any {
	status := "active"
	if !px.Enabled {
		status = "inactive"
	}
	m := map[string]any{"id": compatNumericID(px.ID), "name": px.Name, "protocol": px.Type, "host": px.Host, "port": px.Port, "username": px.Username, "status": status, "created_at": sub2Timestamp(px.CreatedAt), "updated_at": sub2Timestamp(px.UpdatedAt), "has_password": px.HasPassword || px.Password != ""}
	if secrets {
		m["password"] = px.Password
	}
	return m
}

func (p *Proxy) sub2ResourceDelete(w http.ResponseWriter, r *http.Request, id string, group bool) {
	// Native deletion owns relation cleanup and live-cache invalidation.
	r.SetPathValue("id", id)
	capture := &sub2Capture{header: make(http.Header)}
	if group {
		p.handleGroupDelete(capture, r)
	} else {
		p.handleProxyDelete(capture, r)
	}
	if capture.status >= 400 {
		message := "management storage operation failed"
		switch capture.status {
		case http.StatusNotFound:
			message = "not found"
		case http.StatusConflict:
			message = "conflicting account or resource"
		case http.StatusServiceUnavailable:
			message = "management operation canceled or timed out"
		}
		compatErrorStatus(w, r, capture.status, message)
		return
	}
	sub2Success(w, map[string]string{"message": "Deleted successfully"})
}

func sub2ValidateResourceSettings(input map[string]json.RawMessage, group bool) error {
	if platform := sub2String(input, "platform", "openai"); platform != "openai" {
		return sub2Invalid("only OpenAI groups are supported")
	}
	for _, field := range []string{"expires_at", "backup_proxy_id", "fallback_mode", "expiry_warn_days", "subscription_type", "daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd", "model_routing", "model_allowlist", "rate_multiplier"} {
		raw, ok := input[field]
		if !ok || string(raw) == "null" {
			continue
		}
		if field == "rate_multiplier" && string(raw) == "1" {
			continue
		}
		if field == "fallback_mode" && (string(raw) == `"none"` || string(raw) == `""`) {
			continue
		}
		if field == "subscription_type" && string(raw) == `"standard"` {
			continue
		}
		if string(raw) == "0" || string(raw) == "[]" || string(raw) == "{}" {
			continue
		}
		return sub2Invalid(field + " is not supported for this resource")
	}
	if group && sub2String(input, "status", "active") != "active" {
		return sub2Invalid("inactive groups are not supported")
	}
	return nil
}

func (p *Proxy) sub2Resources(w http.ResponseWriter, r *http.Request) {
	group := strings.HasPrefix(r.URL.Path, "/api/v1/admin/groups")
	ref := r.PathValue("id")
	if ref != "" {
		n, err := strconv.ParseInt(ref, 10, 64)
		if err != nil || n <= 0 {
			compatError(w, r, sub2Invalid("invalid resource ID"))
			return
		}
	}
	var input map[string]json.RawMessage
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		if err := compatReadJSON(w, r, &input, 1<<20); err != nil {
			compatError(w, r, err)
			return
		}
		if err := sub2ValidateResourceSettings(input, group); err != nil {
			compatError(w, r, err)
			return
		}
	}
	if group {
		p.sub2GroupResource(w, r, ref, input)
	} else {
		p.sub2ProxyResource(w, r, ref, input)
	}
}

func (p *Proxy) sub2GroupResource(w http.ResponseWriter, r *http.Request, ref string, input map[string]json.RawMessage) {
	// A create request has no resource id to resolve. Avoid loading every
	// existing group before Groups.Create performs the authoritative insert and
	// uniqueness check; this keeps POST cost independent of group cardinality.
	var groups []Group
	var err error
	var found Group
	loadCatalogue := r.Method != http.MethodPost || ref != ""
	if p.Store.DB != nil && ref != "" {
		found, _, err = p.sub2ResourceMetadataPG(r.Context(), ref, true)
		if err != nil {
			compatError(w, r, err)
			return
		}
		loadCatalogue = false
	}
	if p.Store.DB != nil && r.Method == http.MethodGet && ref == "" && !strings.HasSuffix(r.URL.Path, "/all") {
		loadCatalogue = false
	}
	if loadCatalogue {
		groups, err = p.Groups.List()
		if err != nil {
			compatError(w, r, err)
			return
		}
	}
	for _, g := range groups {
		if strconv.FormatInt(compatNumericID(g.ID), 10) == ref {
			if found.ID != "" {
				compatError(w, r, errConflict)
				return
			}
			found = g
		}
	}
	if ref != "" && found.ID == "" {
		compatError(w, r, errNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if ref != "" {
			sub2Success(w, sub2GroupDTO(found))
			return
		}
		// PostgreSQL list requests use a bounded query for ordinary pages.
		// `/all` intentionally retains its complete-catalogue contract.
		if p.Store.DB != nil && !strings.HasSuffix(r.URL.Path, "/all") {
			q := r.URL.Query()
			if (q.Get("platform") != "" && q.Get("platform") != "openai") || (q.Get("status") != "" && q.Get("status") != "active") {
				sub2Page(w, r, []map[string]any{})
				return
			}
			page, size := sub2Pagination(r)
			groups, total, err := p.Groups.listPGPage(q.Get("search"), (page-1)*size, size)
			if err != nil {
				compatError(w, r, err)
				return
			}
			items := make([]map[string]any, 0, len(groups))
			for _, g := range groups {
				items = append(items, sub2GroupDTO(g))
			}
			sub2Success(w, map[string]any{"items": items, "total": total, "page": page, "page_size": size, "pages": max(1, (total+size-1)/size)})
			return
		}
		items := make([]map[string]any, 0, len(groups))
		q := r.URL.Query()
		for _, g := range groups {
			if q.Get("platform") != "" && q.Get("platform") != "openai" {
				continue
			}
			if q.Get("status") != "" && q.Get("status") != "active" {
				continue
			}
			if !strings.Contains(strings.ToLower(g.Name), strings.ToLower(q.Get("search"))) {
				continue
			}
			items = append(items, sub2GroupDTO(g))
		}
		if strings.HasSuffix(r.URL.Path, "/all") {
			sub2Success(w, items)
		} else {
			sub2Page(w, r, items)
		}
	case http.MethodPost, http.MethodPut:
		name, description := found.Name, found.Description
		if err = sub2Field(input, "name", &name); err == nil {
			err = sub2Field(input, "description", &description)
		}
		if err == nil {
			if r.Method == http.MethodPost {
				found, err = p.Groups.Create(name, description)
			} else {
				found, err = p.Groups.Update(found.ID, name, description)
			}
		}
		if err != nil {
			compatError(w, r, err)
			return
		}
		p.invalidateAccounts()
		sub2Success(w, sub2GroupDTO(found))
	case http.MethodDelete:
		p.sub2ResourceDelete(w, r, found.ID, true)
	}
}

func (p *Proxy) sub2ProxyResource(w http.ResponseWriter, r *http.Request, ref string, input map[string]json.RawMessage) {
	// As with groups, POST /proxies has no existing id to resolve. The store's
	// insert transaction is the source of truth for uniqueness, so a full
	// metadata scan here only adds O(n) latency to every create.
	var proxies []ProxyEntry
	var err error
	var found ProxyEntry
	loadCatalogue := r.Method != http.MethodPost || ref != ""
	if p.Store.DB != nil && ref != "" {
		_, found, err = p.sub2ResourceMetadataPG(r.Context(), ref, false)
		if err != nil {
			compatError(w, r, err)
			return
		}
		loadCatalogue = false
	}
	if p.Store.DB != nil && r.Method == http.MethodGet && ref == "" && !strings.HasSuffix(r.URL.Path, "/all") {
		loadCatalogue = false
	}
	if loadCatalogue {
		proxies, err = p.Proxies.managedMetadata()
		if err != nil {
			compatError(w, r, err)
			return
		}
	}
	for _, px := range proxies {
		if strconv.FormatInt(compatNumericID(px.ID), 10) == ref {
			if found.ID != "" {
				compatError(w, r, errConflict)
				return
			}
			found = px
		}
	}
	if ref != "" && found.ID == "" {
		compatError(w, r, errNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if ref != "" {
			sub2Success(w, sub2ProxyDTO(found, false))
			return
		}
		// Metadata pages never need decrypted proxy passwords. Push filtering,
		// counting, and the page window into PostgreSQL instead of scanning every
		// proxy and allocating a full response-sized intermediate slice.
		if p.Store.DB != nil && !strings.HasSuffix(r.URL.Path, "/all") {
			q := r.URL.Query()
			status, protocol, search := q.Get("status"), q.Get("protocol"), q.Get("search")
			if status != "" && status != "active" && status != "inactive" {
				sub2Page(w, r, []map[string]any{})
				return
			}
			if protocol != "" && protocol != "http" && protocol != "socks5" {
				sub2Page(w, r, []map[string]any{})
				return
			}
			page, size := sub2Pagination(r)
			proxies, total, err := p.Proxies.listPGMetadataPage(status, protocol, search, (page-1)*size, size)
			if err != nil {
				compatError(w, r, err)
				return
			}
			items := make([]map[string]any, 0, len(proxies))
			for _, px := range proxies {
				items = append(items, sub2ProxyDTO(px, false))
			}
			sub2Success(w, map[string]any{"items": items, "total": total, "page": page, "page_size": size, "pages": max(1, (total+size-1)/size)})
			return
		}
		items := make([]map[string]any, 0, len(proxies))
		q := r.URL.Query()
		for _, px := range proxies {
			dto := sub2ProxyDTO(px, false)
			if q.Get("status") != "" && q.Get("status") != dto["status"] {
				continue
			}
			if q.Get("protocol") != "" && q.Get("protocol") != px.Type {
				continue
			}
			if !strings.Contains(strings.ToLower(px.Name+" "+px.Host), strings.ToLower(q.Get("search"))) {
				continue
			}
			items = append(items, dto)
		}
		if strings.HasSuffix(r.URL.Path, "/all") {
			sub2Success(w, items)
		} else {
			sub2Page(w, r, items)
		}
	case http.MethodPost, http.MethodPut:
		apply := func(px *ProxyEntry) error {
			for key, target := range map[string]*string{"name": &px.Name, "protocol": &px.Type, "host": &px.Host, "username": &px.Username, "password": &px.Password} {
				if err := sub2Field(input, key, target); err != nil {
					return err
				}
			}
			if err := sub2Field(input, "port", &px.Port); err != nil {
				return err
			}
			if raw, ok := input["status"]; ok {
				var status string
				if json.Unmarshal(raw, &status) != nil || !accountHas([]string{"active", "inactive"}, status) {
					return sub2Invalid("invalid proxy status")
				}
				px.Enabled = status == "active"
			}
			return validProxy(*px)
		}
		if r.Method == http.MethodPost {
			found.Enabled = true
			err = apply(&found)
			if err == nil && !found.Enabled {
				err = sub2Invalid("create proxy first, then set inactive status")
			}
			if err == nil {
				found, err = p.Proxies.Create(found)
			}
		} else {
			found, err = p.Proxies.Update(found.ID, apply)
		}
		if err != nil {
			compatError(w, r, err)
			return
		}
		p.invalidateAccounts()
		sub2Success(w, sub2ProxyDTO(found, false))
	case http.MethodDelete:
		p.sub2ResourceDelete(w, r, found.ID, false)
	}
}
