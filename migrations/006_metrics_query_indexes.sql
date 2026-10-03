-- Exact request identity filters formerly scanned retained JSON records. Hash
-- indexes support long upstream IDs without a btree index-entry size ceiling;
-- equality predicates retain PostgreSQL's collision recheck semantics.
CREATE INDEX request_logs_request_id_idx ON request_logs USING hash ((record->>'request_id'))
    WHERE record->>'request_id' IS NOT NULL;
CREATE INDEX request_logs_response_id_idx ON request_logs USING hash ((record->>'response_id'))
    WHERE record->>'response_id' IS NOT NULL;
CREATE INDEX request_logs_client_request_id_idx ON request_logs USING hash ((record->>'client_request_id'))
    WHERE record->>'client_request_id' IS NOT NULL;

-- The most expensive interactive ranking paths need the full seek order. Do
-- not include record: duplicating large JSON would greatly increase write cost.
CREATE INDEX request_logs_tokens_time_idx ON request_logs
    ((COALESCE((record->>'input_tokens')::bigint,0)+COALESCE((record->>'output_tokens')::bigint,0)),created_at,id);
CREATE INDEX request_logs_latency_time_idx ON request_logs
    ((COALESCE((record->>'latency_ms')::bigint,0)),created_at,id);
