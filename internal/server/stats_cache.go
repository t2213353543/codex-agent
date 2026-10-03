package server

import (
	"context"
	"errors"
	"math"
	"strings"
)

// These observations use retained request details, not the longer-lived token
// aggregates. Cache creation is never inferred from input minus cache reads.
type managedCacheReadTotals struct {
	CacheReadSamples         int64    `json:"cache_read_samples"`
	CacheReadUnknownRequests int64    `json:"cache_read_unknown_requests"`
	CacheReadInputTokens     int64    `json:"cache_read_input_tokens"`
	CacheReadTokens          int64    `json:"cache_read_tokens"`
	CacheReadRate            *float64 `json:"cache_read_rate"`
}

func cacheObservationEligible(r RequestRecord) bool {
	if r.WebSocketWarmup != nil && *r.WebSocketWarmup {
		return false
	}
	path := "/" + strings.TrimPrefix(r.Endpoint, "/")
	return r.Endpoint == "" || dashboardRealtimeInferencePath(path) || path == "/compact" || path == "/realtime" || path == "/chat/completions" || path == "/codex/tasks"
}

func cacheReadObserved(r RequestRecord) bool {
	known := managedUsageKnown(r)
	if known == nil || !*known || r.InputTokens < 0 || r.CachedTokens < 0 || r.CachedTokens > r.InputTokens {
		return false
	}
	if r.CachedTokensKnown != nil {
		return *r.CachedTokensKnown
	}
	// Legacy records do not distinguish an absent cached count from zero.
	// A positive count is evidence of a read; a legacy zero is not.
	return r.CachedTokens > 0
}

func (t *managedCacheReadTotals) addCacheRead(r RequestRecord) {
	if !cacheObservationEligible(r) {
		return
	}
	if !cacheReadObserved(r) {
		t.CacheReadUnknownRequests++
		return
	}
	t.CacheReadSamples++
	t.CacheReadInputTokens += r.InputTokens
	t.CacheReadTokens += r.CachedTokens
	t.finishCacheRead()
}

func (t *managedCacheReadTotals) finishCacheRead() {
	t.CacheReadRate = nil
	if t.CacheReadSamples > 0 && t.CacheReadInputTokens > 0 {
		rate := float64(t.CacheReadTokens) / float64(t.CacheReadInputTokens)
		t.CacheReadRate = &rate
	}
}

// The accepted paths form a finite set. Expanding the optional slash and
// prefix once avoids running regexp_replace for every retained request row.
var cacheObservationEligibleSQL = func() string {
	paths := []string{"", "responses", "responses/compact", "compact", "chat/completions", "codex/tasks", "guardian", "guardian-classifier", "alpha/search", "images/generations", "images/edits", "memories/trace_summarize", "realtime/calls", "realtime", "live"}
	values := make([]string, 0, len(paths)*6)
	for _, prefix := range []string{"", "/", "v1/", "/v1/", "backend-api/codex/", "/backend-api/codex/"} {
		for _, path := range paths {
			values = append(values, "'"+prefix+path+"'")
		}
	}
	return `NOT COALESCE((record->>'websocket_warmup')::boolean,false) AND COALESCE(record->>'endpoint','') IN (` + strings.Join(values, ",") + ")"
}()

const cacheReadObservedSQL = `COALESCE((record->>'usage_known')::boolean,COALESCE((record->>'input_tokens')::bigint,0)<>0 OR COALESCE((record->>'output_tokens')::bigint,0)<>0 OR COALESCE((record->>'cached_tokens')::bigint,0)<>0)
 AND COALESCE((record->>'cached_tokens_known')::boolean,COALESCE((record->>'cached_tokens')::bigint,0)>0)
 AND COALESCE((record->>'cached_tokens')::bigint,0)>=0
 AND COALESCE((record->>'input_tokens')::bigint,0)>=COALESCE((record->>'cached_tokens')::bigint,0)`

type overviewCacheTotals struct {
	managedCacheReadTotals
	CacheWriteSamples int64  `json:"cache_write_samples"`
	CacheWriteTokens  *int64 `json:"cache_write_tokens"`
}

func (t *overviewCacheTotals) add(r RequestRecord) {
	if !cacheObservationEligible(r) {
		return
	}
	t.addCacheRead(r)
	if r.CacheWriteTokens != nil && *r.CacheWriteTokens >= 0 {
		t.CacheWriteSamples++
		if t.CacheWriteTokens == nil {
			t.CacheWriteTokens = new(int64)
		}
		*t.CacheWriteTokens += *r.CacheWriteTokens
	}
}

// Only nonnegative observed counts reach this merge. PostgreSQL previously
// rejected overflowing grand totals; merging bounded chart rows must preserve
// that behavior instead of silently wrapping Go integers.
func (t *overviewCacheTotals) merge(point overviewCacheTotals) error {
	for _, value := range []struct {
		target *int64
		count  int64
	}{
		{&t.CacheReadSamples, point.CacheReadSamples},
		{&t.CacheReadUnknownRequests, point.CacheReadUnknownRequests},
		{&t.CacheReadInputTokens, point.CacheReadInputTokens},
		{&t.CacheReadTokens, point.CacheReadTokens},
		{&t.CacheWriteSamples, point.CacheWriteSamples},
	} {
		if value.count > math.MaxInt64-*value.target {
			return errors.New("cache observation total exceeds int64")
		}
		*value.target += value.count
	}
	if point.CacheWriteTokens != nil {
		if t.CacheWriteTokens == nil {
			t.CacheWriteTokens = new(int64)
		}
		if *point.CacheWriteTokens > math.MaxInt64-*t.CacheWriteTokens {
			return errors.New("cache observation total exceeds int64")
		}
		*t.CacheWriteTokens += *point.CacheWriteTokens
	}
	return nil
}

// Whole minutes use the transactionally maintained retained-detail summary.
// A bucket's partial minutes and unusual fractional-second imports use the
// indexed detail ranges so arbitrary bounds preserve the exact old semantics.
func (m *Metrics) OverviewCacheUsage(ctx context.Context, from, to, step int64) (overviewCacheTotals, map[int64]overviewCacheTotals, error) {
	var total overviewCacheTotals
	points := make(map[int64]overviewCacheTotals)
	if m.db == nil {
		err := m.walkManagedLogs(ctx, managedLogQuery{From: from, To: to}, func(r RequestRecord) error {
			total.add(r)
			bucket := from + (r.Time-from)/step*step
			point := points[bucket]
			point.add(r)
			points[bucket] = point
			return nil
		})
		return total, points, err
	}
	rows, err := m.db.Pool.Query(ctx, overviewCacheRollupSQL(), from, to, step)
	if err != nil {
		return total, points, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket int64
		var values overviewCacheTotals
		if err := rows.Scan(&bucket, &values.CacheReadSamples, &values.CacheReadUnknownRequests, &values.CacheReadInputTokens, &values.CacheReadTokens, &values.CacheWriteSamples, &values.CacheWriteTokens); err != nil {
			return total, points, err
		}
		values.finishCacheRead()
		if err := total.merge(values); err != nil {
			return total, points, err
		}
		points[bucket] = values
	}
	total.finishCacheRead()
	return total, points, rows.Err()
}

func overviewCacheRollupSQL() string {
	return `WITH slots AS MATERIALIZED (
 SELECT point AS bucket,LEAST(point+$3::bigint,$2::bigint) AS end_at,
        (ceil(point::numeric/60)*60)::bigint AS full_from,
        (floor(LEAST(point+$3::bigint,$2::bigint)::numeric/60)*60)::bigint AS full_to
 FROM generate_series($1::bigint,$2::bigint-1,$3::bigint) point
), minutes AS MATERIALIZED (
 SELECT s.bucket AS output_bucket,u.* FROM slots s JOIN retained_log_minute u
 ON u.bucket>=s.full_from AND u.bucket<s.full_to AND u.requests>0
), edges AS (
 SELECT bucket AS lo,LEAST(end_at,full_from) AS hi FROM slots WHERE bucket<LEAST(end_at,full_from)
 UNION ALL SELECT GREATEST(bucket,full_from,full_to),end_at FROM slots WHERE GREATEST(bucket,full_from,full_to)<end_at
 UNION ALL SELECT bucket,bucket+60 FROM minutes WHERE subsecond_requests>0
), details AS (
 SELECT $1::bigint+((extract(epoch FROM created_at)::bigint-$1::bigint)/$3::bigint)*$3::bigint AS bucket,
        COALESCE((record->>'input_tokens')::bigint,0) AS input,
        COALESCE((record->>'cached_tokens')::bigint,0) AS cached,
        (record->>'cache_write_tokens')::bigint AS cache_write,
        (` + cacheReadObservedSQL + `) AS read_observed
 FROM edges CROSS JOIN LATERAL (
 SELECT created_at,record FROM request_logs
 WHERE created_at>=to_timestamp(lo) AND created_at<to_timestamp(hi) OFFSET 0
 ) detail
 WHERE (` + cacheObservationEligibleSQL + `)
), combined AS (
 SELECT output_bucket AS bucket,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,
        cache_read_tokens,cache_write_samples,cache_write_tokens FROM minutes
 WHERE subsecond_requests=0 AND cache_read_samples+cache_read_unknown_requests>0
 UNION ALL
 SELECT bucket,count(*) FILTER(WHERE read_observed)::numeric,count(*) FILTER(WHERE NOT read_observed)::numeric,
        COALESCE(sum(input) FILTER(WHERE read_observed),0),COALESCE(sum(cached) FILTER(WHERE read_observed),0),
        count(cache_write) FILTER(WHERE cache_write>=0)::numeric,COALESCE(sum(cache_write) FILTER(WHERE cache_write>=0),0)
 FROM details GROUP BY bucket
)
SELECT bucket,sum(cache_read_samples)::bigint,sum(cache_read_unknown_requests)::bigint,
       sum(cache_read_input_tokens)::bigint,sum(cache_read_tokens)::bigint,sum(cache_write_samples)::bigint,
       CASE WHEN sum(cache_write_samples)>0 THEN sum(cache_write_tokens)::bigint END
FROM combined GROUP BY bucket`
}
