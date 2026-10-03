-- Exact retained-detail dimensional summaries; permanent usage_events have a
-- different deletion scope and must never supply these counters.
CREATE TABLE retained_usage_minute (
 bucket bigint NOT NULL,status text COLLATE "C" NOT NULL,kind text COLLATE "C" NOT NULL,
 resource_id text COLLATE "C" NOT NULL,name text COLLATE "C" NOT NULL,
 requests numeric NOT NULL,
 success numeric NOT NULL,
 errors numeric NOT NULL,
 aborted numeric NOT NULL,
 input_tokens numeric NOT NULL,
 output_tokens numeric NOT NULL,
 cached_tokens numeric NOT NULL,
 usage_known numeric NOT NULL,
 latency_samples numeric NOT NULL,
 latency_sum numeric NOT NULL,
 ttft_samples numeric NOT NULL,
 ttft_sum numeric NOT NULL,
 reasoning_samples numeric NOT NULL,
 reasoning_sum numeric NOT NULL,
 request_preparation_samples numeric NOT NULL,
 request_preparation_sum numeric NOT NULL,
 account_selection_samples numeric NOT NULL,
 account_selection_sum numeric NOT NULL,
 proxy_selection_samples numeric NOT NULL,
 proxy_selection_sum numeric NOT NULL,
 authentication_samples numeric NOT NULL,
 authentication_sum numeric NOT NULL,
 response_transfer_samples numeric NOT NULL,
 response_transfer_sum numeric NOT NULL,
 image_input_samples numeric NOT NULL,
 image_input_sum numeric NOT NULL,
 image_output_samples numeric NOT NULL,
 image_output_sum numeric NOT NULL,
 cache_write_samples numeric NOT NULL,
 cache_write_tokens numeric NOT NULL,
 cache_read_samples numeric NOT NULL,
 cache_read_unknown_requests numeric NOT NULL,
 cache_read_input_tokens numeric NOT NULL,
 cache_read_tokens numeric NOT NULL,
 PRIMARY KEY(bucket,status,kind,resource_id,name)
);
CREATE TABLE retained_usage_unsafe_minute(bucket bigint PRIMARY KEY,requests numeric NOT NULL);

CREATE FUNCTION retained_usage_dimensions(record jsonb,status text,model text,account_name text,key_name text)
RETURNS TABLE(kind text,resource_id text,name text) LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
WITH classified AS (SELECT CASE WHEN status='ok' THEN ''
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
 WHEN COALESCE((record->>'http_code')::integer,0)>=500 THEN 'server_error' ELSE 'unknown' END AS category)
SELECT d.* FROM classified CROSS JOIN LATERAL (
SELECT 'total'::text AS kind,''::text AS resource_id,''::text AS name
UNION ALL SELECT 'model','',model
UNION ALL SELECT 'endpoint','',COALESCE(record->>'endpoint','')
UNION ALL SELECT CASE WHEN COALESCE(record->>'account_id','')='' THEN 'account_legacy' ELSE 'account' END,COALESCE(NULLIF(record->>'account_id',''),'name:'||account_name),account_name
UNION ALL SELECT CASE WHEN COALESCE(record->>'key_id','')='' THEN 'key_legacy' ELSE 'key' END,COALESCE(NULLIF(record->>'key_id',''),'name:'||key_name),key_name
UNION ALL SELECT 'category','',category WHERE status<>'ok'
UNION ALL SELECT 'severity','',CASE WHEN COALESCE((record->>'http_code')::integer,0) NOT BETWEEN 100 AND 599 THEN 'unknown'
 WHEN category IN ('unsupported_parameter','system_role','invalid_oauth','unsupported_model','encrypted_history','authentication') THEN 'P3'
 WHEN category IN ('rate_limit','overloaded','upstream_error','server_error','timeout') THEN 'P1'
 WHEN (record->>'http_code')::integer>=500 OR (record->>'http_code')::integer=429 THEN 'P1'
 WHEN (record->>'http_code')::integer>=400 THEN 'P2' ELSE 'P3' END WHERE status<>'ok'
UNION ALL SELECT 'upstream_model','',record->>'upstream_model' WHERE COALESCE(record->>'upstream_model','')<>''
UNION ALL SELECT 'sent_model','',record->>'sent_model' WHERE COALESCE(record->>'sent_model','')<>''
UNION ALL SELECT 'returned_model','',record->>'returned_model' WHERE COALESCE(record->>'returned_model','')<>''
UNION ALL SELECT 'inbound_path','',record->>'inbound_path' WHERE COALESCE(record->>'inbound_path','')<>''
UNION ALL SELECT 'upstream_path','',record->>'upstream_path' WHERE COALESCE(record->>'upstream_path','')<>''
UNION ALL SELECT 'group',value,value FROM (SELECT DISTINCT value FROM jsonb_array_elements_text(COALESCE(record->'groups','[]'::jsonb)) WHERE value<>'') g
) d
$$;

CREATE FUNCTION retained_usage_counters(record jsonb,status text)
RETURNS TABLE(requests numeric,success numeric,errors numeric,aborted numeric,input_tokens numeric,output_tokens numeric,cached_tokens numeric,usage_known numeric,latency_samples numeric,latency_sum numeric,ttft_samples numeric,ttft_sum numeric,reasoning_samples numeric,reasoning_sum numeric,request_preparation_samples numeric,request_preparation_sum numeric,account_selection_samples numeric,account_selection_sum numeric,proxy_selection_samples numeric,proxy_selection_sum numeric,authentication_samples numeric,authentication_sum numeric,response_transfer_samples numeric,response_transfer_sum numeric,image_input_samples numeric,image_input_sum numeric,image_output_samples numeric,image_output_sum numeric,cache_write_samples numeric,cache_write_tokens numeric,cache_read_samples numeric,cache_read_unknown_requests numeric,cache_read_input_tokens numeric,cache_read_tokens numeric) LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
WITH observed AS MATERIALIZED (SELECT CASE WHEN COALESCE((record->>'latency_known')::boolean,COALESCE((record->>'latency_ms')::bigint,0)>0) AND COALESCE((record->>'latency_ms')::bigint,0)>=0 THEN COALESCE((record->>'latency_ms')::bigint,0) END AS latency,(record->>'ttft_ms')::bigint AS ttft,(record->>'reasoning_tokens')::bigint AS reasoning,(record->>'request_preparation_ms')::bigint AS request_preparation,(record->>'account_selection_ms')::bigint AS account_selection,(record->>'proxy_selection_ms')::bigint AS proxy_selection,(record->>'authentication_ms')::bigint AS authentication,(record->>'response_transfer_ms')::bigint AS response_transfer,(record->>'image_input_tokens')::bigint AS image_input,(record->>'image_output_tokens')::bigint AS image_output)
SELECT 1::numeric,
 (status='ok')::int::numeric,
 (status<>'ok')::int::numeric,
 (status='aborted')::int::numeric,
 COALESCE((record->>'input_tokens')::bigint,0)::numeric,
 COALESCE((record->>'output_tokens')::bigint,0)::numeric,
 COALESCE((record->>'cached_tokens')::bigint,0)::numeric,
 COALESCE((record->>'usage_known')::boolean,COALESCE((record->>'input_tokens')::bigint,0)<>0 OR COALESCE((record->>'output_tokens')::bigint,0)<>0 OR COALESCE((record->>'cached_tokens')::bigint,0)<>0)::int::numeric,
 CASE WHEN latency>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN latency>=0 THEN latency::numeric ELSE 0 END,
 CASE WHEN ttft>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN ttft>=0 THEN ttft::numeric ELSE 0 END,
 CASE WHEN reasoning>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN reasoning>=0 THEN reasoning::numeric ELSE 0 END,
 CASE WHEN request_preparation>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN request_preparation>=0 THEN request_preparation::numeric ELSE 0 END,
 CASE WHEN account_selection>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN account_selection>=0 THEN account_selection::numeric ELSE 0 END,
 CASE WHEN proxy_selection>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN proxy_selection>=0 THEN proxy_selection::numeric ELSE 0 END,
 CASE WHEN authentication>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN authentication>=0 THEN authentication::numeric ELSE 0 END,
 CASE WHEN response_transfer>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN response_transfer>=0 THEN response_transfer::numeric ELSE 0 END,
 CASE WHEN image_input>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN image_input>=0 THEN image_input::numeric ELSE 0 END,
 CASE WHEN image_output>=0 THEN 1::numeric ELSE 0 END,
 CASE WHEN image_output>=0 THEN image_output::numeric ELSE 0 END,
 c.cache_write_samples,
 c.cache_write_tokens,
 c.cache_read_samples,
 c.cache_read_unknown_requests,
 c.cache_read_input_tokens,
 c.cache_read_tokens
FROM observed CROSS JOIN LATERAL retained_log_cache_observation(record) c
$$;

-- A legal long observation or fractional timestamp never fails an index write.
-- Its whole minute falls back to exact retained records until that observation
-- is removed. 1800 encoded key bytes leave headroom below btree entry limits.
CREATE FUNCTION retained_usage_safe(created_at timestamptz,status text,record jsonb,model text,account_name text,key_name text)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT created_at>=to_timestamp(0) AND created_at=date_trunc('second',created_at)
 AND octet_length(status)<=64
 AND octet_length(model)<=1700
 AND octet_length(COALESCE(NULLIF(record->>'account_id',''),'name:'||account_name))+octet_length(account_name)<=1700
 AND octet_length(COALESCE(NULLIF(record->>'key_id',''),'name:'||key_name))+octet_length(key_name)<=1700
 AND octet_length(COALESCE(record->>'endpoint',''))<=1700
 AND octet_length(COALESCE(record->>'upstream_model',''))<=1700
 AND octet_length(COALESCE(record->>'sent_model',''))<=1700
 AND octet_length(COALESCE(record->>'returned_model',''))<=1700
 AND octet_length(COALESCE(record->>'inbound_path',''))<=1700
 AND octet_length(COALESCE(record->>'upstream_path',''))<=1700
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements_text(COALESCE(record->'groups','[]'::jsonb)) g(value) WHERE octet_length(value)>850)
$$;

CREATE FUNCTION maintain_retained_usage_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 WITH changes AS MATERIALIZED (SELECT l.*,1::numeric AS sign FROM retained_stats_new l),classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes)
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests;
 WITH changes AS MATERIALIZED (SELECT l.*,1::numeric AS sign FROM retained_stats_new l),classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes),counted AS MATERIALIZED (SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign,c.* FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status) c WHERE safe)
 INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
 SELECT bucket,status,d.kind,d.resource_id,d.name,sum(sign*requests),sum(sign*success),sum(sign*errors),sum(sign*aborted),sum(sign*input_tokens),sum(sign*output_tokens),sum(sign*cached_tokens),sum(sign*usage_known),sum(sign*latency_samples),sum(sign*latency_sum),sum(sign*ttft_samples),sum(sign*ttft_sum),sum(sign*reasoning_samples),sum(sign*reasoning_sum),sum(sign*request_preparation_samples),sum(sign*request_preparation_sum),sum(sign*account_selection_samples),sum(sign*account_selection_sum),sum(sign*proxy_selection_samples),sum(sign*proxy_selection_sum),sum(sign*authentication_samples),sum(sign*authentication_sum),sum(sign*response_transfer_samples),sum(sign*response_transfer_sum),sum(sign*image_input_samples),sum(sign*image_input_sum),sum(sign*image_output_samples),sum(sign*image_output_sum),sum(sign*cache_write_samples),sum(sign*cache_write_tokens),sum(sign*cache_read_samples),sum(sign*cache_read_unknown_requests),sum(sign*cache_read_input_tokens),sum(sign*cache_read_tokens)
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name) d
 GROUP BY bucket,status,d.kind,d.resource_id,d.name ORDER BY bucket,status COLLATE "C",d.kind COLLATE "C",d.resource_id COLLATE "C",d.name COLLATE "C"
 ON CONFLICT(bucket,status,kind,resource_id,name) DO UPDATE SET
 requests=summary.requests+EXCLUDED.requests,
 success=summary.success+EXCLUDED.success,
 errors=summary.errors+EXCLUDED.errors,
 aborted=summary.aborted+EXCLUDED.aborted,
 input_tokens=summary.input_tokens+EXCLUDED.input_tokens,
 output_tokens=summary.output_tokens+EXCLUDED.output_tokens,
 cached_tokens=summary.cached_tokens+EXCLUDED.cached_tokens,
 usage_known=summary.usage_known+EXCLUDED.usage_known,
 latency_samples=summary.latency_samples+EXCLUDED.latency_samples,
 latency_sum=summary.latency_sum+EXCLUDED.latency_sum,
 ttft_samples=summary.ttft_samples+EXCLUDED.ttft_samples,
 ttft_sum=summary.ttft_sum+EXCLUDED.ttft_sum,
 reasoning_samples=summary.reasoning_samples+EXCLUDED.reasoning_samples,
 reasoning_sum=summary.reasoning_sum+EXCLUDED.reasoning_sum,
 request_preparation_samples=summary.request_preparation_samples+EXCLUDED.request_preparation_samples,
 request_preparation_sum=summary.request_preparation_sum+EXCLUDED.request_preparation_sum,
 account_selection_samples=summary.account_selection_samples+EXCLUDED.account_selection_samples,
 account_selection_sum=summary.account_selection_sum+EXCLUDED.account_selection_sum,
 proxy_selection_samples=summary.proxy_selection_samples+EXCLUDED.proxy_selection_samples,
 proxy_selection_sum=summary.proxy_selection_sum+EXCLUDED.proxy_selection_sum,
 authentication_samples=summary.authentication_samples+EXCLUDED.authentication_samples,
 authentication_sum=summary.authentication_sum+EXCLUDED.authentication_sum,
 response_transfer_samples=summary.response_transfer_samples+EXCLUDED.response_transfer_samples,
 response_transfer_sum=summary.response_transfer_sum+EXCLUDED.response_transfer_sum,
 image_input_samples=summary.image_input_samples+EXCLUDED.image_input_samples,
 image_input_sum=summary.image_input_sum+EXCLUDED.image_input_sum,
 image_output_samples=summary.image_output_samples+EXCLUDED.image_output_samples,
 image_output_sum=summary.image_output_sum+EXCLUDED.image_output_sum,
 cache_write_samples=summary.cache_write_samples+EXCLUDED.cache_write_samples,
 cache_write_tokens=summary.cache_write_tokens+EXCLUDED.cache_write_tokens,
 cache_read_samples=summary.cache_read_samples+EXCLUDED.cache_read_samples,
 cache_read_unknown_requests=summary.cache_read_unknown_requests+EXCLUDED.cache_read_unknown_requests,
 cache_read_input_tokens=summary.cache_read_input_tokens+EXCLUDED.cache_read_input_tokens,
 cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens;
 RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_usage_insert AFTER INSERT ON request_logs
 REFERENCING NEW TABLE AS retained_stats_new 
 FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_usage_insert();

CREATE FUNCTION maintain_retained_usage_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 WITH changes AS MATERIALIZED (SELECT l.*,1::numeric AS sign FROM retained_stats_new l UNION ALL SELECT l.*,-1::numeric AS sign FROM retained_stats_old l),classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes)
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests;
 WITH changes AS MATERIALIZED (SELECT l.*,1::numeric AS sign FROM retained_stats_new l UNION ALL SELECT l.*,-1::numeric AS sign FROM retained_stats_old l),classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes),counted AS MATERIALIZED (SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign,c.* FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status) c WHERE safe)
 INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
 SELECT bucket,status,d.kind,d.resource_id,d.name,sum(sign*requests),sum(sign*success),sum(sign*errors),sum(sign*aborted),sum(sign*input_tokens),sum(sign*output_tokens),sum(sign*cached_tokens),sum(sign*usage_known),sum(sign*latency_samples),sum(sign*latency_sum),sum(sign*ttft_samples),sum(sign*ttft_sum),sum(sign*reasoning_samples),sum(sign*reasoning_sum),sum(sign*request_preparation_samples),sum(sign*request_preparation_sum),sum(sign*account_selection_samples),sum(sign*account_selection_sum),sum(sign*proxy_selection_samples),sum(sign*proxy_selection_sum),sum(sign*authentication_samples),sum(sign*authentication_sum),sum(sign*response_transfer_samples),sum(sign*response_transfer_sum),sum(sign*image_input_samples),sum(sign*image_input_sum),sum(sign*image_output_samples),sum(sign*image_output_sum),sum(sign*cache_write_samples),sum(sign*cache_write_tokens),sum(sign*cache_read_samples),sum(sign*cache_read_unknown_requests),sum(sign*cache_read_input_tokens),sum(sign*cache_read_tokens)
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name) d
 GROUP BY bucket,status,d.kind,d.resource_id,d.name ORDER BY bucket,status COLLATE "C",d.kind COLLATE "C",d.resource_id COLLATE "C",d.name COLLATE "C"
 ON CONFLICT(bucket,status,kind,resource_id,name) DO UPDATE SET
 requests=summary.requests+EXCLUDED.requests,
 success=summary.success+EXCLUDED.success,
 errors=summary.errors+EXCLUDED.errors,
 aborted=summary.aborted+EXCLUDED.aborted,
 input_tokens=summary.input_tokens+EXCLUDED.input_tokens,
 output_tokens=summary.output_tokens+EXCLUDED.output_tokens,
 cached_tokens=summary.cached_tokens+EXCLUDED.cached_tokens,
 usage_known=summary.usage_known+EXCLUDED.usage_known,
 latency_samples=summary.latency_samples+EXCLUDED.latency_samples,
 latency_sum=summary.latency_sum+EXCLUDED.latency_sum,
 ttft_samples=summary.ttft_samples+EXCLUDED.ttft_samples,
 ttft_sum=summary.ttft_sum+EXCLUDED.ttft_sum,
 reasoning_samples=summary.reasoning_samples+EXCLUDED.reasoning_samples,
 reasoning_sum=summary.reasoning_sum+EXCLUDED.reasoning_sum,
 request_preparation_samples=summary.request_preparation_samples+EXCLUDED.request_preparation_samples,
 request_preparation_sum=summary.request_preparation_sum+EXCLUDED.request_preparation_sum,
 account_selection_samples=summary.account_selection_samples+EXCLUDED.account_selection_samples,
 account_selection_sum=summary.account_selection_sum+EXCLUDED.account_selection_sum,
 proxy_selection_samples=summary.proxy_selection_samples+EXCLUDED.proxy_selection_samples,
 proxy_selection_sum=summary.proxy_selection_sum+EXCLUDED.proxy_selection_sum,
 authentication_samples=summary.authentication_samples+EXCLUDED.authentication_samples,
 authentication_sum=summary.authentication_sum+EXCLUDED.authentication_sum,
 response_transfer_samples=summary.response_transfer_samples+EXCLUDED.response_transfer_samples,
 response_transfer_sum=summary.response_transfer_sum+EXCLUDED.response_transfer_sum,
 image_input_samples=summary.image_input_samples+EXCLUDED.image_input_samples,
 image_input_sum=summary.image_input_sum+EXCLUDED.image_input_sum,
 image_output_samples=summary.image_output_samples+EXCLUDED.image_output_samples,
 image_output_sum=summary.image_output_sum+EXCLUDED.image_output_sum,
 cache_write_samples=summary.cache_write_samples+EXCLUDED.cache_write_samples,
 cache_write_tokens=summary.cache_write_tokens+EXCLUDED.cache_write_tokens,
 cache_read_samples=summary.cache_read_samples+EXCLUDED.cache_read_samples,
 cache_read_unknown_requests=summary.cache_read_unknown_requests+EXCLUDED.cache_read_unknown_requests,
 cache_read_input_tokens=summary.cache_read_input_tokens+EXCLUDED.cache_read_input_tokens,
 cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens;
 RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_usage_update AFTER UPDATE ON request_logs
 REFERENCING OLD TABLE AS retained_stats_old NEW TABLE AS retained_stats_new 
 FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_usage_update();

CREATE FUNCTION maintain_retained_usage_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 WITH changes AS MATERIALIZED (SELECT l.*,-1::numeric AS sign FROM retained_stats_old l),classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes)
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests;
 WITH changes AS MATERIALIZED (SELECT l.*,-1::numeric AS sign FROM retained_stats_old l),classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes),counted AS MATERIALIZED (SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign,c.* FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status) c WHERE safe)
 INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
 SELECT bucket,status,d.kind,d.resource_id,d.name,sum(sign*requests),sum(sign*success),sum(sign*errors),sum(sign*aborted),sum(sign*input_tokens),sum(sign*output_tokens),sum(sign*cached_tokens),sum(sign*usage_known),sum(sign*latency_samples),sum(sign*latency_sum),sum(sign*ttft_samples),sum(sign*ttft_sum),sum(sign*reasoning_samples),sum(sign*reasoning_sum),sum(sign*request_preparation_samples),sum(sign*request_preparation_sum),sum(sign*account_selection_samples),sum(sign*account_selection_sum),sum(sign*proxy_selection_samples),sum(sign*proxy_selection_sum),sum(sign*authentication_samples),sum(sign*authentication_sum),sum(sign*response_transfer_samples),sum(sign*response_transfer_sum),sum(sign*image_input_samples),sum(sign*image_input_sum),sum(sign*image_output_samples),sum(sign*image_output_sum),sum(sign*cache_write_samples),sum(sign*cache_write_tokens),sum(sign*cache_read_samples),sum(sign*cache_read_unknown_requests),sum(sign*cache_read_input_tokens),sum(sign*cache_read_tokens)
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name) d
 GROUP BY bucket,status,d.kind,d.resource_id,d.name ORDER BY bucket,status COLLATE "C",d.kind COLLATE "C",d.resource_id COLLATE "C",d.name COLLATE "C"
 ON CONFLICT(bucket,status,kind,resource_id,name) DO UPDATE SET
 requests=summary.requests+EXCLUDED.requests,
 success=summary.success+EXCLUDED.success,
 errors=summary.errors+EXCLUDED.errors,
 aborted=summary.aborted+EXCLUDED.aborted,
 input_tokens=summary.input_tokens+EXCLUDED.input_tokens,
 output_tokens=summary.output_tokens+EXCLUDED.output_tokens,
 cached_tokens=summary.cached_tokens+EXCLUDED.cached_tokens,
 usage_known=summary.usage_known+EXCLUDED.usage_known,
 latency_samples=summary.latency_samples+EXCLUDED.latency_samples,
 latency_sum=summary.latency_sum+EXCLUDED.latency_sum,
 ttft_samples=summary.ttft_samples+EXCLUDED.ttft_samples,
 ttft_sum=summary.ttft_sum+EXCLUDED.ttft_sum,
 reasoning_samples=summary.reasoning_samples+EXCLUDED.reasoning_samples,
 reasoning_sum=summary.reasoning_sum+EXCLUDED.reasoning_sum,
 request_preparation_samples=summary.request_preparation_samples+EXCLUDED.request_preparation_samples,
 request_preparation_sum=summary.request_preparation_sum+EXCLUDED.request_preparation_sum,
 account_selection_samples=summary.account_selection_samples+EXCLUDED.account_selection_samples,
 account_selection_sum=summary.account_selection_sum+EXCLUDED.account_selection_sum,
 proxy_selection_samples=summary.proxy_selection_samples+EXCLUDED.proxy_selection_samples,
 proxy_selection_sum=summary.proxy_selection_sum+EXCLUDED.proxy_selection_sum,
 authentication_samples=summary.authentication_samples+EXCLUDED.authentication_samples,
 authentication_sum=summary.authentication_sum+EXCLUDED.authentication_sum,
 response_transfer_samples=summary.response_transfer_samples+EXCLUDED.response_transfer_samples,
 response_transfer_sum=summary.response_transfer_sum+EXCLUDED.response_transfer_sum,
 image_input_samples=summary.image_input_samples+EXCLUDED.image_input_samples,
 image_input_sum=summary.image_input_sum+EXCLUDED.image_input_sum,
 image_output_samples=summary.image_output_samples+EXCLUDED.image_output_samples,
 image_output_sum=summary.image_output_sum+EXCLUDED.image_output_sum,
 cache_write_samples=summary.cache_write_samples+EXCLUDED.cache_write_samples,
 cache_write_tokens=summary.cache_write_tokens+EXCLUDED.cache_write_tokens,
 cache_read_samples=summary.cache_read_samples+EXCLUDED.cache_read_samples,
 cache_read_unknown_requests=summary.cache_read_unknown_requests+EXCLUDED.cache_read_unknown_requests,
 cache_read_input_tokens=summary.cache_read_input_tokens+EXCLUDED.cache_read_input_tokens,
 cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens;
 RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_usage_delete AFTER DELETE ON request_logs
 REFERENCING OLD TABLE AS retained_stats_old 
 FOR EACH STATEMENT EXECUTE FUNCTION maintain_retained_usage_delete();

CREATE FUNCTION truncate_retained_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 TRUNCATE retained_usage_minute,retained_usage_unsafe_minute;
 RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_usage_truncate AFTER TRUNCATE ON request_logs
 FOR EACH STATEMENT EXECUTE FUNCTION truncate_retained_usage();

-- Serialize the backfill/trigger boundary with existing DML. Trigger definitions
-- above and the complete backfill commit atomically; ordinary readers continue.
LOCK TABLE request_logs IN SHARE ROW EXCLUSIVE MODE;
WITH classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM request_logs)
INSERT INTO retained_usage_unsafe_minute SELECT bucket,count(*) FROM classified WHERE NOT safe GROUP BY bucket;
WITH classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM request_logs),counted AS MATERIALIZED (SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,c.* FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status) c WHERE safe)
INSERT INTO retained_usage_minute(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT bucket,status,d.kind,d.resource_id,d.name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name) d
GROUP BY bucket,status,d.kind,d.resource_id,d.name;
