package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const adminBodyLimit = 32 << 20

var errAdminBodyTooLarge = errors.New("request body exceeds the size limit")

// readJSONLimit decodes one strict JSON object from the request body.
func readJSONLimit(w http.ResponseWriter, r *http.Request, target any, limit int64) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			return errAdminBodyTooLarge
		}
		return errors.New("invalid JSON body")
	}
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("body must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(target); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func adminError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, errAdminBodyTooLarge) {
		adminAPIError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	adminAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
}

func adminAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "proxy_error", "code": code, "message": hidePaths(message)}})
}

// storeError maps semantic errors to 400/404/409 and suppresses internal
// persistence details, which may contain connection strings or credentials.
func (p *Proxy) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		adminAPIError(w, http.StatusNotFound, "not_found", "entity not found")
	case errors.Is(err, errVersionConflict), errors.Is(err, ErrTokenConflict):
		adminAPIError(w, http.StatusConflict, "version_conflict", "entity changed; reload and retry")
	case errors.Is(err, errConflict):
		adminAPIError(w, http.StatusConflict, "conflict", "entity conflicts with an existing entity")
	case errors.Is(err, errInvalid):
		adminAPIError(w, http.StatusBadRequest, "invalid_request", "invalid management value")
	default:
		adminAPIError(w, http.StatusInternalServerError, "storage_error", "management operation could not be persisted")
	}
}

// todayStart returns the local midnight boundary.
func todayStart() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
}

// parseOverviewRange reads from/to/step query values with sane defaults.
func parseOverviewRange(query map[string][]string) (from, to, step int64) {
	to = time.Now().Unix()
	from = to - 24*3600
	if value := queryValue(query, "from"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
			from = parsed
		}
	}
	if value := queryValue(query, "to"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > from {
			to = parsed
		}
	}
	if value := queryValue(query, "step"); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && validStep(parsed) {
			step = parsed
		}
	}
	if step == 0 {
		span := to - from
		switch {
		case span <= 3*3600:
			step = 60
		case span <= 48*3600:
			step = 300
		case span <= 14*86400:
			step = 3600
		default:
			step = 86400
		}
	}
	for (to-from)/step > 600 {
		step *= 2
		if !validStep(step) {
			step = 86400
			break
		}
	}
	return from, to, step
}

func validStep(step int64) bool {
	switch step {
	case 60, 300, 3600, 86400:
		return true
	}
	return false
}

func queryValue(query map[string][]string, key string) string {
	values, ok := query[key]
	if !ok || len(values) == 0 {
		return ""
	}
	return values[0]
}

// ---- overview ----

func (p *Proxy) handleOverview(w http.ResponseWriter, r *http.Request) {
	if p.serveCachedOverview(w, r) {
		return
	}
	for _, name := range []string{"from", "to", "step"} {
		if value := r.URL.Query().Get(name); value != "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 1 || n > 253402300799 || name == "step" && !validStep(n) {
				adminError(w, errors.New("invalid overview range"))
				return
			}
		}
	}
	if r.URL.Query().Get("from") != "" && r.URL.Query().Get("to") != "" {
		from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		if to <= from {
			adminError(w, errors.New("to must be after from"))
			return
		}
	}
	from, to, step := parseOverviewRange(r.URL.Query())
	// Aggregates use minute buckets; the returned range is also used by logs
	// and recent errors so every dashboard component describes the same period.
	from = from / 60 * 60
	to = (to + 59) / 60 * 60
	if to <= from || to-from > 366*86400 || (to-from)/step > 600 {
		adminError(w, errors.New("overview range must be within 366 days and 600 points"))
		return
	}
	readVersion := p.Metrics.ReadFailureVersion()
	totals := p.Metrics.Totals(from, to)
	grand := p.Metrics.GrandTotals()
	series := p.Metrics.Series(from, to, step)
	today := p.Metrics.Totals(p.Metrics.TodayStart(), time.Now().Unix()+60)
	rpm, tpm := p.Metrics.CurrentRates()
	peakRPM, peakTPM := peakRates(series, step)

	accountCount, enabled, err := p.managedAccountCounts(r.Context())
	if err != nil {
		p.storeError(w, err)
		return
	}
	groupCount, err := p.Groups.managedCount(r.Context())
	if err != nil {
		p.storeError(w, err)
		return
	}
	keyCount, enabledKeys, _, err := p.Keys.managementCounts(r.Context(), nil)
	if err != nil {
		p.storeError(w, err)
		return
	}
	cacheCtx, cacheCancel := context.WithTimeout(r.Context(), metricsTimeout)
	cacheUsage, cacheSeries, err := p.Metrics.OverviewCacheUsage(cacheCtx, from, to, step)
	cacheCancel()
	if err != nil {
		p.storeError(w, err)
		return
	}
	for i := range series {
		series[i].overviewCacheTotals = cacheSeries[series[i].Ts]
	}
	avgLatency := averageMilliseconds(totals.Latency, totals.LatencyN)
	avgLatencyToday := averageMilliseconds(today.Latency, today.LatencyN)
	cards := map[string]any{
		"accounts":                    accountCount,
		"accounts_enabled":            enabled,
		"groups":                      groupCount,
		"keys":                        keyCount,
		"keys_enabled":                enabledKeys,
		"requests":                    totals.Requests,
		"requests_today":              today.Requests,
		"tokens":                      totals.Tokens,
		"tokens_today":                today.Tokens,
		"tokens_total":                grand.Tokens,
		"tokens_retained":             p.Metrics.RetainedTotals().Tokens,
		"cache_scope":                 "retained_request_details",
		"cache_rate":                  cacheUsage.CacheReadRate,
		"cache_rate_has_samples":      cacheUsage.CacheReadRate != nil,
		"cache_read_samples":          cacheUsage.CacheReadSamples,
		"cache_read_unknown_requests": cacheUsage.CacheReadUnknownRequests,
		"cache_read_input_tokens":     cacheUsage.CacheReadInputTokens,
		"cache_read_tokens":           cacheUsage.CacheReadTokens,
		"cache_read_rate":             cacheUsage.CacheReadRate,
		"cache_write_samples":         cacheUsage.CacheWriteSamples,
		"cache_write_tokens":          cacheUsage.CacheWriteTokens,
		"avg_latency_ms":              avgLatency,
		"latency_samples":             totals.LatencyN,
		"avg_latency_today":           avgLatencyToday,
		"latency_samples_today":       today.LatencyN,
		"in_flight":                   p.overviewInFlight(),
		"rpm":                         round2(rpm),
		"tpm":                         round2(tpm),
		"peak_rpm":                    round2(peakRPM),
		"peak_tpm":                    round2(peakTPM),
		"success":                     totals.OK,
		"errors":                      totals.Errors,
	}
	response := map[string]any{
		"timezone":       p.Metrics.Timezone(),
		"retention_days": metricsRetentionDays,
		"totals_scope":   map[string]string{"tokens_total": "lifetime", "tokens_retained": "retained_aggregates", "requests": "selected_range", "tokens": "selected_range"},
		"range":          map[string]int64{"from": from, "to": to, "step": step},
		"cards":          cards,
		"series":         series,
		"account_top":    p.Metrics.AccountTop(from, to, 10),
		"status": []map[string]any{
			{"name": "成功", "value": totals.OK},
			{"name": "失败", "value": totals.Errors},
		},
		"recent_errors": p.Metrics.RecentErrors(from, to, 8),
	}
	if r.URL.Query().Get("include_breakdown") == "true" {
		ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
		breakdown, err := p.Metrics.OverviewBreakdown(ctx, from, to)
		cancel()
		if err != nil {
			p.storeError(w, err)
			return
		}
		response["traffic_breakdown"] = breakdown
	}
	if p.Metrics.ReadFailureVersion() != readVersion {
		adminAPIError(w, http.StatusServiceUnavailable, "statistics_unavailable", "statistics storage unavailable")
		return
	}
	p.writeOverview(w, r, response)
}

func round2(value float64) float64 {
	return float64(int64(value*100+0.5)) / 100
}

func peakRates(series []SeriesPoint, step int64) (float64, float64) {
	var peakRPM, peakTPM float64
	for _, point := range series {
		rpm := float64(point.Requests) / float64(step/60)
		tpm := float64(point.Tokens) / float64(step/60)
		if rpm > peakRPM {
			peakRPM = rpm
		}
		if tpm > peakTPM {
			peakTPM = tpm
		}
	}
	return peakRPM, peakTPM
}

// ---- accounts ----

type accountSummary struct {
	ID               string       `json:"id"`
	Version          int64        `json:"version"`
	Notes            string       `json:"notes"`
	Tags             []string     `json:"tags"`
	UpdatedAt        int64        `json:"updated_at"`
	LastCheckedAt    int64        `json:"last_checked_at"`
	CredentialStatus string       `json:"credential_status"`
	Name             string       `json:"name"`
	Enabled          bool         `json:"enabled"`
	Concurrency      int          `json:"concurrency"`
	InFlight         int64        `json:"in_flight"`
	Priority         int          `json:"priority"`
	Weight           int          `json:"weight"`
	Groups           []string     `json:"groups"`
	GroupNames       []string     `json:"group_names"`
	Proxies          []string     `json:"proxies"`
	ProxyNames       []string     `json:"proxy_names"`
	Quota            int64        `json:"quota"`
	QuotaUsed        int64        `json:"quota_used"`
	QuotaPercent     float64      `json:"quota_percent"`
	WindowDays       int          `json:"window_days"`
	WindowStart      int64        `json:"window_start"`
	ExpiresAt        int64        `json:"expires_at"`
	HasRefreshToken  bool         `json:"has_refresh_token"`
	CreatedAt        int64        `json:"created_at"`
	LastError        string       `json:"last_error"`
	Status           string       `json:"status"`
	Usage            usageSummary `json:"usage"`
}

type usageSummary struct {
	Requests       int64    `json:"requests"`
	OK             int64    `json:"ok"`
	Errors         int64    `json:"errors"`
	Tokens         int64    `json:"tokens"`
	Input          int64    `json:"input"`
	Output         int64    `json:"output"`
	Cached         int64    `json:"cached"`
	AvgLatencyMs   *float64 `json:"avg_latency_ms"`
	LatencySamples int64    `json:"latency_samples"`
}

func (p *Proxy) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	options, err := parseAccountListOptions(r)
	if err != nil {
		adminError(w, err)
		return
	}
	if p.Store.DB == nil && options.Status != "" {
		_, accounts, err := p.Store.ListMetadata()
		if err != nil {
			p.storeError(w, err)
			return
		}
		options.lastStatuses, err = p.managedAccountLastStatuses(r.Context(), accounts)
		if err != nil {
			p.storeError(w, err)
			return
		}
		options.statusCredentialVersions = make(map[string]int64, len(accounts))
		for _, account := range accounts {
			options.statusCredentialVersions[account.ID] = account.CredentialVersion
		}
	}
	page, err := p.Store.ListPage(options)
	if err != nil {
		p.storeError(w, err)
		return
	}
	summaries, err := p.accountSummaries(r, page.Accounts)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"accounts": summaries, "has_more": page.HasMore, "next_cursor": page.NextCursor, "total_kind": "not_computed"})
}

type importRequest struct {
	Name                  string          `json:"name"`
	Credentials           json.RawMessage `json:"credentials"`
	Accounts              json.RawMessage `json:"accounts"`
	Defaults              AccountDefaults `json:"defaults"`
	Raw                   string          `json:"raw"`
	Replace               bool            `json:"replace"`
	DryRun                bool            `json:"dry_run"`
	IncludeExternalLeases bool            `json:"include_external_leases"`
	IncludeExternalCosts  bool            `json:"include_external_costs"`
}

func (p *Proxy) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	p.handleImport(w, r, false)
}
func (p *Proxy) handleAccountsImportPreview(w http.ResponseWriter, r *http.Request) {
	p.handleImport(w, r, true)
}
func (p *Proxy) handleImport(w http.ResponseWriter, r *http.Request, preview bool) {
	var input importRequest
	if err := readJSONLimit(w, r, &input, adminBodyLimit); err != nil {
		adminError(w, err)
		return
	}
	if input.IncludeExternalCosts {
		adminAPIError(w, http.StatusBadRequest, "unsupported", "account cost migration is not supported")
		return
	}
	forms := 0
	if input.Raw != "" {
		forms++
	}
	if input.Name != "" || len(input.Credentials) > 0 {
		forms++
	}
	if input.Accounts != nil {
		forms++
	}
	if forms != 1 {
		adminError(w, errors.New("provide exactly one of raw, name/credentials, or accounts"))
		return
	}
	var items []ImportItem
	if input.Raw != "" {
		var err error
		items, err = ParseImportInput([]byte(input.Raw))
		if err != nil {
			adminError(w, err)
			return
		}
	} else if input.Name != "" || len(input.Credentials) > 0 {
		items = []ImportItem{{Name: input.Name, Credentials: input.Credentials}}
	} else {
		var err error
		items, err = ParseImportInput(input.Accounts)
		if err != nil {
			adminError(w, err)
			return
		}
	}
	dryRun := preview || input.DryRun
	var externalLeases []managedExternalLease
	if input.IncludeExternalLeases {
		externalLeases = prepareManagedExternalLeaseItems(items)
	}
	report, err := p.Store.ImportDetailedContext(r.Context(), items, input.Defaults, ImportOptions{Replace: input.Replace, DryRun: dryRun})
	if err != nil {
		p.storeError(w, err)
		return
	}
	if !dryRun {
		p.invalidateAccounts()
	}
	response := map[string]any{"imported": report.Added + report.Replaced, "added": report.Added, "duplicate": report.Duplicate, "replaced": report.Replaced, "failed": report.Failed, "results": report.Results, "dry_run": dryRun}
	if input.IncludeExternalLeases {
		p.attachManagedExternalLeaseReceipt(r.Context(), response, &report, items, input.Defaults, externalLeases, dryRun)
		response["results"] = report.Results
	}
	writeJSON(w, response)
}

type accountPatch struct {
	Version     int64     `json:"version"`
	Name        *string   `json:"name"`
	Notes       *string   `json:"notes"`
	Tags        *[]string `json:"tags"`
	Enabled     *bool     `json:"enabled"`
	Concurrency *int      `json:"concurrency"`
	Priority    *int      `json:"priority"`
	Weight      *int      `json:"weight"`
	Groups      *[]string `json:"groups"`
	Proxies     *[]string `json:"proxies"`
	Quota       *int64    `json:"quota"`
	WindowDays  *int      `json:"window_days"`
	ResetWindow bool      `json:"reset_window"`
}

func (p *Proxy) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	var patch accountPatch
	if err := readJSONLimit(w, r, &patch, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	updated, err := p.applyAccountPatchContext(r.Context(), r.PathValue("name"), patch)
	if err != nil {
		p.storeError(w, err)
		return
	}
	p.invalidateAccounts()
	writeJSON(w, map[string]any{"ok": true, "id": updated.ID, "version": updated.Version})
}

func (p *Proxy) applyAccountPatch(ref string, patch accountPatch) (Account, error) {
	return p.applyAccountPatchContext(context.Background(), ref, patch)
}

func (p *Proxy) applyAccountPatchContext(ctx context.Context, ref string, patch accountPatch) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if err := validateAccountPatch(patch); err != nil {
		return Account{}, err
	}
	if p.Store.DB == nil {
		if patch.Groups != nil {
			groups, err := p.Groups.List()
			if err != nil {
				return Account{}, err
			}
			valid := map[string]bool{}
			for _, group := range groups {
				valid[group.ID] = true
			}
			for _, id := range *patch.Groups {
				if !valid[id] {
					return Account{}, fmt.Errorf("%w: unknown group", errInvalid)
				}
			}
		}
		if patch.Proxies != nil {
			proxies, err := p.Proxies.ListMetadata()
			if err != nil {
				return Account{}, err
			}
			valid := map[string]bool{}
			for _, proxy := range proxies {
				valid[proxy.ID] = true
			}
			for _, id := range *patch.Proxies {
				if !valid[id] {
					return Account{}, fmt.Errorf("%w: unknown proxy", errInvalid)
				}
			}
		}
	}
	return p.Store.UpdateAccountVersionContext(ctx, ref, patch.Version, func(account *Account) error {
		if patch.Name != nil {
			account.Name = trimSpace(*patch.Name)
		}
		if patch.Notes != nil {
			account.Notes = *patch.Notes
		}
		if patch.Tags != nil {
			account.Tags = uniqueStrings(*patch.Tags)
		}
		if patch.Enabled != nil {
			account.Enabled = *patch.Enabled
		}
		if patch.Concurrency != nil {
			account.Concurrency = *patch.Concurrency
		}
		if patch.Priority != nil {
			account.Priority = *patch.Priority
		}
		if patch.Weight != nil {
			account.Weight = *patch.Weight
		}
		if patch.Groups != nil {
			account.Groups = uniqueStrings(*patch.Groups)
		}
		if patch.Proxies != nil {
			account.Proxies = uniqueStrings(*patch.Proxies)
		}
		if patch.Quota != nil || patch.WindowDays != nil || patch.ResetWindow {
			account.WindowStart = time.Now().Unix()
		}
		if patch.Quota != nil {
			account.Quota = *patch.Quota
		}
		if patch.WindowDays != nil {
			account.WindowDays = *patch.WindowDays
		}
		if account.Enabled {
			account.LastError = ""
		}
		return nil
	})
}

func (p *Proxy) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	expected, err := parseVersionQuery(r)
	if err != nil {
		adminError(w, err)
		return
	}
	a, err := p.Store.GetMetadata(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	if err := p.Store.DeleteVersion(a.ID, expected); err != nil {
		p.storeError(w, err)
		return
	}
	p.cleanupManagedDeletedAccountSchedules(r.Context(), a.ID)
	p.invalidateAccounts()
	writeJSON(w, map[string]bool{"ok": true})
}

func (p *Proxy) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	ref := input.ID
	if ref == "" {
		ref = input.Name
	}
	account, err := p.Store.Get(ref)
	if err != nil {
		p.storeError(w, err)
		return
	}
	if account.RefreshToken == "" {
		adminError(w, errors.New("account has no refresh token"))
		return
	}
	updated, err := p.Store.Refresh(ref, account.AccessToken, func(current Account) (Account, error) {
		return p.refreshAccountWithFingerprint(r.Context(), current)
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	p.invalidateAccounts()
	writeJSON(w, map[string]any{"ok": true, "id": updated.ID, "version": updated.Version, "expires_at": updated.ExpiresAt})
}

func (p *Proxy) handleAccountRequests(w http.ResponseWriter, r *http.Request) {
	account, err := p.Store.GetMetadata(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	query := r.URL.Query()
	if p.Store.DB != nil {
		query.Set("account_id", account.ID)
		query.Del("account")
	} else {
		query.Set("account", account.Name)
		query.Del("account_id")
	}
	if value := query.Get("limit"); value != "" {
		query.Set("page_size", value)
	}
	r.URL.RawQuery = query.Encode()
	p.handleLogs(w, r)
}

// ---- groups ----

func (p *Proxy) handleGroupsList(w http.ResponseWriter, r *http.Request) {
	groups, err := p.Groups.List()
	if err != nil {
		p.storeError(w, err)
		return
	}
	accountCounts, err := p.managedAssociationCounts(r.Context(), false)
	if err != nil {
		p.storeError(w, err)
		return
	}
	groupIDs := make([]string, len(groups))
	for i, group := range groups {
		groupIDs[i] = group.ID
	}
	_, _, keyCounts, err := p.Keys.managementCounts(r.Context(), groupIDs)
	if err != nil {
		p.storeError(w, err)
		return
	}
	type groupSummary struct {
		Group
		Accounts int `json:"accounts"`
		Keys     int `json:"keys"`
	}
	result := make([]groupSummary, 0, len(groups))
	for _, group := range groups {
		summary := groupSummary{Group: group, Accounts: accountCounts[group.ID], Keys: keyCounts[group.ID]}
		result = append(result, summary)
	}
	writeJSON(w, map[string]any{"groups": result})
}

func (p *Proxy) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	group, err := p.Groups.Create(trimSpace(input.Name), input.Description)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, group)
}

func (p *Proxy) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version     int64  `json:"version"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 0 {
		adminError(w, errors.New("version must be positive"))
		return
	}
	group, err := p.Groups.UpdateVersion(r.PathValue("id"), trimSpace(input.Name), input.Description, input.Version)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, group)
}

func (p *Proxy) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p.Store.DB == nil {
		_, accounts, err := p.Store.ListMetadata()
		if err != nil {
			p.storeError(w, err)
			return
		}
		keys, err := p.Keys.List()
		if err != nil {
			p.storeError(w, err)
			return
		}
		for _, account := range accounts {
			for _, group := range account.Groups {
				if group == id {
					adminAPIError(w, 409, "association_conflict", "detach accounts before deleting a group in legacy storage")
					return
				}
			}
		}
		for _, key := range keys {
			for _, group := range key.Groups {
				if group == id {
					adminAPIError(w, 409, "association_conflict", "detach keys before deleting a group in legacy storage")
					return
				}
			}
		}
	}
	if err := p.Groups.Delete(id); err != nil {
		p.storeError(w, err)
		return
	}
	// Database commit already detached every relation. Update the known cache
	// change directly, avoiding a fallible read after a successful transaction.
	p.Keys.mu.Lock()
	for value, key := range p.Keys.index {
		copy := *key
		copy.Groups = removeString(append([]string(nil), key.Groups...), id)
		p.Keys.index[value] = &copy
	}
	p.Keys.mu.Unlock()
	p.invalidateAccounts()
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- keys ----

func (p *Proxy) handleKeysList(w http.ResponseWriter, r *http.Request) {
	keys, err := p.Keys.List()
	if err != nil {
		p.storeError(w, err)
		return
	}
	groupIDs := make(map[string]struct{})
	for _, key := range keys {
		for _, id := range key.Groups {
			groupIDs[id] = struct{}{}
		}
	}
	names, err := p.Groups.managedNames(r.Context(), groupIDs)
	if err != nil {
		p.storeError(w, err)
		return
	}
	groupNames := func(ids []string) []string {
		result := make([]string, 0, len(ids))
		for _, id := range ids {
			if name, ok := names[id]; ok {
				result = append(result, name)
			}
		}
		return result
	}
	type keySummary struct {
		Version    int64    `json:"version"`
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		KeyMasked  string   `json:"key_masked"`
		Groups     []string `json:"groups"`
		GroupNames []string `json:"group_names"`
		Enabled    bool     `json:"enabled"`
		CreatedAt  int64    `json:"created_at"`
		LastUsed   int64    `json:"last_used_at"`
		Requests   int64    `json:"requests"`
		Tokens     int64    `json:"tokens"`
		Errors     int64    `json:"errors"`
	}
	usageByKey, err := p.Metrics.managedKeyUsage(r.Context(), keys)
	if err != nil {
		p.storeError(w, err)
		return
	}
	result := make([]keySummary, 0, len(keys))
	for _, key := range keys {
		usage := usageByKey[key.ID]
		if usage.LastAt > key.LastUsed {
			key.LastUsed = usage.LastAt
		}
		result = append(result, keySummary{
			Version: key.Version, ID: key.ID, Name: key.Name, KeyMasked: maskKey(key.Key),
			Groups: key.Groups, GroupNames: groupNames(key.Groups),
			Enabled: key.Enabled, CreatedAt: key.CreatedAt, LastUsed: key.LastUsed,
			Requests: usage.Requests, Tokens: usage.Tokens, Errors: usage.Errors,
		})
	}
	writeJSON(w, map[string]any{"keys": result})
}

func maskKey(key string) string {
	if len(key) <= 10 {
		return "****"
	}
	return key[:7] + "****" + key[len(key)-4:]
}

func managedStringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func (p *Proxy) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name   string   `json:"name"`
		Groups []string `json:"groups"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	groupIDs := map[string]bool{}
	if p.Store.DB != nil {
		groupNames, err := p.Groups.managedNames(r.Context(), managedStringSet(input.Groups))
		if err != nil {
			p.storeError(w, err)
			return
		}
		for id := range groupNames {
			groupIDs[id] = true
		}
	} else if groups, err := p.Groups.List(); err == nil {
		for _, group := range groups {
			groupIDs[group.ID] = true
		}
	} else {
		p.storeError(w, err)
		return
	}
	for _, id := range input.Groups {
		if !groupIDs[id] {
			adminError(w, fmt.Errorf("group %q not found", id))
			return
		}
	}
	key, err := p.Keys.Create(trimSpace(input.Name), uniqueStrings(input.Groups))
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, key)
}

func (p *Proxy) handleKeyUpdate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version int64     `json:"version"`
		Name    *string   `json:"name"`
		Groups  *[]string `json:"groups"`
		Enabled *bool     `json:"enabled"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	groupIDs := map[string]bool{}
	if p.Store.DB != nil {
		wanted := []string{}
		if input.Groups != nil {
			wanted = *input.Groups
		}
		groupNames, err := p.Groups.managedNames(r.Context(), managedStringSet(wanted))
		if err != nil {
			p.storeError(w, err)
			return
		}
		for id := range groupNames {
			groupIDs[id] = true
		}
	} else if groups, err := p.Groups.List(); err == nil {
		for _, group := range groups {
			groupIDs[group.ID] = true
		}
	} else {
		p.storeError(w, err)
		return
	}
	if input.Groups != nil {
		for _, id := range *input.Groups {
			if !groupIDs[id] {
				adminError(w, fmt.Errorf("group %q not found", id))
				return
			}
		}
	}
	if input.Version < 0 {
		adminError(w, errors.New("version must be positive"))
		return
	}
	key, err := p.Keys.Update(r.PathValue("id"), func(entry *APIKey) error {
		if input.Version > 0 && input.Version != entry.Version {
			return errVersionConflict
		}
		if input.Name != nil {
			if trimSpace(*input.Name) == "" || len(*input.Name) > 60 {
				return fmt.Errorf("%w: 密钥名称需要 1 到 60 个字符", errInvalid)
			}
			entry.Name = trimSpace(*input.Name)
		}
		if input.Groups != nil {
			entry.Groups = uniqueStrings(*input.Groups)
		}
		if input.Enabled != nil {
			entry.Enabled = *input.Enabled
		}
		return nil
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"version": key.Version, "id": key.ID, "name": key.Name,
		"key_masked": maskKey(key.Key), "groups": key.Groups, "enabled": key.Enabled,
		"created_at": key.CreatedAt, "last_used_at": key.LastUsed,
	})
}

func (p *Proxy) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if err := p.Keys.Delete(r.PathValue("id")); err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- proxies ----

func (p *Proxy) handleProxiesList(w http.ResponseWriter, r *http.Request) {
	proxies, err := p.Proxies.managedMetadata()
	if err != nil {
		p.storeError(w, err)
		return
	}
	bound, err := p.managedAssociationCounts(r.Context(), true)
	if err != nil {
		p.storeError(w, err)
		return
	}
	type proxySummary struct {
		ProxyEntry
		HasPassword bool `json:"has_password"`
		Accounts    int  `json:"accounts"`
	}
	result := make([]proxySummary, 0, len(proxies))
	for _, proxy := range proxies {
		result = append(result, proxySummary{ProxyEntry: proxy, HasPassword: proxy.HasPassword, Accounts: bound[proxy.ID]})
	}
	writeJSON(w, map[string]any{"proxies": result})
}

func (p *Proxy) handleProxyCreate(w http.ResponseWriter, r *http.Request) {
	var input ProxyEntry
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	proxy, err := p.Proxies.Create(input)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, proxyMetadataResponse(proxy))
}

func (p *Proxy) handleProxyUpdate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version       int64   `json:"version"`
		ClearPassword bool    `json:"clear_password"`
		Name          *string `json:"name"`
		Type          *string `json:"type"`
		Host          *string `json:"host"`
		Port          *int    `json:"port"`
		Username      *string `json:"username"`
		Password      *string `json:"password"`
		Enabled       *bool   `json:"enabled"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 0 {
		adminError(w, errors.New("version must be positive"))
		return
	}
	proxy, err := p.Proxies.UpdateVersion(r.PathValue("id"), input.Version, func(entry *ProxyEntry) error {
		if input.Name != nil {
			entry.Name = trimSpace(*input.Name)
		}
		if input.Type != nil {
			entry.Type = *input.Type
		}
		if input.Host != nil {
			entry.Host = *input.Host
		}
		if input.Port != nil {
			entry.Port = *input.Port
		}
		if input.Username != nil {
			entry.Username = *input.Username
		}
		if input.ClearPassword {
			entry.Password = ""
		} else if input.Password != nil && *input.Password != "" {
			entry.Password = *input.Password
		}
		if input.Enabled != nil {
			entry.Enabled = *input.Enabled
		}
		return nil
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, proxyMetadataResponse(proxy))
}

func (p *Proxy) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p.Store.DB == nil {
		_, accounts, err := p.Store.ListMetadata()
		if err != nil {
			p.storeError(w, err)
			return
		}
		for _, account := range accounts {
			for _, proxy := range account.Proxies {
				if proxy == id {
					adminAPIError(w, 409, "association_conflict", "detach accounts before deleting a proxy in legacy storage")
					return
				}
			}
		}
	}
	if err := p.Proxies.Delete(id); err != nil {
		p.storeError(w, err)
		return
	}
	p.invalidateAccounts()
	writeJSON(w, map[string]bool{"ok": true})
}

// handleProxyTest checks connectivity through an existing or inline proxy
// by fetching the Cloudflare trace endpoint and reporting the exit IP.
func (p *Proxy) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	entry := ProxyEntry{Name: input.Name, Type: input.Type, Host: input.Host, Port: input.Port, Username: input.Username, Password: input.Password}
	if input.ID != "" {
		stored, err := p.Proxies.GetForAdmin(input.ID)
		if err != nil {
			p.storeError(w, err)
			return
		}
		entry = stored
	}
	if err := validProxy(entry); err != nil {
		adminError(w, err)
		return
	}
	clientValue := *newGatewayProxyClient(p.Client, &entry, gatewayClientHTTP)
	clientValue.Timeout = 20 * time.Second
	clientValue.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer clientValue.CloseIdleConnections()
	connectStatus := 0
	observeDiagnosticConnect(&clientValue, &connectStatus)
	started := time.Now()
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://chatgpt.com/cdn-cgi/trace", nil)
	if err != nil {
		adminError(w, err)
		return
	}
	response, err := clientValue.Do(request)
	if err != nil {
		diagnostic := proxyDiagnosticResult(connectStatus, nil, err, time.Since(started))
		if connectStatus != 0 && connectStatus != http.StatusProxyAuthRequired {
			diagnostic.Authentication = "not_checked"
		}
		writeJSON(w, diagnostic)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	writeJSON(w, proxyDiagnosticResult(response.StatusCode, body, err, time.Since(started)))
}

// ---- logs ----

func (p *Proxy) handleLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	from, to := int64(1), time.Now().Unix()+86400
	for name, target := range map[string]*int64{"from": &from, "to": &to} {
		if value := query.Get(name); value != "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 {
				adminError(w, errors.New("invalid time range"))
				return
			}
			*target = n
		}
	}
	if to <= from {
		adminError(w, errors.New("to must be after from"))
		return
	}
	page, size := 1, 50
	for name, target := range map[string]*int{"page": &page, "size": &size, "page_size": &size} {
		if value := query.Get(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				adminError(w, errors.New("invalid pagination"))
				return
			}
			*target = n
		}
	}
	if page > 100 || size > 500 || query.Get("cursor") != "" && page > 1 {
		adminError(w, errors.New("use cursor pagination; page maximum is 100 and page_size maximum is 500"))
		return
	}
	if query.Get("cursor") != "" {
		if _, err := decodeLogCursor(query.Get("cursor")); err != nil {
			adminError(w, err)
			return
		}
	}
	result, err := p.Metrics.Logs(LogQuery{From: from, To: to, Account: query.Get("account"), AccountID: query.Get("account_id"), KeyName: query.Get("key_name"), Status: query.Get("status"), Model: query.Get("model"), Cursor: query.Get("cursor"), Page: page, Size: size})
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, struct {
		LogPage
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
	}{LogPage: result, Page: page, PageSize: size})
}

// ---- settings ----

func (p *Proxy) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	settings, err := p.managementSettings()
	if err != nil {
		p.storeError(w, err)
		return
	}
	key, err := p.currentAdminKey(r.Context())
	if err != nil {
		adminAPIError(w, http.StatusServiceUnavailable, "storage_unavailable", "admin key store unavailable")
		return
	}
	writeJSON(w, map[string]any{
		"admin_key":                                "",
		"inference":                                p.inferenceInfo(),
		"inference_mode":                           settings.InferenceMode,
		"inference_modes":                          p.inferenceModeOptions(),
		"admin_key_masked":                         maskKey(key),
		"admin_api":                                settings.AdminAPI,
		"websocket_enabled":                        settings.WebsocketEnabled,
		"prefer_websocket":                         settings.PreferWebsocket,
		"bps_websocket_reuse":                      settings.BPSWebSocketReuse,
		"codex_fingerprint_enabled":                settings.CodexFingerprintEnabled,
		"codex_fingerprint_profile":                settings.CodexFingerprintProfile,
		"codex_fingerprint_profiles":               effectiveCodexFingerprintProfiles(),
		"builtin_prompt_enabled":                   settings.BuiltinPromptEnabled,
		"builtin_prompt":                           settings.BuiltinPrompt,
		"sticky_sessions_enabled":                  settings.StickySessionsEnabled,
		"sticky_session_ttl_seconds":               settings.StickySessionTTLSeconds,
		"prompt_cache_key_enabled":                 settings.PromptCacheKeyEnabled,
		"cache_creation_as_read_enabled":           settings.CacheCreationAsReadEnabled,
		"cache_read_ratio_enabled":                 settings.CacheReadRatioEnabled,
		"cache_read_ratio_percent":                 settings.CacheReadRatioPercent,
		"bps_prompt_cache_scope":                   settings.BPSPromptCacheScope,
		"bps_model_routes":                         settings.BPSModelRoutes,
		"bps_model_route_targets":                  bpsStaticModelIDs(),
		"rate_limit_cooldown_enabled":              settings.RateLimitCooldownEnabled,
		"http_stream_enabled":                      settings.HTTPStreamEnabled,
		"http_json_enabled":                        settings.HTTPJSONEnabled,
		"compact_enabled":                          settings.CompactEnabled,
		"models_enabled":                           settings.ModelsEnabled,
		"compatibility_normalization_enabled":      settings.CompatibilityNormalizationEnabled,
		"automatic_token_refresh_enabled":          settings.AutomaticTokenRefreshEnabled,
		"upstream_401_recovery_enabled":            settings.Upstream401RecoveryEnabled,
		"builtin_prompt_mode":                      settings.BuiltinPromptMode,
		"upstream_stream_idle_timeout_seconds":     settings.UpstreamStreamIdleTimeoutSeconds,
		"upstream_response_header_timeout_seconds": settings.UpstreamResponseHeaderTimeoutSeconds,
		"upstream_compact_timeout_seconds":         settings.UpstreamCompactTimeoutSeconds,
		"settings_version":                         settings.Version,
		"version":                                  version,
		"uptime_seconds":                           int64(time.Since(p.startedAt).Seconds()),
	})
}

func (p *Proxy) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SettingsVersion                      int64   `json:"settings_version"`
		InferenceMode                        *string `json:"inference_mode"`
		AdminAPI                             *bool   `json:"admin_api"`
		WebsocketEnabled                     *bool   `json:"websocket_enabled"`
		PreferWebsocket                      *bool   `json:"prefer_websocket"`
		BPSWebSocketReuse                    *bool   `json:"bps_websocket_reuse"`
		CodexFingerprintEnabled              *bool   `json:"codex_fingerprint_enabled"`
		CodexFingerprintProfile              *string `json:"codex_fingerprint_profile"`
		BuiltinPromptEnabled                 *bool   `json:"builtin_prompt_enabled"`
		BuiltinPrompt                        *string `json:"builtin_prompt"`
		StickySessionsEnabled                *bool   `json:"sticky_sessions_enabled"`
		StickySessionTTLSeconds              *int    `json:"sticky_session_ttl_seconds"`
		PromptCacheKeyEnabled                *bool   `json:"prompt_cache_key_enabled"`
		CacheCreationAsReadEnabled           *bool   `json:"cache_creation_as_read_enabled"`
		CacheReadRatioEnabled                *bool   `json:"cache_read_ratio_enabled"`
		CacheReadRatioPercent                *int    `json:"cache_read_ratio_percent"`
		BPSPromptCacheScope                  *string `json:"bps_prompt_cache_scope"`
		BPSModelRoutes                       *string `json:"bps_model_routes"`
		RateLimitCooldownEnabled             *bool   `json:"rate_limit_cooldown_enabled"`
		HTTPStreamEnabled                    *bool   `json:"http_stream_enabled"`
		HTTPJSONEnabled                      *bool   `json:"http_json_enabled"`
		CompactEnabled                       *bool   `json:"compact_enabled"`
		ModelsEnabled                        *bool   `json:"models_enabled"`
		CompatibilityNormalizationEnabled    *bool   `json:"compatibility_normalization_enabled"`
		AutomaticTokenRefreshEnabled         *bool   `json:"automatic_token_refresh_enabled"`
		Upstream401RecoveryEnabled           *bool   `json:"upstream_401_recovery_enabled"`
		BuiltinPromptMode                    *string `json:"builtin_prompt_mode"`
		UpstreamStreamIdleTimeoutSeconds     *int    `json:"upstream_stream_idle_timeout_seconds"`
		UpstreamResponseHeaderTimeoutSeconds *int    `json:"upstream_response_header_timeout_seconds"`
		UpstreamCompactTimeoutSeconds        *int    `json:"upstream_compact_timeout_seconds"`
		Regenerate                           bool    `json:"regenerate"`
		RegenerateKey                        bool    `json:"regenerate_key"`
		RequestID                            string  `json:"request_id"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	hasUpdate := input.InferenceMode != nil || input.AdminAPI != nil || input.WebsocketEnabled != nil || input.PreferWebsocket != nil || input.BPSWebSocketReuse != nil || input.CodexFingerprintEnabled != nil || input.CodexFingerprintProfile != nil || input.BuiltinPromptEnabled != nil || input.BuiltinPrompt != nil || input.StickySessionsEnabled != nil || input.StickySessionTTLSeconds != nil || input.PromptCacheKeyEnabled != nil || input.CacheCreationAsReadEnabled != nil || input.BPSPromptCacheScope != nil || input.BPSModelRoutes != nil || input.RateLimitCooldownEnabled != nil || input.HTTPStreamEnabled != nil || input.HTTPJSONEnabled != nil || input.CompactEnabled != nil || input.ModelsEnabled != nil || input.CompatibilityNormalizationEnabled != nil || input.AutomaticTokenRefreshEnabled != nil || input.Upstream401RecoveryEnabled != nil || input.BuiltinPromptMode != nil || input.UpstreamStreamIdleTimeoutSeconds != nil || input.UpstreamResponseHeaderTimeoutSeconds != nil || input.UpstreamCompactTimeoutSeconds != nil
	hasUpdate = hasUpdate || input.CacheReadRatioEnabled != nil || input.CacheReadRatioPercent != nil
	if input.Regenerate || input.RegenerateKey {
		if hasUpdate || input.SettingsVersion != 0 {
			adminError(w, errors.New("key rotation must be submitted separately from other settings"))
			return
		}
		p.writeAdminKeyRotation(w, r, input.RequestID)
		return
	}
	if input.RequestID != "" {
		adminError(w, errors.New("request_id is only supported for key rotation"))
		return
	}
	if !hasUpdate {
		adminError(w, errors.New("at least one setting is required"))
		return
	}
	if p.Settings == nil {
		adminAPIError(w, http.StatusServiceUnavailable, "storage_unavailable", "settings store unavailable")
		return
	}
	settings, err := p.Settings.readForUpdate()
	if err != nil {
		p.storeError(w, err)
		return
	}
	if input.SettingsVersion < 0 {
		adminError(w, errors.New("settings_version must be positive"))
		return
	}
	if input.SettingsVersion > 0 {
		settings.Version = input.SettingsVersion
	}
	if input.AdminAPI != nil {
		settings.AdminAPI = *input.AdminAPI
	}
	if input.InferenceMode != nil {
		if *input.InferenceMode == inferenceAppServer {
			apiError(w, http.StatusBadRequest, "requested inference mode has been removed")
			return
		}
		settings.InferenceMode = *input.InferenceMode
	}
	if input.WebsocketEnabled != nil {
		settings.WebsocketEnabled = *input.WebsocketEnabled
	}
	if input.PreferWebsocket != nil {
		settings.PreferWebsocket = *input.PreferWebsocket
	}
	if input.BPSWebSocketReuse != nil {
		settings.BPSWebSocketReuse = *input.BPSWebSocketReuse
	}
	if input.CodexFingerprintEnabled != nil {
		settings.CodexFingerprintEnabled = *input.CodexFingerprintEnabled
	}
	if input.CodexFingerprintProfile != nil {
		settings.CodexFingerprintProfile = *input.CodexFingerprintProfile
	}
	if input.BuiltinPromptEnabled != nil {
		settings.BuiltinPromptEnabled = *input.BuiltinPromptEnabled
	}
	if input.BuiltinPrompt != nil {
		settings.BuiltinPrompt = *input.BuiltinPrompt
	}
	if input.StickySessionsEnabled != nil {
		settings.StickySessionsEnabled = *input.StickySessionsEnabled
	}
	if input.StickySessionTTLSeconds != nil {
		settings.StickySessionTTLSeconds = *input.StickySessionTTLSeconds
	}
	if input.PromptCacheKeyEnabled != nil {
		settings.PromptCacheKeyEnabled = *input.PromptCacheKeyEnabled
	}
	if input.CacheCreationAsReadEnabled != nil {
		settings.CacheCreationAsReadEnabled = *input.CacheCreationAsReadEnabled
	}
	if input.CacheReadRatioEnabled != nil {
		settings.CacheReadRatioEnabled = *input.CacheReadRatioEnabled
	}
	if input.CacheReadRatioPercent != nil {
		settings.CacheReadRatioPercent = *input.CacheReadRatioPercent
	}
	if input.BPSPromptCacheScope != nil {
		settings.BPSPromptCacheScope = *input.BPSPromptCacheScope
	}
	if input.BPSModelRoutes != nil {
		settings.BPSModelRoutes = *input.BPSModelRoutes
	}
	if input.RateLimitCooldownEnabled != nil {
		settings.RateLimitCooldownEnabled = *input.RateLimitCooldownEnabled
	}
	if input.HTTPStreamEnabled != nil {
		settings.HTTPStreamEnabled = *input.HTTPStreamEnabled
	}
	if input.HTTPJSONEnabled != nil {
		settings.HTTPJSONEnabled = *input.HTTPJSONEnabled
	}
	if input.CompactEnabled != nil {
		settings.CompactEnabled = *input.CompactEnabled
	}
	if input.ModelsEnabled != nil {
		settings.ModelsEnabled = *input.ModelsEnabled
	}
	if input.CompatibilityNormalizationEnabled != nil {
		settings.CompatibilityNormalizationEnabled = *input.CompatibilityNormalizationEnabled
	}
	if input.AutomaticTokenRefreshEnabled != nil {
		settings.AutomaticTokenRefreshEnabled = *input.AutomaticTokenRefreshEnabled
	}
	if input.Upstream401RecoveryEnabled != nil {
		settings.Upstream401RecoveryEnabled = *input.Upstream401RecoveryEnabled
	}
	if input.BuiltinPromptMode != nil {
		settings.BuiltinPromptMode = *input.BuiltinPromptMode
	}
	if input.UpstreamStreamIdleTimeoutSeconds != nil {
		settings.UpstreamStreamIdleTimeoutSeconds = *input.UpstreamStreamIdleTimeoutSeconds
	}
	if input.UpstreamResponseHeaderTimeoutSeconds != nil {
		settings.UpstreamResponseHeaderTimeoutSeconds = *input.UpstreamResponseHeaderTimeoutSeconds
	}
	if input.UpstreamCompactTimeoutSeconds != nil {
		settings.UpstreamCompactTimeoutSeconds = *input.UpstreamCompactTimeoutSeconds
	}
	if err := validateSettings(settings); err != nil {
		p.storeError(w, err)
		return
	}
	// Check the merged enabled profile before persistence; keep runtime checks
	// out of store reads so older incompatible settings remain repairable.
	if settings.CodexFingerprintEnabled {
		if _, err := effectiveCodexFingerprintProfile(settings.CodexFingerprintProfile); err != nil {
			p.storeError(w, err)
			return
		}
	}
	if err := p.Settings.Set(settings); err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "settings_version": settings.Version + 1})
}

func (p *Proxy) handleAdminKeyRotate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RequestID string `json:"request_id"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	p.writeAdminKeyRotation(w, r, input.RequestID)
}

func (p *Proxy) writeAdminKeyRotation(w http.ResponseWriter, r *http.Request, requestID string) {
	if requestID == "" {
		requestID = newID("rotation_")
	}
	if len(requestID) > 128 {
		adminError(w, errors.New("request_id exceeds 128 bytes"))
		return
	}
	key, err := p.rotateAdminKey(r.Context(), requestID)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "admin_key": key, "request_id": requestID})
}

func writeNewAdminKey(path string) (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	key := "ca_" + hex.EncodeToString(random[:])
	file, err := os.CreateTemp(filepath.Dir(path), ".client-key-*")
	if err != nil {
		return "", err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return "", err
	}
	if _, err := file.WriteString(key + "\n"); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temp, path); err != nil {
		return "", err
	}
	return key, nil
}

// adminRoutes registers the management console API behind admin-key auth.
// adminRouteErrorWriter preserves ServeMux routing and Allow headers while
// rendering its unmatched-route errors in the same format as management APIs.
type adminRouteErrorWriter struct {
	http.ResponseWriter
	suppressed bool
}

func (w *adminRouteErrorWriter) WriteHeader(status int) {
	switch status {
	case http.StatusNotFound:
		w.suppressed = true
		adminAPIError(w.ResponseWriter, status, "not_found", "management endpoint not found")
	case http.StatusMethodNotAllowed:
		w.suppressed = true
		adminAPIError(w.ResponseWriter, status, "method_not_allowed", "method not allowed")
	default:
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *adminRouteErrorWriter) Write(body []byte) (int, error) {
	if w.suppressed {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

func (p *Proxy) adminRoutes() http.Handler {
	mux := http.NewServeMux()
	p.registerManagedCleanupRoutes(mux)
	p.registerManagedUsageOptions(mux)
	mux.HandleFunc("POST /api/session", p.handleSessionLogin)
	mux.HandleFunc("GET /api/session", p.handleSessionGet)
	mux.HandleFunc("DELETE /api/session", p.handleSessionLogout)
	mux.HandleFunc("POST /api/settings/rotate-key", p.handleAdminKeyRotate)
	mux.HandleFunc("GET /api/accounts/{name}", p.handleAccountDetail)
	mux.HandleFunc("POST /api/accounts/import/preview", p.handleAccountsImportPreview)
	mux.HandleFunc("POST /api/accounts/batch", p.handleAccountsBatch)
	mux.HandleFunc("GET /api/accounts/manage", p.handleManagedAccounts)
	mux.HandleFunc("POST /api/accounts/selection", p.handleManagedAccountSelection)
	mux.HandleFunc("POST /api/accounts/actions", p.handleManagedAccountActions)
	mux.HandleFunc("POST /api/accounts/{name}/reauthorize", p.handleManagedAccountReauthorize)
	mux.HandleFunc("POST /api/accounts/reauthorize-batch", p.handleManagedReauthorizeBatch)
	mux.HandleFunc("GET /api/accounts/{name}/metadata", p.handleManagedAccountMetadata)
	mux.HandleFunc("GET /api/accounts/{name}/runtime", p.handleManagedAccountRuntime)
	mux.HandleFunc("POST /api/accounts/last-used", p.handleManagedAccountLastUsed)
	mux.HandleFunc("GET /api/accounts/{name}/upstream", p.handleManagedUpstreamSnapshot)
	mux.HandleFunc("POST /api/accounts/{name}/upstream", p.handleManagedUpstreamCheck)
	mux.HandleFunc("POST /api/accounts/{name}/test-stream", p.handleManagedAccountTestStream)
	mux.HandleFunc("POST /api/accounts/upstream/batch", p.handleManagedUpstreamBatch)
	mux.HandleFunc("POST /api/accounts/{name}/privacy", p.handleManagedAccountPrivacy)
	mux.HandleFunc("POST /api/accounts/{name}/reset-credit", p.handleManagedAccountResetCredit)
	mux.HandleFunc("GET /api/accounts/{name}/auto-reset", p.handleManagedAutoResetGet)
	mux.HandleFunc("PUT /api/accounts/{name}/auto-reset", p.handleManagedAutoResetUpdate)
	mux.HandleFunc("GET /api/accounts/{name}/schedules", p.handleManagedScheduleList)
	mux.HandleFunc("GET /api/accounts/{name}/lease", p.handleManagedAccountLeaseGet)
	mux.HandleFunc("PUT /api/accounts/{name}/lease", p.handleManagedAccountLeaseUpdate)
	mux.HandleFunc("POST /api/accounts/{name}/schedules", p.handleManagedScheduleCreate)
	mux.HandleFunc("PUT /api/account-schedules/{id}", p.handleManagedScheduleUpdate)
	mux.HandleFunc("DELETE /api/account-schedules/{id}", p.handleManagedScheduleDelete)
	mux.HandleFunc("GET /api/account-schedules/{id}/results", p.handleManagedScheduleResults)
	mux.HandleFunc("POST /api/accounts/export", p.handleManagedAccountExport)
	mux.HandleFunc("POST /api/accounts/export/credentials", p.handleManagedCredentialExport)
	mux.HandleFunc("POST /api/accounts/import-bundle", p.handleManagedAccountBundleImport)
	mux.HandleFunc("POST /api/accounts/import-resources", p.handleManagedBundleResourcesImport)
	mux.HandleFunc("POST /api/accounts/import-leases", p.handleManagedBundleLeaseRestore)
	mux.HandleFunc("GET /api/accounts/import-leases/receipts/{receipt}", p.handleManagedBundleLeaseReceipt)
	mux.HandleFunc("POST /api/accounts/sync/crs/preview", p.handleManagedCRSPreview)
	mux.HandleFunc("POST /api/accounts/sync/crs", p.handleManagedCRSSync)
	mux.HandleFunc("POST /api/account-oauth/start", p.handleManagedOAuthStart)
	mux.HandleFunc("POST /api/account-oauth/exchange", p.handleManagedOAuthExchange)
	mux.HandleFunc("POST /api/account-oauth/import", p.handleManagedOAuthImport)
	mux.HandleFunc("POST /api/account-oauth/apply", p.handleManagedOAuthApply)
	mux.HandleFunc("GET /api/account-oauth/{id}", p.handleManagedOAuthGet)
	mux.HandleFunc("DELETE /api/account-oauth/{id}", p.handleManagedOAuthCancel)
	mux.HandleFunc("GET /api/overview", p.handleOverview)
	mux.HandleFunc("GET /api/overview/realtime", p.handleDashboardRealtime)
	mux.HandleFunc("GET /api/diagnostics", p.handleDiagnostics)
	mux.HandleFunc("GET /api/operations", p.handleOperations)
	mux.HandleFunc("GET /api/audit-logs", p.handleAuditLogs)
	mux.HandleFunc("GET /api/accounts", p.handleAccountsList)
	mux.HandleFunc("POST /api/accounts", p.handleAccountsImport)
	mux.HandleFunc("POST /api/accounts/refresh", p.handleAccountRefresh)
	mux.HandleFunc("PUT /api/accounts/{name}", p.handleAccountUpdate)
	mux.HandleFunc("DELETE /api/accounts/{name}", p.handleAccountDelete)
	mux.HandleFunc("GET /api/accounts/{name}/requests", p.handleAccountRequests)
	mux.HandleFunc("GET /api/groups", p.handleGroupsList)
	mux.HandleFunc("POST /api/groups", p.handleGroupCreate)
	mux.HandleFunc("PUT /api/groups/{id}", p.handleGroupUpdate)
	mux.HandleFunc("DELETE /api/groups/{id}", p.handleGroupDelete)
	mux.HandleFunc("GET /api/keys", p.handleKeysList)
	mux.HandleFunc("POST /api/keys", p.handleKeyCreate)
	mux.HandleFunc("PUT /api/keys/{id}", p.handleKeyUpdate)
	mux.HandleFunc("DELETE /api/keys/{id}", p.handleKeyDelete)
	mux.HandleFunc("GET /api/proxies", p.handleProxiesList)
	mux.HandleFunc("POST /api/proxies", p.handleProxyCreate)
	mux.HandleFunc("POST /api/proxies/test", p.handleProxyTest)
	mux.HandleFunc("PUT /api/proxies/{id}", p.handleProxyUpdate)
	mux.HandleFunc("DELETE /api/proxies/{id}", p.handleProxyDelete)
	mux.HandleFunc("GET /api/logs", p.handleManagedLogs)
	mux.HandleFunc("GET /api/ingress-events", p.handleManagedIngressEvents)
	mux.HandleFunc("GET /api/logs/stats", p.handleManagedLogStats)
	mux.HandleFunc("GET /api/logs/export", p.handleManagedLogExport)
	mux.HandleFunc("GET /api/logs/{id}", p.handleManagedLogDetail)
	mux.HandleFunc("GET /api/settings", p.handleSettingsGet)
	mux.HandleFunc("POST /api/settings", p.handleSettingsPost)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !(r.URL.Path == "/api/session" && r.Method == http.MethodPost) && !p.authenticateAdmin(w, r) {
			return
		}
		if retiredManagedBillingPath(r.URL.Path) {
			adminAPIError(w, http.StatusNotFound, "not_found", "management endpoint not found")
			return
		}
		if _, pattern := mux.Handler(r); pattern == "" {
			mux.ServeHTTP(&adminRouteErrorWriter{ResponseWriter: w}, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
