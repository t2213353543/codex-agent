package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codex-agent/internal/bps"
)

// RequestRecord is one gateway request as it appears in logs.jsonl and in
// the in-memory recent-request ring.
type RequestRecord struct {
	BoundSelection      *GatewayBoundSelectionObservation `json:"bound_selection,omitempty"`
	StructuredMixed     *bps.StructuredMixedObservation   `json:"structured_mixed,omitempty"`
	ToolPolicy          *bps.ToolPolicyObservation        `json:"tool_policy,omitempty"`
	turnCostObservation *appServerCostObservation
	// Live response ownership must survive truncation/redaction of ResponseID.
	// This bounded digest is not serialized into logs or the management API.
	responseIdentityDigest   [32]byte
	responseIdentityKnown    bool
	httpCompleted            bool     // Live HTTP-only event semantics, never serialized.
	unboundedPayload         bool     // The lightweight official route has no local payload ceiling.
	ID                       int64    `json:"id,omitempty"`
	EventID                  string   `json:"event_id,omitempty"`
	AccountID                string   `json:"account_id,omitempty"`
	AccountCredentialVersion int64    `json:"account_credential_version,omitempty"`
	AccountNameSnapshot      string   `json:"account_name_snapshot,omitempty"`
	Time                     int64    `json:"time"`
	KeyID                    string   `json:"key_id,omitempty"`
	KeyName                  string   `json:"key_name,omitempty"`
	Account                  string   `json:"account,omitempty"`
	Groups                   []string `json:"groups,omitempty"`
	Model                    string   `json:"model,omitempty"`
	Endpoint                 string   `json:"endpoint,omitempty"` // responses | compact
	Stream                   bool     `json:"stream,omitempty"`
	Status                   string   `json:"status"` // ok | error | aborted
	HTTPCode                 int      `json:"http_code,omitempty"`
	LatencyMs                int64    `json:"latency_ms,omitempty"`
	LatencyKnown             *bool    `json:"latency_known,omitempty"`
	WindowCostUSD            *float64 `json:"window_cost_usd,omitempty"`
	WindowCostPriceVersion   string   `json:"window_cost_price_version,omitempty"`
	InputTokens              int64    `json:"input_tokens,omitempty"`
	OutputTokens             int64    `json:"output_tokens,omitempty"`
	CachedTokens             int64    `json:"cached_tokens,omitempty"`
	CachedTokensKnown        *bool    `json:"cached_tokens_known,omitempty"`
	Error                    string   `json:"error,omitempty"`
	ErrorParam               string   `json:"error_param,omitempty"` // Fixed protocol field identifier only.
	ProxyName                string   `json:"proxy_name,omitempty"`
	IP                       string   `json:"ip,omitempty"`
	// Optional observations distinguish old/unobserved values from a measured 0.
	// They do not participate in admission, retries, quotas or billing totals.
	RequestID                string   `json:"request_id,omitempty"`
	ClientRequestID          string   `json:"client_request_id,omitempty"`
	ClientRequestIDSource    string   `json:"client_request_id_source,omitempty"`
	ClientRequestIDScope     string   `json:"client_request_id_scope,omitempty"`
	AuthenticationMs         *int64   `json:"authentication_ms,omitempty"`
	ResponseTransferMs       *int64   `json:"response_transfer_ms,omitempty"`
	OfficialRuntimeMs        *int64   `json:"official_runtime_ms,omitempty"`
	OfficialInferenceMs      *int64   `json:"official_inference_ms,omitempty"`
	TimingSource             string   `json:"timing_source,omitempty"`
	ImageInputTokens         *int64   `json:"image_input_tokens,omitempty"`
	ImageOutputTokens        *int64   `json:"image_output_tokens,omitempty"`
	ImageInputTokensSource   string   `json:"image_input_tokens_source,omitempty"`
	ImageOutputTokensSource  string   `json:"image_output_tokens_source,omitempty"`
	ResponseID               string   `json:"response_id,omitempty"`
	UpstreamModel            string   `json:"upstream_model,omitempty"`
	UserAgent                string   `json:"user_agent,omitempty"`
	RequestType              string   `json:"request_type,omitempty"`
	WebSocketWarmup          *bool    `json:"websocket_warmup,omitempty"`
	ServiceTier              string   `json:"service_tier,omitempty"`
	ReasoningEffort          string   `json:"reasoning_effort,omitempty"`
	TTFTMs                   *int64   `json:"ttft_ms,omitempty"`
	ReasoningTokens          *int64   `json:"reasoning_tokens,omitempty"`
	CacheWriteTokens         *int64   `json:"cache_write_tokens,omitempty"`
	UsageKnown               *bool    `json:"usage_known,omitempty"`
	RequestedServiceTier     string   `json:"requested_service_tier,omitempty"`
	RequestedReasoningEffort string   `json:"requested_reasoning_effort,omitempty"`
	ServiceTierSource        string   `json:"service_tier_source,omitempty"`
	ReasoningEffortSource    string   `json:"reasoning_effort_source,omitempty"`
	UpstreamModelSource      string   `json:"upstream_model_source,omitempty"`
	NativeCompaction         *bool    `json:"native_compaction,omitempty"`
	UsageTotalTokens         *int64   `json:"usage_total_tokens,omitempty"`
	RolloutBudgetUnits       *float64 `json:"rollout_budget_units,omitempty"`
	UsageAmount              string   `json:"usage_amount,omitempty"`
	UpstreamHeaderMs         *int64   `json:"upstream_header_ms,omitempty"`
	FirstOutputKind          string   `json:"first_output_kind,omitempty"`
	TerminalStatus           string   `json:"terminal_status,omitempty"`
	IncompleteReason         string   `json:"incomplete_reason,omitempty"`
	ErrorSource              string   `json:"error_source,omitempty"`
	// Recognized failure of the generation whose response was selected for this
	// request. Later OAuth attempts do not replace this generation provenance.
	FinalUpstreamErrorCode   string                         `json:"final_upstream_error_code,omitempty"`
	FinalUpstreamErrorSource string                         `json:"final_upstream_error_source,omitempty"`
	ErrorPhase               string                         `json:"error_phase,omitempty"`
	SentModel                string                         `json:"sent_model,omitempty"`
	SentReasoningEffort      string                         `json:"sent_reasoning_effort,omitempty"`
	ReturnedModel            string                         `json:"returned_model,omitempty"`
	InboundPath              string                         `json:"inbound_path,omitempty"`
	UpstreamPath             string                         `json:"upstream_path,omitempty"`
	RequestPreparationMs     *int64                         `json:"request_preparation_ms,omitempty"`
	AccountSelectionMs       *int64                         `json:"account_selection_ms,omitempty"`
	ProxySelectionMs         *int64                         `json:"proxy_selection_ms,omitempty"`
	Attempts                 []GatewayAttemptSummary        `json:"attempts,omitempty"`
	AttemptsTruncated        *bool                          `json:"attempts_truncated,omitempty"`
	ObservationSchemaVersion int                            `json:"observation_schema_version,omitempty"`
	UsageSchemaSource        string                         `json:"usage_schema_source,omitempty"`
	ErrorTruncated           *bool                          `json:"error_truncated,omitempty"`
	PromptCache              *GatewayPromptCacheObservation `json:"prompt_cache,omitempty"`
}

// hasMeasuredLatency preserves the meaning of legacy omitted zero durations.
func (r RequestRecord) hasMeasuredLatency() bool {
	return r.LatencyMs >= 0 && (r.LatencyKnown != nil && *r.LatencyKnown || r.LatencyKnown == nil && r.LatencyMs > 0)
}

func (r RequestRecord) measuredLatency() *int64 {
	if !r.hasMeasuredLatency() {
		return nil
	}
	value := r.LatencyMs
	return &value
}

func averageMilliseconds(sum, samples int64) *float64 {
	if samples <= 0 {
		return nil
	}
	average := float64(sum) / float64(samples)
	return &average
}

func (r RequestRecord) tokens() int64 { return r.InputTokens + r.OutputTokens }

// minuteCounts aggregates one unix minute of gateway traffic.
type minuteCounts struct {
	Requests, OK, Errors          int64
	Tokens, Input, Output, Cached int64
	Latency, LatencyN             int64
}

// dayCounts aggregates one unix day for a single account or key.
type dayCounts struct {
	Requests, OK, Errors          int64
	Tokens, Input, Output, Cached int64
	Latency, LatencyN             int64
}

func (c *dayCounts) add(rec RequestRecord) {
	c.Requests++
	switch rec.Status {
	case "ok":
		c.OK++
	default:
		c.Errors++
	}
	c.Tokens += rec.tokens()
	c.Input += rec.InputTokens
	c.Output += rec.OutputTokens
	c.Cached += rec.CachedTokens
	if rec.hasMeasuredLatency() {
		c.Latency += rec.LatencyMs
		c.LatencyN++
	}
}

// usageCounts is the in-memory per-key aggregate shown on the key page.
type usageCounts struct {
	Requests, Tokens, Errors int64
	LastAt                   int64
}

const (
	metricsRetentionDays = 92
	recentRingCapacity   = 20000
	logsScanCap          = 100000 // max lines scanned backwards per query
)

type statsState struct {
	SavedAt  int64                           `json:"saved_at"`
	Minutes  map[int64]*minuteCounts         `json:"minutes"`
	Accounts map[string]map[int64]*dayCounts `json:"accounts"`
}

// Metrics records gateway usage: a log file for the usage-records page,
// minute buckets for the dashboard, per-account day buckets for quota
// windows, a recent-request ring for account dot charts, and per-key
// aggregates for the key page.
type Metrics struct {
	realtime     dashboardRealtimeMetrics
	db           *Database
	location     *time.Location
	pending      []RequestRecord
	writeErr     error
	readFailures uint64
	lastWriteAt  time.Time
	dir          string
	mu           sync.Mutex
	logFile      *os.File
	minutes      map[int64]*minuteCounts
	accounts     map[string]map[int64]*dayCounts
	recent       []RequestRecord
	recentAt     int
	keyUsage     map[string]*usageCounts
}

func NewMetrics(dir string) (*Metrics, error) {
	location, err := configuredStatsLocation()
	if err != nil {
		return nil, fmt.Errorf("STATS_TIMEZONE: %w", err)
	}
	m := &Metrics{
		location: location,
		dir:      dir,
		minutes:  map[int64]*minuteCounts{},
		accounts: map[string]map[int64]*dayCounts{},
		recent:   make([]RequestRecord, recentRingCapacity),
		keyUsage: map[string]*usageCounts{},
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create stats directory: %w", err)
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(m.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open request log: %w", err)
	}
	m.logFile = file
	return m, nil
}

func (m *Metrics) logPath() string   { return filepath.Join(m.dir, "logs.jsonl") }
func (m *Metrics) statsPath() string { return filepath.Join(m.dir, "stats.json") }

// Close releases the log file; used on shutdown and in tests so temp
// directories can be removed on Windows.
func (m *Metrics) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.logFile != nil {
		_ = m.logFile.Close()
		m.logFile = nil
	}
}

func (m *Metrics) load() error {
	raw, err := os.ReadFile(m.statsPath())
	missingSnapshot := errors.Is(err, os.ErrNotExist)
	if errors.Is(err, os.ErrNotExist) {
		raw = []byte(`{}`)
		err = nil
	}
	if err != nil {
		return fmt.Errorf("read stats store: %w", err)
	}
	var state statsState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode stats store: %w", err)
	}
	for minute, counts := range state.Minutes {
		m.minutes[minute] = counts
	}
	for account, days := range state.Accounts {
		m.accounts[account] = days
	}
	if missingSnapshot {
		file, err := os.Open(m.logPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), 8<<20)
		for scanner.Scan() {
			if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
				continue
			}
			var rec RequestRecord
			if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
				return errors.New("invalid legacy request log record")
			}
			m.aggregateMemory(rec)
		}
		return scanner.Err()
	}
	lines, err := readLinesBackward(m.logPath(), recentRingCapacity)
	if err != nil {
		return fmt.Errorf("restore recent requests: %w", err)
	}
	for _, line := range lines {
		var rec RequestRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		rec.Error = redactMetricError(rec.Error)
		if rec.ID == 0 {
			rec.ID = int64(m.recentAt + 1)
		}
		m.recent[m.recentAt%recentRingCapacity] = rec
		m.recentAt++
	}
	return nil
}

// Record persists and aggregates one request.
func (m *Metrics) Record(rec RequestRecord) {
	m.observeDashboardRealtimeCompleted(rec, time.Now().Unix())
	freezeManagedAccountRequestUSD(&rec)
	if m.db != nil {
		m.recordPG(rec)
		return
	}
	if rec.Time == 0 {
		rec.Time = time.Now().Unix()
	}
	rec.Error = redactMetricError(rec.Error)
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.logFile != nil {
		if _, err := m.logFile.Write(append(line, '\n')); err != nil {
			m.writeErr = fmt.Errorf("write request log: %w", err)
			log.Printf("metrics persistence failed: %v", m.writeErr)
			return
		}
	}
	m.aggregateMemory(rec)
}

func (m *Metrics) aggregateMemory(rec RequestRecord) {
	rec.Error = redactMetricError(rec.Error)
	if rec.ID == 0 {
		rec.ID = int64(m.recentAt + 1)
	}
	minute := rec.Time / 60 * 60
	counts := m.minutes[minute]
	if counts == nil {
		counts = &minuteCounts{}
		m.minutes[minute] = counts
	}
	counts.Requests++
	switch rec.Status {
	case "ok":
		counts.OK++
	default:
		counts.Errors++
	}
	counts.Tokens += rec.tokens()
	counts.Input += rec.InputTokens
	counts.Output += rec.OutputTokens
	counts.Cached += rec.CachedTokens
	if rec.hasMeasuredLatency() {
		counts.Latency += rec.LatencyMs
		counts.LatencyN++
	}
	if rec.Account != "" {
		days := m.accounts[rec.Account]
		if days == nil {
			days = map[int64]*dayCounts{}
			m.accounts[rec.Account] = days
		}
		day := rec.Time / 86400 * 86400
		accountCounts := days[day]
		if accountCounts == nil {
			accountCounts = &dayCounts{}
			days[day] = accountCounts
		}
		accountCounts.add(rec)
	}
	if rec.KeyID != "" {
		usage := m.keyUsage[rec.KeyID]
		if usage == nil {
			usage = &usageCounts{}
			m.keyUsage[rec.KeyID] = usage
		}
		usage.Requests++
		usage.Tokens += rec.tokens()
		if rec.Status != "ok" {
			usage.Errors++
		}
		if rec.Time > usage.LastAt {
			usage.LastAt = rec.Time
		}
	}
	m.recent[m.recentAt%recentRingCapacity] = rec
	m.recentAt++
}

// Save persists aggregates and syncs the log file. Called on a timer and at
// shutdown.
func (m *Metrics) Save() error {
	if m.db != nil {
		return m.savePG()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return m.writeErr
	}
	cutoff := time.Now().Add(-metricsRetentionDays * 24 * time.Hour).Unix()
	for minute := range m.minutes {
		if minute < cutoff {
			delete(m.minutes, minute)
		}
	}
	for account, days := range m.accounts {
		for day := range days {
			if day < cutoff {
				delete(days, day)
			}
		}
		if len(days) == 0 {
			delete(m.accounts, account)
		}
	}
	state := statsState{SavedAt: time.Now().Unix(), Minutes: m.minutes, Accounts: m.accounts}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(m.statsPath(), raw, 0600); err != nil {
		return err
	}
	if m.logFile != nil {
		return m.logFile.Sync()
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".stats-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// SeriesPoint is one aggregated step of the dashboard trend.
type SeriesPoint struct {
	overviewCacheTotals
	Ts        int64 `json:"ts"`
	Requests  int64 `json:"requests"`
	OK        int64 `json:"ok"`
	Errors    int64 `json:"errors"`
	Tokens    int64 `json:"tokens"`
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Cached    int64 `json:"cached"`
	LatencyMs int64 `json:"latency_ms"`
}

type rangeTotals struct {
	Requests, OK, Errors          int64
	Tokens, Input, Output, Cached int64
	Latency, LatencyN             int64
}

func (t *rangeTotals) add(minute *minuteCounts) {
	t.Requests += minute.Requests
	t.OK += minute.OK
	t.Errors += minute.Errors
	t.Tokens += minute.Tokens
	t.Input += minute.Input
	t.Output += minute.Output
	t.Cached += minute.Cached
	t.Latency += minute.Latency
	t.LatencyN += minute.LatencyN
}

// Series builds step-aggregated trend points for [from, to).
func (m *Metrics) Series(from, to, step int64) []SeriesPoint {
	if m.db != nil {
		return m.seriesPG(from, to, step)
	}
	if step < 60 {
		step = 60
	}
	if to <= from {
		to = from + step
	}
	if (to-from)/step > 10000 {
		step = (to-from)/10000 + 1
	}
	slots := int((to-from)/step) + 1
	points := make([]SeriesPoint, slots)
	for i := range points {
		points[i].Ts = from + int64(i)*step
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for minute, counts := range m.minutes {
		if minute < from || minute >= to {
			continue
		}
		index := int((minute - from) / step)
		if index < 0 || index >= slots {
			continue
		}
		point := &points[index]
		point.Requests += counts.Requests
		point.OK += counts.OK
		point.Errors += counts.Errors
		point.Tokens += counts.Tokens
		point.Input += counts.Input
		point.Output += counts.Output
		point.Cached += counts.Cached
		point.LatencyMs += counts.Latency
	}
	for i := range points {
		if points[i].Requests == 0 && points[i].LatencyMs > 0 {
			points[i].LatencyMs = 0
		}
	}
	return points
}

// Totals sums minute buckets in [from, to).
func (m *Metrics) Totals(from, to int64) rangeTotals {
	if m.db != nil {
		return m.totalsPG(from, to)
	}
	var totals rangeTotals
	m.mu.Lock()
	defer m.mu.Unlock()
	for minute, counts := range m.minutes {
		if minute < from || minute >= to {
			continue
		}
		totals.add(counts)
	}
	return totals
}

// GrandTotals sums every retained minute bucket.
func (m *Metrics) GrandTotals() rangeTotals {
	if m.db != nil {
		return m.grandTotalsPG()
	}
	var totals rangeTotals
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, counts := range m.minutes {
		totals.add(counts)
	}
	return totals
}

// CurrentRates reports requests/tokens per minute averaged over the last
// five minutes, matching the conventional RPM/TPM definition.
func (m *Metrics) CurrentRates() (rpm, tpm float64) {
	now := time.Now().Unix()
	from := (now/60 - 4) * 60
	totals := m.Totals(from, now+60)
	return float64(totals.Requests) / 5, float64(totals.Tokens) / 5
}

// AccountWindow aggregates one account's requests since windowStart using
// day buckets (the window start day is included).
func (m *Metrics) AccountWindow(account string, windowStart int64) (usage dayCounts) {
	if m.db != nil {
		return m.accountWindowPG(account, windowStart)
	}
	if windowStart == 0 {
		windowStart = 1
	}
	startDay := windowStart / 86400 * 86400
	m.mu.Lock()
	defer m.mu.Unlock()
	for day, counts := range m.accounts[account] {
		if day >= startDay {
			usage.Requests += counts.Requests
			usage.OK += counts.OK
			usage.Errors += counts.Errors
			usage.Tokens += counts.Tokens
			usage.Input += counts.Input
			usage.Output += counts.Output
			usage.Cached += counts.Cached
			usage.Latency += counts.Latency
			usage.LatencyN += counts.LatencyN
		}
	}
	return usage
}

// AccountTop ranks accounts by tokens inside [from, to). Short ranges use
// the recent ring, longer ranges use day buckets.
func (m *Metrics) AccountTop(from, to int64, limit int) []struct {
	Name     string `json:"name"`
	Tokens   int64  `json:"tokens"`
	Requests int64  `json:"requests"`
	Errors   int64  `json:"errors"`
} {
	if m.db != nil {
		return m.accountTopPG(from, to, limit)
	}
	type totals struct{ tokens, requests, errors int64 }
	byAccount := map[string]*totals{}
	m.mu.Lock()
	if to-from <= 48*3600 {
		for i := 0; i < recentRingCapacity; i++ {
			rec := m.recent[(m.recentAt+i)%recentRingCapacity]
			if rec.Time < from || rec.Time >= to || rec.Account == "" {
				continue
			}
			entry := byAccount[rec.Account]
			if entry == nil {
				entry = &totals{}
				byAccount[rec.Account] = entry
			}
			entry.tokens += rec.tokens()
			entry.requests++
			if rec.Status != "ok" {
				entry.errors++
			}
		}
	} else {
		startDay := from / 86400 * 86400
		endDay := (to-1)/86400*86400 + 86400
		for account, days := range m.accounts {
			for day, counts := range days {
				if day < startDay || day >= endDay {
					continue
				}
				entry := byAccount[account]
				if entry == nil {
					entry = &totals{}
					byAccount[account] = entry
				}
				entry.tokens += counts.Tokens
				entry.requests += counts.Requests
				entry.errors += counts.Errors
			}
		}
	}
	m.mu.Unlock()
	ranked := make([]struct {
		Name     string `json:"name"`
		Tokens   int64  `json:"tokens"`
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
	}, 0, len(byAccount))
	for account, entry := range byAccount {
		ranked = append(ranked, struct {
			Name     string `json:"name"`
			Tokens   int64  `json:"tokens"`
			Requests int64  `json:"requests"`
			Errors   int64  `json:"errors"`
		}{account, entry.tokens, entry.requests, entry.errors})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Tokens > ranked[j].Tokens })
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

// RecentForAccount returns the newest requests for one account.
func (m *Metrics) RecentForAccount(account string, limit int) []RequestRecord {
	if m.db != nil {
		page, err := m.Logs(LogQuery{Account: account, Size: limit})
		if err != nil {
			m.reportReadError(err)
		}
		return page.Items
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]RequestRecord, 0, limit)
	for i := 0; i < recentRingCapacity; i++ {
		rec := m.recent[(m.recentAt+i)%recentRingCapacity]
		if rec.Account == account && rec.Time > 0 {
			result = append(result, rec)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Time == result[j].Time {
			return result[i].ID > result[j].ID
		}
		return result[i].Time > result[j].Time
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

// KeyUsage returns the in-memory aggregate for one API key.
func (m *Metrics) KeyUsage(keyID string) usageCounts {
	if m.db != nil {
		return m.keyUsagePG(keyID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if usage := m.keyUsage[keyID]; usage != nil {
		return *usage
	}
	return usageCounts{}
}

// LastFor returns the most recent request record of one account.
func (m *Metrics) LastFor(account string) (RequestRecord, bool) {
	if m.db != nil {
		return m.lastForPG(account)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best RequestRecord
	found := false
	for i := 0; i < recentRingCapacity; i++ {
		rec := m.recent[(m.recentAt+i)%recentRingCapacity]
		if rec.Account == account && (rec.Time > best.Time || (rec.Time == best.Time && rec.ID > best.ID)) {
			best = rec
			found = true
		}
	}
	return best, found
}

// RecentErrors returns the newest failed requests inside [from, to).
func (m *Metrics) RecentErrors(from, to int64, limit int) []RequestRecord {
	if m.db != nil {
		page, err := m.Logs(LogQuery{From: from, To: to, Status: "failed", Size: limit})
		if err != nil {
			m.reportReadError(err)
		}
		return page.Items
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var errors []RequestRecord
	for i := 0; i < recentRingCapacity; i++ {
		rec := m.recent[(m.recentAt+i)%recentRingCapacity]
		if rec.Time >= from && rec.Time < to && rec.Status != "ok" && rec.Time > 0 {
			errors = append(errors, rec)
		}
	}
	sort.Slice(errors, func(i, j int) bool {
		if errors[i].Time == errors[j].Time {
			return errors[i].ID > errors[j].ID
		}
		return errors[i].Time > errors[j].Time
	})
	if len(errors) > limit {
		errors = errors[:limit]
	}
	return errors
}

// LogQuery filters the request log.
type LogQuery struct {
	From, To   int64
	Account    string
	AccountID  string
	Cursor     string
	Model      string
	KeyName    string
	Status     string
	Page, Size int
}

// LogPage is one page of the usage-records list.
type LogPage struct {
	Total      int64           `json:"total"`
	Items      []RequestRecord `json:"items"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor,omitempty"`
	TotalKind  string          `json:"total_kind"`
	Truncated  bool            `json:"truncated,omitempty"`
}

// Logs scans the append-only request log backwards and applies filters.
// Scanning stops early at logsScanCap lines, so totals are capped for very
// large files; the page itself always has the newest matches.
func (m *Metrics) Logs(query LogQuery) (LogPage, error) {
	if m.db != nil {
		return m.logsPG(query)
	}
	if query.Size <= 0 || query.Size > 500 {
		query.Size = 50
	}
	if query.Page < 1 {
		query.Page = 1
	}
	if query.From == 0 {
		query.From = 1
	}
	if query.To == 0 {
		query.To = time.Now().Unix() + 86400
	}
	lines, err := readLinesBackward(m.logPath(), logsScanCap+1)
	if err != nil {
		return LogPage{}, err
	}
	truncated := len(lines) > logsScanCap
	if truncated {
		lines = lines[len(lines)-logsScanCap:]
	}
	var matches []RequestRecord
	for index, line := range lines {
		if line == "" {
			continue
		}
		var rec RequestRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.ID == 0 {
			rec.ID = int64(index + 1)
		}
		rec.Error = redactMetricError(rec.Error)
		if !m.logMatches(rec, query) {
			continue
		}
		matches = append(matches, rec)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Time == matches[j].Time {
			return matches[i].ID > matches[j].ID
		}
		return matches[i].Time > matches[j].Time
	})
	total := int64(len(matches))
	totalKind := "exact"
	if truncated {
		totalKind = "scanned_window"
	}
	start := (query.Page - 1) * query.Size
	if start >= len(matches) {
		return LogPage{Total: total, Items: []RequestRecord{}, TotalKind: totalKind, Truncated: truncated}, nil
	}
	end := start + query.Size
	if end > len(matches) {
		end = len(matches)
	}
	return LogPage{Total: total, Items: matches[start:end], HasMore: end < len(matches), TotalKind: totalKind, Truncated: truncated}, nil
}

func (m *Metrics) logMatches(rec RequestRecord, query LogQuery) bool {
	if rec.Time < query.From || rec.Time >= query.To {
		return false
	}
	if query.Account != "" && rec.Account != query.Account {
		return false
	}
	if query.AccountID != "" && rec.AccountID != query.AccountID {
		return false
	}
	if query.Model != "" && rec.Model != query.Model {
		return false
	}
	if query.KeyName != "" && rec.KeyName != query.KeyName {
		return false
	}
	if query.Status == "failed" {
		return rec.Status != "ok"
	}
	if query.Status != "" && rec.Status != query.Status {
		return false
	}
	return true
}

// readLinesBackward reads up to maxLines from the end of a file in
// chronological order (oldest first).
func readLinesBackward(path string, maxLines int) ([]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 64 << 10
	lines := make([]string, 0, maxLines)
	var tail []byte
	for offset := info.Size(); offset > 0 && len(lines) < maxLines; {
		readSize := int64(chunk)
		if offset < readSize {
			readSize = offset
		}
		offset -= readSize
		buffer := make([]byte, readSize)
		if _, err := file.ReadAt(buffer, offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		buffer = append(buffer, tail...)
		tail = nil
		parts := bytes.Split(buffer, []byte{'\n'})
		if offset > 0 {
			tail = parts[0]
			parts = parts[1:]
		} else if len(parts) > 0 && len(parts[0]) == 0 {
			parts = parts[1:]
		}
		for i := len(parts) - 1; i >= 0 && len(lines) < maxLines; i-- {
			lines = append(lines, string(parts[i]))
		}
	}
	if len(tail) > 0 && len(lines) < maxLines {
		lines = append(lines, string(tail))
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines, nil
}

// usageFromResponse extracts token counts from a Responses API response
// object (either the nested response event or a compact JSON body).
func usageFromResponse(raw json.RawMessage) (input, output, cached int64, ok bool) {
	object := gatewayResponseObject(raw)
	if !jsonObject(object["usage"]) {
		return 0, 0, 0, false
	}
	var usage, nativeDetails, compatibleDetails map[string]json.RawMessage
	_ = json.Unmarshal(object["usage"], &usage)
	for _, name := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if details := bytes.TrimSpace(usage[name]); len(details) > 0 && !bytes.Equal(details, []byte("null")) && !jsonObject(details) {
			return 0, 0, 0, false
		}
	}
	_ = json.Unmarshal(usage["input_tokens_details"], &nativeDetails)
	_ = json.Unmarshal(usage["prompt_tokens_details"], &compatibleDetails)
	// Native fields are authoritative even when explicitly zero. An invalid
	// present field must not silently fall through to a different schema.
	var inputOK, outputOK, cachedOK bool
	input, inputOK = gatewayUsageCount(usage["input_tokens"], usage["prompt_tokens"])
	output, outputOK = gatewayUsageCount(usage["output_tokens"], usage["completion_tokens"])
	cached, cachedOK = gatewayUsageCount(nativeDetails["cached_tokens"], usage["cached_tokens"], compatibleDetails["cached_tokens"])
	if !inputOK || !outputOK || !cachedOK || output > math.MaxInt64-input {
		return 0, 0, 0, false
	}
	return input, output, cached, true
}

func gatewayUsageCount(candidates ...json.RawMessage) (int64, bool) {
	for _, raw := range candidates {
		if len(raw) == 0 {
			continue
		}
		count := observedGatewayCount(raw)
		if count == nil {
			return 0, false
		}
		return *count, true
	}
	return 0, true
}

// errorFromPayload extracts a concise upstream error message.
func errorFromPayload(raw json.RawMessage) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string          `json:"message"`
		Code    string          `json:"code"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		text := strings.TrimSpace(string(raw))
		if len(text) > 200 {
			text = text[:200] + "…"
		}
		return text
	}
	if payload.Error.Message != "" {
		return payload.Error.Message
	}
	if payload.Error.Code != "" {
		return payload.Error.Code
	}
	if payload.Message != "" {
		return payload.Message
	}
	if payload.Code != "" {
		return payload.Code
	}
	// FastAPI-style backend failures use detail. Keep non-string validation
	// payloads opaque instead of treating arrays or objects as diagnostics.
	var detail string
	if json.Unmarshal(payload.Detail, &detail) == nil {
		return detail
	}
	return ""
}

// sseScanner watches streamed SSE bytes for the terminal response event so
// streaming requests still contribute tokens and error state to metrics.
type sseScanner struct {
	line            []byte
	eventData       []byte
	rec             *RequestRecord
	finished        bool
	terminal        bool // a protocol terminal event, not a local observation limit
	closed          bool
	skipLF          bool
	started         bool
	observe         func([]byte)
	pending         []byte // Latest deferred HTTP event; legacy routes bound it by maxBody.
	pendingPriority int
	identity        gatewayHTTPCooldownIdentity
}

func (s *sseScanner) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !s.finished && !s.closed {
		if s.skipLF {
			s.skipLF = false
			s.consumeLine(s.line)
			s.line = s.line[:0]
			if s.finished {
				break
			}
			if p[0] == '\n' {
				p = p[1:]
				continue
			}
		}
		index := bytes.IndexAny(p, "\r\n")
		size := index
		if size < 0 {
			size = len(p)
		}
		if !s.rec.unboundedPayload && size > maxBody-len(s.line)-len(s.eventData) {
			s.rejectOversized()
			break
		}
		s.line = append(s.line, p[:size]...)
		if index < 0 {
			break
		}
		s.skipLF = p[index] == '\r'
		if !s.skipLF {
			s.consumeLine(s.line)
			s.line = s.line[:0]
		}
		p = p[index+1:]
	}
	return n, nil
}

// Finish closes the observer at EOF. Codex's eventsource-stream parser requires
// a blank line to dispatch an event, so an unterminated tail is not a success.
func (s *sseScanner) Finish() {
	if !s.closed && !s.finished && !validSSELine(s.line) {
		s.rejectInvalidUTF8()
	}
	if !s.closed && !s.finished && s.skipLF {
		s.consumeLine(s.line)
		s.skipLF = false
	}
	if !s.closed && !s.finished && s.pending != nil {
		payload := s.pending
		s.pending = nil
		if s.pendingPriority > 1 {
			s.finishTerminal(payload)
		}
	}
	s.closed = true
	if !s.finished && s.rec.Status == "" {
		s.rec.Status = "aborted"
		if s.rec.Error == "" {
			s.rec.Error = "upstream stream ended without a terminal response"
		}
	}
	s.line = nil
	s.eventData = nil
	s.pending = nil
}

func (s *sseScanner) rejectOversized() {
	s.finished = true
	s.rec.Status = "error"
	s.rec.Error = "upstream SSE event exceeds 32 MiB"
	s.line = nil
	s.eventData = nil
	s.pending = nil
}

func (s *sseScanner) consumeLine(line []byte) {
	line = stripInitialSSEBOM(line, &s.started)
	if !validSSELine(line) {
		s.rejectInvalidUTF8()
		return
	}
	if len(line) == 0 {
		s.consume()
		return
	}
	var value []byte
	switch {
	case bytes.HasPrefix(line, []byte("data:")):
		value = bytes.TrimPrefix(line[len("data:"):], []byte(" "))
	case bytes.Equal(line, []byte("data")):
		// A colon is optional for an empty SSE data field.
	default:
		return
	}
	if !s.rec.unboundedPayload && len(value)+1 > maxBody-len(s.eventData) {
		s.rejectOversized()
		return
	}
	s.eventData = append(s.eventData, value...)
	s.eventData = append(s.eventData, '\n')
}

func (s *sseScanner) rejectInvalidUTF8() {
	// This observer must stop counting, but must not tell writeStreamPipe to
	// truncate the opaque bytes being forwarded to the caller.
	s.finished = true
	s.rec.Status, s.rec.Error = "error", "upstream SSE contains invalid UTF-8"
	s.line, s.eventData = nil, nil
	s.pending = nil
}

func (s *sseScanner) consume() {
	if s.finished || len(s.eventData) == 0 {
		return
	}
	payload := bytes.TrimSuffix(s.eventData, []byte{'\n'})
	s.eventData = nil
	if !s.identity.known && s.rec.responseIdentityKnown {
		s.identity.digest, s.identity.known = s.rec.responseIdentityDigest, true
	}
	_, terminal, err := gatewayHTTPTerminalResponse(payload)
	priority := gatewayHTTPDeferredResponsePriority(payload)
	if priority != 1 && !(err != nil && gatewayHTTPIsCompleted(payload)) {
		_ = s.identity.accepts(payload)
	}
	if terminal && priority > 0 {
		if priority != 1 && !gatewayHTTPIsCompleted(payload) {
			observeGatewayHTTPPendingIdentity(s.rec, payload)
		}
		if priority >= s.pendingPriority {
			s.pending, s.pendingPriority = payload, priority
		}
		return
	}
	if !terminal {
		if s.observe != nil {
			s.observe(payload)
		}
		if err != nil {
			s.rejectInvalidUTF8()
		}
		return
	}
	s.pending = nil
	s.finishTerminal(payload)
}

func (s *sseScanner) finishTerminal(payload []byte) {
	response, _, err := gatewayHTTPTerminalResponse(payload)
	s.terminal = true
	s.finished = true
	if err != nil && gatewayHTTPIsCompleted(payload) {
		s.rec.Status, s.rec.Error = "error", err.Error()
		observeGatewayError(s.rec, "upstream", "response_event")
		s.line = nil
		return
	}
	// Terminal framing ends this SSE stream, but it does not authorize usage
	// from a conflicting envelope. Reuse the exact cooldown identity contract.
	if !s.identity.accepts(payload) {
		s.rec.Status, s.rec.Error = "error", "upstream response ID changed"
		observeGatewayError(s.rec, "upstream", "response_event")
		s.line = nil
		return
	}
	if s.observe != nil {
		s.observe(payload)
	}
	s.rec.httpCompleted = err == nil && gatewayHTTPIsCompleted(payload)
	var event responseEvent
	_ = gatewayWSDecodeExact(payload, map[string]any{"type": &event.Type})
	if gatewayResponseMatchesRecord(s.rec, gatewayResponseObject(response)) {
		observeGatewayTerminal(s.rec, gatewayResponseObject(response), event.Type)
	}
	recordGatewayHTTPResponseOutcome(s.rec, response)
	if err != nil {
		s.rec.Status = "error"
		s.rec.Error = err.Error()
		var upstream *upstreamResponseError
		if errors.As(err, &upstream) {
			if message := errorFromPayload(upstream.Payload); message != "" {
				s.rec.Error = message
			}
		}
	}
	s.line = nil
}
