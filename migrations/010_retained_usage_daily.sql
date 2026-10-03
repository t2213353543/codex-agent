-- Coarse retained dimensions derive from the exact minute counters. Total
-- minute rows remain the compact timezone-independent source for charts and
-- first/last boundaries; no hourly layer or duplicate JSON parsing is needed.
CREATE TABLE retained_usage_daily (LIKE retained_usage_minute INCLUDING ALL);

-- Take the request writer boundary before the derived-minute boundary, matching
-- the application lock order and avoiding a backfill/writer lock inversion.
LOCK TABLE request_logs IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE retained_usage_minute IN SHARE ROW EXCLUSIVE MODE;

CREATE INDEX retained_usage_minute_total_time_idx ON retained_usage_minute(bucket,status)
 WHERE kind='total' AND requests>0;

INSERT INTO retained_usage_daily(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
SELECT (bucket/86400)*86400,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
FROM retained_usage_minute WHERE kind<>'total' GROUP BY 1,2,3,4,5;

-- The request transition owns both summary levels. Keeping maintenance in one
-- statement avoids the AFTER UPDATE / AFTER INSERT split of a minute-table
-- UPSERT acquiring daily keys in opposite orders across concurrent requests.
-- Both levels reuse the same parsed numeric contributions; the minute write
-- completes before daily locks are taken. The immutable 009 migration is not
-- edited; this migration replaces only its maintenance functions atomically.

CREATE OR REPLACE FUNCTION maintain_retained_usage_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
WITH changes AS MATERIALIZED (SELECT l.*,1::numeric AS sign FROM retained_stats_new l),
classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes),
guard_written AS (
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), counted AS MATERIALIZED (
 SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign*c.requests AS requests,l.sign*c.success AS success,l.sign*c.errors AS errors,l.sign*c.aborted AS aborted,l.sign*c.input_tokens AS input_tokens,l.sign*c.output_tokens AS output_tokens,l.sign*c.cached_tokens AS cached_tokens,l.sign*c.usage_known AS usage_known,l.sign*c.latency_samples AS latency_samples,l.sign*c.latency_sum AS latency_sum,l.sign*c.ttft_samples AS ttft_samples,l.sign*c.ttft_sum AS ttft_sum,l.sign*c.reasoning_samples AS reasoning_samples,l.sign*c.reasoning_sum AS reasoning_sum,l.sign*c.request_preparation_samples AS request_preparation_samples,l.sign*c.request_preparation_sum AS request_preparation_sum,l.sign*c.account_selection_samples AS account_selection_samples,l.sign*c.account_selection_sum AS account_selection_sum,l.sign*c.proxy_selection_samples AS proxy_selection_samples,l.sign*c.proxy_selection_sum AS proxy_selection_sum,l.sign*c.authentication_samples AS authentication_samples,l.sign*c.authentication_sum AS authentication_sum,l.sign*c.response_transfer_samples AS response_transfer_samples,l.sign*c.response_transfer_sum AS response_transfer_sum,l.sign*c.image_input_samples AS image_input_samples,l.sign*c.image_input_sum AS image_input_sum,l.sign*c.image_output_samples AS image_output_samples,l.sign*c.image_output_sum AS image_output_sum,l.sign*c.cache_write_samples AS cache_write_samples,l.sign*c.cache_write_tokens AS cache_write_tokens,l.sign*c.cache_read_samples AS cache_read_samples,l.sign*c.cache_read_unknown_requests AS cache_read_unknown_requests,l.sign*c.cache_read_input_tokens AS cache_read_input_tokens,l.sign*c.cache_read_tokens AS cache_read_tokens
 FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status)c WHERE safe
), dimensions AS NOT MATERIALIZED (
 SELECT bucket,status,d.kind,d.resource_id,d.name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
), minute_written AS (
 INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
 SELECT bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
 FROM dimensions WHERE (SELECT count(*) FROM guard_written)>=0
 GROUP BY 1,2,3,4,5 ORDER BY bucket,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
)
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens;
RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION maintain_retained_usage_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
WITH changes AS MATERIALIZED (SELECT l.*,1::numeric AS sign FROM retained_stats_new l UNION ALL SELECT l.*,-1::numeric AS sign FROM retained_stats_old l),
classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes),
guard_written AS (
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), counted AS MATERIALIZED (
 SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign*c.requests AS requests,l.sign*c.success AS success,l.sign*c.errors AS errors,l.sign*c.aborted AS aborted,l.sign*c.input_tokens AS input_tokens,l.sign*c.output_tokens AS output_tokens,l.sign*c.cached_tokens AS cached_tokens,l.sign*c.usage_known AS usage_known,l.sign*c.latency_samples AS latency_samples,l.sign*c.latency_sum AS latency_sum,l.sign*c.ttft_samples AS ttft_samples,l.sign*c.ttft_sum AS ttft_sum,l.sign*c.reasoning_samples AS reasoning_samples,l.sign*c.reasoning_sum AS reasoning_sum,l.sign*c.request_preparation_samples AS request_preparation_samples,l.sign*c.request_preparation_sum AS request_preparation_sum,l.sign*c.account_selection_samples AS account_selection_samples,l.sign*c.account_selection_sum AS account_selection_sum,l.sign*c.proxy_selection_samples AS proxy_selection_samples,l.sign*c.proxy_selection_sum AS proxy_selection_sum,l.sign*c.authentication_samples AS authentication_samples,l.sign*c.authentication_sum AS authentication_sum,l.sign*c.response_transfer_samples AS response_transfer_samples,l.sign*c.response_transfer_sum AS response_transfer_sum,l.sign*c.image_input_samples AS image_input_samples,l.sign*c.image_input_sum AS image_input_sum,l.sign*c.image_output_samples AS image_output_samples,l.sign*c.image_output_sum AS image_output_sum,l.sign*c.cache_write_samples AS cache_write_samples,l.sign*c.cache_write_tokens AS cache_write_tokens,l.sign*c.cache_read_samples AS cache_read_samples,l.sign*c.cache_read_unknown_requests AS cache_read_unknown_requests,l.sign*c.cache_read_input_tokens AS cache_read_input_tokens,l.sign*c.cache_read_tokens AS cache_read_tokens
 FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status)c WHERE safe
), dimensions AS NOT MATERIALIZED (
 SELECT bucket,status,d.kind,d.resource_id,d.name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
), minute_written AS (
 INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
 SELECT bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
 FROM dimensions WHERE (SELECT count(*) FROM guard_written)>=0
 GROUP BY 1,2,3,4,5 ORDER BY bucket,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
)
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens;
RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION maintain_retained_usage_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
WITH changes AS MATERIALIZED (SELECT l.*,-1::numeric AS sign FROM retained_stats_old l),
classified AS MATERIALIZED (SELECT *,floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe FROM changes),
guard_written AS (
 INSERT INTO retained_usage_unsafe_minute AS guard(bucket,requests)
 SELECT bucket,sum(sign) FROM classified WHERE NOT safe GROUP BY bucket ORDER BY bucket
 ON CONFLICT(bucket) DO UPDATE SET requests=guard.requests+EXCLUDED.requests RETURNING bucket
), counted AS MATERIALIZED (
 SELECT l.bucket,l.status,l.record,l.model,l.account_name,l.key_name,l.sign*c.requests AS requests,l.sign*c.success AS success,l.sign*c.errors AS errors,l.sign*c.aborted AS aborted,l.sign*c.input_tokens AS input_tokens,l.sign*c.output_tokens AS output_tokens,l.sign*c.cached_tokens AS cached_tokens,l.sign*c.usage_known AS usage_known,l.sign*c.latency_samples AS latency_samples,l.sign*c.latency_sum AS latency_sum,l.sign*c.ttft_samples AS ttft_samples,l.sign*c.ttft_sum AS ttft_sum,l.sign*c.reasoning_samples AS reasoning_samples,l.sign*c.reasoning_sum AS reasoning_sum,l.sign*c.request_preparation_samples AS request_preparation_samples,l.sign*c.request_preparation_sum AS request_preparation_sum,l.sign*c.account_selection_samples AS account_selection_samples,l.sign*c.account_selection_sum AS account_selection_sum,l.sign*c.proxy_selection_samples AS proxy_selection_samples,l.sign*c.proxy_selection_sum AS proxy_selection_sum,l.sign*c.authentication_samples AS authentication_samples,l.sign*c.authentication_sum AS authentication_sum,l.sign*c.response_transfer_samples AS response_transfer_samples,l.sign*c.response_transfer_sum AS response_transfer_sum,l.sign*c.image_input_samples AS image_input_samples,l.sign*c.image_input_sum AS image_input_sum,l.sign*c.image_output_samples AS image_output_samples,l.sign*c.image_output_sum AS image_output_sum,l.sign*c.cache_write_samples AS cache_write_samples,l.sign*c.cache_write_tokens AS cache_write_tokens,l.sign*c.cache_read_samples AS cache_read_samples,l.sign*c.cache_read_unknown_requests AS cache_read_unknown_requests,l.sign*c.cache_read_input_tokens AS cache_read_input_tokens,l.sign*c.cache_read_tokens AS cache_read_tokens
 FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status)c WHERE safe
), dimensions AS NOT MATERIALIZED (
 SELECT bucket,status,d.kind,d.resource_id,d.name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens
 FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
), minute_written AS (
 INSERT INTO retained_usage_minute AS summary(bucket,status,kind,resource_id,name,requests,success,errors,aborted,input_tokens,output_tokens,cached_tokens,usage_known,latency_samples,latency_sum,ttft_samples,ttft_sum,reasoning_samples,reasoning_sum,request_preparation_samples,request_preparation_sum,account_selection_samples,account_selection_sum,proxy_selection_samples,proxy_selection_sum,authentication_samples,authentication_sum,response_transfer_samples,response_transfer_sum,image_input_samples,image_input_sum,image_output_samples,image_output_sum,cache_write_samples,cache_write_tokens,cache_read_samples,cache_read_unknown_requests,cache_read_input_tokens,cache_read_tokens)
 SELECT bucket,status,kind,resource_id,name,sum(requests),sum(success),sum(errors),sum(aborted),sum(input_tokens),sum(output_tokens),sum(cached_tokens),sum(usage_known),sum(latency_samples),sum(latency_sum),sum(ttft_samples),sum(ttft_sum),sum(reasoning_samples),sum(reasoning_sum),sum(request_preparation_samples),sum(request_preparation_sum),sum(account_selection_samples),sum(account_selection_sum),sum(proxy_selection_samples),sum(proxy_selection_sum),sum(authentication_samples),sum(authentication_sum),sum(response_transfer_samples),sum(response_transfer_sum),sum(image_input_samples),sum(image_input_sum),sum(image_output_samples),sum(image_output_sum),sum(cache_write_samples),sum(cache_write_tokens),sum(cache_read_samples),sum(cache_read_unknown_requests),sum(cache_read_input_tokens),sum(cache_read_tokens)
 FROM dimensions WHERE (SELECT count(*) FROM guard_written)>=0
 GROUP BY 1,2,3,4,5 ORDER BY bucket,status COLLATE "C",kind COLLATE "C",resource_id COLLATE "C",name COLLATE "C"
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
)
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
cache_read_tokens=summary.cache_read_tokens+EXCLUDED.cache_read_tokens;
RETURN NULL;
END
$$;

CREATE FUNCTION truncate_retained_usage_daily() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 TRUNCATE retained_usage_daily;
 RETURN NULL;
END
$$;
CREATE TRIGGER retained_usage_minute_daily_truncate AFTER TRUNCATE ON retained_usage_minute
 FOR EACH STATEMENT EXECUTE FUNCTION truncate_retained_usage_daily();
