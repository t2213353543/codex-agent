package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A fixed 61-second ring holds counters only. It never scans historical logs,
// performs database I/O or retains request bodies on the inference hot path.
type dashboardRealtimeMetrics struct {
	mu      sync.Mutex
	buckets [61]dashboardRealtimeBucket
}

type dashboardRealtimeBucket struct {
	second                                      int64
	requests, completed, tokens, known, unknown int64
}

type dashboardRealtimeSnapshot struct {
	Scope           string `json:"scope"`
	WindowSeconds   int64  `json:"window_seconds"`
	From            int64  `json:"from"`
	UpdatedAt       int64  `json:"updated_at"`
	CoverageSeconds int64  `json:"coverage_seconds"`
	Warming         bool   `json:"warming_up"`
	RPM             int64  `json:"rpm"`
	TPM             *int64 `json:"tpm"`
	Completed       int64  `json:"completed_requests"`
	Known           int64  `json:"usage_known_requests"`
	Unknown         int64  `json:"usage_unknown_requests"`
	TokensPartial   bool   `json:"tokens_partial"`
	RequestBasis    string `json:"request_basis"`
	TokenBasis      string `json:"token_basis"`
}

func dashboardRealtimeInferencePath(path string) bool {
	path = strings.TrimPrefix(path, "/backend-api/codex")
	path = strings.TrimPrefix(path, "/v1")
	switch path {
	case "/responses", "/responses/compact", "/guardian", "/guardian-classifier", "/alpha/search", "/images/generations", "/images/edits", "/memories/trace_summarize", "/realtime/calls", "/live":
		return true
	}
	return false
}

func (p *Proxy) observeDashboardRealtimeIngress(r *http.Request) {
	if p.Metrics == nil || r.Method != http.MethodPost || !dashboardRealtimeInferencePath(r.URL.Path) {
		return
	}
	p.Metrics.realtime.add(time.Now().Unix(), true, nil)
}

func (p *Proxy) observeDashboardRealtimeWSCreate(payload []byte) {
	if p.Metrics == nil {
		return
	}
	var event struct {
		Type     string
		Generate *bool
	}
	if gatewayWSDecodeExact(payload, map[string]any{"type": &event.Type, "generate": &event.Generate}) != nil || event.Type != "response.create" || event.Generate != nil && !*event.Generate {
		return
	}
	p.Metrics.realtime.add(time.Now().Unix(), true, nil)
}

func (m *Metrics) observeDashboardRealtimeCompleted(rec RequestRecord, now int64) {
	if !dashboardRealtimeInferencePath("/"+strings.TrimPrefix(rec.Endpoint, "/")) && rec.Endpoint != "compact" && rec.Endpoint != "realtime" {
		return
	}
	if rec.WebSocketWarmup != nil && *rec.WebSocketWarmup {
		return
	}
	m.realtime.add(now, false, &rec)
}

func (s *dashboardRealtimeMetrics) add(second int64, ingress bool, rec *RequestRecord) {
	if second < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket := &s.buckets[second%int64(len(s.buckets))]
	if bucket.second != second {
		*bucket = dashboardRealtimeBucket{second: second}
	}
	if ingress {
		bucket.requests++
	}
	if rec != nil {
		bucket.completed++
		if known := managedUsageKnown(*rec); known != nil && *known {
			bucket.known++
			bucket.tokens += rec.InputTokens + rec.OutputTokens
		} else {
			bucket.unknown++
		}
	}
}

func (s *dashboardRealtimeMetrics) snapshot(now, started int64) dashboardRealtimeSnapshot {
	out := dashboardRealtimeSnapshot{Scope: "current_process", WindowSeconds: 60, From: now - 59, UpdatedAt: now, CoverageSeconds: 60, RequestBasis: "inference_ingress", TokenBasis: "completed_reported_usage"}
	if started > out.From {
		out.CoverageSeconds = max(int64(0), min(int64(60), now-started+1))
		out.Warming = out.CoverageSeconds < 60
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var tokens int64
	for _, bucket := range s.buckets {
		if bucket.second < out.From || bucket.second > now {
			continue
		}
		out.RPM += bucket.requests
		out.Completed += bucket.completed
		out.Known += bucket.known
		out.Unknown += bucket.unknown
		tokens += bucket.tokens
	}
	out.TokensPartial = out.Unknown > 0
	if out.Known > 0 || out.RPM == 0 && out.Completed == 0 {
		out.TPM = &tokens
	}
	return out
}

func (p *Proxy) handleDashboardRealtime(w http.ResponseWriter, r *http.Request) {
	if p.Metrics == nil {
		apiError(w, http.StatusServiceUnavailable, "realtime metrics unavailable")
		return
	}
	now := time.Now().Unix()
	started := p.startedAt.Unix()
	if p.startedAt.IsZero() {
		started = now
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p.Metrics.realtime.snapshot(now, started))
}
