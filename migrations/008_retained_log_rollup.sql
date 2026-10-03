-- Retained request details have independent lifetimes from durable usage_events.
-- Numeric intermediate sums preserve valid extreme observations until readers
-- perform the same final bigint bounds check as the original detail aggregate.
CREATE TABLE retained_log_minute (
    bucket bigint PRIMARY KEY,
    requests numeric NOT NULL,
    ok numeric NOT NULL,
    errors numeric NOT NULL,
    cache_read_samples numeric NOT NULL,
    cache_read_unknown_requests numeric NOT NULL,
    cache_read_input_tokens numeric NOT NULL,
    cache_read_tokens numeric NOT NULL,
    cache_write_samples numeric NOT NULL,
    cache_write_tokens numeric NOT NULL,
    subsecond_requests numeric NOT NULL
);

-- Block DML across the backfill/trigger boundary while allowing ordinary reads.
LOCK TABLE request_logs IN SHARE ROW EXCLUSIVE MODE;

CREATE FUNCTION retained_log_cache_observation(record jsonb)
RETURNS TABLE(cache_read_samples numeric,cache_read_unknown_requests numeric,cache_read_input_tokens numeric,cache_read_tokens numeric,cache_write_samples numeric,cache_write_tokens numeric)
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
WITH eligible AS (
    SELECT NOT COALESCE((record->>'websocket_warmup')::boolean,false)
       AND COALESCE(record->>'endpoint','') IN ('','responses','responses/compact','compact','chat/completions','codex/tasks','guardian','guardian-classifier','alpha/search','images/generations','images/edits','memories/trace_summarize','realtime/calls','realtime','live','/','/responses','/responses/compact','/compact','/chat/completions','/codex/tasks','/guardian','/guardian-classifier','/alpha/search','/images/generations','/images/edits','/memories/trace_summarize','/realtime/calls','/realtime','/live','v1/','v1/responses','v1/responses/compact','v1/compact','v1/chat/completions','v1/codex/tasks','v1/guardian','v1/guardian-classifier','v1/alpha/search','v1/images/generations','v1/images/edits','v1/memories/trace_summarize','v1/realtime/calls','v1/realtime','v1/live','/v1/','/v1/responses','/v1/responses/compact','/v1/compact','/v1/chat/completions','/v1/codex/tasks','/v1/guardian','/v1/guardian-classifier','/v1/alpha/search','/v1/images/generations','/v1/images/edits','/v1/memories/trace_summarize','/v1/realtime/calls','/v1/realtime','/v1/live','backend-api/codex/','backend-api/codex/responses','backend-api/codex/responses/compact','backend-api/codex/compact','backend-api/codex/chat/completions','backend-api/codex/codex/tasks','backend-api/codex/guardian','backend-api/codex/guardian-classifier','backend-api/codex/alpha/search','backend-api/codex/images/generations','backend-api/codex/images/edits','backend-api/codex/memories/trace_summarize','backend-api/codex/realtime/calls','backend-api/codex/realtime','backend-api/codex/live','/backend-api/codex/','/backend-api/codex/responses','/backend-api/codex/responses/compact','/backend-api/codex/compact','/backend-api/codex/chat/completions','/backend-api/codex/codex/tasks','/backend-api/codex/guardian','/backend-api/codex/guardian-classifier','/backend-api/codex/alpha/search','/backend-api/codex/images/generations','/backend-api/codex/images/edits','/backend-api/codex/memories/trace_summarize','/backend-api/codex/realtime/calls','/backend-api/codex/realtime','/backend-api/codex/live') AS value
), tokens AS (
    SELECT value,
       CASE WHEN value THEN COALESCE((record->>'input_tokens')::bigint,0) ELSE 0 END AS input,
       CASE WHEN value THEN COALESCE((record->>'output_tokens')::bigint,0) ELSE 0 END AS output,
       CASE WHEN value THEN COALESCE((record->>'cached_tokens')::bigint,0) ELSE 0 END AS cached,
       CASE WHEN value THEN (record->>'cache_write_tokens')::bigint END AS cache_write
    FROM eligible
), observed AS (
    SELECT *,value AND
       COALESCE((record->>'usage_known')::boolean,input<>0 OR output<>0 OR cached<>0)
       AND COALESCE((record->>'cached_tokens_known')::boolean,cached>0)
       AND cached>=0 AND input>=cached AS read_observed
    FROM tokens
)
SELECT (value AND read_observed)::int::numeric,
       (value AND NOT read_observed)::int::numeric,
       CASE WHEN value AND read_observed THEN input::numeric ELSE 0 END,
       CASE WHEN value AND read_observed THEN cached::numeric ELSE 0 END,
       CASE WHEN value AND cache_write>=0 THEN 1::numeric ELSE 0 END,
       CASE WHEN value AND cache_write>=0 THEN cache_write::numeric ELSE 0 END
FROM observed
$$;

INSERT INTO retained_log_minute(bucket,requests,ok,errors,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens,cache_write_samples,cache_write_tokens,subsecond_requests)
SELECT bucket,sum(requests),sum(ok),sum(errors),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens),sum(cache_write_samples),sum(cache_write_tokens),sum(subsecond_requests)
    FROM (SELECT floor(extract(epoch FROM l.created_at)/60)::bigint*60 AS bucket,
        1::numeric AS requests,1*(l.status='ok')::int AS ok,1*(l.status<>'ok')::int AS errors,
        1*c.cache_read_samples AS cache_read_samples,1*c.cache_read_unknown_requests AS cache_read_unknown_requests,1*c.cache_read_input_tokens AS cache_read_input_tokens,1*c.cache_read_tokens AS cache_read_tokens,1*c.cache_write_samples AS cache_write_samples,1*c.cache_write_tokens AS cache_write_tokens,
        1*(l.created_at<>date_trunc('second',l.created_at))::int AS subsecond_requests
    FROM request_logs l CROSS JOIN LATERAL retained_log_cache_observation(l.record) c) changes GROUP BY bucket ORDER BY bucket;

-- Transition tables reduce bulk inserts, cleanup and repairs to one upsert per
-- affected minute. Deterministic bucket order prevents opposing move deadlocks.

CREATE FUNCTION maintain_retained_log_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO retained_log_minute AS summary(bucket,requests,ok,errors,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens,cache_write_samples,cache_write_tokens,subsecond_requests)
    SELECT bucket,sum(requests),sum(ok),sum(errors),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens),sum(cache_write_samples),sum(cache_write_tokens),sum(subsecond_requests)
    FROM (SELECT floor(extract(epoch FROM l.created_at)/60)::bigint*60 AS bucket,
        1::numeric AS requests,1*(l.status='ok')::int AS ok,1*(l.status<>'ok')::int AS errors,
        1*c.cache_read_samples AS cache_read_samples,1*c.cache_read_unknown_requests AS cache_read_unknown_requests,1*c.cache_read_input_tokens AS cache_read_input_tokens,1*c.cache_read_tokens AS cache_read_tokens,1*c.cache_write_samples AS cache_write_samples,1*c.cache_write_tokens AS cache_write_tokens,
        1*(l.created_at<>date_trunc('second',l.created_at))::int AS subsecond_requests
    FROM retained_new_rows l CROSS JOIN LATERAL retained_log_cache_observation(l.record) c) changes GROUP BY bucket ORDER BY bucket
    ON CONFLICT(bucket) DO UPDATE SET
        requests=summary.requests+EXCLUDED.requests,
        ok=summary.ok+EXCLUDED.ok,
        errors=summary.errors+EXCLUDED.errors,
        cache_read_samples=summary.cache_read_samples+EXCLUDED.cache_read_samples,
        cache_read_unknown_requests=summary.cache_read_unknown_requests+EXCLUDED.cache_read_unknown_requests,
        cache_read_input_tokens=summary.cache_read_input_tokens+EXCLUDED.cache_read_input_tokens,
        cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens,
        cache_write_samples=summary.cache_write_samples+EXCLUDED.cache_write_samples,
        cache_write_tokens=summary.cache_write_tokens+EXCLUDED.cache_write_tokens,
        subsecond_requests=summary.subsecond_requests+EXCLUDED.subsecond_requests;
    RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_retained_insert AFTER INSERT ON request_logs
    REFERENCING  NEW TABLE AS retained_new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_log_insert();

CREATE FUNCTION maintain_retained_log_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO retained_log_minute AS summary(bucket,requests,ok,errors,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens,cache_write_samples,cache_write_tokens,subsecond_requests)
    SELECT bucket,sum(requests),sum(ok),sum(errors),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens),sum(cache_write_samples),sum(cache_write_tokens),sum(subsecond_requests)
    FROM (SELECT floor(extract(epoch FROM l.created_at)/60)::bigint*60 AS bucket,
        1::numeric AS requests,1*(l.status='ok')::int AS ok,1*(l.status<>'ok')::int AS errors,
        1*c.cache_read_samples AS cache_read_samples,1*c.cache_read_unknown_requests AS cache_read_unknown_requests,1*c.cache_read_input_tokens AS cache_read_input_tokens,1*c.cache_read_tokens AS cache_read_tokens,1*c.cache_write_samples AS cache_write_samples,1*c.cache_write_tokens AS cache_write_tokens,
        1*(l.created_at<>date_trunc('second',l.created_at))::int AS subsecond_requests
    FROM retained_new_rows l CROSS JOIN LATERAL retained_log_cache_observation(l.record) c
    UNION ALL
    SELECT floor(extract(epoch FROM l.created_at)/60)::bigint*60 AS bucket,
        -1::numeric AS requests,-1*(l.status='ok')::int AS ok,-1*(l.status<>'ok')::int AS errors,
        -1*c.cache_read_samples AS cache_read_samples,-1*c.cache_read_unknown_requests AS cache_read_unknown_requests,-1*c.cache_read_input_tokens AS cache_read_input_tokens,-1*c.cache_read_tokens AS cache_read_tokens,-1*c.cache_write_samples AS cache_write_samples,-1*c.cache_write_tokens AS cache_write_tokens,
        -1*(l.created_at<>date_trunc('second',l.created_at))::int AS subsecond_requests
    FROM retained_old_rows l CROSS JOIN LATERAL retained_log_cache_observation(l.record) c) changes GROUP BY bucket ORDER BY bucket
    ON CONFLICT(bucket) DO UPDATE SET
        requests=summary.requests+EXCLUDED.requests,
        ok=summary.ok+EXCLUDED.ok,
        errors=summary.errors+EXCLUDED.errors,
        cache_read_samples=summary.cache_read_samples+EXCLUDED.cache_read_samples,
        cache_read_unknown_requests=summary.cache_read_unknown_requests+EXCLUDED.cache_read_unknown_requests,
        cache_read_input_tokens=summary.cache_read_input_tokens+EXCLUDED.cache_read_input_tokens,
        cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens,
        cache_write_samples=summary.cache_write_samples+EXCLUDED.cache_write_samples,
        cache_write_tokens=summary.cache_write_tokens+EXCLUDED.cache_write_tokens,
        subsecond_requests=summary.subsecond_requests+EXCLUDED.subsecond_requests;
    RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_retained_update AFTER UPDATE ON request_logs
    REFERENCING OLD TABLE AS retained_old_rows NEW TABLE AS retained_new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_log_update();

CREATE FUNCTION maintain_retained_log_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO retained_log_minute AS summary(bucket,requests,ok,errors,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens,cache_write_samples,cache_write_tokens,subsecond_requests)
    SELECT bucket,sum(requests),sum(ok),sum(errors),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens),sum(cache_write_samples),sum(cache_write_tokens),sum(subsecond_requests)
    FROM (SELECT floor(extract(epoch FROM l.created_at)/60)::bigint*60 AS bucket,
        -1::numeric AS requests,-1*(l.status='ok')::int AS ok,-1*(l.status<>'ok')::int AS errors,
        -1*c.cache_read_samples AS cache_read_samples,-1*c.cache_read_unknown_requests AS cache_read_unknown_requests,-1*c.cache_read_input_tokens AS cache_read_input_tokens,-1*c.cache_read_tokens AS cache_read_tokens,-1*c.cache_write_samples AS cache_write_samples,-1*c.cache_write_tokens AS cache_write_tokens,
        -1*(l.created_at<>date_trunc('second',l.created_at))::int AS subsecond_requests
    FROM retained_old_rows l CROSS JOIN LATERAL retained_log_cache_observation(l.record) c) changes GROUP BY bucket ORDER BY bucket
    ON CONFLICT(bucket) DO UPDATE SET
        requests=summary.requests+EXCLUDED.requests,
        ok=summary.ok+EXCLUDED.ok,
        errors=summary.errors+EXCLUDED.errors,
        cache_read_samples=summary.cache_read_samples+EXCLUDED.cache_read_samples,
        cache_read_unknown_requests=summary.cache_read_unknown_requests+EXCLUDED.cache_read_unknown_requests,
        cache_read_input_tokens=summary.cache_read_input_tokens+EXCLUDED.cache_read_input_tokens,
        cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens,
        cache_write_samples=summary.cache_write_samples+EXCLUDED.cache_write_samples,
        cache_write_tokens=summary.cache_write_tokens+EXCLUDED.cache_write_tokens,
        subsecond_requests=summary.subsecond_requests+EXCLUDED.subsecond_requests;
    RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_retained_delete AFTER DELETE ON request_logs
    REFERENCING OLD TABLE AS retained_old_rows 
    FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_log_delete();

CREATE FUNCTION truncate_retained_log_minute() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    TRUNCATE retained_log_minute;
    RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_retained_truncate AFTER TRUNCATE ON request_logs
    FOR EACH STATEMENT EXECUTE FUNCTION truncate_retained_log_minute();

