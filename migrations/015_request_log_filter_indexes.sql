-- Run through the resumable concurrent-index migration path on existing logs.
-- jsonb_path_ops hashes values with PostgreSQL rechecks, avoiding BTree text
-- item limits while supporting an AND of several observed request fields.
CREATE INDEX request_logs_filter_tags_idx ON request_logs USING gin
 (request_log_filter_tags(record,model,key_name,status) jsonb_path_ops);

CREATE INDEX request_logs_input_tokens_time_idx ON request_logs
 ((COALESCE((record->>'input_tokens')::bigint,0)),created_at,id);
CREATE INDEX request_logs_output_tokens_time_idx ON request_logs
 ((COALESCE((record->>'output_tokens')::bigint,0)),created_at,id);
CREATE INDEX request_logs_cached_tokens_time_idx ON request_logs
 ((COALESCE((record->>'cached_tokens')::bigint,0)),created_at,id);

-- Arbitrarily long historical model strings remain legal. The model reader
-- merges a bounded sorted page from each branch instead of rejecting writes
-- whose indexed text would exceed the BTree item limit.
CREATE INDEX request_logs_model_short_time_idx ON request_logs
 (model COLLATE "C",created_at,id) WHERE octet_length(model)<=1700;
CREATE INDEX request_logs_model_long_time_idx ON request_logs
 (created_at,id) WHERE octet_length(model)>1700;
