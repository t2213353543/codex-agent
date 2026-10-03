package server

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkSystemPGRetainedRollupWrites(b *testing.B) {
	p, _ := accountPerformanceFixture(b)
	ctx := context.Background()
	statement := `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status) SELECT $1::text||'-'||j,to_timestamp(1700000000+j%60),'ok'
 FROM generate_series(1,$2::int)j RETURNING event_id,created_at,status)
 INSERT INTO request_logs(event_id,created_at,status,record)
 SELECT event_id,created_at,status,'{"input_tokens":100,"cached_tokens":30,"cached_tokens_known":true,"usage_known":true,"cache_write_tokens":5}'::jsonb FROM events`
	for _, count := range []int{1, 1000} {
		for _, enabled := range []bool{false, true} {
			b.Run(fmt.Sprintf("batch_%d/summary_%t", count, enabled), func(b *testing.B) {
				action := "DISABLE"
				if enabled {
					action = "ENABLE"
				}
				if _, err := p.Store.DB.Pool.Exec(ctx, "ALTER TABLE request_logs "+action+" TRIGGER request_logs_retained_insert"); err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					_, _ = p.Store.DB.Pool.Exec(ctx, "ALTER TABLE request_logs ENABLE TRIGGER request_logs_retained_insert")
				})
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					// Keep relation size and FK planner statistics identical. Letting
					// prior samples accumulate confounds trigger overhead with growth.
					b.StopTimer()
					if _, err := p.Store.DB.Pool.Exec(ctx, "TRUNCATE request_logs,usage_events"); err != nil {
						b.Fatal(err)
					}
					if _, err := p.Store.DB.Pool.Exec(ctx, "ANALYZE request_logs,usage_events,retained_log_minute"); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					if _, err := p.Store.DB.Pool.Exec(ctx, statement, newID("retained-benchmark-"), count); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkSystemPGRetainedCacheReads(b *testing.B) {
	p, _ := accountPerformanceFixture(b)
	ctx := context.Background()
	_, err := p.Store.DB.Pool.Exec(ctx, `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status) SELECT 'cache-bench-'||i,to_timestamp(1700000000+i%86400),'ok'
 FROM generate_series(1,100000)i RETURNING event_id,created_at,status)
 INSERT INTO request_logs(event_id,created_at,status,record)
 SELECT event_id,created_at,status,'{"input_tokens":100,"cached_tokens":30,"cached_tokens_known":true,"usage_known":true,"cache_write_tokens":5}'::jsonb FROM events`)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(ctx, "ANALYZE request_logs,usage_events,retained_log_minute"); err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct{ name, query string }{
		{"retained_rollup", overviewCacheRollupSQL()},
		{"detail_aggregate", `WITH source AS MATERIALIZED (
 SELECT $1::bigint+((extract(epoch FROM created_at)::bigint-$1::bigint)/$3::bigint)*$3::bigint AS bucket,
 COALESCE((record->>'input_tokens')::bigint,0) AS input,COALESCE((record->>'cached_tokens')::bigint,0) AS cached,
 (record->>'cache_write_tokens')::bigint AS cache_write,(` + cacheReadObservedSQL + `) AS observed
 FROM request_logs WHERE created_at>=to_timestamp($1) AND created_at<to_timestamp($2) AND (` + legacyCacheObservationEligibleSQL + `)
 ) SELECT bucket,count(*) FILTER(WHERE observed)::bigint,count(*) FILTER(WHERE NOT observed)::bigint,
 COALESCE(sum(input) FILTER(WHERE observed),0)::bigint,COALESCE(sum(cached) FILTER(WHERE observed),0)::bigint,
 count(cache_write) FILTER(WHERE cache_write>=0)::bigint,(sum(cache_write) FILTER(WHERE cache_write>=0))::bigint
 FROM source GROUP BY GROUPING SETS((),(bucket))`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var plan []byte
			if err := p.Store.DB.Pool.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+tc.query, int64(1699999980), int64(1700086440), int64(3600)).Scan(&plan); err != nil {
				b.Fatal(err)
			}
			b.Logf("RETAINED_CACHE_PLAN %s", plan)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rows, err := p.Store.DB.Pool.Query(ctx, tc.query, int64(1699999980), int64(1700086440), int64(3600))
				if err != nil {
					b.Fatal(err)
				}
				for rows.Next() {
					if _, err := rows.Values(); err != nil {
						b.Fatal(err)
					}
				}
				if err := rows.Err(); err != nil {
					b.Fatal(err)
				}
				rows.Close()
			}
		})
	}
}
