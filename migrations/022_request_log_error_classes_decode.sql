-- Preserve every category/severity value already indexed by 019/020 while
-- decoding wide toasted records only once. No rows or historical facts change.
CREATE OR REPLACE FUNCTION request_log_error_classes(record jsonb, status text)
RETURNS jsonb LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 WITH decoded AS MATERIALIZED (
  SELECT lower(COALESCE(observed.error,'')) AS error_lower,
         observed.http_code AS observed_http_code,
         observed.error_source AS observed_error_source,
         observed.final_upstream_error_source AS observed_final_upstream_error_source,
         observed.final_upstream_error_code AS observed_final_upstream_error_code
  FROM jsonb_to_record(CASE WHEN jsonb_typeof(record)='object' THEN record ELSE '{}'::jsonb END)
   AS observed(error text,http_code text,error_source text,final_upstream_error_source text,final_upstream_error_code text)
 ), classified AS MATERIALIZED (
  SELECT observed_http_code,(CASE WHEN status='ok' THEN ''
 WHEN status='error' AND COALESCE(observed_error_source,'') IN ('','upstream') AND observed_final_upstream_error_source='upstream' AND observed_final_upstream_error_code IN ('rateLimitExceeded','usageLimitExceeded','rate_limit_exceeded','rate_limit_error','insufficient_quota','billing_hard_limit_reached') THEN 'rate_limit'
 WHEN status='error' AND COALESCE(observed_error_source,'') IN ('','upstream') AND observed_final_upstream_error_source='upstream' AND observed_final_upstream_error_code IN ('unauthorized','authentication_error','invalid_api_key','authentication_unavailable','upstream_account_rejected') THEN 'authentication'
 WHEN status='error' AND COALESCE(observed_error_source,'') IN ('','upstream') AND observed_final_upstream_error_source='upstream' AND observed_final_upstream_error_code IN ('serverOverloaded','server_overloaded','model_overloaded','overloaded_error') THEN 'overloaded'
 WHEN position('unsupported parameter' in error_lower)>0 THEN 'unsupported_parameter'
 WHEN position('system messages are not allowed' in error_lower)>0 THEN 'system_role'
 WHEN position('no enabled account' in error_lower)>0 THEN 'no_enabled_account'
 WHEN position('invalidated oauth' in error_lower)>0 OR position('refresh_token_reused' in error_lower)>0 THEN 'invalid_oauth'
 WHEN (position('model' in error_lower)>0 AND position('not supported' in error_lower)>0) OR position('model_not_found' in error_lower)>0 THEN 'unsupported_model'
 WHEN position('encrypted content' in error_lower)>0 OR position('invalid_encrypted_content' in error_lower)>0 THEN 'encrypted_history'
 WHEN observed_http_code='413' OR position('exceeds 32 mib' in error_lower)>0 THEN 'request_too_large'
 WHEN position('flagged for possible' in error_lower)>0 OR position('content_filter' in error_lower)>0 OR position('policy violation' in error_lower)>0 THEN 'upstream_refusal'
 WHEN observed_http_code='429' OR position('rate limit' in error_lower)>0 OR position('rate_limit' in error_lower)>0 THEN 'rate_limit'
 WHEN position('overloaded' in error_lower)>0 THEN 'overloaded'
 WHEN position('an error occurred while processing' in error_lower)>0 THEN 'upstream_error'
 WHEN status='aborted' OR observed_http_code='499' OR position('context canceled' in error_lower)>0 THEN 'aborted'
 WHEN observed_http_code='504' OR position('timeout' in error_lower)>0 OR position('timed out' in error_lower)>0 OR position('deadline exceeded' in error_lower)>0 THEN 'timeout'
 WHEN observed_http_code IN ('401','403') THEN 'authentication'
 WHEN COALESCE((observed_http_code)::integer,0)>=500 THEN 'server_error' ELSE 'unknown' END) AS category FROM decoded
 ) SELECT jsonb_build_object('category',category,'severity',CASE
  WHEN status='ok' THEN ''
  WHEN COALESCE(observed_http_code::integer,0) NOT BETWEEN 100 AND 599 THEN 'unknown'
  WHEN category IN ('unsupported_parameter','system_role','invalid_oauth','unsupported_model','encrypted_history','authentication') THEN 'P3'
  WHEN category IN ('rate_limit','overloaded','upstream_error','server_error','timeout') THEN 'P1'
  WHEN observed_http_code::integer>=500 OR observed_http_code::integer=429 THEN 'P1'
  WHEN observed_http_code::integer>=400 THEN 'P2' ELSE 'P3' END)
 FROM classified
$$;
