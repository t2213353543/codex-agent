-- Exact scalar keys avoid GIN's per-matching-row JSON classification recheck.
-- The timestamp/ID suffix also supports bounded ordered error-list pages.
CREATE INDEX request_logs_error_category_time_idx ON request_logs
 ((request_log_error_classes(record,status)->>'category'),created_at DESC,id DESC);
CREATE INDEX request_logs_error_severity_time_idx ON request_logs
 ((request_log_error_classes(record,status)->>'severity'),created_at DESC,id DESC);
