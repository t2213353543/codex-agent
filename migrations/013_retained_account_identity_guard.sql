-- The public batch filter uses account_ref, while the retained account
-- dimension uses record.account_id. Do not assume historical/imported rows
-- agree. An empty partial index proves consistency cheaply; any disagreement
-- in the selected time span sends the entire account batch to raw SQL.
CREATE INDEX request_logs_account_identity_mismatch_time_idx ON request_logs(created_at)
 WHERE account_ref IS DISTINCT FROM COALESCE(record->>'account_id','');
