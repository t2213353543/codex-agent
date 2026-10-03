package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func parseVersionQuery(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("version")
	if value == "" {
		return 0, nil
	}
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil || version < 1 {
		return 0, errors.New("version must be positive")
	}
	return version, nil
}

func parseAccountListOptions(r *http.Request) (AccountListOptions, error) {
	q := r.URL.Query()
	options := AccountListOptions{Limit: 50, Cursor: q.Get("cursor"), Search: q.Get("search"), GroupID: q.Get("group_id"), ProxyID: q.Get("proxy_id"), Status: q.Get("status")}
	if v := q.Get("limit"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > 500 {
			return options, errors.New("limit must be between 1 and 500")
		}
		options.Limit = n
	}
	if v := q.Get("enabled"); v != "" {
		b, e := strconv.ParseBool(v)
		if e != nil {
			return options, errors.New("enabled must be a boolean")
		}
		options.Enabled = &b
	}
	return options, nil
}

func validateAccountPatch(patch accountPatch) error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", errInvalid, message) }
	if patch.Version < 0 {
		return invalid("version must be positive")
	}
	if patch.Name != nil && (strings.TrimSpace(*patch.Name) == "" || len(*patch.Name) > 120) {
		return invalid("name must have 1 to 120 bytes")
	}
	if patch.Notes != nil && len(*patch.Notes) > 8192 {
		return invalid("notes exceed 8192 bytes")
	}
	if patch.Tags != nil {
		if len(*patch.Tags) > 50 {
			return invalid("at most 50 tags")
		}
		for _, tag := range *patch.Tags {
			if strings.TrimSpace(tag) == "" || len(tag) > 100 {
				return invalid("invalid tag")
			}
		}
	}
	if patch.Concurrency != nil && (*patch.Concurrency < 1 || *patch.Concurrency > 1000) {
		return invalid("concurrency must be 1 to 1000")
	}
	if patch.Weight != nil && (*patch.Weight < 1 || *patch.Weight > 1000) {
		return invalid("weight must be 1 to 1000")
	}
	if patch.Quota != nil && *patch.Quota < 0 {
		return invalid("quota must not be negative")
	}
	if patch.WindowDays != nil && (*patch.WindowDays < 1 || *patch.WindowDays > 365) {
		return invalid("window_days must be 1 to 365")
	}
	return nil
}

func (p *Proxy) accountSummaries(r *http.Request, accounts []Account) ([]accountSummary, error) {
	groupNames, proxyNames, err := p.managedAccountBindingNames(r.Context(), accounts)
	if err != nil {
		return nil, err
	}
	names := func(ids []string, lookup map[string]string) []string {
		result := make([]string, 0, len(ids))
		for _, id := range ids {
			if name, ok := lookup[id]; ok {
				result = append(result, name)
			}
		}
		return result
	}
	starts := map[string]int64{}
	now := time.Now().Unix()
	for _, account := range accounts {
		ref := account.ID
		if ref == "" || p.Store.DB == nil {
			ref = account.Name
		}
		start := account.WindowStart
		if start == 0 {
			start = now - int64(account.effectiveWindowDays())*86400
		}
		starts[ref] = start
	}
	metrics := make(map[string]AccountMetricsSummary, len(starts))
	batch := make(map[string]int64, 1000)
	readBatch := func() error {
		if p.Metrics.db == nil {
			for key, start := range batch {
				metrics[key] = AccountMetricsSummary{Usage: p.Metrics.AccountWindow(key, start)}
			}
			clear(batch)
			return r.Context().Err()
		}
		values, err := p.Metrics.managedAccountUsage(r.Context(), batch)
		if err != nil {
			return err
		}
		for key, value := range values {
			metrics[key] = value
		}
		clear(batch)
		return nil
	}
	for key, start := range starts {
		batch[key] = start
		if len(batch) == 1000 {
			if err := readBatch(); err != nil {
				return nil, err
			}
		}
	}
	if len(batch) > 0 {
		if err := readBatch(); err != nil {
			return nil, err
		}
	}
	lastStatuses, err := p.managedAccountLastStatuses(r.Context(), accounts)
	if err != nil {
		return nil, err
	}
	result := make([]accountSummary, 0, len(accounts))
	for _, account := range accounts {
		ref := account.ID
		if ref == "" || p.Store.DB == nil {
			ref = account.Name
		}
		data := metrics[ref]
		usage := data.Usage
		summary := accountSummary{ID: account.ID, Version: account.Version, Name: account.Name, Notes: account.Notes, Tags: account.Tags, UpdatedAt: account.UpdatedAt, LastCheckedAt: account.LastCheckedAt, CredentialStatus: account.CredentialStatus,
			Enabled: account.Enabled, Concurrency: account.effectiveConcurrency(), InFlight: p.inFlight(gatewayAdmissionKey(account)), Priority: account.Priority, Weight: account.effectiveWeight(), Groups: account.Groups, GroupNames: names(account.Groups, groupNames), Proxies: account.Proxies, ProxyNames: names(account.Proxies, proxyNames), Quota: account.Quota, WindowDays: account.effectiveWindowDays(), WindowStart: account.WindowStart, ExpiresAt: account.ExpiresAt, HasRefreshToken: account.HasRefreshToken, CreatedAt: account.CreatedAt, LastError: redactMetricError(account.LastError), Status: "unverified"}
		summary.Usage = usageSummary{Requests: usage.Requests, OK: usage.OK, Errors: usage.Errors, Tokens: usage.Tokens, Input: usage.Input, Output: usage.Output, Cached: usage.Cached}
		summary.Usage.AvgLatencyMs = averageMilliseconds(usage.Latency, usage.LatencyN)
		summary.Usage.LatencySamples = usage.LatencyN
		summary.QuotaUsed = usage.Tokens
		if account.Quota > 0 {
			summary.QuotaPercent = round2(float64(usage.Tokens) / float64(account.Quota) * 100)
		}
		summary.Status = accountManagementStatus(account, lastStatuses[account.ID], now)

		if summary.Groups == nil {
			summary.Groups = []string{}
		}
		if summary.Proxies == nil {
			summary.Proxies = []string{}
		}
		if summary.Tags == nil {
			summary.Tags = []string{}
		}
		result = append(result, summary)
	}
	return result, nil
}

func (p *Proxy) handleAccountDetail(w http.ResponseWriter, r *http.Request) {
	account, err := p.Store.GetMetadata(r.PathValue("name"))
	if err != nil {
		p.storeError(w, err)
		return
	}
	items, err := p.accountSummaries(r, []Account{account})
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"account": items[0]})
}

func (p *Proxy) handleAccountsBatch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Items []struct {
			ID      string       `json:"id"`
			Version int64        `json:"version"`
			Patch   accountPatch `json:"patch"`
		} `json:"items"`
	}
	if err := readJSONLimit(w, r, &input, 2<<20); err != nil {
		adminError(w, err)
		return
	}
	if len(input.Items) == 0 || len(input.Items) > 100 {
		adminError(w, errors.New("items must contain 1 to 100 accounts"))
		return
	}
	results := make([]map[string]any, 0, len(input.Items))
	succeeded := 0
	for _, item := range input.Items {
		patch := item.Patch
		patch.Version = item.Version
		result := map[string]any{"id": item.ID, "ok": false}
		updated, err := p.applyAccountPatchContext(r.Context(), item.ID, patch)
		if err != nil {
			code := "storage_error"
			if errors.Is(err, errVersionConflict) || errors.Is(err, ErrTokenConflict) {
				code = "version_conflict"
			} else if errors.Is(err, errConflict) {
				code = "conflict"
			} else if errors.Is(err, errNotFound) {
				code = "not_found"
			} else if errors.Is(err, errInvalid) {
				code = "invalid_request"
			}
			result["code"] = code
			result["error"] = "account update failed"
		} else {
			succeeded++
			result["ok"] = true
			result["version"] = updated.Version
		}
		results = append(results, result)
	}
	if succeeded > 0 {
		p.invalidateAccounts()
	}
	writeJSON(w, map[string]any{"results": results, "succeeded": succeeded, "failed": len(input.Items) - succeeded})
}

type proxyDiagnostic struct {
	OK             bool   `json:"ok"`
	LatencyMS      int64  `json:"latency_ms"`
	IP             string `json:"ip"`
	HTTPCode       int    `json:"http_code"`
	CheckedAt      int64  `json:"checked_at"`
	Stage          string `json:"stage"`
	ErrorCode      string `json:"error_code,omitempty"`
	Error          string `json:"error,omitempty"`
	Connection     string `json:"connection"`
	Authentication string `json:"authentication"`
	HTTP           string `json:"http"`
	ExitIP         string `json:"exit_ip"`
}

func proxyDiagnosticResult(status int, body []byte, requestErr error, latency time.Duration) proxyDiagnostic {
	result := proxyDiagnostic{LatencyMS: latency.Milliseconds(), HTTPCode: status, CheckedAt: time.Now().Unix(), Stage: "connection", Connection: "not_checked", Authentication: "not_checked", HTTP: "not_checked", ExitIP: "not_checked"}
	if requestErr != nil && status == 0 {
		result.Connection = "failed"
		result.ErrorCode = "connection_failed"
		result.Error = "proxy connection failed"
		// CONNECT and SOCKS authentication failures are returned by net/http as errors.
		message := strings.ToLower(requestErr.Error())
		if strings.Contains(message, "proxy authentication required") || strings.Contains(message, "username/password authentication failed") || strings.Contains(message, "no acceptable authentication methods") || strings.Contains(message, "socks5 authentication") || strings.Contains(message, "socks5 proxy requires authentication") || strings.Contains(message, "socks5 proxy rejected all authentication methods") {
			result.Connection = "ok"
			result.Authentication = "failed"
			result.Stage = "authentication"
			result.ErrorCode = "proxy_authentication_failed"
			result.Error = "proxy authentication failed"
		} else if e, ok := requestErr.(net.Error); ok && e.Timeout() {
			result.ErrorCode = "connection_timeout"
			result.Error = "proxy connection timed out"
		}
		return result
	}
	result.Connection = "ok"
	if status == http.StatusProxyAuthRequired {
		result.Authentication = "failed"
		result.Stage = "authentication"
		result.ErrorCode = "proxy_authentication_failed"
		result.Error = "proxy authentication failed"
		return result
	}
	result.Authentication = "ok"
	result.Stage = "http"
	if status < 200 || status >= 300 {
		result.HTTP = "failed"
		result.ErrorCode = "http_status"
		result.Error = fmt.Sprintf("probe returned HTTP %d", status)
		return result
	}
	result.HTTP = "ok"
	result.Stage = "exit_ip"
	if requestErr != nil {
		result.ExitIP = "failed"
		result.ErrorCode = "response_read_failed"
		result.Error = "probe response could not be read"
		return result
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "ip=") {
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "ip="))
			if parsed := net.ParseIP(candidate); parsed != nil {
				result.IP = parsed.String()
				break
			}
		}
	}
	if result.IP == "" {
		result.ExitIP = "failed"
		result.ErrorCode = "invalid_exit_ip"
		result.Error = "probe response did not contain a valid exit IP"
		return result
	}
	result.ExitIP = "ok"
	result.Stage = "complete"
	result.OK = true
	return result
}

func observeDiagnosticConnect(client *http.Client, status *int) {
	if transport, ok := client.Transport.(*http.Transport); ok {
		previous := transport.OnProxyConnectResponse
		transport.OnProxyConnectResponse = func(ctx context.Context, proxyURL *url.URL, request *http.Request, response *http.Response) error {
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				*status = response.StatusCode
			}
			if previous != nil {
				return previous(ctx, proxyURL, request, response)
			}
			return nil
		}
	}
}

func proxyMetadataResponse(entry ProxyEntry) any {
	hasPassword := entry.Password != ""
	entry.Password = ""
	return struct {
		ProxyEntry
		HasPassword bool `json:"has_password"`
	}{ProxyEntry: entry, HasPassword: hasPassword}
}

// overviewCacheKey returns the Redis key for a cached overview response, or ""
// when the query is not cacheable. The default dashboard shares one key;
// ranged queries are keyed by their validated window so different ranges never
// collide, and include_breakdown responses get their own suffix. The key
// computation repeats handleOverview's validation so a cache hit can never
// bypass range checks.
func overviewCacheKey(namespace string, query map[string][]string) string {
	if len(query) == 0 {
		return namespace + "overview"
	}
	if len(query) > 4 {
		return ""
	}
	fromText, toText, stepText := queryValue(query, "from"), queryValue(query, "to"), queryValue(query, "step")
	if fromText == "" || toText == "" || stepText == "" {
		return ""
	}
	from, fromErr := strconv.ParseInt(fromText, 10, 64)
	to, toErr := strconv.ParseInt(toText, 10, 64)
	step, stepErr := strconv.ParseInt(stepText, 10, 64)
	if fromErr != nil || toErr != nil || stepErr != nil || from < 1 || to < 1 || to > 253402300799 || !validStep(step) || to <= from {
		return ""
	}
	alignedFrom, alignedTo := from/60*60, (to+59)/60*60
	if alignedTo <= alignedFrom || alignedTo-alignedFrom > 366*86400 || (alignedTo-alignedFrom)/step > 600 {
		return ""
	}
	key := namespace + "overview:" + fromText + ":" + toText + ":" + stepText
	for name, values := range query {
		if name == "from" || name == "to" || name == "step" {
			continue
		}
		if name != "include_breakdown" || len(values) != 1 || values[0] != "true" {
			return ""
		}
		key += ":breakdown"
	}
	return key
}

// Cache errors always fall through to PostgreSQL; ranged and default overviews
// share the same 10s staleness the dashboard already tolerates.
func (p *Proxy) serveCachedOverview(w http.ResponseWriter, r *http.Request) bool {
	d := p.Store.DB
	if d == nil || d.Redis == nil {
		return false
	}
	key := overviewCacheKey(d.Namespace, r.URL.Query())
	if key == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
	defer cancel()
	raw, err := d.Redis.Get(ctx, key).Bytes()
	if err != nil || !json.Valid(raw) {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
	return true
}

func (p *Proxy) writeOverview(w http.ResponseWriter, r *http.Request, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		p.storeError(w, err)
		return
	}
	if d := p.Store.DB; d != nil && d.Redis != nil {
		if key := overviewCacheKey(d.Namespace, r.URL.Query()); key != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
			_ = d.Redis.Set(ctx, key, raw, 10*time.Second).Err()
			cancel()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (p *Proxy) managementSettings() (Settings, error) {
	if p.Settings != nil {
		return p.Settings.Latest()
	}
	return p.settingsValue()
}
