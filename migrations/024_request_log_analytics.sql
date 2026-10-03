-- Keep a small transactional view of retained observations for aggregation.
-- Request bodies, headers, attempts and other large protocol metadata are not
-- copied. Original request_logs remains authoritative for filtering/details.
CREATE TABLE request_log_analytics (
 log_id bigint PRIMARY KEY,
 record jsonb NOT NULL
);

CREATE FUNCTION request_log_analytics_record(source jsonb)
RETURNS jsonb LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
 SELECT CASE WHEN jsonb_typeof(source)='object' THEN (
  SELECT COALESCE(jsonb_object_agg(key,value),'{}'::jsonb)
  FROM jsonb_each(source) WHERE key IN (
   'groups','account_id','key_id','endpoint','upstream_model','sent_model','returned_model','inbound_path','upstream_path',
   'input_tokens','output_tokens','cached_tokens','latency_known','latency_ms','ttft_ms','reasoning_tokens','cache_write_tokens','usage_known',
   'account_selection_ms','authentication_ms','image_input_tokens','image_output_tokens','proxy_selection_ms','request_preparation_ms','response_transfer_ms',
   'websocket_warmup','cached_tokens_known','error','http_code','error_source','final_upstream_error_source','final_upstream_error_code'
  )
 ) ELSE source END
$$;

CREATE FUNCTION maintain_request_log_analytics() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF NEW.id IS DISTINCT FROM OLD.id THEN
   DELETE FROM request_log_analytics WHERE log_id=OLD.id;
  ELSIF NEW.record IS NOT DISTINCT FROM OLD.record THEN
   RETURN NULL;
  END IF;
 END IF;
 INSERT INTO request_log_analytics(log_id,record)
 VALUES(NEW.id,request_log_analytics_record(NEW.record))
 ON CONFLICT(log_id) DO UPDATE SET record=EXCLUDED.record;
 RETURN NULL;
END;
$$;

CREATE TRIGGER request_log_analytics_change
AFTER INSERT OR UPDATE OF record,id ON request_logs
FOR EACH ROW EXECUTE FUNCTION maintain_request_log_analytics();

CREATE FUNCTION delete_request_log_analytics() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 DELETE FROM request_log_analytics a USING analytics_deleted_rows removed WHERE a.log_id=removed.id;
 RETURN NULL;
END;
$$;
CREATE TRIGGER request_log_analytics_delete
AFTER DELETE ON request_logs REFERENCING OLD TABLE AS analytics_deleted_rows
FOR EACH STATEMENT EXECUTE FUNCTION delete_request_log_analytics();

CREATE FUNCTION truncate_request_log_analytics() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 TRUNCATE request_log_analytics;
 RETURN NULL;
END;
$$;
CREATE TRIGGER request_log_analytics_truncate
AFTER TRUNCATE ON request_logs
FOR EACH STATEMENT EXECUTE FUNCTION truncate_request_log_analytics();

-- Historical rows are intentionally not backfilled while the trigger DDL
-- holds its writer lock. Run backfill-log-analytics afterwards: bounded row
-- batches commit independently, while readers fall back for missing rows.
