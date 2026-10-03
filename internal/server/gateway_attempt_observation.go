package server

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"time"
)

const gatewayAttemptObservationLimit = 4

// This type contains metadata only. It must never grow request/response body,
// raw error, headers, host, query, credential or account identity fields.
type GatewayAttemptSummary struct {
	Attempt    int    `json:"attempt"`
	Phase      string `json:"phase"`
	Endpoint   string `json:"endpoint,omitempty"`
	StatusCode *int   `json:"status_code,omitempty"`
	// ObservedHTTPStatusCode is only set when a real transport response supplied
	// headers. StatusCode keeps the legacy logical/protocol outcome contract.
	ObservedHTTPStatusCode *int   `json:"observed_http_status_code,omitempty"`
	DurationMs             *int64 `json:"duration_ms,omitempty"`
	HeaderMs               *int64 `json:"header_ms,omitempty"`
	ErrorSource            string `json:"error_source,omitempty"`
	ErrorCode              string `json:"error_code,omitempty"`
}

// A snapshot does not populate RequestRecord itself: the owning handler decides
// its existing accounting boundary and applies these passive fields once.
type GatewayAttemptSnapshot struct {
	InboundPath          string
	UpstreamPath         string
	InboundModel         string
	SentModel            string
	SentReasoningEffort  string
	ReturnedModel        string
	RequestPreparationMs *int64
	AccountSelectionMs   *int64
	ProxySelectionMs     *int64
	Attempts             []GatewayAttemptSummary
	AttemptsTruncated    *bool
	ErrorTruncated       *bool
}

type gatewayAttemptState struct {
	summary     GatewayAttemptSummary
	started     time.Time
	headerAt    time.Time
	headersSeen bool
	finished    bool
	unsent      bool
}

type gatewayAttemptObservation struct {
	mu        sync.Mutex
	metadata  GatewayAttemptSnapshot
	attempts  [gatewayAttemptObservationLimit]gatewayAttemptState
	count     int
	truncated bool
}

type gatewayAttemptObservationContextKey struct{}

type gatewayAttemptHandle struct {
	collector *gatewayAttemptObservation
	index     int
}

// Context propagation shares only the observer, not a shadow request/policy.
// Derived request/refresh contexts must retain this value to join one record.
func withGatewayAttemptObservation(ctx context.Context) (context.Context, *gatewayAttemptObservation) {
	if ctx == nil {
		ctx = context.Background()
	}
	if existing := gatewayAttemptObservationFrom(ctx); existing != nil {
		return ctx, existing
	}
	observer := &gatewayAttemptObservation{}
	return context.WithValue(ctx, gatewayAttemptObservationContextKey{}, observer), observer
}

func gatewayAttemptObservationFrom(ctx context.Context) *gatewayAttemptObservation {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(gatewayAttemptObservationContextKey{}).(*gatewayAttemptObservation)
	return observer
}

// Call only when an actual HTTP/WS/OAuth send begins. Refresh cache hits,
// lock waits and credential reloads are not network attempts. Omitted attempts
// do not allocate handles or retain later request information.
func (o *gatewayAttemptObservation) Begin(phase, requestURI string, started time.Time) *gatewayAttemptHandle {
	if o == nil || !gatewayObservedAttemptPhase(phase) {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.count == gatewayAttemptObservationLimit {
		o.truncated = true
		return nil
	}
	index := o.count
	o.count++
	o.attempts[index] = gatewayAttemptState{started: started, summary: GatewayAttemptSummary{
		Attempt: index + 1, Phase: phase, Endpoint: gatewayObservedRoute(requestURI),
	}}
	return &gatewayAttemptHandle{collector: o, index: index}
}

func gatewayObservedAttemptPhase(phase string) bool {
	return phase == "request" || phase == "refresh" || phase == "websocket"
}

// Only recognized paths survive. Absolute URL input is parsed structurally;
// scheme, authority, userinfo, query and fragment never enter the snapshot.
func gatewayObservedRoute(requestURI string) string {
	parsed, err := url.Parse(requestURI)
	if err != nil || parsed.Opaque != "" || !strings.HasPrefix(parsed.Path, "/") || parsed.RawPath != "" {
		return ""
	}
	path := parsed.Path
	for _, prefix := range []string{"/v1", "/backend-api/codex", "/basispoints/api"} {
		if strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
			break
		}
	}
	switch path {
	case "/responses", "/responses/compact", "/models", "/oauth/token", "/guardian", "/guardian-classifier", "/attachments":
		return path
	}
	return ""
}

// Record only a response that actually supplied its headers/status. A timeout
// before headers leaves both optional observations unknown, not measured zero.
func (h *gatewayAttemptHandle) Headers(status int, observed time.Time) {
	if h == nil || h.collector == nil || status < 100 || status > 599 {
		return
	}
	o := h.collector
	o.mu.Lock()
	defer o.mu.Unlock()
	state := &o.attempts[h.index]
	if state.finished || state.headersSeen {
		return
	}
	state.headersSeen, state.headerAt = true, observed
	state.summary.StatusCode = new(int)
	*state.summary.StatusCode = status
	state.summary.ObservedHTTPStatusCode = new(int)
	*state.summary.ObservedHTTPStatusCode = status
	state.summary.HeaderMs = gatewayObservedDuration(state.started, observed)
}

// The owner chooses the exact completed boundary: request dispatch normally
// ends at headers, whereas an OAuth exchange may finish after body parsing.
// Finish is idempotent and cannot overwrite the first observed failure.
func (h *gatewayAttemptHandle) Finish(source, code string, finished time.Time) {
	if h == nil || h.collector == nil {
		return
	}
	o := h.collector
	o.mu.Lock()
	defer o.mu.Unlock()
	state := &o.attempts[h.index]
	if state.finished {
		return
	}
	state.finished = true
	if state.headerAt.IsZero() || !finished.Before(state.headerAt) {
		state.summary.DurationMs = gatewayObservedDuration(state.started, finished)
	}
	state.summary.ErrorSource = gatewayObservedErrorSource(source)
	state.summary.ErrorCode = gatewayObservedErrorCode(code)
}

// Retain stable handle indices for concurrent observations. Only an explicit
// pre-IPC dispatch rejection may hide a candidate that never sent its request.
func (h *gatewayAttemptHandle) discardUnsent() {
	if h == nil || h.collector == nil {
		return
	}
	o := h.collector
	o.mu.Lock()
	defer o.mu.Unlock()
	state := &o.attempts[h.index]
	if state.headersSeen || state.finished {
		return
	}
	state.unsent, state.finished = true, true
}

func gatewayObservedDuration(started, finished time.Time) *int64 {
	if started.IsZero() || finished.IsZero() || finished.Before(started) {
		return nil
	}
	value := finished.Sub(started).Milliseconds()
	return &value
}

func gatewayObservedErrorSource(source string) string {
	switch source {
	case "gateway", "client", "transport", "upstream":
		return source
	}
	return ""
}

func gatewayObservedErrorCode(code string) string {
	switch code {
	case "", "unknown", "canceled", "deadline", "timeout", "http_status", "proxy_unavailable", "capacity", "protocol", "transport", "refresh_failed", "credential_changed", "disabled", "invalid_request":
		return code
	}
	return "unknown"
}

func (o *gatewayAttemptObservation) ObserveInbound(requestURI string, payload []byte) {
	if o == nil {
		return
	}
	model := gatewayObservedModel(payload, false)
	path := gatewayObservedRoute(requestURI)
	o.mu.Lock()
	defer o.mu.Unlock()
	if path != "" {
		o.metadata.InboundPath = path
	}
	if model != "" {
		o.metadata.InboundModel = model
	}
}

// ObserveSent belongs at prepared-request dispatch, not before request policy.
// If retries really send a different model, this field is the last observed
// sent model; it is not copied from InboundModel when no send was observed.
func (o *gatewayAttemptObservation) ObserveSent(requestURI string, payload []byte) {
	if o == nil {
		return
	}
	model := gatewayObservedModel(payload, false)
	var request struct {
		Effort    string `json:"reasoning_effort"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	_ = json.Unmarshal(payload, &request)
	effort := request.Effort
	if request.Reasoning.Effort != "" {
		effort = request.Reasoning.Effort
	}
	path := gatewayObservedRoute(requestURI)
	o.mu.Lock()
	defer o.mu.Unlock()
	if path != "" {
		o.metadata.UpstreamPath = path
	}
	if model != "" {
		o.metadata.SentModel = model
		o.metadata.SentReasoningEffort = boundedGatewayMetadata(effort, 128)
	}
}

// Returned model must come from an actual response object/event, not a request
// alias or model header. Those have separate sources in existing observations.
func (o *gatewayAttemptObservation) ObserveReturned(payload []byte) {
	if o == nil {
		return
	}
	model := gatewayObservedModel(payload, true)
	if model == "" {
		return
	}
	o.mu.Lock()
	o.metadata.ReturnedModel = model
	o.mu.Unlock()
}

func gatewayObservedModel(payload []byte, response bool) string {
	var object struct {
		Type     string          `json:"type"`
		Model    string          `json:"model"`
		Response json.RawMessage `json:"response"`
	}
	if !validSSELine(payload) || json.Unmarshal(payload, &object) != nil {
		return ""
	}
	if response && object.Type != "" && len(object.Response) == 0 {
		return ""
	}
	model := object.Model
	if response && len(object.Response) > 0 {
		var nested struct {
			Model string `json:"model"`
		}
		if !jsonObject(object.Response) || json.Unmarshal(object.Response, &nested) != nil {
			return ""
		}
		model = nested.Model
	}
	return boundedGatewayMetadata(model, 512)
}

// These are local wall-clock intervals at explicit handler boundaries. The
// first valid observation wins; no unobserved DNS/connect/TLS stages are added.
func (o *gatewayAttemptObservation) ObserveStage(phase string, started, finished time.Time) {
	if o == nil {
		return
	}
	duration := gatewayObservedDuration(started, finished)
	if duration == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var target **int64
	switch phase {
	case "request_preparation":
		target = &o.metadata.RequestPreparationMs
	case "account_selection":
		target = &o.metadata.AccountSelectionMs
	case "proxy_selection":
		target = &o.metadata.ProxySelectionMs
	default:
		return
	}
	if *target == nil {
		*target = duration
	}
}

// Call only from the branch that actually bounds the stored diagnostic. No
// observation means nil; a known non-truncated diagnostic is explicit false.
func (o *gatewayAttemptObservation) ObserveErrorTruncation(truncated bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.metadata.ErrorTruncated == nil {
		o.metadata.ErrorTruncated = new(bool)
	}
	*o.metadata.ErrorTruncated = *o.metadata.ErrorTruncated || truncated
}

func (o *gatewayAttemptObservation) Snapshot() GatewayAttemptSnapshot {
	if o == nil {
		return GatewayAttemptSnapshot{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	result := o.metadata
	result.RequestPreparationMs = cloneGatewayObservationPointer(result.RequestPreparationMs)
	result.AccountSelectionMs = cloneGatewayObservationPointer(result.AccountSelectionMs)
	result.ProxySelectionMs = cloneGatewayObservationPointer(result.ProxySelectionMs)
	result.ErrorTruncated = cloneGatewayObservationPointer(result.ErrorTruncated)
	if o.count > 0 {
		result.AttemptsTruncated = new(bool)
		*result.AttemptsTruncated = o.truncated
		result.Attempts = make([]GatewayAttemptSummary, 0, o.count)
		for index := range o.count {
			if o.attempts[index].unsent {
				continue
			}
			attempt := o.attempts[index].summary
			attempt.Attempt = len(result.Attempts) + 1
			attempt.StatusCode = cloneGatewayObservationPointer(attempt.StatusCode)
			attempt.ObservedHTTPStatusCode = cloneGatewayObservationPointer(attempt.ObservedHTTPStatusCode)
			attempt.DurationMs = cloneGatewayObservationPointer(attempt.DurationMs)
			attempt.HeaderMs = cloneGatewayObservationPointer(attempt.HeaderMs)
			result.Attempts = append(result.Attempts, attempt)
		}
	}
	return result
}

func cloneGatewayObservationPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
