package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestResponseCacheCreationAndReadAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		readKnown   bool
		read        int64
		write       *int64
	}{
		{"both", `"input_tokens":100,"output_tokens":3,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":20}`, true, 80, cacheTestCount(20)},
		{"cold read", `"input_tokens":100,"output_tokens":3,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":100}`, true, 0, cacheTestCount(100)},
		{"creation without read", `"input_tokens":100,"output_tokens":3,"input_tokens_details":{"cache_write_tokens":100}`, false, 0, cacheTestCount(100)},
		{"read without creation", `"input_tokens":100,"output_tokens":3,"input_tokens_details":{"cached_tokens":80}`, true, 80, nil},
		{"measured creation zero", `"input_tokens":100,"output_tokens":3,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":0}`, true, 80, cacheTestCount(0)},
		{"chat compatibility", `"prompt_tokens":100,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":80}`, true, 80, nil},
		{"impossible read", `"input_tokens":100,"output_tokens":3,"input_tokens_details":{"cached_tokens":101}`, false, 101, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, transport := range []string{"json", "sse"} {
				t.Run(transport, func(t *testing.T) {
					raw := []byte(`{"id":"cache-fixture","status":"completed","usage":{` + tc.usage + `}}`)
					rec := RequestRecord{}
					if transport == "json" {
						recordResponseOutcome(&rec, raw)
					} else {
						scanner := sseScanner{rec: &rec}
						_, _ = scanner.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + string(raw) + "}\n\n"))
						scanner.Finish()
					}
					if rec.CachedTokensKnown == nil || *rec.CachedTokensKnown != tc.readKnown || rec.CachedTokens != tc.read {
						t.Fatalf("read observation: %+v", rec)
					}
					if (rec.CacheWriteTokens == nil) != (tc.write == nil) || tc.write != nil && *rec.CacheWriteTokens != *tc.write {
						t.Fatalf("creation observation got=%v want=%v", rec.CacheWriteTokens, tc.write)
					}
				})
			}
		})
	}
}

func cacheTestCount(n int64) *int64 { return &n }

func checkCacheReadCreationStatistics(t *testing.T, m *Metrics) {
	t.Helper()
	yes, no := true, false
	base := time.Now().Unix() / 60 * 60
	rows := []RequestRecord{
		{InputTokens: 100, CachedTokens: 80, CachedTokensKnown: &yes, CacheWriteTokens: cacheTestCount(20)},
		{InputTokens: 100, CachedTokensKnown: &yes, CacheWriteTokens: cacheTestCount(100)},
		{InputTokens: 1000},
		{InputTokens: 80, CachedTokens: 40},
		{InputTokens: 100, CachedTokens: 30, CachedTokensKnown: &no},
		{InputTokens: 100, CachedTokens: 50, CachedTokensKnown: &yes, UsageKnown: &no},
		{InputTokens: 100, CachedTokens: 99, CachedTokensKnown: &yes, Endpoint: "models"},
		{InputTokens: 100, CachedTokens: 99, CachedTokensKnown: &yes, WebSocketWarmup: &yes},
		{InputTokens: 100, CachedTokens: 101, CachedTokensKnown: &yes},
		{InputTokens: 10, CacheWriteTokens: cacheTestCount(0)},
	}
	for i, r := range rows {
		r.Time, r.EventID, r.Model, r.Status = base+int64(i), "cache-measurement-"+itoa(int64(i)), "cache-fixture", "ok"
		if r.Endpoint == "" {
			r.Endpoint = "responses"
		}
		if r.UsageKnown == nil {
			r.UsageKnown = &yes
		}
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertRead := func(got managedCacheReadTotals) {
		t.Helper()
		if got.CacheReadSamples != 3 || got.CacheReadUnknownRequests != 5 || got.CacheReadInputTokens != 280 || got.CacheReadTokens != 120 || got.CacheReadRate == nil || math.Abs(*got.CacheReadRate-120.0/280) > 1e-12 {
			t.Fatalf("unknown/control/creation must not become cache reads: %+v", got)
		}
	}
	total, points, err := m.OverviewCacheUsage(t.Context(), base, base+60, 60)
	if err != nil {
		t.Fatal(err)
	}
	assertRead(total.managedCacheReadTotals)
	assertRead(points[base].managedCacheReadTotals)
	if total.CacheWriteSamples != 3 || total.CacheWriteTokens == nil || *total.CacheWriteTokens != 120 {
		t.Fatalf("creation lost unknown vs zero: %+v", total)
	}
	stats, err := m.managedLogStats(t.Context(), managedLogQuery{From: base, To: base + 60, Step: 60, Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	assertRead(stats.managedCacheReadTotals)
	assertRead(stats.Models[0].managedCacheReadTotals)
	assertRead(stats.Series[0].managedCacheReadTotals)
	if stats.CacheWriteSamples != 3 || stats.CacheWriteTokens == nil || *stats.CacheWriteTokens != 120 || !stats.Available["cache_creation_tokens"] || !stats.Available["cache_read_tokens"] {
		t.Fatalf("incorrect creation coverage: %+v", stats)
	}
	empty, _, err := m.OverviewCacheUsage(t.Context(), base+60, base+120, 60)
	if err != nil || empty.CacheReadRate != nil || empty.CacheWriteTokens != nil {
		t.Fatalf("empty must stay unknown: %+v err=%v", empty, err)
	}
	serialized, _ := json.Marshal(empty)
	var fields map[string]any
	_ = json.Unmarshal(serialized, &fields)
	if fields["cache_read_rate"] != nil || fields["cache_write_tokens"] != nil {
		t.Fatalf("unknown serialized as zero: %s", serialized)
	}
}

func TestCacheReadCreationStatisticsFile(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	checkCacheReadCreationStatistics(t, m)
}

func TestCacheReadCreationStatisticsPG(t *testing.T) {
	checkCacheReadCreationStatistics(t, newTestPGMetrics(t, newMetricsTestDB(t)))
}

func TestOverviewCacheReadAndCreationContract(t *testing.T) {
	p, handler := webTestSetup(t)
	yes := true
	base := time.Now().Unix() / 60 * 60
	p.Metrics.Record(RequestRecord{EventID: "overview-cache-read", Time: base, Endpoint: "responses", Status: "ok", InputTokens: 100, CachedTokens: 80, CachedTokensKnown: &yes, CacheWriteTokens: cacheTestCount(20), UsageKnown: &yes})
	p.Metrics.Record(RequestRecord{EventID: "overview-cache-unreported", Time: base + 1, Endpoint: "responses", Status: "ok", InputTokens: 1000, UsageKnown: &yes})
	response := webTestRequest(handler, http.MethodGet, "/api/overview?from="+strconv.FormatInt(base, 10)+"&to="+strconv.FormatInt(base+60, 10)+"&step=60", "", "Bearer "+webTestKey)
	var body struct {
		Cards  map[string]any `json:"cards"`
		Series []SeriesPoint  `json:"series"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("overview response: %d %s", response.Code, response.Body.String())
	}
	if body.Cards["cache_scope"] != "retained_request_details" || body.Cards["cache_rate_has_samples"] != true || body.Cards["cache_rate"] != 0.8 || body.Cards["cache_read_unknown_requests"] != float64(1) || body.Cards["cache_write_tokens"] != float64(20) {
		t.Fatalf("overview mixed cache creation, reads or unknown input: %+v", body.Cards)
	}
	if len(body.Series) == 0 || body.Series[0].CacheReadRate == nil || *body.Series[0].CacheReadRate != 0.8 || body.Series[0].CacheWriteTokens == nil || *body.Series[0].CacheWriteTokens != 20 {
		t.Fatalf("overview chart lost cache observations: %+v", body.Series)
	}
}
