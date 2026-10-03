package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
)

func TestPGOperationsSQLParity(t *testing.T) {
	db := newMetricsTestDB(t)
	pg := newTestPGMetrics(t, db)
	file, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(file.Close)
	const from int64 = 1800000007 // Deliberately not aligned to a minute.
	latencies := []int64{-1, 0, 1, 1000, 1001, 5000, 5001, 15000, 15001, 30000, 30001, 60000, 60001, 180000, 180001}
	errors := []string{"", "unsupported parameter", "system messages are not allowed", "no enabled account", "invalidated oauth", "model_not_found", "invalid_encrypted_content", "exceeds 32 mib", "content_filter", "rate limit", "overloaded", "an error occurred while processing", "context canceled", "timeout"}
	for i := 0; i < 256; i++ {
		r := RequestRecord{EventID: fmt.Sprintf("operations-parity-%d", i), Time: from + int64((255-i)/2)*29 - 2,
			Status: []string{"ok", "error", "aborted"}[i%3], HTTPCode: []int{200, 401, 403, 413, 429, 499, 500, 504}[i%8],
			Model: "synthetic", Error: errors[i%len(errors)], LatencyMs: latencies[i%len(latencies)],
			InputTokens: int64(i % 13), OutputTokens: int64(i % 7), CachedTokens: int64(i % 5)}
		if i%3 != 0 {
			known := i%3 == 1
			r.LatencyKnown, r.UsageKnown = &known, &known
		}
		if i%4 != 0 {
			ttft := int64(i%4 - 2) // Missing, negative, observed zero, positive.
			r.TTFTMs = &ttft
		}
		pg.Record(r)
		file.Record(r)
	}
	if err := pg.HealthError(); err != nil {
		t.Fatal(err)
	}
	for _, interval := range [][2]int64{{from, from + 900}, {from, from + 3600}, {from - 86400, from}, {from + 10000, from + 13600}} {
		want, err := file.operationsTraffic(context.Background(), interval[0], interval[1])
		if err != nil {
			t.Fatal(err)
		}
		got, err := pg.operationsTraffic(context.Background(), interval[0], interval[1])
		if err != nil {
			t.Fatal(err)
		}
		compareAverages := func(got *operationsTotals, want operationsTotals) {
			t.Helper()
			for _, pair := range []struct {
				got  **float64
				want *float64
			}{{&got.AverageLatency, want.AverageLatency}, {&got.AverageTTFT, want.AverageTTFT}} {
				if (*pair.got == nil) != (pair.want == nil) {
					t.Fatal("SQL changed whether a measured average is known")
				}
				// PostgreSQL divides exact numeric sums; Go divides float64
				// sums. The last rounding bit may differ for the same counts.
				if pair.want != nil && (math.IsNaN(**pair.got) || math.Abs(**pair.got-*pair.want) > 1e-12*math.Max(1, math.Abs(*pair.want))) {
					t.Fatalf("average differs: SQL=%v file=%v", **pair.got, *pair.want)
				}
				*pair.got = pair.want
			}
		}
		compareAverages(&got.Traffic, want.Traffic)
		if len(got.Series) != len(want.Series) {
			t.Fatal("SQL changed series length")
		}
		for i := range got.Series {
			compareAverages(&got.Series[i].operationsTotals, want.Series[i].operationsTotals)
		}
		// Compare the public representation: internal running sums are not part
		// of the result. IDs use the same insertion order in both stores.
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("range %v SQL/file mismatch\nSQL: %s\nfile: %s", interval, gotJSON, wantJSON)
		}
	}
}

func BenchmarkOperationsPGTraffic(b *testing.B) {
	m := managedPerformanceMetrics(b, true)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := m.operationsTraffic(context.Background(), 1700000000, 1700086400)
		if err != nil || out.Traffic.Requests != 30000 {
			b.Fatalf("invalid operations result: %d %v", out.Traffic.Requests, err)
		}
	}
}

func TestPGOperationsFractionalTimestampBoundaries(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const from int64 = 1800000000
	for i, offset := range []int64{14, 899} {
		m.Record(RequestRecord{EventID: fmt.Sprintf("ops-fraction-%d", i), Time: from + offset, Status: "error", HTTPCode: 500, LatencyMs: 1})
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	// PostgreSQL rounds epoch::bigint. A timestamp within the final fraction
	// of a slot/window must remain inside that half-open slot/window.
	if _, err := db.Pool.Exec(t.Context(), "UPDATE request_logs SET created_at=created_at+interval '0.75 seconds'"); err != nil {
		t.Fatal(err)
	}
	out, err := m.operationsTraffic(t.Context(), from, from+900)
	if err != nil || out.Traffic.Requests != 2 || len(out.Series) != 60 {
		t.Fatalf("fractional window failed: requests=%d err=%v", out.Traffic.Requests, err)
	}
	if out.Series[0].Requests != 1 || out.Series[59].Requests != 1 || len(out.RecentErrors) != 2 || out.RecentErrors[0].Time != from+899 {
		t.Fatalf("fractional timestamps escaped their slots: %+v", out)
	}
}
