package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestManagedTimingCoverage(t *testing.T)   { runManagedTimingCoverage(t, webTestSetup) }
func TestPGManagedTimingCoverage(t *testing.T) { runManagedTimingCoverage(t, adminPGTestProxy) }

func runManagedTimingCoverage(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	measured, unknown := true, false
	now := time.Now().Unix()
	rows := []RequestRecord{
		{EventID: "timing-absent", Time: now - 3, Status: "ok"},
		{EventID: "timing-zero", Time: now - 2, Status: "ok", LatencyKnown: &measured},
		{EventID: "timing-old-positive", Time: now - 1, Status: "ok", LatencyMs: 101},
		{EventID: "timing-explicit-unknown", Time: now, Status: "ok", LatencyMs: 900, LatencyKnown: &unknown},
		{EventID: "timing-invalid-negative", Time: now, Status: "ok", LatencyMs: -10, LatencyKnown: &measured},
	}
	var memory managedUsageTotals
	var operations operationsTotals
	for _, row := range rows {
		p.Metrics.Record(row)
		memory.add(row)
		operations.add(row)
	}
	if err := p.Metrics.Save(); err != nil {
		t.Fatal(err)
	}
	query := managedLogQuery{From: now - 10, To: now + 1, Timezone: "Asia/Shanghai"}
	stats, err := p.Metrics.managedLogStats(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct {
		samples int64
		avg     *float64
	}{{stats.LatencySamples, stats.AverageLatency}, {memory.LatencySamples, memory.AverageLatency}, {operations.LatencySamples, operations.AverageLatency}} {
		if value.samples != 2 || value.avg == nil || *value.avg != 50.5 {
			t.Fatalf("duration coverage: %+v", value)
		}
	}
	for i, row := range rows {
		raw, err := json.Marshal(managedRecord(row))
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		expected := any(nil)
		if i == 1 {
			expected = float64(0)
		}
		if i == 2 {
			expected = float64(101)
		}
		if parsed["latency_ms"] != expected {
			t.Fatalf("row %d: %s", i, raw)
		}
	}
	// Totals intentionally operates on whole minute buckets. Use an aligned
	// boundary so the test is independent of the wall clock's current second.
	fromMinute := (now - 10) / 60 * 60
	totals := p.Metrics.Totals(fromMinute, now+1)
	if totals.LatencyN != 2 || totals.Latency != 101 {
		t.Fatalf("aggregate: %+v", totals)
	}
	if p.Metrics.db != nil {
		if _, err := p.Metrics.Prune(context.Background(), time.Unix(now+1, 0), 100); err != nil {
			t.Fatal(err)
		}
		if err := p.Metrics.RebuildAggregates(context.Background()); err != nil {
			t.Fatal(err)
		}
		totals = p.Metrics.Totals(fromMinute, now+1)
		if totals.LatencyN != 2 || totals.Latency != 101 {
			t.Fatalf("pruned rebuild lost zero: %+v", totals)
		}
		var exported bytes.Buffer
		if err := p.Metrics.ExportRecords(context.Background(), &exported); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range bytes.Split(bytes.TrimSpace(exported.Bytes()), []byte("\n")) {
			var row RequestRecord
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			if row.EventID == "timing-zero" {
				found = true
				if !row.hasMeasuredLatency() {
					t.Fatal("pruned export lost measured zero")
				}
			}
		}
		if !found {
			t.Fatal("zero sample not exported")
		}
	}
}

func TestManagedTimingAveragesDoNotOverflowInt64(t *testing.T) {
	large := int64(math.MaxInt64)
	measured := true
	var totals managedUsageTotals
	for i := 0; i < 3; i++ {
		totals.add(RequestRecord{LatencyMs: large, LatencyKnown: &measured, TTFTMs: &large})
	}
	if totals.AverageLatency == nil || *totals.AverageLatency != float64(large) || totals.AverageTTFT == nil || *totals.AverageTTFT != float64(large) {
		t.Fatalf("duration sums overflowed: %+v", totals)
	}
}

func TestManagedAccountCostSub2APIComponentsAndFrozenPrice(t *testing.T) {
	write, imageIn, imageOut := int64(20000), int64(10000), int64(100)
	record := RequestRecord{Model: "gpt-5.6-sol", InputTokens: 100000, CachedTokens: 30000, OutputTokens: 1000, CacheWriteTokens: &write, ImageInputTokens: &imageIn, ImageOutputTokens: &imageOut}
	// 50k normal input + 20k cache creation + 30k cached + 1k output.
	value, known := managedAccountRequestUSD(record)
	if !known || math.Abs(value-.42) > 1e-12 {
		t.Fatalf("mutually exclusive tokens: %g %v", value, known)
	}
	freezeManagedAccountRequestUSD(&record)
	version := record.WindowCostPriceVersion
	record.ServiceTier = "fast" // Subsequent catalog/policy changes cannot reprice persisted records.
	value, known = managedAccountRequestUSD(record)
	if !known || math.Abs(value-.42) > 1e-12 || version == "" {
		t.Fatalf("frozen cost changed: %g %v", value, known)
	}
	raw, _ := json.Marshal(managedRecord(record))
	if bytes.Contains(raw, []byte("window_cost")) {
		t.Fatal("window-only estimate leaked into usage logs API")
	}
}
