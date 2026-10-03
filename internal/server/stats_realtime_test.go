package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardRealtimeRollingWindowAndActualUsage(t *testing.T) {
	var counters dashboardRealtimeMetrics
	yes, no := true, false
	counters.add(1000, true, nil)
	counters.add(1001, true, nil)
	counters.add(1060, true, nil)
	// Completion time, not the request's old start timestamp, places actual
	// reported tokens into the current live window for a long generation.
	counters.add(1060, false, &RequestRecord{Time: 10, InputTokens: 90, OutputTokens: 10, UsageKnown: &yes})
	counters.add(1060, false, &RequestRecord{InputTokens: 999, UsageKnown: &no})
	got := counters.snapshot(1060, 900)
	if got.RPM != 2 || got.TPM == nil || *got.TPM != 100 || got.Known != 1 || got.Unknown != 1 || !got.TokensPartial || got.Warming {
		t.Fatalf("window/count/usage mismatch: %+v", got)
	}
	got = counters.snapshot(1120, 900)
	if got.RPM != 0 || got.TPM == nil || *got.TPM != 0 || got.Completed != 0 {
		t.Fatalf("expired buckets leaked: %+v", got)
	}
	counters.add(1120, true, nil)
	got = counters.snapshot(1120, 1110)
	if got.RPM != 1 || got.TPM != nil || !got.Warming || got.CoverageSeconds != 11 {
		t.Fatalf("pending usage or startup fabricated zero: %+v", got)
	}
}

func TestDashboardRealtimeIngressExcludesManagementModelsAndWarmup(t *testing.T) {
	p := newTestProxy(t, 1)
	for _, path := range []string{"/v1/models", "/api/overview", "/health", "/backend-api/ps/mcp", "/v1/alpha/notes/v2/read_file"} {
		p.observeDashboardRealtimeIngress(httptest.NewRequest(http.MethodPost, path, nil))
	}
	p.observeDashboardRealtimeIngress(httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	p.observeDashboardRealtimeWSCreate([]byte(`{"type":"response.create","generate":false}`))
	p.observeDashboardRealtimeWSCreate([]byte(`{"type":"ping"}`))
	response := httptest.NewRecorder()
	p.handleDashboardRealtime(response, httptest.NewRequest(http.MethodGet, "/api/overview/realtime", nil))
	var baseline dashboardRealtimeSnapshot
	_ = json.Unmarshal(response.Body.Bytes(), &baseline)
	if baseline.RPM != 0 {
		t.Fatal("administration, models or warmup polluted RPM")
	}
	for _, path := range []string{"/v1/responses", "/backend-api/codex/responses/compact", "/guardian", "/v1/images/generations"} {
		p.observeDashboardRealtimeIngress(httptest.NewRequest(http.MethodPost, path, nil))
	}
	p.observeDashboardRealtimeWSCreate([]byte(`{"type":"response.create","model":"fixture"}`))
	response = httptest.NewRecorder()
	p.handleDashboardRealtime(response, httptest.NewRequest(http.MethodGet, "/api/overview/realtime?from=1&to=2", nil))
	var current dashboardRealtimeSnapshot
	_ = json.Unmarshal(response.Body.Bytes(), &current)
	if current.RPM != 5 || current.TPM != nil || current.WindowSeconds != 60 || current.Scope != "current_process" {
		t.Fatalf("live scope changed with historic range: %+v", current)
	}
}

func TestDashboardRealtimeAPITracksFailedIngressWithoutHistoryScan(t *testing.T) {
	p, handler := webTestSetup(t)
	p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`invalid`)))
	yes := true
	p.Metrics.Record(RequestRecord{Time: 10, Endpoint: "responses", Status: "ok", InputTokens: 120, OutputTokens: 8, UsageKnown: &yes})
	p.Metrics.Record(RequestRecord{Endpoint: "models", Status: "ok", InputTokens: 90000, UsageKnown: &yes})
	p.Metrics.Record(RequestRecord{Endpoint: "responses", Status: "ok", InputTokens: 90000, UsageKnown: &yes, WebSocketWarmup: &yes})
	response := managedTestRequest(t, handler, http.MethodGet, "/api/overview/realtime?from=1&to=2", nil, 200)
	var result dashboardRealtimeSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.RPM != 1 || result.TPM == nil || *result.TPM != 128 || result.Completed != 1 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("API did not retain real ingress/finish measurements: %+v", result)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/overview/realtime", nil))
	if unauthorized.Code != 401 {
		t.Fatal("realtime admin endpoint lacks authentication")
	}
}
