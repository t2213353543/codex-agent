-- Model-conditioned retained summaries preserve cross-dimensional counts for
-- genuinely mixed-model selections. Counters and observation semantics are
-- identical to 009/010; no existing migration source/checksum is changed.
CREATE TABLE retained_usage_model_minute (
 selected_model text COLLATE "C" NOT NULL,
 LIKE retained_usage_minute INCLUDING DEFAULTS INCLUDING CONSTRAINTS,
 PRIMARY KEY(selected_model,bucket,status,kind,resource_id,name)
);
CREATE TABLE retained_usage_model_daily (
 selected_model text COLLATE "C" NOT NULL,
 LIKE retained_usage_minute INCLUDING DEFAULTS INCLUDING CONSTRAINTS,
 PRIMARY KEY(selected_model,bucket,status,kind,resource_id,name)
);
CREATE TABLE retained_usage_model_unsafe_minute(bucket bigint PRIMARY KEY,requests numeric NOT NULL);
CREATE INDEX retained_usage_model_minute_total_time_idx ON retained_usage_model_minute(selected_model,bucket,status)
 WHERE kind='total' AND requests>0;

-- Old-safe dimension keys total at most 1700 bytes; cap only this additional
-- model axis at 512 bytes. Longer legal models keep their raw fallback rather
-- than exceeding a BTree item limit or silently colliding in a digest key.
-- Block request DML before derived state to retain the normal writer lock order.
LOCK TABLE request_logs IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE retained_usage_minute IN SHARE ROW EXCLUSIVE MODE;

CREATE TEMP TABLE retained_model_backfill_buckets ON COMMIT DROP AS
SELECT bucket,status,count(*) AS models,min(name COLLATE "C") AS sole_model,
 bool_or(octet_length(name)<=512) AS has_short
FROM retained_usage_minute WHERE kind='model' AND requests>0 GROUP BY bucket,status;
CREATE UNIQUE INDEX ON retained_model_backfill_buckets(bucket,status);
ANALYZE retained_model_backfill_buckets;

-- Every safe request has exactly one model marginal. With one positive model
-- per minute/status, every old dimension belongs to it and can be copied exactly
-- without reopening JSON. Unsafe old records were never in these counters.
INSERT INTO retained_usage_model_minute(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT b.sole_model,m.bucket,m.status,m.kind,m.resource_id,m.name,m.requests,m.success,m.errors,m.aborted,m.input_tokens,m.output_tokens,m.cached_tokens,m.usage_known,m.latency_samples,m.latency_sum,m.ttft_samples,m.ttft_sum,m.reasoning_samples,m.reasoning_sum,m.request_preparation_samples,m.request_preparation_sum,m.account_selection_samples,m.account_selection_sum,m.proxy_selection_samples,m.proxy_selection_sum,m.authentication_samples,m.authentication_sum,m.response_transfer_samples,m.response_transfer_sum,m.image_input_samples,m.image_input_sum,m.image_output_samples,m.image_output_sum,m.cache_write_samples,m.cache_write_tokens,m.cache_read_samples,m.cache_read_unknown_requests,m.cache_read_input_tokens,m.cache_read_tokens
FROM retained_usage_minute m JOIN retained_model_backfill_buckets b USING(bucket,status)
WHERE b.models=1 AND b.has_short AND m.requests>0;

-- Old-safe models outside the new key budget are a second conservative guard.
-- Existing unsafe observations remain covered by retained_usage_unsafe_minute.
INSERT INTO retained_usage_model_unsafe_minute(bucket,requests)
SELECT bucket,sum(requests) FROM retained_usage_minute
WHERE kind='model' AND requests>0 AND octet_length(name)>512 GROUP BY bucket;

-- Only mixed safe model/status minutes require reconstruction from records.
-- Parameterized timestamp probes avoid a second whole-table JSON extraction
-- on installations whose traffic uses one model per minute/status.
WITH candidates AS MATERIALIZED (
 SELECT r.* FROM retained_model_backfill_buckets b CROSS JOIN LATERAL (
 SELECT created_at,status,model,record,account_name,key_name FROM request_logs
 WHERE created_at>=to_timestamp(b.bucket) AND created_at<to_timestamp(b.bucket+60)
 AND status=b.status AND octet_length(model)<=512 OFFSET 0
 ) r WHERE b.models>1 AND b.has_short
), counted AS MATERIALIZED (
 SELECT floor(extract(epoch FROM r.created_at)/60)::bigint*60 AS bucket,r.status,r.model,r.record,r.account_name,r.key_name,c.*
 FROM candidates r CROSS JOIN LATERAL retained_usage_counters(r.record,r.status)c
 WHERE retained_usage_safe(r.created_at,r.status,r.record,r.model,r.account_name,r.key_name)
)
INSERT INTO retained_usage_model_minute(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT model,bucket,status,d.kind,d.resource_id,d.name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
GROUP BY model,bucket,status,d.kind,d.resource_id,d.name;

INSERT INTO retained_usage_model_daily(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT selected_model,(bucket/86400)*86400,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM retained_usage_model_minute WHERE kind<>'total' GROUP BY 1,2,3,4,5,6;

CREATE OR REPLACE FUNCTION maintain_retained_usage_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
WITH changes AS MATERIALIZED (SELECT l.*,1::numeric AS sign FROM retained_stats_new l),
classified AS MATERIALIZED (
 SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,
 retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes
), guard_written AS (
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), model_guard_written AS (
 INSERT INTO retained_usage_model_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE safe AND octet_length(model)>512 AND (SELECT count(*) FROM guard_written)>=0
 GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), counted AS MATERIALIZED (
 SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign*c.requests AS requests,l.sign*c.success AS success,l.sign*c.errors AS errors,l.sign*c.aborted AS aborted,l.sign*c.input_tokens AS input_tokens,l.sign*c.output_tokens AS output_tokens,l.sign*c.cached_tokens AS cached_tokens,l.sign*c.usage_known AS usage_known,l.sign*c.latency_samples AS latency_samples,l.sign*c.latency_sum AS latency_sum,l.sign*c.ttft_samples AS ttft_samples,l.sign*c.ttft_sum AS ttft_sum,l.sign*c.reasoning_samples AS reasoning_samples,l.sign*c.reasoning_sum AS reasoning_sum,l.sign*c.request_preparation_samples AS request_preparation_samples,l.sign*c.request_preparation_sum AS request_preparation_sum,l.sign*c.account_selection_samples AS account_selection_samples,l.sign*c.account_selection_sum AS account_selection_sum,l.sign*c.proxy_selection_samples AS proxy_selection_samples,l.sign*c.proxy_selection_sum AS proxy_selection_sum,l.sign*c.authentication_samples AS authentication_samples,l.sign*c.authentication_sum AS authentication_sum,l.sign*c.response_transfer_samples AS response_transfer_samples,l.sign*c.response_transfer_sum AS response_transfer_sum,l.sign*c.image_input_samples AS image_input_samples,l.sign*c.image_input_sum AS image_input_sum,l.sign*c.image_output_samples AS image_output_samples,l.sign*c.image_output_sum AS image_output_sum,l.sign*c.cache_write_samples AS cache_write_samples,l.sign*c.cache_write_tokens AS cache_write_tokens,l.sign*c.cache_read_samples AS cache_read_samples,l.sign*c.cache_read_unknown_requests AS cache_read_unknown_requests,l.sign*c.cache_read_input_tokens AS cache_read_input_tokens,l.sign*c.cache_read_tokens AS cache_read_tokens
 FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status)c WHERE safe
), dimensions AS NOT MATERIALIZED (
 SELECT model AS selected_model,bucket,status,d.kind,d.resource_id,d.name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
), minute_written AS (
INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE (SELECT count(*) FROM model_guard_written)>=0
GROUP BY 1,2,3,4,5 ORDER BY 1,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
), daily_written AS (
INSERT INTO retained_usage_daily AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT (bucket/86400)*86400,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE kind<>'total' AND (SELECT count(*) FROM minute_written)>=0
GROUP BY 1,2,3,4,5 ORDER BY 1,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
), model_minute_written AS (
INSERT INTO retained_usage_model_minute AS summary(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT selected_model,bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE octet_length(selected_model)<=512 AND (SELECT count(*) FROM daily_written)>=0
GROUP BY 1,2,3,4,5,6 ORDER BY selected_model COLLATE "C",2,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
ON CONFLICT(selected_model,bucket,status,kind,resource_id,name) DO UPDATE SET
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
)
INSERT INTO retained_usage_model_daily AS summary(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT selected_model,(bucket/86400)*86400,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE octet_length(selected_model)<=512 AND kind<>'total' AND (SELECT count(*) FROM model_minute_written)>=0
GROUP BY 1,2,3,4,5,6 ORDER BY selected_model COLLATE "C",2,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
ON CONFLICT(selected_model,bucket,status,kind,resource_id,name) DO UPDATE SET
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

CREATE OR REPLACE FUNCTION maintain_retained_usage_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
WITH changes AS MATERIALIZED (SELECT l.*,1::numeric AS sign FROM retained_stats_new l UNION ALL SELECT l.*,-1::numeric AS sign FROM retained_stats_old l),
classified AS MATERIALIZED (
 SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,
 retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes
), guard_written AS (
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), model_guard_written AS (
 INSERT INTO retained_usage_model_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE safe AND octet_length(model)>512 AND (SELECT count(*) FROM guard_written)>=0
 GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), counted AS MATERIALIZED (
 SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign*c.requests AS requests,l.sign*c.success AS success,l.sign*c.errors AS errors,l.sign*c.aborted AS aborted,l.sign*c.input_tokens AS input_tokens,l.sign*c.output_tokens AS output_tokens,l.sign*c.cached_tokens AS cached_tokens,l.sign*c.usage_known AS usage_known,l.sign*c.latency_samples AS latency_samples,l.sign*c.latency_sum AS latency_sum,l.sign*c.ttft_samples AS ttft_samples,l.sign*c.ttft_sum AS ttft_sum,l.sign*c.reasoning_samples AS reasoning_samples,l.sign*c.reasoning_sum AS reasoning_sum,l.sign*c.request_preparation_samples AS request_preparation_samples,l.sign*c.request_preparation_sum AS request_preparation_sum,l.sign*c.account_selection_samples AS account_selection_samples,l.sign*c.account_selection_sum AS account_selection_sum,l.sign*c.proxy_selection_samples AS proxy_selection_samples,l.sign*c.proxy_selection_sum AS proxy_selection_sum,l.sign*c.authentication_samples AS authentication_samples,l.sign*c.authentication_sum AS authentication_sum,l.sign*c.response_transfer_samples AS response_transfer_samples,l.sign*c.response_transfer_sum AS response_transfer_sum,l.sign*c.image_input_samples AS image_input_samples,l.sign*c.image_input_sum AS image_input_sum,l.sign*c.image_output_samples AS image_output_samples,l.sign*c.image_output_sum AS image_output_sum,l.sign*c.cache_write_samples AS cache_write_samples,l.sign*c.cache_write_tokens AS cache_write_tokens,l.sign*c.cache_read_samples AS cache_read_samples,l.sign*c.cache_read_unknown_requests AS cache_read_unknown_requests,l.sign*c.cache_read_input_tokens AS cache_read_input_tokens,l.sign*c.cache_read_tokens AS cache_read_tokens
 FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status)c WHERE safe
), dimensions AS NOT MATERIALIZED (
 SELECT model AS selected_model,bucket,status,d.kind,d.resource_id,d.name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
), minute_written AS (
INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE (SELECT count(*) FROM model_guard_written)>=0
GROUP BY 1,2,3,4,5 ORDER BY 1,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
), daily_written AS (
INSERT INTO retained_usage_daily AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT (bucket/86400)*86400,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE kind<>'total' AND (SELECT count(*) FROM minute_written)>=0
GROUP BY 1,2,3,4,5 ORDER BY 1,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
), model_minute_written AS (
INSERT INTO retained_usage_model_minute AS summary(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT selected_model,bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE octet_length(selected_model)<=512 AND (SELECT count(*) FROM daily_written)>=0
GROUP BY 1,2,3,4,5,6 ORDER BY selected_model COLLATE "C",2,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
ON CONFLICT(selected_model,bucket,status,kind,resource_id,name) DO UPDATE SET
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
)
INSERT INTO retained_usage_model_daily AS summary(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT selected_model,(bucket/86400)*86400,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE octet_length(selected_model)<=512 AND kind<>'total' AND (SELECT count(*) FROM model_minute_written)>=0
GROUP BY 1,2,3,4,5,6 ORDER BY selected_model COLLATE "C",2,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
ON CONFLICT(selected_model,bucket,status,kind,resource_id,name) DO UPDATE SET
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

CREATE OR REPLACE FUNCTION maintain_retained_usage_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
WITH changes AS MATERIALIZED (SELECT l.*,-1::numeric AS sign FROM retained_stats_old l),
classified AS MATERIALIZED (
 SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,
 retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes
), guard_written AS (
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), model_guard_written AS (
 INSERT INTO retained_usage_model_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE safe AND octet_length(model)>512 AND (SELECT count(*) FROM guard_written)>=0
 GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), counted AS MATERIALIZED (
 SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign*c.requests AS requests,l.sign*c.success AS success,l.sign*c.errors AS errors,l.sign*c.aborted AS aborted,l.sign*c.input_tokens AS input_tokens,l.sign*c.output_tokens AS output_tokens,l.sign*c.cached_tokens AS cached_tokens,l.sign*c.usage_known AS usage_known,l.sign*c.latency_samples AS latency_samples,l.sign*c.latency_sum AS latency_sum,l.sign*c.ttft_samples AS ttft_samples,l.sign*c.ttft_sum AS ttft_sum,l.sign*c.reasoning_samples AS reasoning_samples,l.sign*c.reasoning_sum AS reasoning_sum,l.sign*c.request_preparation_samples AS request_preparation_samples,l.sign*c.request_preparation_sum AS request_preparation_sum,l.sign*c.account_selection_samples AS account_selection_samples,l.sign*c.account_selection_sum AS account_selection_sum,l.sign*c.proxy_selection_samples AS proxy_selection_samples,l.sign*c.proxy_selection_sum AS proxy_selection_sum,l.sign*c.authentication_samples AS authentication_samples,l.sign*c.authentication_sum AS authentication_sum,l.sign*c.response_transfer_samples AS response_transfer_samples,l.sign*c.response_transfer_sum AS response_transfer_sum,l.sign*c.image_input_samples AS image_input_samples,l.sign*c.image_input_sum AS image_input_sum,l.sign*c.image_output_samples AS image_output_samples,l.sign*c.image_output_sum AS image_output_sum,l.sign*c.cache_write_samples AS cache_write_samples,l.sign*c.cache_write_tokens AS cache_write_tokens,l.sign*c.cache_read_samples AS cache_read_samples,l.sign*c.cache_read_unknown_requests AS cache_read_unknown_requests,l.sign*c.cache_read_input_tokens AS cache_read_input_tokens,l.sign*c.cache_read_tokens AS cache_read_tokens
 FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status)c WHERE safe
), dimensions AS NOT MATERIALIZED (
 SELECT model AS selected_model,bucket,status,d.kind,d.resource_id,d.name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
), minute_written AS (
INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE (SELECT count(*) FROM model_guard_written)>=0
GROUP BY 1,2,3,4,5 ORDER BY 1,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
), daily_written AS (
INSERT INTO retained_usage_daily AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT (bucket/86400)*86400,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE kind<>'total' AND (SELECT count(*) FROM minute_written)>=0
GROUP BY 1,2,3,4,5 ORDER BY 1,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
), model_minute_written AS (
INSERT INTO retained_usage_model_minute AS summary(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT selected_model,bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE octet_length(selected_model)<=512 AND (SELECT count(*) FROM daily_written)>=0
GROUP BY 1,2,3,4,5,6 ORDER BY selected_model COLLATE "C",2,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
ON CONFLICT(selected_model,bucket,status,kind,resource_id,name) DO UPDATE SET
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens
RETURNING bucket
)
INSERT INTO retained_usage_model_daily AS summary(selected_model,bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT selected_model,(bucket/86400)*86400,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM dimensions WHERE octet_length(selected_model)<=512 AND kind<>'total' AND (SELECT count(*) FROM model_minute_written)>=0
GROUP BY 1,2,3,4,5,6 ORDER BY selected_model COLLATE "C",2,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
ON CONFLICT(selected_model,bucket,status,kind,resource_id,name) DO UPDATE SET
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

CREATE FUNCTION truncate_retained_usage_model() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 TRUNCATE retained_usage_model_minute,retained_usage_model_daily,retained_usage_model_unsafe_minute;
 RETURN NULL;
END
$$;
CREATE TRIGGER request_logs_usage_model_truncate AFTER TRUNCATE ON request_logs
 FOR EACH STATEMENT EXECUTE FUNCTION truncate_retained_usage_model();
