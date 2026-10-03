package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const managedAccountSelectionLimit = 10000

var errManagedExportTooLarge = errors.New("export exceeds supported account count or backup size; narrow the filters")

type managedAccountFilters struct {
	Search          string `json:"search,omitempty"`
	GroupID         string `json:"group_id,omitempty"`
	ProxyID         string `json:"proxy_id,omitempty"`
	Status          string `json:"status,omitempty"`
	Privacy         string `json:"privacy,omitempty"`
	privacyIDs      []string
	Tag             string `json:"tag,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"`
	HasRefreshToken *bool  `json:"has_refresh_token,omitempty"`
	Ungrouped       *bool  `json:"ungrouped,omitempty"`
	NoProxy         *bool  `json:"no_proxy,omitempty"`
	ExpiresBefore   int64  `json:"expires_before,omitempty"`
	ExpiresAfter    int64  `json:"expires_after,omitempty"`
	SortBy          string `json:"sort_by,omitempty"`
	SortOrder       string `json:"sort_order,omitempty"`
}

var managedAccountSort = map[string]string{
	"id": `a.id COLLATE "C"`, "status": "",
	"created_at": "a.created_at", "updated_at": "a.updated_at", "name": `a.display_name COLLATE "C"`,
	"priority": "a.priority", "weight": "a.weight", "concurrency": "a.concurrency",
	"quota": "a.quota", "expires_at": "a.expires_at",
	"last_used_at": "last_usage.last_at",
}

func (f *managedAccountFilters) normalize() error {
	f.Search = strings.TrimSpace(f.Search)
	if f.SortBy == "" {
		f.SortBy = "created_at"
	}
	if f.SortOrder == "" {
		f.SortOrder = "desc"
	}
	if _, ok := managedAccountSort[f.SortBy]; !ok || (f.SortOrder != "asc" && f.SortOrder != "desc") {
		return fmt.Errorf("%w: unsupported account sort", errInvalid)
	}
	if f.Status != "" && !accountHas([]string{"ok", "error", "unverified", "disabled", "expired"}, f.Status) {
		return fmt.Errorf("%w: invalid account status", errInvalid)
	}
	if f.Privacy != "" && f.Privacy != "disabled" && f.Privacy != "unknown" {
		return fmt.Errorf("%w: privacy must be disabled or unknown", errInvalid)
	}
	for _, value := range []string{f.Search, f.GroupID, f.ProxyID, f.Tag} {
		if len(value) > 500 {
			return fmt.Errorf("%w: account filter exceeds 500 bytes", errInvalid)
		}
	}
	if f.ExpiresAfter < 0 || f.ExpiresBefore < 0 || f.ExpiresBefore > 253402300799 || f.ExpiresAfter > 253402300799 || (f.ExpiresBefore > 0 && f.ExpiresAfter >= f.ExpiresBefore) {
		return fmt.Errorf("%w: invalid expiry range", errInvalid)
	}
	return nil
}

func parseManagedAccountQuery(q url.Values) (managedAccountFilters, int, int, error) {
	f := managedAccountFilters{}
	page, size := 1, 50
	for key, values := range q {
		if len(values) != 1 {
			return f, 0, 0, errors.New("duplicate account filter")
		}
		v := values[0]
		switch key {
		case "search":
			f.Search = v
		case "group_id":
			f.GroupID = v
		case "proxy_id":
			f.ProxyID = v
		case "status":
			f.Status = v
		case "privacy":
			f.Privacy = v
		case "tag":
			f.Tag = v
		case "sort_by":
			f.SortBy = v
		case "sort_order":
			f.SortOrder = v
		case "enabled", "has_refresh_token", "ungrouped", "no_proxy":
			if v == "" {
				continue
			}
			b, err := strconv.ParseBool(v)
			if err != nil {
				return f, 0, 0, errors.New("invalid boolean account filter")
			}
			switch key {
			case "enabled":
				f.Enabled = &b
			case "has_refresh_token":
				f.HasRefreshToken = &b
			case "ungrouped":
				f.Ungrouped = &b
			case "no_proxy":
				f.NoProxy = &b
			}
		case "expires_before", "expires_after":
			if v == "" {
				continue
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return f, 0, 0, errors.New("invalid expiry filter")
			}
			if key == "expires_before" {
				f.ExpiresBefore = n
			} else {
				f.ExpiresAfter = n
			}
		case "page", "page_size":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return f, 0, 0, errors.New("invalid account pagination")
			}
			if key == "page" {
				page = n
			} else {
				size = n
			}
		default:
			return f, 0, 0, errors.New("unknown account filter")
		}
	}
	if size > 500 || page > 20000 {
		return f, 0, 0, errors.New("page_size must be 1 to 500 and page at most 20000")
	}
	return f, page, size, f.normalize()
}

func managedAccountWhere(f managedAccountFilters, now int64) (string, []any) {
	conditions, args := []string{"TRUE"}, []any{}
	add := func(expr string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(expr, len(args)))
	}
	if f.Privacy != "" {
		expression := "a.id=ANY($%d::text[])"
		if f.Privacy == "unknown" {
			expression = "NOT (" + expression + ")"
		}
		add(expression, f.privacyIDs)
	}
	if f.Search != "" {
		add("strpos(lower(a.display_name || ' ' || a.id || ' ' || a.notes || ' ' || array_to_string(a.tags,' ')),lower($%d))>0", f.Search)
	}
	if f.GroupID != "" {
		add("EXISTS(SELECT 1 FROM account_groups g WHERE g.account_id=a.id AND g.group_id=$%d)", f.GroupID)
	}
	if f.ProxyID != "" {
		add("EXISTS(SELECT 1 FROM account_proxies p WHERE p.account_id=a.id AND p.proxy_id=$%d)", f.ProxyID)
	}
	if f.Tag != "" {
		add("$%d=ANY(a.tags)", f.Tag)
	}
	if f.Enabled != nil {
		add("a.enabled=$%d", *f.Enabled)
	}
	if f.HasRefreshToken != nil {
		add("a.has_refresh_token=$%d", *f.HasRefreshToken)
	}
	if f.Ungrouped != nil {
		add("(NOT EXISTS(SELECT 1 FROM account_groups g WHERE g.account_id=a.id))=$%d", *f.Ungrouped)
	}
	if f.NoProxy != nil {
		add("(NOT EXISTS(SELECT 1 FROM account_proxies p WHERE p.account_id=a.id))=$%d", *f.NoProxy)
	}
	if f.ExpiresBefore > 0 {
		add("a.expires_at>0 AND a.expires_at<$%d", f.ExpiresBefore)
	}
	if f.ExpiresAfter > 0 {
		add("a.expires_at>=$%d", f.ExpiresAfter)
	}
	if f.Status != "" {
		args = append(args, now)
		expr := managedAccountStatusSQL(fmt.Sprintf("$%d", len(args)), managedCurrentRequestStatusSQL("status_state"))
		add(expr+"=$%d", f.Status)
	}
	return strings.Join(conditions, " AND "), args
}

func (p *Proxy) managedAccounts(ctx context.Context, f managedAccountFilters, offset, limit int, credentials bool) ([]Account, int64, error) {
	now := time.Now().Unix()
	if f.Privacy != "" {
		var err error
		f.privacyIDs, err = p.managedDisabledPrivacyIDs(ctx, now)
		if err != nil {
			return nil, 0, err
		}
	}
	if p.Store.DB != nil {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		tx, err := p.Store.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, 0, err
		}
		defer tx.Rollback(context.Background())
		where, args := managedAccountWhere(f, now)
		join, nullOrder := "", ""
		if f.Status != "" || f.SortBy == "status" {
			join = "LEFT JOIN account_credential_request_state status_state ON status_state.account_ref=a.id "
		}
		var total int64
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM accounts a "+join+"WHERE "+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
		sortExpression := managedAccountSort[f.SortBy]
		if f.SortBy == "status" {
			args = append(args, now)
			sortExpression = managedAccountStatusSQL(fmt.Sprintf("$%d", len(args)), managedCurrentRequestStatusSQL("status_state")) + ` COLLATE "C"`
		}
		args = append(args, limit, offset)
		if f.SortBy == "last_used_at" {
			// This durable snapshot is advanced in the same transaction as each
			// event and survives detail cleanup. Sorting must not aggregate the
			// entire event history, or use legacy totals with invented timestamps.
			join += "LEFT JOIN account_request_state last_usage ON last_usage.account_ref=a.id AND last_usage.last_at>to_timestamp(0) "
			nullOrder = " NULLS LAST"
		}
		suffix := join + "WHERE " + where + " ORDER BY " + sortExpression + " " + f.SortOrder + nullOrder + `,a.id COLLATE "C" ` + f.SortOrder + fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
		accounts, err := p.Store.queryAccounts(ctx, tx, suffix, args, credentials)
		if err != nil {
			return nil, 0, err
		}
		return accounts, total, tx.Commit(ctx)
	}
	var accounts []Account
	var err error
	if credentials {
		_, accounts, err = p.Store.List()
	} else {
		_, accounts, err = p.Store.ListMetadata()
	}
	if err != nil {
		return nil, 0, err
	}
	lastStatuses := map[string]string{}
	if f.Status != "" || f.SortBy == "status" {
		lastStatuses, err = p.managedAccountLastStatuses(ctx, accounts)
		if err != nil {
			return nil, 0, err
		}
	}
	matches := []Account{}
	for _, a := range accounts {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if f.Privacy != "" && (accountHas(f.privacyIDs, a.ID) != (f.Privacy == "disabled")) {
			continue
		}
		if f.Search != "" && !strings.Contains(strings.ToLower(a.Name+" "+a.ID+" "+a.Notes+" "+strings.Join(a.Tags, " ")), strings.ToLower(f.Search)) {
			continue
		}
		if f.GroupID != "" && !accountHas(a.Groups, f.GroupID) || f.ProxyID != "" && !accountHas(a.Proxies, f.ProxyID) || f.Tag != "" && !accountHas(a.Tags, f.Tag) {
			continue
		}
		if f.Enabled != nil && a.Enabled != *f.Enabled || f.HasRefreshToken != nil && a.HasRefreshToken != *f.HasRefreshToken || f.Ungrouped != nil && (len(a.Groups) == 0) != *f.Ungrouped || f.NoProxy != nil && (len(a.Proxies) == 0) != *f.NoProxy {
			continue
		}
		if f.ExpiresBefore > 0 && (a.ExpiresAt <= 0 || a.ExpiresAt >= f.ExpiresBefore) || f.ExpiresAfter > 0 && a.ExpiresAt < f.ExpiresAfter {
			continue
		}
		if f.Status != "" && accountManagementStatus(a, lastStatuses[a.ID], now) != f.Status {
			continue
		}
		matches = append(matches, a)
	}
	lastUsed := map[string]*int64{}
	if f.SortBy == "last_used_at" && len(matches) > 0 {
		ids := make([]string, len(matches))
		for i, a := range matches {
			ids[i] = a.ID
		}
		rows, _, err := p.managedAccountLastUsed(ctx, ids)
		if err != nil {
			return nil, 0, err
		}
		for _, row := range rows {
			lastUsed[row.ID] = row.At
		}
	}
	value := func(a Account) int64 {
		switch f.SortBy {
		case "last_used_at":
			if at := lastUsed[a.ID]; at != nil {
				return *at
			}
			return 0
		case "updated_at":
			return a.UpdatedAt
		case "priority":
			return int64(a.Priority)
		case "weight":
			return int64(a.effectiveWeight())
		case "concurrency":
			return int64(a.effectiveConcurrency())
		case "quota":
			return a.Quota
		case "expires_at":
			return a.ExpiresAt
		default:
			return a.CreatedAt
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if f.SortBy == "last_used_at" && (lastUsed[a.ID] == nil) != (lastUsed[b.ID] == nil) {
			return lastUsed[a.ID] != nil
		}
		cmp := 0
		if f.SortBy == "name" {
			cmp = strings.Compare(a.Name, b.Name)
		} else if f.SortBy == "id" {
			cmp = strings.Compare(a.ID, b.ID)
		} else if f.SortBy == "status" {
			cmp = strings.Compare(accountManagementStatus(a, lastStatuses[a.ID], now), accountManagementStatus(b, lastStatuses[b.ID], now))
		} else if value(a) < value(b) {
			cmp = -1
		} else if value(a) > value(b) {
			cmp = 1
		}
		if cmp == 0 {
			cmp = strings.Compare(a.ID, b.ID)
		}
		if f.SortOrder == "asc" {
			return cmp < 0
		}
		return cmp > 0
	})
	total := int64(len(matches))
	if offset >= len(matches) {
		return []Account{}, total, nil
	}
	end := offset + limit
	if end > len(matches) {
		end = len(matches)
	}
	return matches[offset:end], total, nil
}

func (p *Proxy) handleManagedAccounts(w http.ResponseWriter, r *http.Request) {
	f, page, size, err := parseManagedAccountQuery(r.URL.Query())
	if err != nil {
		adminError(w, err)
		return
	}
	accounts, total, err := p.managedAccounts(r.Context(), f, (page-1)*size, size, false)
	if err != nil {
		p.storeError(w, err)
		return
	}
	summaries, err := p.accountSummaries(r, accounts)
	if err != nil {
		p.storeError(w, err)
		return
	}
	payload := map[string]any{"accounts": summaries, "total": total, "total_kind": "exact", "page": page, "page_size": size, "has_more": int64(page*size) < total}
	raw, err := json.Marshal(payload)
	if err != nil {
		p.storeError(w, err)
		return
	}
	digest := sha256.Sum256(raw)
	etag := `"` + hex.EncodeToString(digest[:]) + `"`
	w.Header().Set("ETag", etag)
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	// Reuse the representation hashed above instead of serializing every account
	// twice. Preserve writeJSON's trailing newline and Content-Type contract.
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(raw, '\n'))
}

type managedAccountItem struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}
type managedAccountSelection struct {
	Items   []managedAccountItem   `json:"items,omitempty"`
	Filters *managedAccountFilters `json:"filters,omitempty"`
}

func (p *Proxy) handleManagedAccountSelection(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Filters managedAccountFilters `json:"filters"`
	}
	if err := readJSONLimit(w, r, &input, 32<<10); err != nil {
		adminError(w, err)
		return
	}
	if err := input.Filters.normalize(); err != nil {
		adminError(w, err)
		return
	}
	accounts, total, err := p.managedAccounts(r.Context(), input.Filters, 0, managedAccountSelectionLimit+1, false)
	if err != nil {
		p.storeError(w, err)
		return
	}
	if total > managedAccountSelectionLimit {
		adminAPIError(w, 413, "selection_too_large", "selection exceeds 10000 accounts; narrow the filters")
		return
	}
	items := make([]managedAccountItem, 0, len(accounts))
	for _, a := range accounts {
		items = append(items, managedAccountItem{a.ID, a.Version})
	}
	raw, _ := json.Marshal(input.Filters)
	sum := sha256.Sum256(raw)
	writeJSON(w, map[string]any{"items": items, "total": total, "limit": managedAccountSelectionLimit, "filter_hash": hex.EncodeToString(sum[:]), "selected_at": time.Now().Unix()})
}

func validateManagedItems(items []managedAccountItem, limit int) error {
	if len(items) < 1 || len(items) > limit {
		return fmt.Errorf("items must contain 1 to %d accounts", limit)
	}
	seen := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || len(item.ID) > 128 || item.Version < 1 || seen[item.ID] {
			return errors.New("items require unique account IDs and positive versions")
		}
		seen[item.ID] = true
	}
	return nil
}

func managedAccountErrorCode(err error) string {
	switch {
	case errors.Is(err, errVersionConflict), errors.Is(err, ErrTokenConflict):
		return "version_conflict"
	case errors.Is(err, errNotFound):
		return "not_found"
	case errors.Is(err, errConflict):
		return "conflict"
	case errors.Is(err, errInvalid):
		return "invalid_request"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "operation_failed"
	}
}

func (p *Proxy) handleManagedAccountActions(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Action string               `json:"action"`
		Items  []managedAccountItem `json:"items"`
		Patch  *accountPatch        `json:"patch,omitempty"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	limit := 100
	if input.Action == "refresh" {
		limit = 10
	}
	if err := validateManagedItems(input.Items, limit); err != nil {
		adminError(w, err)
		return
	}
	if !accountHas([]string{"update", "delete", "refresh"}, input.Action) || (input.Action == "update") != (input.Patch != nil) {
		adminError(w, errors.New("action must be update with patch, delete or refresh without patch"))
		return
	}
	if input.Patch != nil {
		if err := validateAccountPatch(*input.Patch); err != nil {
			adminError(w, err)
			return
		}
		if input.Patch.Version != 0 || input.Patch.Priority != nil && (*input.Patch.Priority < -2147483648 || *input.Patch.Priority > 2147483647) || input.Patch.Quota != nil && *input.Patch.Quota > 9007199254740991 {
			adminError(w, errors.New("patch version is supplied per item; priority must fit int32 and quota must be a safe integer"))
			return
		}
	}
	// Resolve all stable IDs once.  The mutation below still performs its own
	// CAS read/transaction, so a concurrent edit between this snapshot and the
	// write remains a normal version_conflict rather than a stale success.  The
	// old per-item GetMetadataContext call made update/delete/refresh actions do
	// an extra pool checkout and query for every selected account.
	ids := make([]string, len(input.Items))
	for i, item := range input.Items {
		ids[i] = item.ID
	}
	metadata, metadataErr := p.Store.metadataByIDsContext(r.Context(), ids)
	results := make([]map[string]any, 0, len(input.Items))
	succeeded := 0
	for _, item := range input.Items {
		result := map[string]any{"id": item.ID, "ok": false}
		err := r.Context().Err()
		var updated Account
		if err == nil {
			var current Account
			if metadataErr != nil {
				// A transient bulk-read failure must not turn an otherwise
				// independent batch into an all-or-nothing result.  Retry the
				// original per-item lookup so successful items can still commit;
				// cancellation/deadline errors return immediately from this call.
				current, err = p.Store.GetMetadataContext(r.Context(), item.ID)
				if err == nil && current.ID != item.ID {
					err = errNotFound
				}
			} else {
				var found bool
				current, found = metadata[item.ID]
				if !found {
					err = errNotFound
				}
			}
			if err == nil && current.Version != item.Version {
				err = errVersionConflict
			}
			if err == nil {
				switch input.Action {
				case "update":
					patch := *input.Patch
					patch.Version = item.Version
					updated, err = p.applyAccountPatchContext(r.Context(), item.ID, patch)
				case "delete":
					err = p.Store.DeleteVersion(item.ID, item.Version)
					if err == nil {
						p.cleanupManagedDeletedAccountSchedules(r.Context(), item.ID)
					}
				case "refresh":
					var account Account
					account, err = p.Store.Get(item.ID)
					if err == nil && account.Version != item.Version {
						err = errVersionConflict
					}
					if err == nil && account.RefreshToken == "" {
						err = fmt.Errorf("%w: no refresh token", errInvalid)
					}
					if err == nil {
						called := false
						updated, err = p.Store.Refresh(item.ID, account.AccessToken, func(current Account) (Account, error) {
							called = true
							if current.Version != item.Version {
								return current, errManagedRefreshPrecondition
							}
							return p.refreshAccountWithFingerprint(r.Context(), current)
						})
						if err == nil && !called {
							err = errVersionConflict
						}
					}
				}
			}
		}
		if err != nil {
			result["code"] = managedAccountErrorCode(err)
			result["error"] = "account operation failed"
		} else {
			succeeded++
			result["ok"] = true
			if updated.Version > 0 {
				result["version"] = updated.Version
			}
		}
		results = append(results, result)
	}
	if succeeded > 0 {
		p.invalidateAccounts()
	}
	writeJSON(w, map[string]any{"results": results, "succeeded": succeeded, "failed": len(input.Items) - succeeded})
}

var errManagedIdentityChange = errors.New("upstream identity changes require explicit confirmation")
var errManagedRefreshPrecondition = fmt.Errorf("management refresh precondition: %w", errVersionConflict)

// Management-only replacement uses CAS for both metadata and credentials. It
// intentionally does not reuse a missing refresh token from old credentials.
func managedReauthorizedAccount(current Account, id string, version int64, incoming Account, allowIdentityChange bool) (Account, error) {
	if current.ID != id {
		return Account{}, errNotFound
	}
	if current.Version != version {
		return Account{}, errVersionConflict
	}
	if managedIdentityChanged(current, incoming) && !allowIdentityChange {
		return Account{}, errManagedIdentityChange
	}
	updated := mergeImport(current, incoming)
	updated.Name = current.Name
	updated.HasRefreshToken = updated.RefreshToken != ""
	updated.CredentialStatus = "unverified"
	updated.LastCheckedAt = 0
	return updated, nil
}

func (s Store) reauthorizeManagedAccount(ctx context.Context, id string, version int64, incoming Account, allowIdentityChange bool) (Account, error) {
	apply := func(current Account) (Account, error) {
		return managedReauthorizedAccount(current, id, version, incoming, allowIdentityChange)
	}
	if s.DB == nil {
		var updated Account
		err := s.modify(func(state *accountState) error {
			for i, current := range state.Accounts {
				if current.ID == id {
					var err error
					updated, err = apply(current)
					if err != nil {
						return err
					}
					for _, other := range state.Accounts {
						if other.ID != id && other.AccessToken == updated.AccessToken {
							return errConflict
						}
					}
					state.Accounts[i] = updated
					return nil
				}
			}
			return errNotFound
		})
		return accountMetadata(updated), err
	}
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback(context.Background())
	current, err := s.pgLockManagedCredentialAccount(ctx, tx, id)
	if err != nil {
		return Account{}, err
	}
	updated, err := s.pgReauthorizeManagedAccountFrom(ctx, tx, current, id, version, incoming, allowIdentityChange)
	if err != nil {
		return Account{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Account{}, err
	}
	s.DB.invalidate()
	return updated, nil
}

// Keep credential replacement and legacy OAuth acknowledgement recovery under
// the same locks. Every caller uses import -> credentials -> metadata order.
func (s Store) pgLockManagedCredentialAccount(ctx context.Context, tx pgx.Tx, id string) (Account, error) {
	// Serialize credential de-duplication with imports and other reauthorizations.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('codex-agent:import:' || current_schema(), 0))`); err != nil {
		return Account{}, err
	}
	// Same lock order as credential refresh failure: credentials, then metadata.
	if _, err := tx.Exec(ctx, "SELECT 1 FROM account_credentials WHERE account_id=$1 FOR UPDATE", id); err != nil {
		return Account{}, err
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM accounts WHERE id=$1 FOR NO KEY UPDATE", id); err != nil {
		return Account{}, err
	}
	current, err := s.pgGetFrom(ctx, tx, id, true)
	if err != nil {
		return Account{}, err
	}
	return current, nil
}

// Caller owns the transaction and has locked current via
// pgLockManagedCredentialAccount. No commit or pool acquisition occurs here.
func (s Store) pgReauthorizeManagedAccountFrom(ctx context.Context, tx pgx.Tx, current Account, id string, version int64, incoming Account, allowIdentityChange bool) (Account, error) {
	updated, err := managedReauthorizedAccount(current, id, version, incoming, allowIdentityChange)
	if err != nil {
		return Account{}, err
	}
	var duplicate bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM account_credentials WHERE token_fingerprint=$1 AND account_id<>$2)", tokenFingerprint(updated.AccessToken), id).Scan(&duplicate); err != nil {
		return Account{}, err
	}
	if duplicate {
		return Account{}, errConflict
	}
	if err = s.pgWriteTokens(ctx, tx, current, updated); err != nil {
		return Account{}, err
	}
	return accountMetadata(updated), nil
}

func (p *Proxy) handleManagedAccountReauthorize(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version             int64           `json:"version"`
		Credentials         json.RawMessage `json:"credentials"`
		AllowIdentityChange bool            `json:"allow_identity_change"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 1 {
		adminError(w, errors.New("positive version is required"))
		return
	}
	incoming, err := parseAccount("reauthorize", input.Credentials)
	if err != nil {
		adminError(w, errors.New("invalid OAuth credentials"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	updated, err := p.Store.reauthorizeManagedAccount(ctx, r.PathValue("name"), input.Version, incoming, input.AllowIdentityChange)
	if errors.Is(err, errManagedIdentityChange) {
		adminAPIError(w, 409, "identity_change_requires_confirmation", err.Error())
		return
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	p.invalidateAccounts()
	writeJSON(w, map[string]any{"ok": true, "id": updated.ID, "version": updated.Version})
}

func (p *Proxy) managedExportAccounts(ctx context.Context, input managedAccountSelection, credentials bool) ([]Account, error) {
	if (input.Filters != nil) == (input.Items != nil) {
		return nil, fmt.Errorf("%w: provide filters or items", errInvalid)
	}
	if input.Filters != nil {
		if err := input.Filters.normalize(); err != nil {
			return nil, err
		}
		accounts, total, err := p.managedAccounts(ctx, *input.Filters, 0, managedAccountSelectionLimit+1, credentials)
		if err != nil {
			return nil, err
		}
		if total > managedAccountSelectionLimit {
			return nil, errManagedExportTooLarge
		}
		return accounts, nil
	}
	if err := validateManagedItems(input.Items, managedAccountSelectionLimit); err != nil {
		return nil, fmt.Errorf("%w: %s", errInvalid, err)
	}
	ids := make([]string, len(input.Items))
	for i, item := range input.Items {
		ids[i] = item.ID
	}
	var accounts []Account
	if p.Store.DB != nil {
		tx, err := p.Store.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(context.Background())
		accounts, err = p.Store.queryAccounts(ctx, tx, "WHERE a.id=ANY($1::text[])", []any{ids}, credentials)
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, err
		}
	} else {
		var err error
		if credentials {
			_, accounts, err = p.Store.List()
		} else {
			_, accounts, err = p.Store.ListMetadata()
		}
		if err != nil {
			return nil, err
		}
	}
	byID := map[string]Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	result := make([]Account, 0, len(ids))
	for _, item := range input.Items {
		a, ok := byID[item.ID]
		if !ok {
			return nil, errNotFound
		}
		if a.Version != item.Version {
			return nil, errVersionConflict
		}
		result = append(result, a)
	}
	return result, nil
}

type managedAccountBackup struct {
	Name        string          `json:"name"`
	Notes       string          `json:"notes"`
	Tags        []string        `json:"tags"`
	Enabled     bool            `json:"enabled"`
	Concurrency int             `json:"concurrency"`
	Priority    int             `json:"priority"`
	Weight      int             `json:"weight"`
	Groups      []string        `json:"groups"`
	Proxies     []string        `json:"proxies"`
	Quota       int64           `json:"quota"`
	WindowDays  int             `json:"window_days"`
	WindowStart int64           `json:"window_start"`
	CreatedAt   int64           `json:"created_at"`
	Credentials json.RawMessage `json:"credentials"`
}
type managedAccountBundle struct {
	Type       string                   `json:"type"`
	Version    int                      `json:"version"`
	ExportedAt int64                    `json:"exported_at"`
	Accounts   []managedAccountBackup   `json:"accounts"`
	Resources  *managedBundleResources  `json:"resources,omitempty"`
	Management *managedBundleManagement `json:"management,omitempty"`
}

func (p *Proxy) handleManagedAccountExport(w http.ResponseWriter, r *http.Request) {
	p.exportManagedAccounts(w, r, false)
}
func (p *Proxy) handleManagedCredentialExport(w http.ResponseWriter, r *http.Request) {
	p.exportManagedAccounts(w, r, true)
}
func (p *Proxy) exportManagedAccounts(w http.ResponseWriter, r *http.Request, credentials bool) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		managedAccountSelection
		IncludeResources        bool `json:"include_resources"`
		IncludeProxyCredentials bool `json:"include_proxy_credentials"`
		ConfirmProxyCredentials bool `json:"confirm_proxy_credentials"`
		IncludeLeases           bool `json:"include_leases"`
		IncludeCosts            bool `json:"include_costs"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	if input.IncludeCosts {
		adminAPIError(w, http.StatusBadRequest, "unsupported", "account cost export is not supported")
		return
	}
	if err := validateManagedResourceExport(input.IncludeResources, input.IncludeProxyCredentials, input.ConfirmProxyCredentials, credentials); err != nil {
		adminError(w, err)
		return
	}
	if input.IncludeLeases && !credentials {
		adminError(w, errInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	accounts, err := p.managedExportAccounts(ctx, input.managedAccountSelection, credentials)
	if errors.Is(err, errManagedExportTooLarge) {
		adminAPIError(w, 413, "export_too_large", err.Error())
		return
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	var payload any
	if !credentials {
		summaries, err := p.accountSummaries(r, accounts)
		if err != nil {
			p.storeError(w, err)
			return
		}
		payload = map[string]any{"type": "codex-agent-account-metadata", "version": 1, "exported_at": time.Now().Unix(), "accounts": summaries}
		w.Header().Set("Content-Disposition", `attachment; filename="codex-agent-account-metadata.json"`)
	} else {
		bundle := managedAccountBundle{Type: "codex-agent-accounts", Version: 1, ExportedAt: time.Now().Unix(), Accounts: make([]managedAccountBackup, 0, len(accounts))}
		for _, a := range accounts {
			secret := canonicalManagedCredentials(a)
			bundle.Accounts = append(bundle.Accounts, managedAccountBackup{a.Name, a.Notes, a.Tags, a.Enabled, a.effectiveConcurrency(), a.Priority, a.effectiveWeight(), a.Groups, a.Proxies, a.Quota, a.effectiveWindowDays(), a.WindowStart, a.CreatedAt, secret})
		}
		if input.IncludeResources {
			bundle.Resources, err = p.exportManagedBundleResources(ctx, accounts, input.IncludeProxyCredentials)
			if err != nil {
				p.storeError(w, err)
				return
			}
		}
		if input.IncludeLeases {
			bundle.Management, err = p.exportManagedBundleLeases(ctx, accounts)
			if err != nil {
				p.storeError(w, err)
				return
			}
		}
		payload = bundle
		w.Header().Set("Content-Disposition", `attachment; filename="codex-agent-accounts-credentials.json"`)
	}
	raw, err := json.Marshal(payload)
	// Leave space within the 32 MiB import request for its envelope and mapping.
	if err != nil || len(raw) > 30<<20 {
		w.Header().Del("Content-Disposition")
		adminAPIError(w, 413, "export_too_large", errManagedExportTooLarge.Error())
		return
	}
	if credentials && p.Store.DB != nil {
		if _, err := p.Store.DB.Pool.Exec(ctx, "INSERT INTO audit_logs(action,entity_id) VALUES('account.credentials.export',$1)", strconv.Itoa(len(accounts))); err != nil {
			w.Header().Del("Content-Disposition")
			p.storeError(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(raw, '\n'))
}

// Native bundles preserve local management values. Foreign references need an
// explicit mapping; generic Sub2API imports retain their existing behavior.
func (p *Proxy) handleManagedAccountBundleImport(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Data         managedAccountImportBundle `json:"data"`
		DryRun       bool                       `json:"dry_run"`
		Replace      bool                       `json:"replace"`
		GroupMapping map[string]string          `json:"group_mapping,omitempty"`
		ProxyMapping map[string]string          `json:"proxy_mapping,omitempty"`
	}
	if err := readJSONLimit(w, r, &input, adminBodyLimit); err != nil {
		adminError(w, err)
		return
	}
	if input.Data.Type != "codex-agent-accounts" || input.Data.Version != 1 || len(input.Data.Accounts) < 1 || len(input.Data.Accounts) > managedAccountSelectionLimit {
		adminError(w, errors.New("expected a version 1 codex-agent-accounts bundle containing 1 to 10000 accounts"))
		return
	}
	if err := validateManagedBundleLeases(input.Data.Management, len(input.Data.Accounts)); err != nil {
		adminError(w, err)
		return
	}
	groupIDs, proxyIDs, err := p.managedBundleBindingIDs(r.Context(), input.Data, input.GroupMapping, input.ProxyMapping)
	if err != nil {
		p.storeError(w, err)
		return
	}
	resolve := func(refs []string, mapping map[string]string, known map[string]bool) ([]string, error) {
		result := []string{}
		for _, ref := range refs {
			if target, ok := mapping[ref]; ok {
				ref = target
			}
			if !known[ref] {
				return nil, errors.New("bundle references an unknown group or proxy; provide an explicit local mapping")
			}
			result = append(result, ref)
		}
		return uniqueStrings(result), nil
	}
	parsed := make([]Account, len(input.Data.Accounts))
	report := ImportReport{Results: make([]ImportResult, len(parsed))}
	for i, entry := range input.Data.Accounts {
		result := ImportResult{Index: i + 1, Name: entry.Name}
		account, err := parseAccount(entry.Name, entry.Credentials)
		if err == nil {
			patch := accountPatch{Name: &entry.Name, Concurrency: &entry.Concurrency, Priority: &entry.Priority, Weight: &entry.Weight, Quota: &entry.Quota, WindowDays: &entry.WindowDays}
			err = validateAccountPatch(patch)
			// Match generic import's persisted metadata limits, including older
			// notes/tags larger than the interactive edit form permits.
			if len(entry.Notes) > 65536 || len(entry.Tags) > 100 {
				err = errInvalid
			}
			for _, tag := range entry.Tags {
				if len(tag) > 256 {
					err = errInvalid
				}
			}
			if entry.WindowStart < 0 || entry.CreatedAt < 0 || entry.WindowStart > 253402300799 || entry.CreatedAt > 253402300799 {
				err = errInvalid
			}
			if err == nil {
				account.Groups, err = resolve(entry.Groups, input.GroupMapping, groupIDs)
			}
			if err == nil {
				account.Proxies, err = resolve(entry.Proxies, input.ProxyMapping, proxyIDs)
			}
			account.Name, account.Notes, account.Tags, account.Enabled = entry.Name, entry.Notes, uniqueStrings(entry.Tags), entry.Enabled
			account.Concurrency, account.Priority, account.Weight = entry.Concurrency, entry.Priority, entry.Weight
			account.Quota, account.WindowDays, account.WindowStart, account.CreatedAt = entry.Quota, entry.WindowDays, entry.WindowStart, entry.CreatedAt
			account.ID, account.Version, account.CredentialVersion = accountID(), 1, 1
			account.CredentialStatus = "unverified"
			account.HasRefreshToken = account.RefreshToken != ""
			account.UpdatedAt = time.Now().Unix()
			if account.CreatedAt == 0 {
				account.CreatedAt = account.UpdatedAt
			}
			normalizeAccount(&account)
		}
		if err != nil {
			result.Status, result.Code, result.Error = "failed", "invalid_bundle_account", "invalid account credentials, management values or unmapped references"
			report.Failed++
		} else {
			parsed[i] = account
		}
		report.Results[i] = result
	}
	if p.Store.DB != nil {
		report, err = p.Store.pgImportDetailedContext(r.Context(), parsed, report, ImportOptions{Replace: input.Replace, DryRun: input.DryRun})
	} else {
		dryRun := errors.New("native import preview rollback")
		err = p.Store.modify(func(state *accountState) error {
			for i, incoming := range parsed {
				result := &report.Results[i]
				if result.Status == "failed" {
					continue
				}
				duplicate, index := -1, -1
				for j, current := range state.Accounts {
					if current.AccessToken == incoming.AccessToken {
						duplicate = j
					}
					if current.Name == incoming.Name {
						index = j
					}
				}
				if duplicate >= 0 {
					updated, rotated, rotationErr := importedRefreshRotation(state.Accounts[duplicate], incoming)
					if rotationErr != nil {
						result.Status, result.Code, result.Error = "failed", "identity_conflict", "duplicate access token has conflicting identity"
						report.Failed++
						continue
					}
					if rotated {
						state.Accounts[duplicate] = updated
						result.Status, result.ID = "replaced", updated.ID
						report.Replaced++
					} else {
						result.Status, result.ID = "duplicate", updated.ID
						report.Duplicate++
					}
					continue
				}
				if index >= 0 {
					if !input.Replace {
						result.Status, result.Code, result.Error = "failed", "name_conflict", "an account with this name already exists; replacement must be explicit"
						report.Failed++
						continue
					}
					state.Accounts[index] = mergeImport(state.Accounts[index], incoming)
					result.Status, result.ID = "replaced", state.Accounts[index].ID
					report.Replaced++
				} else {
					if len(state.Accounts) == 0 {
						state.Active = incoming.Name
					}
					state.Accounts = append(state.Accounts, incoming)
					result.Status, result.ID = "added", incoming.ID
					report.Added++
				}
			}
			if input.DryRun {
				return dryRun
			}
			return nil
		})
		if errors.Is(err, dryRun) {
			err = nil
		}
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	if !input.DryRun {
		p.invalidateAccounts()
	}
	response := map[string]any{"imported": report.Added + report.Replaced, "added": report.Added, "duplicate": report.Duplicate, "replaced": report.Replaced, "failed": report.Failed, "results": report.Results, "dry_run": input.DryRun}
	if input.Data.Management != nil {
		response["lease_restore"] = p.prepareManagedBundleLeases(r.Context(), input.Data.Management, report, parsed, input.DryRun)
	}
	writeJSON(w, response)
}
