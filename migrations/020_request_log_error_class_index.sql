-- Run through the resumable concurrent-index migration path on existing logs.
CREATE INDEX request_logs_error_class_tags_idx ON request_logs USING gin
 (request_log_error_classes(record,status) jsonb_path_ops);
