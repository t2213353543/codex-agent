package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
)

const (
	maxCacheDiagnosticPending = 512
	maxCacheDiagnosticScopes  = 256
	maxCacheDiagnosticItems   = 4096
)

// gatewayCacheDiagnostics is an opt-in, in-memory experiment observer. It keeps
// keyed digests, counts and reported usage, never prompts, tool schemas, tokens,
// session identifiers or cache keys. A fresh random key prevents correlating
// content across experiments or guessing low-entropy text from stored digests.
type gatewayCacheDiagnostics struct {
	mu      sync.Mutex
	key     [32]byte
	next    uint64
	pending map[uint64]struct{}
	latest  map[[32]byte]gatewayCacheRequestDigest
	summary gatewayCacheDiagnosticsSummary
}

type gatewayCacheRequestDigest struct {
	sequence     uint64
	model        [32]byte
	instructions [32]byte
	tools        [32]byte
	controls     [32]byte
	cacheKey     [32]byte
	cachePresent bool
	input        [][32]byte
	inputCount   int
	inputIsArray bool
}

// Configuration and input-item equality are diagnostics, not token estimates.
// Hit ratios use only responses that explicitly report both input_tokens and
// cached_tokens; missing usage must never be silently counted as a cache miss.
type gatewayCacheDiagnosticsSummary struct {
	Requests                       int64    `json:"requests"`
	UnscopedRequests               int64    `json:"unscoped_requests"`
	ComparablePairs                int64    `json:"comparable_pairs"`
	StableConfigurationPairs       int64    `json:"stable_configuration_pairs"`
	ModelChangedPairs              int64    `json:"model_changed_pairs"`
	InstructionsChangedPairs       int64    `json:"instructions_changed_pairs"`
	ToolsChangedPairs              int64    `json:"tools_changed_pairs"`
	ControlsChangedPairs           int64    `json:"controls_changed_pairs"`
	CacheKeyChangedPairs           int64    `json:"cache_key_changed_pairs"`
	MissingCacheKeyRequests        int64    `json:"missing_cache_key_requests"`
	InputComparablePairs           int64    `json:"input_comparable_pairs"`
	SharedInputItems               int64    `json:"shared_input_items"`
	PreviousInputItemsCompared     int64    `json:"previous_input_items_compared"`
	InputHistoryExtensionPairs     int64    `json:"input_history_extension_pairs"`
	InputHistoryChangedPairs       int64    `json:"input_history_changed_pairs"`
	InputComparisonLimitedRequests int64    `json:"input_comparison_limited_requests"`
	ScopeEvictions                 int64    `json:"scope_evictions"`
	Responses                      int64    `json:"responses"`
	SuccessfulResponses            int64    `json:"successful_responses"`
	FailedResponses                int64    `json:"failed_responses"`
	IncompleteResponses            int64    `json:"incomplete_responses"`
	UnfinishedResponses            int64    `json:"unfinished_responses"`
	UsageResponses                 int64    `json:"usage_responses"`
	CacheUsageResponses            int64    `json:"cache_usage_responses"`
	CacheHitResponses              int64    `json:"cache_hit_responses"`
	InvalidUsageResponses          int64    `json:"invalid_usage_responses"`
	InputTokens                    int64    `json:"input_tokens"`
	OutputTokens                   int64    `json:"output_tokens"`
	CachedInputTokens              int64    `json:"cached_input_tokens"`
	CacheUsageInputTokens          int64    `json:"cache_usage_input_tokens"`
	CacheHitRatio                  *float64 `json:"cache_hit_ratio,omitempty"`
	PendingRequests                int      `json:"pending_requests"`
}

func newGatewayCacheDiagnostics() (*gatewayCacheDiagnostics, error) {
	d := &gatewayCacheDiagnostics{
		pending: make(map[uint64]struct{}),
		latest:  make(map[[32]byte]gatewayCacheRequestDigest),
	}
	if _, err := rand.Read(d.key[:]); err != nil {
		return nil, errors.New("cannot initialize cache diagnostics")
	}
	return d, nil
}

func (d *gatewayCacheDiagnostics) digest(domain string, value []byte) [32]byte {
	mac := hmac.New(sha256.New, d.key[:])
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(value)
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func (d *gatewayCacheDiagnostics) fieldDigest(body map[string]json.RawMessage, field string) [32]byte {
	raw, exists := body[field]
	if !exists {
		return d.digest(field+":absent", nil)
	}
	canonical, _ := canonicalGatewayCacheJSON(raw)
	return d.digest(field, canonical)
}

// ObserveRequest receives the actual prepared outbound body. Call it from an
// experiment RoundTripper, after gateway policy and transport normalization.
// Requests are compared within a thread, not across sibling agents that share
// the same root session/cache key but have different histories and toolsets.
func (d *gatewayCacheDiagnostics) ObserveRequest(r *http.Request, body []byte) (uint64, error) {
	if len(body) > maxBody {
		return 0, errors.New("cache diagnostics request exceeds 32 MiB")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return 0, errors.New("cache diagnostics request must be a JSON object")
	}
	snapshot := gatewayCacheRequestDigest{
		model:        d.fieldDigest(fields, "model"),
		instructions: d.fieldDigest(fields, "instructions"),
		tools:        d.fieldDigest(fields, "tools"),
		cacheKey:     d.fieldDigest(fields, "prompt_cache_key"),
	}
	controls := make(map[string]json.RawMessage)
	for _, key := range []string{"reasoning", "text", "tool_choice", "parallel_tool_calls", "prompt_cache_options", "access_programs"} {
		if raw, exists := fields[key]; exists {
			controls[key] = raw
		}
	}
	controlJSON, _ := json.Marshal(controls)
	controlJSON, _ = canonicalGatewayCacheJSON(controlJSON)
	snapshot.controls = d.digest("controls", controlJSON)
	var cacheKey string
	_ = json.Unmarshal(fields["prompt_cache_key"], &cacheKey)
	snapshot.cachePresent = cacheKey != ""
	var input []json.RawMessage
	if raw := bytes.TrimSpace(fields["input"]); len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &input) == nil {
		snapshot.inputIsArray, snapshot.inputCount = true, len(input)
		snapshot.input = make([][32]byte, min(len(input), maxCacheDiagnosticItems))
		for i := range snapshot.input {
			item, _ := canonicalGatewayCacheJSON(input[i])
			snapshot.input[i] = d.digest("input-item", item)
		}
	}
	var metadata map[string]json.RawMessage
	_ = json.Unmarshal(fields["client_metadata"], &metadata)
	var session, thread string
	_ = json.Unmarshal(metadata["session_id"], &session)
	_ = json.Unmarshal(metadata["thread_id"], &thread)
	authorization := ""
	if r != nil {
		for _, header := range []string{"session-id", "session_id"} {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				session = value
				break
			}
		}
		for _, header := range []string{"thread-id", "conversation_id"} {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				thread = value
				break
			}
		}
		authorization = r.Header.Get("Authorization")
	}
	knownScope := session != "" || thread != ""
	// A shared application cache key alone is not a conversation identity.
	scopeJSON, _ := json.Marshal([]string{authorization, session, thread})
	scope := d.digest("request-scope", scopeJSON)
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pending) >= maxCacheDiagnosticPending {
		return 0, errors.New("cache diagnostics pending request limit reached")
	}
	d.next++
	snapshot.sequence = d.next
	d.pending[d.next] = struct{}{}
	d.summary.Requests++
	if !snapshot.cachePresent {
		d.summary.MissingCacheKeyRequests++
	}
	if snapshot.inputCount > maxCacheDiagnosticItems {
		d.summary.InputComparisonLimitedRequests++
	}
	if !knownScope {
		d.summary.UnscopedRequests++
		return d.next, nil
	}
	if previous, exists := d.latest[scope]; exists {
		d.compare(previous, snapshot)
	} else if len(d.latest) >= maxCacheDiagnosticScopes {
		var oldest [32]byte
		sequence := ^uint64(0)
		for candidate, item := range d.latest {
			if item.sequence < sequence {
				oldest, sequence = candidate, item.sequence
			}
		}
		delete(d.latest, oldest)
		d.summary.ScopeEvictions++
	}
	d.latest[scope] = snapshot
	return d.next, nil
}

func (d *gatewayCacheDiagnostics) compare(previous, current gatewayCacheRequestDigest) {
	s := &d.summary
	s.ComparablePairs++
	if previous.model != current.model {
		s.ModelChangedPairs++
	}
	if previous.instructions != current.instructions {
		s.InstructionsChangedPairs++
	}
	if previous.tools != current.tools {
		s.ToolsChangedPairs++
	}
	if previous.controls != current.controls {
		s.ControlsChangedPairs++
	}
	if previous.cacheKey != current.cacheKey {
		s.CacheKeyChangedPairs++
	}
	if previous.model == current.model && previous.instructions == current.instructions && previous.tools == current.tools && previous.controls == current.controls {
		s.StableConfigurationPairs++
	}
	if previous.inputIsArray && current.inputIsArray {
		s.InputComparablePairs++
		shared := 0
		for shared < min(len(previous.input), len(current.input)) && previous.input[shared] == current.input[shared] {
			shared++
		}
		s.SharedInputItems += int64(shared)
		s.PreviousInputItemsCompared += int64(len(previous.input))
		if previous.inputCount <= maxCacheDiagnosticItems && shared == previous.inputCount && current.inputCount > previous.inputCount {
			s.InputHistoryExtensionPairs++
		} else if shared < min(len(previous.input), len(current.input)) || current.inputCount < previous.inputCount {
			s.InputHistoryChangedPairs++
		}
	}
}

// ObserveResponse accepts either a terminal SSE event object or its response
// object. The caller may record HTTP status/transport attempts separately.
func (d *gatewayCacheDiagnostics) ObserveResponse(id uint64, terminalJSON []byte) error {
	if len(terminalJSON) > maxBody {
		return errors.New("cache diagnostics response exceeds 32 MiB")
	}
	var event struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
	}
	if !jsonObject(terminalJSON) || json.Unmarshal(terminalJSON, &event) != nil {
		return errors.New("cache diagnostics response must be a JSON object")
	}
	response := terminalJSON
	if len(event.Response) > 0 {
		response = event.Response
	}
	var result struct {
		Status string          `json:"status"`
		Usage  json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(response, &result) != nil {
		return errors.New("cache diagnostics response fields are invalid")
	}
	switch event.Type {
	case "error", "response.failed", "response.completed", "response.done", "response.incomplete":
		if event.Type != "error" && event.Type != "response.failed" && !jsonObject(event.Response) {
			return errors.New("cache diagnostics terminal response is missing or invalid")
		}
	default:
		// Direct response objects have a status, rather than an SSE event type.
		if event.Type != "" || len(event.Response) > 0 || (result.Status != "completed" && result.Status != "failed" && result.Status != "incomplete" && result.Status != "cancelled") {
			return errors.New("cache diagnostics response is not terminal")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.pending[id]; !exists {
		return errors.New("cache diagnostics request is unknown or already completed")
	}
	delete(d.pending, id)
	s := &d.summary
	s.Responses++
	switch {
	case event.Type == "response.failed" || event.Type == "error" || result.Status == "failed" || result.Status == "cancelled":
		s.FailedResponses++
	case event.Type == "response.incomplete" || result.Status == "incomplete":
		s.IncompleteResponses++
	default:
		s.SuccessfulResponses++
	}
	if len(result.Usage) == 0 || bytes.Equal(bytes.TrimSpace(result.Usage), []byte("null")) {
		return nil
	}
	var usage struct {
		Input   *int64 `json:"input_tokens"`
		Output  *int64 `json:"output_tokens"`
		Cached  *int64 `json:"cached_tokens"`
		Details *struct {
			Cached *int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	if json.Unmarshal(result.Usage, &usage) != nil {
		s.InvalidUsageResponses++
		return nil
	}
	if usage.Input == nil || usage.Output == nil || *usage.Input < 0 || *usage.Output < 0 || *usage.Input > math.MaxInt64-s.InputTokens || *usage.Output > math.MaxInt64-s.OutputTokens {
		s.InvalidUsageResponses++
		return nil
	}
	s.UsageResponses++
	s.InputTokens += *usage.Input
	s.OutputTokens += *usage.Output
	cached := usage.Cached
	if usage.Details != nil && usage.Details.Cached != nil {
		cached = usage.Details.Cached
	}
	if cached == nil {
		return nil
	}
	if *cached < 0 || *cached > *usage.Input || *usage.Input > math.MaxInt64-s.CacheUsageInputTokens || *cached > math.MaxInt64-s.CachedInputTokens {
		s.InvalidUsageResponses++
		return nil
	}
	s.CacheUsageResponses++
	s.CacheUsageInputTokens += *usage.Input
	s.CachedInputTokens += *cached
	if *cached > 0 {
		s.CacheHitResponses++
	}
	return nil
}

func (d *gatewayCacheDiagnostics) abandon(id uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.pending[id]; exists {
		delete(d.pending, id)
		d.summary.UnfinishedResponses++
	}
}

func (d *gatewayCacheDiagnostics) Snapshot() gatewayCacheDiagnosticsSummary {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := d.summary
	result.PendingRequests = len(d.pending)
	if result.CacheUsageInputTokens > 0 {
		ratio := float64(result.CachedInputTokens) / float64(result.CacheUsageInputTokens)
		result.CacheHitRatio = &ratio
	}
	return result
}
