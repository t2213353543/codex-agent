-- Operations queries have second-aligned windows and 15/60-second chart
-- slots. Minute totals alone cannot answer offset slots without rereading
-- every detail. Store seconds plus minutes and use disjoint range pieces.
-- Retained details, summaries and cleanup always commit in one transaction.
CREATE TABLE retained_operations (
 resolution integer NOT NULL CHECK(resolution IN (1,60)),
 bucket bigint NOT NULL,
 category text COLLATE "C" NOT NULL,

 requests numeric NOT NULL,
 success numeric NOT NULL,
 errors numeric NOT NULL,
 aborted numeric NOT NULL,
 tokens numeric NOT NULL,
 usage_known numeric NOT NULL,
 latency_samples numeric NOT NULL,
 latency_sum numeric NOT NULL,
 ttft_samples numeric NOT NULL,
 ttft_sum numeric NOT NULL,
 latency_0 numeric NOT NULL,
 latency_1 numeric NOT NULL,
 latency_2 numeric NOT NULL,
 latency_3 numeric NOT NULL,
 latency_4 numeric NOT NULL,
 latency_5 numeric NOT NULL,
 latency_6 numeric NOT NULL,

 PRIMARY KEY(resolution,bucket,category)
);

CREATE FUNCTION retained_operations_values(status text,record jsonb)
RETURNS TABLE(category text,requests numeric,success numeric,errors numeric,aborted numeric,tokens numeric,usage_known numeric,latency_samples numeric,latency_sum numeric,ttft_samples numeric,ttft_sum numeric,latency_0 numeric,latency_1 numeric,latency_2 numeric,latency_3 numeric,latency_4 numeric,latency_5 numeric,latency_6 numeric)
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$

SELECT (CASE WHEN status='ok' THEN ''
 WHEN status='error' AND COALESCE(record->>'error_source','') IN ('','upstream') AND record->>'final_upstream_error_source'='upstream' AND record->>'final_upstream_error_code' IN ('rateLimitExceeded','usageLimitExceeded','rate_limit_exceeded','rate_limit_error','insufficient_quota','billing_hard_limit_reached') THEN 'rate_limit'
 WHEN status='error' AND COALESCE(record->>'error_source','') IN ('','upstream') AND record->>'final_upstream_error_source'='upstream' AND record->>'final_upstream_error_code' IN ('unauthorized','authentication_error','invalid_api_key','authentication_unavailable','upstream_account_rejected') THEN 'authentication'
 WHEN status='error' AND COALESCE(record->>'error_source','') IN ('','upstream') AND record->>'final_upstream_error_source'='upstream' AND record->>'final_upstream_error_code' IN ('serverOverloaded','server_overloaded','model_overloaded','overloaded_error') THEN 'overloaded'
 WHEN position('unsupported parameter' in lower(COALESCE(record->>'error','')))>0 THEN 'unsupported_parameter'
 WHEN position('system messages are not allowed' in lower(COALESCE(record->>'error','')))>0 THEN 'system_role'
 WHEN position('no enabled account' in lower(COALESCE(record->>'error','')))>0 THEN 'no_enabled_account'
 WHEN position('invalidated oauth' in lower(COALESCE(record->>'error','')))>0 OR position('refresh_token_reused' in lower(COALESCE(record->>'error','')))>0 THEN 'invalid_oauth'
 WHEN (position('model' in lower(COALESCE(record->>'error','')))>0 AND position('not supported' in lower(COALESCE(record->>'error','')))>0) OR position('model_not_found' in lower(COALESCE(record->>'error','')))>0 THEN 'unsupported_model'
 WHEN position('encrypted content' in lower(COALESCE(record->>'error','')))>0 OR position('invalid_encrypted_content' in lower(COALESCE(record->>'error','')))>0 THEN 'encrypted_history'
 WHEN record->>'http_code'='413' OR position('exceeds 32 mib' in lower(COALESCE(record->>'error','')))>0 THEN 'request_too_large'
 WHEN position('flagged for possible' in lower(COALESCE(record->>'error','')))>0 OR position('content_filter' in lower(COALESCE(record->>'error','')))>0 OR position('policy violation' in lower(COALESCE(record->>'error','')))>0 THEN 'upstream_refusal'
 WHEN record->>'http_code'='429' OR position('rate limit' in lower(COALESCE(record->>'error','')))>0 OR position('rate_limit' in lower(COALESCE(record->>'error','')))>0 THEN 'rate_limit'
 WHEN position('overloaded' in lower(COALESCE(record->>'error','')))>0 THEN 'overloaded'
 WHEN position('an error occurred while processing' in lower(COALESCE(record->>'error','')))>0 THEN 'upstream_error'
 WHEN status='aborted' OR record->>'http_code'='499' OR position('context canceled' in lower(COALESCE(record->>'error','')))>0 THEN 'aborted'
 WHEN record->>'http_code'='504' OR position('timeout' in lower(COALESCE(record->>'error','')))>0 OR position('timed out' in lower(COALESCE(record->>'error','')))>0 OR position('deadline exceeded' in lower(COALESCE(record->>'error','')))>0 THEN 'timeout'
 WHEN record->>'http_code' IN ('401','403') THEN 'authentication'
 WHEN COALESCE((record->>'http_code')::integer,0)>=500 THEN 'server_error' ELSE 'unknown' END) AS category,
 1::numeric,
 (status='ok')::int::numeric,
 (status<>'ok')::int::numeric,
 (status='aborted')::int::numeric,
 COALESCE((record->>'input_tokens')::bigint,0)::numeric+COALESCE((record->>'output_tokens')::bigint,0)::numeric,
 COALESCE((record->>'usage_known')::boolean,COALESCE((record->>'input_tokens')::bigint,0)<>0 OR COALESCE((record->>'output_tokens')::bigint,0)<>0 OR COALESCE((record->>'cached_tokens')::bigint,0)<>0)::int::numeric,
 CASE WHEN latency IS NOT NULL THEN 1::numeric ELSE 0 END,
 COALESCE(latency,0)::numeric,
 CASE WHEN ttft IS NOT NULL THEN 1::numeric ELSE 0 END,
 COALESCE(ttft,0)::numeric,
 CASE WHEN latency<=1000 THEN 1::numeric ELSE 0 END,
 CASE WHEN latency>1000 AND latency<=5000 THEN 1::numeric ELSE 0 END,
 CASE WHEN latency>5000 AND latency<=15000 THEN 1::numeric ELSE 0 END,
 CASE WHEN latency>15000 AND latency<=30000 THEN 1::numeric ELSE 0 END,
 CASE WHEN latency>30000 AND latency<=60000 THEN 1::numeric ELSE 0 END,
 CASE WHEN latency>60000 AND latency<=180000 THEN 1::numeric ELSE 0 END,
 CASE WHEN latency>180000 THEN 1::numeric ELSE 0 END
FROM (SELECT
 CASE WHEN COALESCE((record->>'latency_known')::boolean,COALESCE((record->>'latency_ms')::bigint,0)>0) AND COALESCE((record->>'latency_ms')::bigint,0)>=0 THEN COALESCE((record->>'latency_ms')::bigint,0) END AS latency,
 CASE WHEN (record->>'ttft_ms')::bigint>=0 THEN (record->>'ttft_ms')::bigint END AS ttft
) observations
$$;

LOCK TABLE request_logs IN SHARE ROW EXCLUSIVE MODE;

CREATE FUNCTION maintain_retained_operations_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN

WITH changes AS MATERIALIZED (SELECT created_at,status,record,1::numeric AS sign FROM operations_new), contributions AS MATERIALIZED (
 SELECT floor(extract(epoch FROM created_at))::bigint AS stamp,sign,c.* FROM changes CROSS JOIN LATERAL retained_operations_values(status,record)c
) INSERT INTO retained_operations AS summary(resolution,bucket,category,requests,success,errors,aborted,tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,latency_0,latency_1,latency_2,latency_3,latency_4,latency_5,latency_6)
SELECT resolution,(floor(stamp::numeric/resolution)*resolution)::bigint AS bucket,category,sum(sign*requests),sum(sign*success),sum(sign*errors),sum(sign*aborted),sum(sign*tokens),sum(sign*usage_known),sum(sign*latency_samples),sum(sign*latency_sum),sum(sign*ttft_samples),sum(sign*ttft_sum),sum(sign*latency_0),sum(sign*latency_1),sum(sign*latency_2),sum(sign*latency_3),sum(sign*latency_4),sum(sign*latency_5),sum(sign*latency_6) FROM contributions CROSS JOIN (VALUES(1),(60)) resolutions(resolution) GROUP BY resolution,bucket,category ORDER BY resolution,bucket,category COLLATE "C"
 ON CONFLICT(resolution,bucket,category) DO UPDATE SET
 requests=summary.requests+EXCLUDED.requests,
 success=summary.success+EXCLUDED.success,
 errors=summary.errors+EXCLUDED.errors,
 aborted=summary.aborted+EXCLUDED.aborted,
 tokens=summary.tokens+EXCLUDED.tokens,
 usage_known=summary.usage_known+EXCLUDED.usage_known,
 latency_samples=summary.latency_samples+EXCLUDED.latency_samples,
 latency_sum=summary.latency_sum+EXCLUDED.latency_sum,
 ttft_samples=summary.ttft_samples+EXCLUDED.ttft_samples,
 ttft_sum=summary.ttft_sum+EXCLUDED.ttft_sum,
 latency_0=summary.latency_0+EXCLUDED.latency_0,
 latency_1=summary.latency_1+EXCLUDED.latency_1,
 latency_2=summary.latency_2+EXCLUDED.latency_2,
 latency_3=summary.latency_3+EXCLUDED.latency_3,
 latency_4=summary.latency_4+EXCLUDED.latency_4,
 latency_5=summary.latency_5+EXCLUDED.latency_5,
 latency_6=summary.latency_6+EXCLUDED.latency_6;

 RETURN NULL;
END
$$;

CREATE TRIGGER request_logs_operations_insert AFTER INSERT ON request_logs
 REFERENCING NEW TABLE AS operations_new 
 FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_operations_insert();

CREATE FUNCTION maintain_retained_operations_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN

WITH changes AS MATERIALIZED (SELECT created_at,status,record,1::numeric AS sign FROM operations_new UNION ALL SELECT created_at,status,record,-1::numeric AS sign FROM operations_old), contributions AS MATERIALIZED (
 SELECT floor(extract(epoch FROM created_at))::bigint AS stamp,sign,c.* FROM changes CROSS JOIN LATERAL retained_operations_values(status,record)c
) INSERT INTO retained_operations AS summary(resolution,bucket,category,requests,success,errors,aborted,tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,latency_0,latency_1,latency_2,latency_3,latency_4,latency_5,latency_6)
SELECT resolution,(floor(stamp::numeric/resolution)*resolution)::bigint AS bucket,category,sum(sign*requests),sum(sign*success),sum(sign*errors),sum(sign*aborted),sum(sign*tokens),sum(sign*usage_known),sum(sign*latency_samples),sum(sign*latency_sum),sum(sign*ttft_samples),sum(sign*ttft_sum),sum(sign*latency_0),sum(sign*latency_1),sum(sign*latency_2),sum(sign*latency_3),sum(sign*latency_4),sum(sign*latency_5),sum(sign*latency_6) FROM contributions CROSS JOIN (VALUES(1),(60)) resolutions(resolution) GROUP BY resolution,bucket,category ORDER BY resolution,bucket,category COLLATE "C"
 ON CONFLICT(resolution,bucket,category) DO UPDATE SET
 requests=summary.requests+EXCLUDED.requests,
 success=summary.success+EXCLUDED.success,
 errors=summary.errors+EXCLUDED.errors,
 aborted=summary.aborted+EXCLUDED.aborted,
 tokens=summary.tokens+EXCLUDED.tokens,
 usage_known=summary.usage_known+EXCLUDED.usage_known,
 latency_samples=summary.latency_samples+EXCLUDED.latency_samples,
 latency_sum=summary.latency_sum+EXCLUDED.latency_sum,
 ttft_samples=summary.ttft_samples+EXCLUDED.ttft_samples,
 ttft_sum=summary.ttft_sum+EXCLUDED.ttft_sum,
 latency_0=summary.latency_0+EXCLUDED.latency_0,
 latency_1=summary.latency_1+EXCLUDED.latency_1,
 latency_2=summary.latency_2+EXCLUDED.latency_2,
 latency_3=summary.latency_3+EXCLUDED.latency_3,
 latency_4=summary.latency_4+EXCLUDED.latency_4,
 latency_5=summary.latency_5+EXCLUDED.latency_5,
 latency_6=summary.latency_6+EXCLUDED.latency_6;

 RETURN NULL;
END
$$;

CREATE TRIGGER request_logs_operations_update AFTER UPDATE ON request_logs
 REFERENCING OLD TABLE AS operations_old NEW TABLE AS operations_new 
 FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_operations_update();

CREATE FUNCTION maintain_retained_operations_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN

WITH changes AS MATERIALIZED (SELECT created_at,status,record,-1::numeric AS sign FROM operations_old), contributions AS MATERIALIZED (
 SELECT floor(extract(epoch FROM created_at))::bigint AS stamp,sign,c.* FROM changes CROSS JOIN LATERAL retained_operations_values(status,record)c
) INSERT INTO retained_operations AS summary(resolution,bucket,category,requests,success,errors,aborted,tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,latency_0,latency_1,latency_2,latency_3,latency_4,latency_5,latency_6)
SELECT resolution,(floor(stamp::numeric/resolution)*resolution)::bigint AS bucket,category,sum(sign*requests),sum(sign*success),sum(sign*errors),sum(sign*aborted),sum(sign*tokens),sum(sign*usage_known),sum(sign*latency_samples),sum(sign*latency_sum),sum(sign*ttft_samples),sum(sign*ttft_sum),sum(sign*latency_0),sum(sign*latency_1),sum(sign*latency_2),sum(sign*latency_3),sum(sign*latency_4),sum(sign*latency_5),sum(sign*latency_6) FROM contributions CROSS JOIN (VALUES(1),(60)) resolutions(resolution) GROUP BY resolution,bucket,category ORDER BY resolution,bucket,category COLLATE "C"
 ON CONFLICT(resolution,bucket,category) DO UPDATE SET
 requests=summary.requests+EXCLUDED.requests,
 success=summary.success+EXCLUDED.success,
 errors=summary.errors+EXCLUDED.errors,
 aborted=summary.aborted+EXCLUDED.aborted,
 tokens=summary.tokens+EXCLUDED.tokens,
 usage_known=summary.usage_known+EXCLUDED.usage_known,
 latency_samples=summary.latency_samples+EXCLUDED.latency_samples,
 latency_sum=summary.latency_sum+EXCLUDED.latency_sum,
 ttft_samples=summary.ttft_samples+EXCLUDED.ttft_samples,
 ttft_sum=summary.ttft_sum+EXCLUDED.ttft_sum,
 latency_0=summary.latency_0+EXCLUDED.latency_0,
 latency_1=summary.latency_1+EXCLUDED.latency_1,
 latency_2=summary.latency_2+EXCLUDED.latency_2,
 latency_3=summary.latency_3+EXCLUDED.latency_3,
 latency_4=summary.latency_4+EXCLUDED.latency_4,
 latency_5=summary.latency_5+EXCLUDED.latency_5,
 latency_6=summary.latency_6+EXCLUDED.latency_6;

 RETURN NULL;
END
$$;

CREATE TRIGGER request_logs_operations_delete AFTER DELETE ON request_logs
 REFERENCING OLD TABLE AS operations_old 
 FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_operations_delete();

CREATE FUNCTION truncate_retained_operations() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 TRUNCATE retained_operations;
 RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_operations_truncate AFTER TRUNCATE ON request_logs
 FOR EACH STATEMENT EXECUTE FUNCTION truncate_retained_operations();

WITH contributions AS MATERIALIZED (
 SELECT floor(extract(epoch FROM created_at))::bigint AS stamp,c.* FROM request_logs CROSS JOIN LATERAL retained_operations_values(status,record)c
) INSERT INTO retained_operations(resolution,bucket,category,requests,success,errors,aborted,tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,latency_0,latency_1,latency_2,latency_3,latency_4,latency_5,latency_6)
SELECT resolution,(floor(stamp::numeric/resolution)*resolution)::bigint AS bucket,category,sum(requests),sum(success),sum(errors),sum(aborted),sum(tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(latency_0),sum(latency_1),sum(latency_2),sum(latency_3),sum(latency_4),sum(latency_5),sum(latency_6) FROM contributions CROSS JOIN (VALUES(1),(60)) resolutions(resolution) GROUP BY resolution,bucket,category ORDER BY resolution,bucket,category COLLATE "C";
