package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func parseManagedLogRequest(r *http.Request) (managedLogQuery, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return managedLogQuery{}, errInvalid
	}
	return parseManagedLogQuery(values)
}

func (p *Proxy) handleManagedLogs(w http.ResponseWriter, r *http.Request) {
	q, err := parseManagedLogRequest(r)
	if err != nil {
		adminError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
	defer cancel()
	err = p.resolveManagedUsageSnapshot(ctx, &q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	result, err := p.Metrics.managedLogs(ctx, q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, result)
}

type managedUsageTotals struct {
	managedCacheReadTotals
	AuthenticationSamples     int64    `json:"authentication_samples"`
	AverageAuthentication     *float64 `json:"average_authentication_ms"`
	ResponseTransferSamples   int64    `json:"response_transfer_samples"`
	AverageResponseTransfer   *float64 `json:"average_response_transfer_ms"`
	ImageInputSamples         int64    `json:"image_input_samples"`
	ImageInputTokens          *int64   `json:"total_image_input_tokens"`
	ImageInputTotalOverflow   bool     `json:"image_input_total_overflow,omitempty"`
	ImageOutputSamples        int64    `json:"image_output_samples"`
	ImageOutputTokens         *int64   `json:"total_image_output_tokens"`
	ImageOutputTotalOverflow  bool     `json:"image_output_total_overflow,omitempty"`
	RequestPreparationSamples int64    `json:"request_preparation_samples"`
	AverageRequestPreparation *float64 `json:"average_request_preparation_ms"`
	AccountSelectionSamples   int64    `json:"account_selection_samples"`
	AverageAccountSelection   *float64 `json:"average_account_selection_ms"`
	ProxySelectionSamples     int64    `json:"proxy_selection_samples"`
	AverageProxySelection     *float64 `json:"average_proxy_selection_ms"`
	Requests                  int64    `json:"total_requests"`
	Success                   int64    `json:"success"`
	Errors                    int64    `json:"errors"`
	Aborted                   int64    `json:"aborted"`
	Input                     int64    `json:"total_input_tokens"`
	Output                    int64    `json:"total_output_tokens"`
	Cached                    int64    `json:"total_cached_tokens"`
	Tokens                    int64    `json:"total_tokens"`
	LatencySamples            int64    `json:"latency_samples"`
	AverageLatency            *float64 `json:"average_latency_ms"`
	UsageKnownRequests        int64    `json:"usage_known_requests"`
	UsageUnknownRequests      int64    `json:"usage_unknown_requests"`
	TTFTSamples               int64    `json:"ttft_samples"`
	AverageTTFT               *float64 `json:"average_ttft_ms"`
	ReasoningSamples          int64    `json:"reasoning_samples"`
	ReasoningTokens           *int64   `json:"total_reasoning_tokens"`
	CacheWriteSamples         int64    `json:"cache_write_samples"`
	CacheWriteTokens          *int64   `json:"total_cache_write_tokens"`
	ttftSum                   float64
	latency                   float64
}

func (t *managedUsageTotals) add(r RequestRecord) {
	t.addCacheRead(r)
	for _, stage := range []struct {
		value   *int64
		samples *int64
		average **float64
	}{{r.RequestPreparationMs, &t.RequestPreparationSamples, &t.AverageRequestPreparation}, {r.AccountSelectionMs, &t.AccountSelectionSamples, &t.AverageAccountSelection}, {r.ProxySelectionMs, &t.ProxySelectionSamples, &t.AverageProxySelection}, {r.AuthenticationMs, &t.AuthenticationSamples, &t.AverageAuthentication}, {r.ResponseTransferMs, &t.ResponseTransferSamples, &t.AverageResponseTransfer}} {
		if stage.value != nil && *stage.value >= 0 {
			sum := float64(0)
			if *stage.average != nil {
				sum = **stage.average * float64(*stage.samples)
			}
			*stage.samples++
			avg := (sum + float64(*stage.value)) / float64(*stage.samples)
			if *stage.average == nil {
				*stage.average = new(float64)
			}
			**stage.average = avg
		}
	}
	for _, image := range []struct {
		value    *int64
		samples  *int64
		total    **int64
		overflow *bool
	}{
		{r.ImageInputTokens, &t.ImageInputSamples, &t.ImageInputTokens, &t.ImageInputTotalOverflow},
		{r.ImageOutputTokens, &t.ImageOutputSamples, &t.ImageOutputTokens, &t.ImageOutputTotalOverflow},
	} {
		if image.value == nil || *image.value < 0 {
			continue
		}
		*image.samples++
		if *image.overflow {
			continue
		}
		if *image.total == nil {
			*image.total = new(int64)
		}
		if *image.value > math.MaxInt64-**image.total {
			*image.overflow, *image.total = true, nil
		} else {
			**image.total += *image.value
		}
	}
	t.Requests++
	if r.TTFTMs != nil && *r.TTFTMs >= 0 {
		t.TTFTSamples++
		t.ttftSum += float64(*r.TTFTMs)
		avg := float64(t.ttftSum) / float64(t.TTFTSamples)
		if t.AverageTTFT == nil {
			t.AverageTTFT = new(float64)
		}
		*t.AverageTTFT = avg
	}
	if r.ReasoningTokens != nil && *r.ReasoningTokens >= 0 {
		t.ReasoningSamples++
		if t.ReasoningTokens == nil {
			t.ReasoningTokens = new(int64)
		}
		*t.ReasoningTokens += *r.ReasoningTokens
	}
	if cacheObservationEligible(r) && r.CacheWriteTokens != nil && *r.CacheWriteTokens >= 0 {
		t.CacheWriteSamples++
		if t.CacheWriteTokens == nil {
			t.CacheWriteTokens = new(int64)
		}
		*t.CacheWriteTokens += *r.CacheWriteTokens
	}
	if known := managedUsageKnown(r); known != nil && *known {
		t.UsageKnownRequests++
	} else {
		t.UsageUnknownRequests++
	}
	if r.Status == "ok" {
		t.Success++
	} else {
		t.Errors++
	}
	if r.Status == "aborted" {
		t.Aborted++
	}
	t.Input += r.InputTokens
	t.Output += r.OutputTokens
	t.Cached += r.CachedTokens
	t.Tokens += r.InputTokens + r.OutputTokens
	if r.hasMeasuredLatency() {
		t.LatencySamples++
		t.latency += float64(r.LatencyMs)
		avg := t.latency / float64(t.LatencySamples)
		t.AverageLatency = &avg
	}
}

type managedUsageDimension struct {
	Name string `json:"name"`
	managedUsageTotals
}
type managedUsageResource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	managedUsageTotals
}

type managedUsagePoint struct {
	Time int64 `json:"time"`
	managedUsageTotals
}
type managedUsageStats struct {
	SentModels     []managedUsageDimension `json:"sent_models"`
	ReturnedModels []managedUsageDimension `json:"returned_models"`
	InboundPaths   []managedUsageDimension `json:"inbound_paths"`
	UpstreamPaths  []managedUsageDimension `json:"upstream_paths"`
	managedUsageTotals
	Timezone            string                  `json:"timezone"`
	Groups              []managedUsageResource  `json:"groups"`
	Scope               string                  `json:"scope"`
	From                int64                   `json:"from"`
	To                  int64                   `json:"to"`
	FirstRecordAt       int64                   `json:"first_record_at"`
	LastRecordAt        int64                   `json:"last_record_at"`
	CheckedAt           int64                   `json:"checked_at"`
	Step                int64                   `json:"step"`
	Accounts            []managedUsageResource  `json:"accounts"`
	Keys                []managedUsageResource  `json:"keys"`
	Models              []managedUsageDimension `json:"models"`
	UpstreamModels      []managedUsageDimension `json:"upstream_models"`
	Endpoints           []managedUsageDimension `json:"endpoints"`
	ErrorCategories     []managedUsageDimension `json:"error_categories"`
	ErrorSeverities     []managedUsageDimension `json:"error_severities"`
	ErrorSeveritySource string                  `json:"error_severity_source"`
	Series              []managedUsagePoint     `json:"series"`
	Available           map[string]bool         `json:"available"`
}

func (m *Metrics) managedLogStats(ctx context.Context, q managedLogQuery) (managedUsageStats, error) {
	now := time.Now().Unix()
	span := q.To - q.From
	if q.To == 0 {
		span = now - q.From
	}
	step := q.Step
	if step == 0 {
		step = 3600
		if q.From == 0 || span > 90*86400 {
			step = 2592000
		} else if span > 7*86400 {
			step = 86400
		}
	}
	end := q.To
	if end == 0 {
		end = now
	}
	if q.From == 0 && step < 86400 || q.From > 0 && (end-q.From)/step > 5000 {
		return managedUsageStats{}, fmt.Errorf("%w: narrow the range for this chart interval", errInvalid)
	}
	location, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return managedUsageStats{}, errInvalid
	}
	out := managedUsageStats{Timezone: q.Timezone, Groups: []managedUsageResource{}, Scope: "retained_request_details", From: q.From, To: q.To, CheckedAt: now, Step: step,
		Accounts: []managedUsageResource{}, Keys: []managedUsageResource{}, Models: []managedUsageDimension{}, UpstreamModels: []managedUsageDimension{}, Endpoints: []managedUsageDimension{}, ErrorCategories: []managedUsageDimension{}, Series: []managedUsagePoint{},
		Available: map[string]bool{"tokens": true, "latency": true, "ttft": false, "reasoning_tokens": false, "cache_creation_tokens": false}}
	out.SentModels, out.ReturnedModels, out.InboundPaths, out.UpstreamPaths = []managedUsageDimension{}, []managedUsageDimension{}, []managedUsageDimension{}, []managedUsageDimension{}
	out.ErrorSeverities, out.ErrorSeveritySource = []managedUsageDimension{}, managedErrorSeveritySource
	if m.db != nil {
		return m.managedLogStatsPG(ctx, q, out)
	}
	models, endpoints, categories := map[string]*managedUsageTotals{}, map[string]*managedUsageTotals{}, map[string]*managedUsageTotals{}
	upstreamModels := map[string]*managedUsageTotals{}
	severities := map[string]*managedUsageTotals{}
	observed := map[string]map[string]*managedUsageTotals{"sent_model": {}, "returned_model": {}, "inbound_path": {}, "upstream_path": {}}
	points := map[int64]*managedUsageTotals{}
	accounts, keys := map[string]*managedUsageResource{}, map[string]*managedUsageResource{}
	groups := map[string]*managedUsageResource{}
	addResource := func(values map[string]*managedUsageResource, id, name string, r RequestRecord) {
		key := id
		if key == "" {
			key = "name:" + name
		}
		if values[key] == nil {
			values[key] = &managedUsageResource{ID: id, Name: name}
		}
		v := values[key]
		if name > v.Name {
			v.Name = name
		}
		v.add(r)
	}
	add := func(values map[string]*managedUsageTotals, key string, r RequestRecord) {
		if values[key] == nil {
			values[key] = &managedUsageTotals{}
		}
		values[key].add(r)
	}
	err = m.walkManagedLogs(ctx, q, func(r RequestRecord) error {
		addResource(accounts, r.AccountID, r.Account, r)
		if q.accountsOnly {
			return nil
		}
		out.add(r)
		addResource(keys, r.KeyID, r.KeyName, r)
		for _, id := range uniqueStrings(r.Groups) {
			addResource(groups, id, id, r)
		}
		if out.FirstRecordAt == 0 || r.Time < out.FirstRecordAt {
			out.FirstRecordAt = r.Time
		}
		if r.Time > out.LastRecordAt {
			out.LastRecordAt = r.Time
		}
		add(models, r.Model, r)
		if r.UpstreamModel != "" {
			add(upstreamModels, r.UpstreamModel, r)
		}
		for _, field := range []struct{ name, value string }{{"sent_model", r.SentModel}, {"returned_model", r.ReturnedModel}, {"inbound_path", r.InboundPath}, {"upstream_path", r.UpstreamPath}} {
			if field.value != "" {
				add(observed[field.name], field.value, r)
			}
		}
		add(endpoints, r.Endpoint, r)
		if r.Status != "ok" {
			add(categories, managedFailureCategory(r), r)
			add(severities, managedErrorSeverity(r), r)
		}
		bucket := managedUsageBucket(r.Time, step, location)
		if points[bucket] == nil {
			points[bucket] = &managedUsageTotals{}
		}
		points[bucket].add(r)
		return nil
	})
	if err != nil {
		return out, err
	}
	dimensions := func(values map[string]*managedUsageTotals) []managedUsageDimension {
		result := []managedUsageDimension{}
		for k, v := range values {
			result = append(result, managedUsageDimension{k, *v})
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].Requests == result[j].Requests {
				return result[i].Name < result[j].Name
			}
			return result[i].Requests > result[j].Requests
		})
		return result
	}
	for _, v := range accounts {
		out.Accounts = append(out.Accounts, *v)
	}
	if q.accountsOnly {
		sortManagedResources(&out)
		return out, nil
	}
	for _, v := range keys {
		out.Keys = append(out.Keys, *v)
	}
	for _, v := range groups {
		out.Groups = append(out.Groups, *v)
	}
	sortManagedResources(&out)
	out.Models = dimensions(models)
	out.UpstreamModels = dimensions(upstreamModels)
	out.SentModels, out.ReturnedModels, out.InboundPaths, out.UpstreamPaths = dimensions(observed["sent_model"]), dimensions(observed["returned_model"]), dimensions(observed["inbound_path"]), dimensions(observed["upstream_path"])
	out.Endpoints = dimensions(endpoints)
	out.ErrorCategories = dimensions(categories)
	out.ErrorSeverities = dimensions(severities)
	sort.Slice(out.ErrorSeverities, func(i, j int) bool {
		return managedErrorSeverityOrder(out.ErrorSeverities[i].Name) < managedErrorSeverityOrder(out.ErrorSeverities[j].Name)
	})
	for t, v := range points {
		out.Series = append(out.Series, managedUsagePoint{t, *v})
	}
	sort.Slice(out.Series, func(i, j int) bool { return out.Series[i].Time < out.Series[j].Time })
	out.setCoverage()
	err = out.fillSeries()
	return out, err
}
func (out *managedUsageStats) setCoverage() {
	if out.Available == nil {
		// Callers that build the struct directly (e.g. OverviewBreakdown's
		// daily grouping) skip the managedLogStats initialization.
		out.Available = map[string]bool{}
	}
	out.Available["image_input_tokens"] = out.ImageInputSamples > 0
	out.Available["image_output_tokens"] = out.ImageOutputSamples > 0
	out.Available["authentication_ms"] = out.AuthenticationSamples > 0
	out.Available["response_transfer_ms"] = out.ResponseTransferSamples > 0
	out.Available["ttft"] = out.TTFTSamples > 0
	out.Available["reasoning_tokens"] = out.ReasoningSamples > 0
	out.Available["cache_write_tokens"] = out.CacheWriteSamples > 0
	out.Available["cache_creation_tokens"] = out.CacheWriteSamples > 0
	out.Available["cache_read_tokens"] = out.CacheReadSamples > 0
}
func (p *Proxy) handleManagedLogStats(w http.ResponseWriter, r *http.Request) {
	q, err := parseManagedLogRequest(r)
	if err != nil {
		adminError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
	defer cancel()
	err = p.resolveManagedUsageSnapshot(ctx, &q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	result, err := p.Metrics.managedLogStats(ctx, q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	if p.Groups != nil {
		// Stats already contains only group IDs observed in the filtered
		// snapshot. Resolve those names with bounded ID probes instead of
		// loading the entire groups table on every logs/stats request.
		wanted := make(map[string]struct{}, len(result.Groups))
		for _, group := range result.Groups {
			if group.ID != "" {
				wanted[group.ID] = struct{}{}
			}
		}
		groups, e := p.Groups.managedNames(r.Context(), wanted)
		if e != nil {
			p.storeError(w, e)
			return
		}
		for i := range result.Groups {
			if name := groups[result.Groups[i].ID]; name != "" {
				result.Groups[i].Name = name
			}
		}
	}
	writeJSON(w, result)
}
func (p *Proxy) handleManagedLogDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > 200 {
		adminError(w, errInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
	defer cancel()
	q, err := parseManagedLogRequest(r)
	if err != nil {
		adminError(w, err)
		return
	}
	err = p.resolveManagedUsageSnapshot(ctx, &q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	record, err := p.Metrics.managedLogDetail(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errManagedLogNotFound) {
		adminAPIError(w, 404, "not_found", "usage record not found")
		return
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	member, err := p.Metrics.managedDetailInSnapshot(ctx, q, record)
	if err != nil {
		p.storeError(w, err)
		return
	}
	if !member {
		adminAPIError(w, http.StatusNotFound, "not_found", "usage record is not part of this query snapshot")
		return
	}
	managed := managedRecord(record)
	writeJSON(w, map[string]any{"record": managed, "scope": "retained_request_details"})
}

func safeCSV(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}
func (m *Metrics) exportManagedLogs(ctx context.Context, q managedLogQuery, format string, writer io.Writer) (int64, error) {
	return m.exportManagedLogsWithPrivacy(ctx, q, format, writer, false)
}
func (p *Proxy) handleManagedLogExport(w http.ResponseWriter, r *http.Request) {
	q, err := parseManagedLogRequest(r)
	if err != nil {
		adminError(w, err)
		return
	}
	redacted := true
	if value := r.URL.Query().Get("redacted"); value != "" {
		if value != "true" && value != "false" {
			adminError(w, errInvalid)
			return
		}
		redacted = value == "true"
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "csv"
	}
	if !stringIn(format, "csv", "jsonl") {
		adminError(w, errInvalid)
		return
	}
	// A failed scan must never download a deceptively successful partial file.
	// Materialize to a private temporary file before committing response headers.
	file, err := os.CreateTemp("", "codex-usage-export-*")
	if err != nil {
		p.storeError(w, err)
		return
	}
	defer func() { file.Close(); os.Remove(file.Name()) }()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	err = p.resolveManagedUsageSnapshot(ctx, &q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	count, err := p.Metrics.exportManagedLogsWithPrivacy(ctx, q, format, file, redacted)
	if err != nil {
		p.storeError(w, err)
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		p.storeError(w, err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		p.storeError(w, err)
		return
	}
	contentType := "application/x-ndjson; charset=utf-8"
	if format == "csv" {
		contentType = "text/csv; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="usage-%s.%s"`, time.Now().UTC().Format("20060102-150405"), format))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Export-Count", strconv.FormatInt(count, 10))
	w.Header().Set("X-Usage-Scope", "retained_request_details")
	w.Header().Set("X-Export-Redacted", strconv.FormatBool(redacted))
	w.Header().Set("X-Export-Schema", managedUsageExportSchema)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, file)
	}
}

func managedNullableNumber(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}
func managedNullableBool(value *bool) string {
	if value == nil {
		return ""
	}
	return strconv.FormatBool(*value)
}
func managedKnownText(r RequestRecord) string {
	if value := managedUsageKnown(r); value != nil {
		return strconv.FormatBool(*value)
	}
	return ""
}

func sortManagedResources(out *managedUsageStats) {
	for _, rows := range [][]managedUsageResource{out.Accounts, out.Keys, out.Groups} {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Requests == rows[j].Requests {
				return rows[i].ID+rows[i].Name < rows[j].ID+rows[j].Name
			}
			return rows[i].Requests > rows[j].Requests
		})
	}
}
