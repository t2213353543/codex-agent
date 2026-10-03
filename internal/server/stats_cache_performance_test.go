package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
)

const legacyCacheObservationEligibleSQL = `NOT COALESCE((record->>'websocket_warmup')::boolean,false) AND
 regexp_replace(COALESCE(record->>'endpoint',''),'^/?(backend-api/codex/|v1/)?','') IN
 ('','responses','responses/compact','compact','chat/completions','codex/tasks','guardian','guardian-classifier','alpha/search','images/generations','images/edits','memories/trace_summarize','realtime/calls','realtime','live')`

func TestPGCacheObservationEndpointPredicateParity(t *testing.T) {
	db := newMetricsTestDB(t)
	var records []string
	for _, path := range []string{"", "responses", "responses/compact", "compact", "chat/completions", "codex/tasks", "guardian", "guardian-classifier", "alpha/search", "images/generations", "images/edits", "memories/trace_summarize", "realtime/calls", "realtime", "live", "models", "Responses", "responses/", "responses?stream=true", "\nresponses"} {
		for _, prefix := range []string{"", "/", "v1/", "/v1/", "backend-api/codex/", "/backend-api/codex/", "//", "/backend-api/codex/v1/", "/v1/v1/", "foo/"} {
			for _, warmup := range []any{nil, true, false} {
				raw, _ := json.Marshal(map[string]any{"endpoint": prefix + path, "websocket_warmup": warmup})
				records = append(records, string(raw))
			}
		}
	}
	records = append(records, `{}`, `{"endpoint":null}`)
	var differences int
	err := db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM (SELECT value::jsonb AS record FROM unnest($1::text[]) value) records WHERE (`+legacyCacheObservationEligibleSQL+`) IS DISTINCT FROM (`+cacheObservationEligibleSQL+`)`, records).Scan(&differences)
	if err != nil || differences != 0 {
		t.Fatalf("endpoint predicate changed: %d %v", differences, err)
	}
}

func insertCacheObservationForTest(t *testing.T, m *Metrics, id string, at int64, record map[string]any) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.db.Pool.Exec(t.Context(), `WITH event AS (
		INSERT INTO usage_events(event_id,created_at,status) VALUES($1,to_timestamp($2),'ok') RETURNING event_id,created_at,status
	) INSERT INTO request_logs(event_id,created_at,status,record) SELECT event_id,created_at,status,$3::jsonb FROM event`, id, at, raw)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPGOverviewCacheBucketMergeParity(t *testing.T) {
	m := newTestPGMetrics(t, newMetricsTestDB(t))
	base := int64(1700000000)
	var expected overviewCacheTotals
	expectedPoints := map[int64]overviewCacheTotals{}
	rows := []map[string]any{
		{"usage_known": true, "input_tokens": 100, "cached_tokens": 80, "cached_tokens_known": true, "cache_write_tokens": 20},
		{"usage_known": true, "input_tokens": 50, "cached_tokens_known": true, "cache_write_tokens": 0},
		{"input_tokens": 100, "cached_tokens": 50},
		{"input_tokens": -1, "cached_tokens": 0, "cached_tokens_known": true, "cache_write_tokens": -1},
		{"input_tokens": 100, "cached_tokens": 30, "usage_known": false},
		{"input_tokens": 10, "cached_tokens": 11},
		{"input_tokens": 20, "cached_tokens": 2, "cached_tokens_known": false},
		{"input_tokens": 0, "cached_tokens": 0, "usage_known": true, "cached_tokens_known": true, "cache_write_tokens": nil},
		{},
	}
	for i, record := range rows {
		at := base + int64(i)*30
		insertCacheObservationForTest(t, m, fmt.Sprintf("cache-bucket-%d", i), at, record)
		raw, _ := json.Marshal(record)
		var request RequestRecord
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		expected.add(request)
		bucket := base + (at-base)/60*60
		point := expectedPoints[bucket]
		point.add(request)
		expectedPoints[bucket] = point
	}
	got, points, err := m.OverviewCacheUsage(context.Background(), base, base+600, 60)
	if err != nil || !reflect.DeepEqual(got, expected) || !reflect.DeepEqual(points, expectedPoints) {
		t.Fatalf("bucket merge changed observations: got=%+v want=%+v points=%+v wantPoints=%+v err=%v", got, expected, points, expectedPoints, err)
	}
}

func TestPGOverviewCacheTotalsOverflow(t *testing.T) {
	for _, field := range []string{"input_tokens", "cache_write_tokens"} {
		for _, spacing := range []int64{1, 60} {
			t.Run(fmt.Sprintf("%s/spacing_%d", field, spacing), func(t *testing.T) {
				m := newTestPGMetrics(t, newMetricsTestDB(t))
				base := int64(1700000000)
				for i, amount := range []int64{math.MaxInt64, 1} {
					record := map[string]any{"usage_known": true, "cached_tokens_known": true, field: amount}
					insertCacheObservationForTest(t, m, fmt.Sprintf("cache-overflow-%d", i), base+int64(i)*spacing, record)
				}
				if _, _, err := m.OverviewCacheUsage(t.Context(), base, base+120, 60); err == nil {
					t.Fatal("overflowing observation totals were returned as success")
				}
			})
		}
	}
}
