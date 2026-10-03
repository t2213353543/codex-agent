-- A compact immutable projection lets one GIN path index serve equality and
-- conjunction filters without indexing large error/protocol payloads. Values
-- use the same ->> text extraction, boolean casts and defaults as the readers.
-- Keep the original WHERE clauses as rechecks; this is only an access path.
CREATE FUNCTION request_log_filter_tags(record jsonb,model_name text,key_label text,request_status text)
RETURNS jsonb LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT jsonb_strip_nulls(jsonb_build_object(
  'model',model_name,'key_name',key_label,'status',request_status,
  'key_id',record->>'key_id','endpoint',record->>'endpoint','ip',record->>'ip',
  'upstream_model',record->>'upstream_model','request_type',record->>'request_type',
  'service_tier',record->>'service_tier','reasoning_effort',record->>'reasoning_effort',
  'error_phase',record->>'error_phase','sent_model',record->>'sent_model',
  'returned_model',record->>'returned_model','inbound_path',record->>'inbound_path',
  'upstream_path',record->>'upstream_path',
  'http_code',COALESCE((record->>'http_code')::integer,0),
  'stream',COALESCE((record->>'stream')::boolean,false),
  'websocket_warmup',(record->>'websocket_warmup')::boolean,
  'native_compaction',(record->>'native_compaction')::boolean,
  'error_truncated',(record->>'error_truncated')::boolean,
  'groups',CASE jsonb_typeof(record->'groups')
    WHEN 'array' THEN record->'groups'
    WHEN 'string' THEN jsonb_build_array(record->'groups')
    WHEN 'object' THEN (SELECT COALESCE(jsonb_agg(k),'[]'::jsonb) FROM jsonb_object_keys(record->'groups') keys(k))
    ELSE '[]'::jsonb END
 ))
$$;
