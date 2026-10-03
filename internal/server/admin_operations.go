package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"time"
)

// Operations reads retained, completed request records. Runtime/pool counters
// are local to this process; durable traffic may include other database nodes.
type operationsSnapshot struct {
	CheckedAt       int64                     `json:"checked_at"`
	From            int64                     `json:"from"`
	To              int64                     `json:"to"`
	Step            int64                     `json:"step_seconds"`
	Scope           string                    `json:"scope"`
	Traffic         operationsTotals          `json:"traffic"`
	Series          []operationsPoint         `json:"series"`
	Latency         []operationsLatencyBucket `json:"latency_histogram"`
	ErrorCategories []operationsCategory      `json:"error_categories"`
	RecentErrors    []operationsError         `json:"recent_errors"`
	Runtime         operationsRuntime         `json:"runtime"`
}

type operationsTotals struct {
	Requests       int64    `json:"requests"`
	Success        int64    `json:"success"`
	Errors         int64    `json:"errors"`
	Aborted        int64    `json:"aborted"`
	Tokens         int64    `json:"tokens"`
	UsageKnown     int64    `json:"usage_known_requests"`
	LatencySamples int64    `json:"latency_samples"`
	AverageLatency *float64 `json:"average_latency_ms"`
	TTFTSamples    int64    `json:"ttft_samples"`
	AverageTTFT    *float64 `json:"average_ttft_ms"`
	latencySum     float64
	ttftSum        float64
}

func (t *operationsTotals) add(r RequestRecord) {
	t.Requests++
	if r.Status == "ok" {
		t.Success++
	} else {
		t.Errors++
	}
	if r.Status == "aborted" {
		t.Aborted++
	}
	t.Tokens += r.tokens()
	if known := managedUsageKnown(r); known != nil && *known {
		t.UsageKnown++
	}
	// Legacy records omit latency when it was not observed. Do not invent a
	// zero-latency sample from an absent field.
	if r.hasMeasuredLatency() {
		t.LatencySamples++
		t.latencySum += float64(r.LatencyMs)
		average := t.latencySum / float64(t.LatencySamples)
		t.AverageLatency = &average
	}
	if r.TTFTMs != nil && *r.TTFTMs >= 0 {
		t.TTFTSamples++
		t.ttftSum += float64(*r.TTFTMs)
		average := t.ttftSum / float64(t.TTFTSamples)
		t.AverageTTFT = &average
	}
}

type operationsPoint struct {
	Time int64 `json:"time"`
	operationsTotals
}
type operationsLatencyBucket struct {
	Label    string `json:"label"`
	UpperMS  int64  `json:"upper_ms"`
	Requests int64  `json:"requests"`
}
type operationsCategory struct {
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
}
type operationsError struct {
	ID       int64  `json:"id"`
	Time     int64  `json:"time"`
	Model    string `json:"model"`
	Status   string `json:"status"`
	HTTPCode int    `json:"http_code"`
	Category string `json:"category"`
}
type operationsRuntime struct {
	Host            operationsHostResources `json:"host"`
	Scope           string                  `json:"scope"`
	InFlight        int64                   `json:"in_flight"`
	CPUCores        int                     `json:"cpu_cores"`
	GoMaxProcs      int                     `json:"go_max_procs"`
	Goroutines      int                     `json:"goroutines"`
	HeapBytes       uint64                  `json:"heap_bytes"`
	HeapInUseBytes  uint64                  `json:"heap_in_use_bytes"`
	GoReservedBytes uint64                  `json:"go_reserved_bytes"`
	StackBytes      uint64                  `json:"stack_bytes"`
	GCCount         uint32                  `json:"gc_count"`
	LastGCPauseMS   *float64                `json:"last_gc_pause_ms"`
	PendingWrites   int                     `json:"pending_writes"`
	MetricsStatus   string                  `json:"metrics_status"`
	ReadFailures    uint64                  `json:"read_failures"`
	Postgres        *operationsPostgresPool `json:"postgres_pool"`
	Redis           *operationsRedisPool    `json:"redis_pool"`
}
type operationsPostgresPool struct {
	Total            int32 `json:"total"`
	Idle             int32 `json:"idle"`
	InUse            int32 `json:"in_use"`
	Max              int32 `json:"max"`
	WaitCount        int64 `json:"wait_count"`
	CanceledAcquires int64 `json:"canceled_acquires"`
}
type operationsRedisPool struct {
	Total    uint32 `json:"total"`
	Idle     uint32 `json:"idle"`
	Hits     uint32 `json:"hits"`
	Misses   uint32 `json:"misses"`
	Timeouts uint32 `json:"timeouts"`
}

func (p *Proxy) operationsRuntimeSnapshot() operationsRuntime {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	out := operationsRuntime{Host: collectOperationsHost(), Scope: "current_process", InFlight: p.overviewInFlight(), CPUCores: runtime.NumCPU(), GoMaxProcs: runtime.GOMAXPROCS(0), Goroutines: runtime.NumGoroutine(), HeapBytes: mem.HeapAlloc, HeapInUseBytes: mem.HeapInuse, GoReservedBytes: mem.Sys, StackBytes: mem.StackInuse, GCCount: mem.NumGC, MetricsStatus: "unavailable"}
	if mem.NumGC > 0 {
		pause := float64(mem.PauseNs[(mem.NumGC-1)%256]) / 1e6
		out.LastGCPauseMS = &pause
	}
	if p.Metrics != nil {
		p.Metrics.mu.Lock()
		out.PendingWrites, out.ReadFailures = len(p.Metrics.pending), p.Metrics.readFailures
		out.MetricsStatus = "ok"
		if p.Metrics.writeErr != nil || out.PendingWrites > 0 {
			out.MetricsStatus = "degraded"
		}
		p.Metrics.mu.Unlock()
	}
	if db := p.Store.DB; db != nil {
		if db.Pool != nil {
			s := db.Pool.Stat()
			out.Postgres = &operationsPostgresPool{Total: s.TotalConns(), Idle: s.IdleConns(), InUse: s.AcquiredConns(), Max: s.MaxConns(), WaitCount: s.EmptyAcquireCount(), CanceledAcquires: s.CanceledAcquireCount()}
		}
		if db.Redis != nil {
			s := db.Redis.PoolStats()
			out.Redis = &operationsRedisPool{Total: s.TotalConns, Idle: s.IdleConns, Hits: s.Hits, Misses: s.Misses, Timeouts: s.Timeouts}
		}
	}
	return out
}

func operationsRange(q url.Values) (int64, error) {
	for key, values := range q {
		if key != "range" || len(values) != 1 {
			return 0, errInvalid
		}
	}
	switch q.Get("range") {
	case "15m":
		return 900, nil
	case "", "1h":
		return 3600, nil
	case "6h":
		return 21600, nil
	case "24h":
		return 86400, nil
	default:
		return 0, errInvalid
	}
}

func (m *Metrics) operationsTraffic(ctx context.Context, from, to int64) (operationsSnapshot, error) {
	out := operationsSnapshot{From: from, To: to, Step: (to - from) / 60, Scope: "retained_request_details", Series: []operationsPoint{}, ErrorCategories: []operationsCategory{}, RecentErrors: []operationsError{}, Latency: []operationsLatencyBucket{
		{Label: "≤ 1 秒", UpperMS: 1000}, {Label: "1–5 秒", UpperMS: 5000}, {Label: "5–15 秒", UpperMS: 15000}, {Label: "15–30 秒", UpperMS: 30000}, {Label: "30–60 秒", UpperMS: 60000}, {Label: "1–3 分钟", UpperMS: 180000}, {Label: "> 3 分钟", UpperMS: -1},
	}}
	if out.Step <= 0 {
		return out, errInvalid
	}
	if m == nil || m.db != nil && m.db.Pool == nil {
		return out, errors.New("metrics unavailable")
	}
	for stamp := from; stamp < to; stamp += out.Step {
		out.Series = append(out.Series, operationsPoint{Time: stamp})
	}
	if m.db != nil {
		return m.operationsTrafficPG(ctx, out)
	}
	categories := map[string]int64{}
	err := m.walkManagedLogs(ctx, managedLogQuery{From: from, To: to, SortBy: "time", SortOrder: "desc"}, func(r RequestRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The file and SQL paths use the same half-open request timestamp window.
		if r.Time < from || r.Time >= to {
			return nil
		}
		out.Traffic.add(r)
		out.Series[(r.Time-from)/out.Step].add(r)
		if r.hasMeasuredLatency() {
			for i := range out.Latency {
				if out.Latency[i].UpperMS < 0 || r.LatencyMs <= out.Latency[i].UpperMS {
					out.Latency[i].Requests++
					break
				}
			}
		}
		if r.Status != "ok" {
			category := managedFailureCategory(r)
			categories[category]++
			out.RecentErrors = append(out.RecentErrors, operationsError{ID: r.ID, Time: r.Time, Model: r.Model, Status: r.Status, HTTPCode: r.HTTPCode, Category: category})
			sort.Slice(out.RecentErrors, func(i, j int) bool {
				a, b := out.RecentErrors[i], out.RecentErrors[j]
				if a.Time == b.Time {
					return a.ID > b.ID
				}
				return a.Time > b.Time
			})
			if len(out.RecentErrors) > 8 {
				out.RecentErrors = out.RecentErrors[:8]
			}
		}
		return nil
	})
	if err != nil {
		return operationsSnapshot{}, err
	}
	for name, requests := range categories {
		out.ErrorCategories = append(out.ErrorCategories, operationsCategory{Name: name, Requests: requests})
	}
	sort.Slice(out.ErrorCategories, func(i, j int) bool {
		a, b := out.ErrorCategories[i], out.ErrorCategories[j]
		if a.Requests == b.Requests {
			return a.Name < b.Name
		}
		return a.Requests > b.Requests
	})
	return out, nil
}

func (p *Proxy) handleOperations(w http.ResponseWriter, r *http.Request) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		adminError(w, errInvalid)
		return
	}
	seconds, err := operationsRange(q)
	if err != nil {
		adminError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	now := time.Now().Unix()
	result, err := p.Metrics.operationsTraffic(ctx, now+1-seconds, now+1)
	if err != nil {
		adminAPIError(w, http.StatusServiceUnavailable, "operations_unavailable", "运维监控暂时不可用，请稍后重试")
		return
	}
	result.CheckedAt = now
	result.Runtime = p.operationsRuntimeSnapshot()
	writeJSON(w, result)
}
