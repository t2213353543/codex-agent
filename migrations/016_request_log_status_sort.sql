-- Failed requests often have lower token totals than successful ones. Without
-- the leading status key, a highest-token error page can walk hundreds of
-- thousands of successful records before finding its first result.
CREATE INDEX request_logs_status_tokens_time_idx ON request_logs
 (status,(COALESCE((record->>'input_tokens')::bigint,0)+COALESCE((record->>'output_tokens')::bigint,0)),created_at,id);
