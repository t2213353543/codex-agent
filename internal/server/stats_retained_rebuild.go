package server

import (
	"context"
	"errors"
	"strings"
)

// retainedUsageInsertColumns expands the shared rollup column list for INSERT
// statements so the rebuild cannot drift from the table definitions.
func retainedUsageInsertColumns(leading ...string) string {
	return strings.Join(append(append([]string{}, leading...), strings.Split(managedUsageRollupColumns, ",")...), ",")
}

func retainedUsageSumColumns() string {
	sums := make([]string, 0, 36)
	for _, column := range strings.Split(managedUsageRollupColumns, ",") {
		sums = append(sums, "sum("+column+")")
	}
	return strings.Join(sums, ",")
}

// RebuildRetainedRollups reconstructs every retained rollup family from the
// current request_logs details inside one transaction. Writers pause on the
// same shared advisory boundary as RebuildAggregates, so no request is counted
// twice or missed. The statements mirror the migrations' online backfills:
// detail rows are classified once per family and every summary row is written
// exactly once, instead of re-upserting hot minute rows per event.
//
// Bulk loaders may additionally disable the request_logs statement triggers
// while inserting and call this once afterwards; trigger state is
// transactional in that flow and the advisory lock still fences live writers.
func (m *Metrics) RebuildRetainedRollups(ctx context.Context) error {
	if m.db == nil {
		return errors.New("retained rollup rebuild requires PostgreSQL")
	}
	tx, err := m.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	// Maintenance statements legitimately run far longer than the interactive
	// pool default; SET LOCAL keeps the raised deadline inside this transaction.
	// The classification functions are PARALLEL SAFE, so a large history can
	// classify across every gather worker the server allows.
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout='30min'"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "SET LOCAL work_mem='256MB'"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "SET LOCAL max_parallel_workers_per_gather=4"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema(),$1))", metricsAggregateLock); err != nil {
		return err
	}
	// retained_usage_minute's truncate trigger also clears retained_usage_daily;
	// listing that table here too would fail with "in use in this session".
	// The request_logs truncate triggers stay dormant because details are never
	// truncated by a rebuild.
	if _, err = tx.Exec(ctx, "TRUNCATE retained_log_minute,retained_usage_minute,retained_usage_unsafe_minute,retained_usage_model_minute,retained_usage_model_unsafe_minute,retained_usage_model_daily,retained_operations"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO retained_log_minute(bucket,requests,ok,errors,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens,cache_write_samples,cache_write_tokens,subsecond_requests)
SELECT bucket,sum(requests),sum(ok),sum(errors),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens),sum(cache_write_samples),sum(cache_write_tokens),sum(subsecond_requests)
FROM (SELECT floor(extract(epoch FROM l.created_at)/60)::bigint*60 AS bucket,
 1::numeric AS requests,1*(l.status='ok')::int AS ok,1*(l.status<>'ok')::int AS errors,
 1*c.cache_read_samples AS cache_read_samples,1*c.cache_read_unknown_requests AS cache_read_unknown_requests,1*c.cache_read_input_tokens AS cache_read_input_tokens,1*c.cache_read_tokens AS cache_read_tokens,1*c.cache_write_samples AS cache_write_samples,1*c.cache_write_tokens AS cache_write_tokens,
 1*(l.created_at<>date_trunc('second',l.created_at))::int AS subsecond_requests
FROM request_logs l CROSS JOIN LATERAL retained_log_cache_observation(l.record) c) changes GROUP BY bucket ORDER BY bucket`); err != nil {
		return err
	}
	classified := "SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM request_logs"
	if _, err = tx.Exec(ctx, `WITH classified AS MATERIALIZED (`+classified+`)
INSERT INTO retained_usage_unsafe_minute SELECT bucket,count(*) FROM classified WHERE NOT safe GROUP BY bucket`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `WITH classified AS MATERIALIZED (`+classified+`),counted AS MATERIALIZED (
 SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,c.* FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status) c WHERE l.safe)
INSERT INTO retained_usage_minute(`+retainedUsageInsertColumns("bucket", "status", "kind", "resource_id", "name")+`)
SELECT bucket,status,d.kind,d.resource_id,d.name,`+retainedUsageSumColumns()+`
FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name) d
GROUP BY bucket,status,d.kind,d.resource_id,d.name`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO retained_usage_daily(`+retainedUsageInsertColumns("bucket", "status", "kind", "resource_id", "name")+`)
SELECT (bucket/86400)*86400,status,kind,resource_id,name,`+retainedUsageSumColumns()+`
FROM retained_usage_minute WHERE kind<>'total' GROUP BY 1,2,3,4,5`); err != nil {
		return err
	}
	// Model marginals copy the one-model-per-minute fast path and reconstruct
	// only genuinely mixed minutes from details (migration 017's backfill).
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE retained_model_backfill_buckets ON COMMIT DROP AS
SELECT bucket,status,count(*) AS models,min(name COLLATE "C") AS sole_model,bool_or(octet_length(name)<=512) AS has_short
FROM retained_usage_minute WHERE kind='model' AND requests>0 GROUP BY bucket,status`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE UNIQUE INDEX ON retained_model_backfill_buckets(bucket,status)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO retained_usage_model_minute(`+retainedUsageInsertColumns("selected_model", "bucket", "status", "kind", "resource_id", "name")+`)
SELECT b.sole_model,m.bucket,m.status,m.kind,m.resource_id,m.name,`+managedUsageRollupColumns+`
FROM retained_usage_minute m JOIN retained_model_backfill_buckets b USING(bucket,status)
WHERE b.models=1 AND b.has_short AND m.requests>0`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO retained_usage_model_unsafe_minute(bucket,requests)
SELECT bucket,sum(requests) FROM retained_usage_minute
WHERE kind='model' AND requests>0 AND octet_length(name)>512 GROUP BY bucket`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `WITH candidates AS MATERIALIZED (
 SELECT r.* FROM retained_model_backfill_buckets b CROSS JOIN LATERAL (
 SELECT created_at,status,model,record,account_name,key_name FROM request_logs
 WHERE created_at>=to_timestamp(b.bucket) AND created_at<to_timestamp(b.bucket+60)
 AND status=b.status AND octet_length(model)<=512 OFFSET 0
 ) r WHERE b.models>1 AND b.has_short), counted AS MATERIALIZED (
 SELECT floor(extract(epoch FROM r.created_at)/60)::bigint*60 AS bucket,r.status,r.model,r.record,r.account_name,r.key_name,c.*
 FROM candidates r CROSS JOIN LATERAL retained_usage_counters(r.record,r.status) c
 WHERE retained_usage_safe(r.created_at,r.status,r.record,r.model,r.account_name,r.key_name))
INSERT INTO retained_usage_model_minute(`+retainedUsageInsertColumns("selected_model", "bucket", "status", "kind", "resource_id", "name")+`)
SELECT model,bucket,status,d.kind,d.resource_id,d.name,`+retainedUsageSumColumns()+`
FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name) d
GROUP BY model,bucket,status,d.kind,d.resource_id,d.name`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO retained_usage_model_daily(`+retainedUsageInsertColumns("selected_model", "bucket", "status", "kind", "resource_id", "name")+`)
SELECT selected_model,(bucket/86400)*86400,status,kind,resource_id,name,`+retainedUsageSumColumns()+`
FROM retained_usage_model_minute WHERE kind<>'total' GROUP BY 1,2,3,4,5,6`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `WITH contributions AS MATERIALIZED (
 SELECT floor(extract(epoch FROM created_at))::bigint AS stamp,c.* FROM request_logs CROSS JOIN LATERAL retained_operations_values(status,record) c
) INSERT INTO retained_operations(resolution,bucket,category,requests,success,errors,aborted,tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,latency_0,latency_1,latency_2,latency_3,latency_4,latency_5,latency_6)
SELECT resolution,(floor(stamp::numeric/resolution)*resolution)::bigint AS bucket,category,sum(requests),sum(success),sum(errors),sum(aborted),sum(tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(latency_0),sum(latency_1),sum(latency_2),sum(latency_3),sum(latency_4),sum(latency_5),sum(latency_6)
FROM contributions CROSS JOIN (VALUES(1),(60)) resolutions(resolution) GROUP BY resolution,bucket,category ORDER BY resolution,bucket,category COLLATE "C"`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
