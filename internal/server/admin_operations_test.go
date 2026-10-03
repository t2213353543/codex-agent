package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func operationsTestRecords(from int64) []RequestRecord {
	zero, ttft, known := int64(0), int64(120), true
	return []RequestRecord{
		{Time: from - 1, Status: "ok", InputTokens: 99999},
		{Time: from, Status: "ok", LatencyMs: 1000, TTFTMs: &zero, InputTokens: 100, OutputTokens: 10, UsageKnown: &known},
		{Time: from + 70, Status: "error", LatencyMs: 5000, TTFTMs: &ttft, HTTPCode: 429, Error: "Bearer secret-token account private-name", Model: "gpt-test"},
		{Time: from + 75, Status: "aborted", LatencyMs: 0},
		{Time: from + 3600, Status: "ok", InputTokens: 99999},
	}
}

func checkOperationsTraffic(t *testing.T, m *Metrics) {
	t.Helper()
	const from = int64(1800000000)
	for _, r := range operationsTestRecords(from) {
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	out, err := m.operationsTraffic(context.Background(), from, from+3600)
	if err != nil {
		t.Fatal(err)
	}
	if out.Traffic.Requests != 3 || out.Traffic.Success != 1 || out.Traffic.Errors != 2 || out.Traffic.Aborted != 1 || out.Traffic.Tokens != 110 || out.Traffic.UsageKnown != 1 {
		t.Fatalf("wrong half-open aggregate: %+v", out.Traffic)
	}
	if out.Traffic.LatencySamples != 2 || out.Traffic.AverageLatency == nil || *out.Traffic.AverageLatency != 3000 || out.Traffic.TTFTSamples != 2 || out.Traffic.AverageTTFT == nil || *out.Traffic.AverageTTFT != 60 {
		t.Fatalf("unknown latency or observed zero TTFT lost: %+v", out.Traffic)
	}
	if len(out.Series) != 60 || out.Series[0].Requests != 1 || out.Series[1].Requests != 2 || out.Series[2].AverageLatency != nil || out.Series[2].AverageTTFT != nil {
		t.Fatalf("wrong empty or populated buckets: %+v", out.Series[:3])
	}
	if out.Latency[0].Requests != 1 || out.Latency[1].Requests != 1 || len(out.RecentErrors) != 2 || out.RecentErrors[0].Status != "aborted" || len(out.ErrorCategories) != 2 {
		t.Fatalf("wrong errors/latency distribution: %+v", out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret-token") || strings.Contains(string(raw), "private-name") {
		t.Fatal("operations exposed raw errors")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = m.operationsTraffic(ctx, from, from+3600); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan: %v", err)
	}
}

func TestOperationsTrafficFileAggregation(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	checkOperationsTraffic(t, m)
}

func TestPGOperationsTrafficAggregation(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	checkOperationsTraffic(t, p.Metrics)
	pool := p.operationsRuntimeSnapshot().Postgres
	if pool == nil || pool.Max <= 0 || pool.InUse < 0 {
		t.Fatalf("missing actual PG pool counters: %+v", pool)
	}
}

func TestOperationsAPIAndRuntime(t *testing.T) {
	p, handler := webTestSetup(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		response := webTestRequest(handler, method, "/api/operations", "", "")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("monitor auth: %d", response.Code)
		}
	}
	for _, query := range []string{"range=invalid", "range=1h&range=6h", "other=1", "range=%zz"} {
		response := webTestRequest(handler, http.MethodGet, "/api/operations?"+query, "", "Bearer "+webTestKey)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("query %q: %d %s", query, response.Code, response.Body.String())
		}
	}
	now := time.Now().Unix()
	p.Metrics.Record(RequestRecord{Time: now, Status: "ok", InputTokens: 100})
	p.admissionMu.Lock()
	active := int64(3)
	p.admission = map[string]*int64{"local": &active}
	p.admissionMu.Unlock()
	p.Metrics.mu.Lock()
	p.Metrics.writeErr = errors.New("private-write-error")
	p.Metrics.pending = []RequestRecord{{Time: now}}
	p.Metrics.mu.Unlock()
	response := webTestRequest(handler, http.MethodGet, "/api/operations?range=15m", "", "Bearer "+webTestKey)
	var out operationsSnapshot
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil {
		t.Fatalf("monitor response: %d %s", response.Code, response.Body.String())
	}
	if out.To-out.From != 900 || out.Traffic.Requests != 1 || out.Runtime.InFlight != 3 || out.Runtime.MetricsStatus != "degraded" || out.Runtime.PendingWrites != 1 || out.Runtime.CPUCores <= 0 || out.Runtime.Goroutines <= 0 || out.Runtime.GoReservedBytes == 0 || out.Runtime.Postgres != nil || out.Runtime.Redis != nil {
		t.Fatalf("wrong runtime: %+v", out)
	}
	if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "private-write-error") {
		t.Fatal("unsafe operational response")
	}
	response = webTestRequest(handler, http.MethodPost, "/api/operations", "", "Bearer "+webTestKey)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("write method allowed: %d", response.Code)
	}
	// A damaged log must produce an error, never a successful zero-traffic view.
	if err := os.WriteFile(p.Metrics.logPath(), []byte("{invalid}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	response = webTestRequest(handler, http.MethodGet, "/api/operations", "", "Bearer "+webTestKey)
	if response.Code < 500 || strings.Contains(response.Body.String(), p.Metrics.logPath()) {
		t.Fatalf("unsafe log failure: %d %s", response.Code, response.Body.String())
	}
}

func TestOperationsRecentErrorsBoundedAndOrdered(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	const from = int64(1800000000)
	// Ingestion order differs from request time, as for concurrent completions.
	for i := 11; i >= 0; i-- {
		m.Record(RequestRecord{Time: from + int64(i), Status: "error", HTTPCode: 500})
	}
	out, err := m.operationsTraffic(context.Background(), from, from+3600)
	if err != nil || len(out.RecentErrors) != 8 {
		t.Fatalf("bounded errors: %d %v", len(out.RecentErrors), err)
	}
	for i, r := range out.RecentErrors {
		if r.Time != from+11-int64(i) {
			t.Fatal("errors sorted by append order instead of request time")
		}
	}
}

func TestOperationsHostSampling(t *testing.T) {
	var sampler operationsHostSampler
	start := time.Unix(1800000000, 0)
	first := sampler.snapshot(operationsHostCounters{Supported: true, CPUAvailable: true, Total: 100, Idle: 50}, start)
	if first.CPUStatus != "warming_up" || first.CPUPercent != nil {
		t.Fatalf("invented first CPU sample: %+v", first)
	}
	next := sampler.snapshot(operationsHostCounters{Supported: true, CPUAvailable: true, Total: 200, Idle: 125}, start.Add(time.Second))
	if next.CPUPercent == nil || *next.CPUPercent != 25 || next.CPUSampleSeconds == nil || *next.CPUSampleSeconds != 1 {
		t.Fatalf("wrong CPU delta: %+v", next)
	}
	reset := sampler.snapshot(operationsHostCounters{Supported: true, CPUAvailable: true, Total: 1, Idle: 1}, start.Add(2*time.Second))
	if reset.CPUPercent != nil || reset.CPUStatus != "warming_up" {
		t.Fatalf("counter reset invented usage: %+v", reset)
	}
	missing := sampler.snapshot(operationsHostCounters{}, start.Add(3*time.Second))
	if missing.Supported || missing.CPUStatus != "unsupported" || missing.CPUPercent != nil {
		t.Fatalf("unsupported host fabricated data: %+v", missing)
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		live := readOperationsHostCounters()
		if !live.Supported || !live.CPUAvailable || live.Total == 0 || live.MemoryTotal == nil || *live.MemoryTotal == 0 || live.MemoryAvailable == nil || *live.MemoryAvailable > *live.MemoryTotal {
			t.Fatalf("OS resource probe failed: %+v", live)
		}
	}
}
