package server

import (
	"testing"
	"time"
)

func TestCollabCacheBoundary20260928ControlCreationExcluded(t *testing.T) {
	checkCollabCacheCreationControlExclusion20260928(t, func(t *testing.T) *Metrics {
		m, err := NewMetrics(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	})
}

func TestPGCollabCacheCreationControlExclusion20260928(t *testing.T) {
	checkCollabCacheCreationControlExclusion20260928(t, func(t *testing.T) *Metrics {
		return newTestPGMetrics(t, newMetricsTestDB(t))
	})
}

func checkCollabCacheCreationControlExclusion20260928(t *testing.T, newMetrics func(*testing.T) *Metrics) {
	t.Helper()
	for _, controlWrite := range []int64{0, 100} {
		name := "measured_zero"
		if controlWrite > 0 {
			name = "measured_positive"
		}
		t.Run(name, func(t *testing.T) {
			m := newMetrics(t)
			yes, no := true, false
			base := time.Now().Unix() / 60 * 60
			rows := []RequestRecord{
				{EventID: "inference-ok", Endpoint: "responses", Status: "ok", InputTokens: 100, CachedTokens: 80, CacheWriteTokens: cacheTestCount(20)},
				{EventID: "inference-failed-with-usage", Endpoint: "responses", Status: "error", InputTokens: 100, CachedTokens: 90, CacheWriteTokens: cacheTestCount(10)},
				{EventID: "inference-known-zero", Endpoint: "responses", Status: "ok", InputTokens: 100, CacheWriteTokens: cacheTestCount(0)},
				{EventID: "inference-unknown-main-usage", Endpoint: "responses", Status: "ok", UsageKnown: &no, CacheWriteTokens: cacheTestCount(7)},
				{EventID: "websocket-warmup", Endpoint: "responses", WebSocketWarmup: &yes, Status: "ok", InputTokens: 100, CachedTokens: 50, CacheWriteTokens: cacheTestCount(controlWrite)},
				{EventID: "model-catalog", Endpoint: "models", Status: "ok", InputTokens: 100, CachedTokens: 50, CacheWriteTokens: cacheTestCount(controlWrite)},
			}
			for i, row := range rows {
				row.Time, row.Model, row.CachedTokensKnown = base+int64(i), "synthetic-cache-boundary", &yes
				if row.UsageKnown == nil {
					row.UsageKnown = &yes
				}
				m.Record(row)
			}
			if err := m.HealthError(); err != nil {
				t.Fatal(err)
			}
			overview, _, err := m.OverviewCacheUsage(t.Context(), base, base+60, 60)
			if err != nil {
				t.Fatal(err)
			}
			managed, err := m.managedLogStats(t.Context(), managedLogQuery{From: base, To: base + 60, Step: 60, Timezone: "UTC"})
			if err != nil {
				t.Fatal(err)
			}
			if overview.CacheWriteSamples != 4 || overview.CacheWriteTokens == nil || *overview.CacheWriteTokens != 37 || overview.CacheReadSamples != 3 || overview.CacheReadTokens != 170 || overview.CacheReadUnknownRequests != 1 {
				t.Fatalf("overview lost measured failure usage or included controls: %+v", overview)
			}
			if managed.Requests != 6 || managed.Errors != 1 || managed.CacheReadSamples != overview.CacheReadSamples || managed.CacheReadTokens != overview.CacheReadTokens || managed.CacheReadUnknownRequests != 1 {
				t.Fatalf("request/read totals changed: %+v", managed)
			}
			if managed.CacheWriteSamples != overview.CacheWriteSamples || managed.CacheWriteTokens == nil || *managed.CacheWriteTokens != *overview.CacheWriteTokens {
				var got int64
				if managed.CacheWriteTokens != nil {
					got = *managed.CacheWriteTokens
				}
				t.Fatalf("control creation leaked: overview samples=%d tokens=%d, managed samples=%d tokens=%d; read samples agree=%d", overview.CacheWriteSamples, *overview.CacheWriteTokens, managed.CacheWriteSamples, got, managed.CacheReadSamples)
			}
			if len(managed.Models) != 1 || len(managed.Series) != 1 {
				t.Fatal("synthetic model/series grouping changed")
			}
			for name, dimension := range map[string]managedUsageTotals{"model": managed.Models[0].managedUsageTotals, "series": managed.Series[0].managedUsageTotals} {
				if dimension.CacheWriteSamples != overview.CacheWriteSamples || dimension.CacheWriteTokens == nil || *dimension.CacheWriteTokens != *overview.CacheWriteTokens {
					t.Fatalf("%s dimension disagrees with overview: samples=%d tokens=%v", name, dimension.CacheWriteSamples, dimension.CacheWriteTokens)
				}
			}
		})
	}
}

func TestCollabCacheBoundary20260928FailureReadKnowledge(t *testing.T) {
	yes, no := true, false
	for _, status := range []string{"ok", "error", "aborted"} {
		t.Run(status, func(t *testing.T) {
			var total overviewCacheTotals
			for _, row := range []RequestRecord{
				{InputTokens: 100, CachedTokens: 80, CachedTokensKnown: &yes, UsageKnown: &yes, CacheWriteTokens: cacheTestCount(20)},
				{InputTokens: 100, CachedTokensKnown: &yes, UsageKnown: &yes, CacheWriteTokens: cacheTestCount(100)},
				{InputTokens: 1000, UsageKnown: &yes},
				{InputTokens: 100, CachedTokens: 60, CachedTokensKnown: &no, UsageKnown: &yes},
				{InputTokens: 100, CachedTokens: 70, CachedTokensKnown: &yes, UsageKnown: &no},
				{InputTokens: 100, CachedTokens: 101, CachedTokensKnown: &yes, UsageKnown: &yes},
			} {
				row.Endpoint, row.Status = "responses", status
				total.add(row)
			}
			if total.CacheReadSamples != 2 || total.CacheReadUnknownRequests != 4 || total.CacheReadInputTokens != 200 || total.CacheReadTokens != 80 || total.CacheReadRate == nil || *total.CacheReadRate != 0.4 || total.CacheWriteSamples != 2 || total.CacheWriteTokens == nil || *total.CacheWriteTokens != 120 {
				t.Fatalf("failure status changed cache knowledge or mixed creation with reads: %+v", total)
			}
		})
	}
}
