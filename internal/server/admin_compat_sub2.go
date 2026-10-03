package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The compatibility surface shares the native account store. Sub2API SaaS
// billing/provider features are not emulated by accepting ineffective settings.
func (p *Proxy) registerSub2CompatRoutes(mux *http.ServeMux) {
	base := "/api/v1/admin"
	mux.HandleFunc("GET "+base+"/capabilities", p.sub2Capabilities)
	mux.HandleFunc("GET "+base+"/accounts", p.sub2ListAccounts)
	mux.HandleFunc("POST "+base+"/accounts", p.sub2WriteAccount)
	mux.HandleFunc("GET "+base+"/accounts/data", p.sub2ExportData)
	mux.HandleFunc("POST "+base+"/accounts/data", p.sub2ImportData)
	mux.HandleFunc("GET "+base+"/accounts/{id}", p.sub2GetAccount)
	mux.HandleFunc("PUT "+base+"/accounts/{id}", p.sub2WriteAccount)
	mux.HandleFunc("DELETE "+base+"/accounts/{id}", p.sub2DeleteAccount)
	mux.HandleFunc("POST "+base+"/accounts/{id}/drain", p.sub2AccountDrain)
	mux.HandleFunc("GET "+base+"/accounts/{id}/drain", p.sub2AccountDrain)
	for _, action := range []string{"schedulable", "clear-error", "refresh", "test", "set-privacy"} {
		mux.HandleFunc("POST "+base+"/accounts/{id}/"+action, p.sub2AccountAction)
	}
	for _, resource := range []string{"groups", "proxies"} {
		mux.HandleFunc("GET "+base+"/"+resource, p.sub2Resources)
		mux.HandleFunc("GET "+base+"/"+resource+"/all", p.sub2Resources)
		mux.HandleFunc("POST "+base+"/"+resource, p.sub2Resources)
		mux.HandleFunc("GET "+base+"/"+resource+"/{id}", p.sub2Resources)
		mux.HandleFunc("PUT "+base+"/"+resource+"/{id}", p.sub2Resources)
		mux.HandleFunc("DELETE "+base+"/"+resource+"/{id}", p.sub2Resources)
	}
}

func sub2Success(w http.ResponseWriter, data any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"code": 0, "message": "success", "data": data})
}

func sub2Invalid(message string) error { return fmt.Errorf("%w: %s", errInvalid, message) }
func sub2Raw(v any) json.RawMessage    { b, _ := json.Marshal(v); return b }
func sub2String(m map[string]json.RawMessage, key string, fallback string) string {
	var value string
	if json.Unmarshal(m[key], &value) == nil {
		return value
	}
	return fallback
}
func sub2Bool(m map[string]json.RawMessage, key string, fallback bool) bool {
	var value bool
	if json.Unmarshal(m[key], &value) == nil && string(m[key]) != "null" {
		return value
	}
	return fallback
}
func sub2Object(raw json.RawMessage) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m
}
func sub2Field(m map[string]json.RawMessage, key string, out any) error {
	if raw, ok := m[key]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, out); err != nil {
			return sub2Invalid("invalid " + key)
		}
	}
	return nil
}
func sub2Timestamp(at int64) any {
	if at <= 0 {
		return nil
	}
	return time.Unix(at, 0).UTC().Format(time.RFC3339)
}

func sub2AccountCredentials(row adminCompatAccount, secrets bool) map[string]json.RawMessage {
	stored := sub2Object(row.Record.Sub2["credentials"])
	m := map[string]json.RawMessage{}
	// Foreign credential objects may contain arbitrary token aliases or nested
	// envelopes. Only documented scalar metadata belongs in ordinary DTOs.
	for _, key := range []string{"chatgpt_user_id", "organization_id", "expires_in", "scope", "token_type", "email", "plan_type"} {
		if raw, ok := stored[key]; ok {
			var scalar any
			if json.Unmarshal(raw, &scalar) == nil {
				switch scalar.(type) {
				case string, float64, bool, nil:
					m[key] = raw
				}
			}
		}
	}
	a := row.Account
	m["chatgpt_account_id"], m["account_id"], m["client_id"], m["expires_at"] = sub2Raw(a.AccountID), sub2Raw(a.AccountID), sub2Raw(a.ClientID), sub2Raw(a.ExpiresAt)
	if secrets {
		m["access_token"], m["refresh_token"] = sub2Raw(a.AccessToken), sub2Raw(a.RefreshToken)
		if token := sub2String(stored, "id_token", ""); token != "" {
			m["id_token"] = sub2Raw(token)
		}
	}
	return m
}

func sub2AccountStatus(row adminCompatAccount) (string, bool) {
	status, schedulable, _, _ := sub2AccountAvailability(row, time.Now().Unix())
	return status, schedulable
}

// Report temporary billing unavailability separately from the user's enabled
// preference. Expiry removes only our own stale payment message; unrelated
// account errors and explicit disabled settings keep their existing meaning.
func sub2AccountAvailability(row adminCompatAccount, now int64) (status string, schedulable bool, message string, paymentUntil int64) {
	a, meta := row.Account, row.Record.Sub2
	status = sub2String(meta, "status", "active")
	schedulable = sub2Bool(meta, "schedulable", a.Enabled)
	message = a.LastError
	if a.CredentialStatus == gatewayPaymentStatus {
		until := a.LastCheckedAt + int64(gatewayPaymentCooldown/time.Second)
		if until > now {
			paymentUntil = until
		} else if message == gatewayPaymentMessage {
			message = ""
		}
	}
	if a.Enabled {
		status, schedulable = "active", true
	}
	if !a.Enabled && status == "active" && schedulable {
		schedulable = false
	}
	if message != "" && status == "active" {
		status = "error"
	}
	if paymentUntil > 0 {
		status, schedulable = "error", false
	}
	return
}

func sub2AccountDTO(row adminCompatAccount) map[string]any {
	a, meta := row.Account, row.Record.Sub2
	status, schedulable, message, paymentUntil := sub2AccountAvailability(row, time.Now().Unix())
	var temporaryUntil any
	temporaryReason := ""
	if paymentUntil > 0 {
		temporaryUntil, temporaryReason = sub2Timestamp(paymentUntil), gatewayPaymentMessage
	}
	groups := make([]int64, 0, len(a.Groups))
	for _, id := range a.Groups {
		groups = append(groups, compatNumericID(id))
	}
	var proxyID any
	if len(a.Proxies) > 0 {
		proxyID = compatNumericID(a.Proxies[0])
	}
	var expiresAt any
	var expiry int64
	_ = json.Unmarshal(meta["expires_at"], &expiry)
	if expiry > 0 {
		expiresAt = expiry
	}
	credentials := sub2AccountCredentials(row, false)
	return map[string]any{
		"id": compatNumericID(a.ID), "name": a.Name, "notes": a.Notes, "platform": "openai", "type": "oauth",
		"credentials": credentials, "credentials_status": map[string]bool{"has_access_token": a.AccessToken != "", "has_refresh_token": a.HasRefreshToken || a.RefreshToken != "", "has_id_token": sub2String(sub2Object(meta["credentials"]), "id_token", "") != ""},
		"extra": sub2Object(meta["extra"]), "group_ids": groups, "proxy_id": proxyID,
		"concurrency": a.effectiveConcurrency(), "priority": -int64(a.Priority), "rate_multiplier": 1,
		"status": status, "schedulable": schedulable, "error_message": message,
		"expires_at": expiresAt, "auto_pause_on_expired": sub2Bool(meta, "auto_pause_on_expired", true),
		"created_at": sub2Timestamp(a.CreatedAt), "updated_at": sub2Timestamp(a.UpdatedAt),
		"last_used_at": nil, "rate_limited_at": nil, "rate_limit_reset_at": nil, "overload_until": nil, "temp_unschedulable_until": temporaryUntil, "temp_unschedulable_reason": temporaryReason,
	}
}

func sub2Pagination(r *http.Request) (int, int) {
	page, size := 1, 20
	if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && n > 0 && n <= 1000000 {
		page = n
	}
	raw := r.URL.Query().Get("page_size")
	if raw == "" {
		raw = r.URL.Query().Get("limit")
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1000 {
		size = n
	}
	return page, size
}

func sub2Page(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	page, size := sub2Pagination(r)
	total := len(items)
	start := min((page-1)*size, total)
	end := min(start+size, total)
	sub2Success(w, map[string]any{"items": items[start:end], "total": total, "page": page, "page_size": size, "pages": max(1, (total+size-1)/size)})
}

func (p *Proxy) sub2FilteredAccounts(ctx context.Context, r *http.Request, secrets bool) ([]adminCompatAccount, error) {
	rows, err := p.compatListAccounts(ctx, secrets)
	if err != nil {
		return nil, err
	}
	return sub2FilterAccountRows(r, rows), nil
}

func sub2FilterAccountRows(r *http.Request, rows []adminCompatAccount) []adminCompatAccount {
	q := r.URL.Query()
	if v := q.Get("platform"); v != "" && v != "openai" {
		return []adminCompatAccount{}
	}
	if v := q.Get("type"); v != "" && v != "oauth" {
		return []adminCompatAccount{}
	}
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))
	out := make([]adminCompatAccount, 0, len(rows))
	for _, row := range rows {
		a := row.Account
		extra := sub2Object(row.Record.Sub2["extra"])
		if search != "" && !strings.Contains(strings.ToLower(a.Name+" "+a.Notes+" "+sub2String(extra, "email", "")), search) {
			continue
		}
		if group := q.Get("group"); group != "" && group != "0" {
			found := false
			for _, id := range a.Groups {
				if strconv.FormatInt(compatNumericID(id), 10) == group {
					found = true
				}
			}
			if group == "ungrouped" {
				found = len(a.Groups) == 0
			}
			if !found {
				continue
			}
		}
		if status := q.Get("status"); status != "" {
			actual, _ := sub2AccountStatus(row)
			if actual != status {
				continue
			}
		}
		if privacy := q.Get("privacy_mode"); privacy != "" && sub2String(extra, "privacy_mode", "") != privacy {
			continue
		}
		out = append(out, row)
	}
	field, descending := q.Get("sort_by"), q.Get("sort_order") == "desc"
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Account, out[j].Account
		cmp := 0
		switch field {
		case "id":
			if compatNumericID(a.ID) < compatNumericID(b.ID) {
				cmp = -1
			} else if compatNumericID(a.ID) > compatNumericID(b.ID) {
				cmp = 1
			}
		case "priority", "concurrency", "created_at", "updated_at":
			v1, v2 := -int64(a.Priority), -int64(b.Priority)
			if field == "concurrency" {
				v1, v2 = int64(a.Concurrency), int64(b.Concurrency)
			}
			if field == "created_at" {
				v1, v2 = a.CreatedAt, b.CreatedAt
			}
			if field == "updated_at" {
				v1, v2 = a.UpdatedAt, b.UpdatedAt
			}
			if v1 < v2 {
				cmp = -1
			} else if v1 > v2 {
				cmp = 1
			}
		default:
			cmp = strings.Compare(a.Name, b.Name)
		}
		if cmp == 0 {
			cmp = strings.Compare(a.ID, b.ID)
		}
		if descending {
			return cmp > 0
		}
		return cmp < 0
	})
	return out
}

func (p *Proxy) sub2ListAccounts(w http.ResponseWriter, r *http.Request) {
	rows, total, err := p.sub2AccountPage(r)
	if err != nil {
		compatError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, sub2AccountDTO(row))
	}
	page, size := sub2Pagination(r)
	sub2Success(w, map[string]any{"items": items, "total": total, "page": page, "page_size": size, "pages": max(1, (total+size-1)/size)})
}
func (p *Proxy) sub2LoadAccount(r *http.Request) (adminCompatAccount, error) {
	ref := r.PathValue("id")
	if n, err := strconv.ParseInt(ref, 10, 64); err != nil || n <= 0 {
		return adminCompatAccount{}, sub2Invalid("invalid account ID")
	}
	id, err := p.compatResolveAccount(r.Context(), ref)
	if err != nil {
		return adminCompatAccount{}, err
	}
	return p.compatGetAccount(r.Context(), id)
}
func (p *Proxy) sub2GetAccount(w http.ResponseWriter, r *http.Request) {
	row, err := p.sub2LoadAccount(r)
	if err != nil {
		compatError(w, r, err)
		return
	}
	sub2Success(w, sub2AccountDTO(row))
}
func (p *Proxy) sub2DeleteAccount(w http.ResponseWriter, r *http.Request) {
	ctx, err := accountDrainDeleteContext(r)
	if err != nil {
		accountDrainHTTPError(w, r, err)
		return
	}
	row, err := p.sub2LoadAccount(r)
	if err == nil {
		err = p.compatDeleteAccount(ctx, row.Account.ID, 0)
	}
	if err != nil {
		accountDrainHTTPError(w, r, err)
		return
	}
	sub2Success(w, map[string]string{"message": "Account deleted successfully"})
}

func (p *Proxy) sub2ApplyAccount(ctx context.Context, id string, input map[string]json.RawMessage) (adminCompatAccount, error) {
	creating := id == ""
	platform, kind := "openai", "oauth"
	if err := sub2Field(input, "platform", &platform); err != nil {
		return adminCompatAccount{}, err
	}
	if err := sub2Field(input, "type", &kind); err != nil {
		return adminCompatAccount{}, err
	}
	if platform != "openai" || kind != "oauth" {
		return adminCompatAccount{}, sub2Invalid("only OpenAI OAuth accounts are supported")
	}
	if creating && (len(input["name"]) == 0 || len(input["credentials"]) == 0 || len(input["platform"]) == 0 || len(input["type"]) == 0 || string(input["platform"]) == "null" || string(input["type"]) == "null") {
		return adminCompatAccount{}, sub2Invalid("name, platform, type and credentials are required")
	}
	for _, field := range []string{"rate_multiplier", "load_factor", "upstream_billing_probe_enabled", "upstream_billing_rate_sync_enabled"} {
		if raw, ok := input[field]; ok && string(raw) != "null" {
			if field == "rate_multiplier" {
				var rate float64
				if json.Unmarshal(raw, &rate) == nil && rate == 1 {
					continue
				}
			}
			if strings.HasSuffix(field, "_enabled") && string(raw) == "false" {
				continue
			}
			return adminCompatAccount{}, sub2Invalid(field + " is not supported by this gateway")
		}
	}
	var groups []string
	if raw, ok := input["group_ids"]; ok && string(raw) != "null" {
		var ids []int64
		if json.Unmarshal(raw, &ids) != nil {
			return adminCompatAccount{}, sub2Invalid("invalid group_ids")
		}
		known, err := p.sub2BindingIDs(ctx, ids, true)
		if err != nil {
			return adminCompatAccount{}, err
		}
		for _, num := range ids {
			found := known[num]
			if found == "" {
				return adminCompatAccount{}, sub2Invalid("unknown group ID")
			}
			groups = append(groups, found)
		}
	}
	var proxy string
	if raw, ok := input["proxy_id"]; ok && string(raw) != "null" {
		var num int64
		if json.Unmarshal(raw, &num) != nil || num < 0 {
			return adminCompatAccount{}, sub2Invalid("invalid proxy_id")
		}
		if num > 0 {
			known, err := p.sub2BindingIDs(ctx, []int64{num}, false)
			if err != nil {
				return adminCompatAccount{}, err
			}
			proxy = known[num]
			if proxy == "" {
				return adminCompatAccount{}, sub2Invalid("unknown proxy ID")
			}
		}
	}
	return p.compatMutateAccount(ctx, id, 0, func(a *Account, record *adminCompatRecord) error {
		if record.Sub2 == nil {
			record.Sub2 = map[string]json.RawMessage{}
		}
		meta := record.Sub2
		// A native/CPA enable operation is authoritative even when an older
		// Sub2API request left a disabled desired-state marker in the metadata.
		if !creating && a.Enabled {
			meta["status"], meta["schedulable"] = sub2Raw("active"), sub2Raw(true)
		}
		if creating {
			a.Enabled = true
			a.Concurrency = 1
			a.Weight = 1
			meta["status"] = sub2Raw("active")
			meta["schedulable"] = sub2Raw(true)
			meta["auto_pause_on_expired"] = sub2Raw(true)
		}
		if err := sub2Field(input, "name", &a.Name); err != nil {
			return err
		}
		if err := validAccountName(a.Name); err != nil {
			return sub2Invalid("invalid account name")
		}
		if err := sub2Field(input, "notes", &a.Notes); err != nil {
			return err
		}
		if err := sub2Field(input, "concurrency", &a.Concurrency); err != nil {
			return err
		}
		if a.Concurrency < 0 || a.Concurrency > 1000 {
			return sub2Invalid("invalid concurrency")
		}
		if a.Concurrency == 0 {
			a.Concurrency = 1
		}
		priority := -int64(a.Priority)
		if err := sub2Field(input, "priority", &priority); err != nil {
			return err
		}
		if priority < -2147483647 || priority > 2147483647 {
			return sub2Invalid("invalid priority")
		}
		a.Priority = -int(priority)
		if raw, ok := input["group_ids"]; ok && string(raw) != "null" {
			a.Groups = uniqueStrings(groups)
		}
		if raw, ok := input["proxy_id"]; ok && string(raw) != "null" {
			a.Proxies = []string{}
			if proxy != "" {
				a.Proxies = []string{proxy}
			}
		}
		if raw, ok := input["credentials"]; ok && string(raw) != "null" {
			var supplied map[string]json.RawMessage
			if json.Unmarshal(raw, &supplied) != nil || supplied == nil {
				return sub2Invalid("credentials must be an object")
			}
			if len(supplied) > 0 {
				current := sub2AccountCredentials(adminCompatAccount{Account: *a, Record: *record}, true)
				for _, key := range []string{"access_token", "refresh_token", "id_token", "client_secret"} {
					if _, exists := supplied[key]; !exists {
						if v, yes := current[key]; yes {
							supplied[key] = v
						}
					}
				}
				if value, ok := supplied["model_mapping"]; ok && string(value) != "null" && string(value) != "{}" {
					return sub2Invalid("account model_mapping is not supported; configure native API-key model restrictions")
				}
				if _, provided := supplied["account_id"]; !provided {
					if explicit, yes := supplied["chatgpt_account_id"]; yes {
						supplied["account_id"] = explicit
					} else if a.AccountID != "" {
						supplied["account_id"] = sub2Raw(a.AccountID)
					}
				}
				if _, provided := supplied["client_id"]; !provided && a.ClientID != "" {
					supplied["client_id"] = sub2Raw(a.ClientID)
				}
				if _, provided := supplied["expires_at"]; !provided && sub2String(supplied, "access_token", "") == a.AccessToken {
					supplied["expires_at"] = sub2Raw(a.ExpiresAt)
				}
				parsed, err := parseAccount(a.Name, sub2Raw(map[string]any{"platform": "openai", "type": "oauth", "credentials": supplied}))
				if err != nil {
					return sub2Invalid("invalid OAuth credentials")
				}
				a.AccessToken, a.RefreshToken, a.AccountID, a.ClientID, a.ExpiresAt, a.Identity = parsed.AccessToken, parsed.RefreshToken, parsed.AccountID, parsed.ClientID, parsed.ExpiresAt, parsed.Identity
				meta["credentials"] = sub2Raw(supplied)
			}
		}
		if a.AccessToken == "" {
			return sub2Invalid("access_token is required")
		}
		if raw, ok := input["extra"]; ok && string(raw) != "null" {
			var extra map[string]json.RawMessage
			if json.Unmarshal(raw, &extra) != nil || extra == nil {
				return sub2Invalid("extra must be an object")
			}
			meta["extra"] = sub2Raw(extra)
		}
		if raw, ok := input["expires_at"]; ok && string(raw) != "null" {
			var expiry int64
			if json.Unmarshal(raw, &expiry) != nil || expiry > 253402300799 {
				return sub2Invalid("invalid expires_at")
			}
			meta["expires_at"] = sub2Raw(max(int64(0), expiry))
		}
		if raw, ok := input["auto_pause_on_expired"]; ok && string(raw) != "null" {
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				return sub2Invalid("invalid auto_pause_on_expired")
			}
			meta["auto_pause_on_expired"] = sub2Raw(v)
		}
		if raw, ok := input["status"]; ok && string(raw) != "null" {
			var status string
			if json.Unmarshal(raw, &status) != nil || !accountHas([]string{"active", "inactive", "error"}, status) {
				return sub2Invalid("invalid status")
			}
			meta["status"] = sub2Raw(status)
		}
		if raw, ok := input["schedulable"]; ok && string(raw) != "null" {
			var sched bool
			if json.Unmarshal(raw, &sched) != nil {
				return sub2Invalid("invalid schedulable")
			}
			meta["schedulable"] = sub2Raw(sched)
		}
		if creating || input["status"] != nil || input["schedulable"] != nil {
			a.Enabled = sub2String(meta, "status", "active") == "active" && sub2Bool(meta, "schedulable", true)
		}
		return nil
	})
}

func (p *Proxy) sub2WriteAccount(w http.ResponseWriter, r *http.Request) {
	var input map[string]json.RawMessage
	if err := compatReadJSON(w, r, &input, 2<<20); err != nil {
		compatError(w, r, err)
		return
	}
	id := ""
	if r.Method == http.MethodPut {
		row, err := p.sub2LoadAccount(r)
		if err != nil {
			compatError(w, r, err)
			return
		}
		id = row.Account.ID
	}
	row, err := p.sub2ApplyAccount(r.Context(), id, input)
	if err != nil {
		compatError(w, r, err)
		return
	}
	sub2Success(w, sub2AccountDTO(row))
}
