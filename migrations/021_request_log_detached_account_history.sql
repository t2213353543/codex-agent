-- Request details are historical observations, just like usage_events.
-- All application reads use account_ref / record; account_id is retained as
-- the account association observed at insertion for older database consumers.
-- A nullable FK with ON DELETE SET NULL rewrites every historical request when
-- an account is deleted, rebuilding wide JSON indexes and retained aggregates.
-- Detach that live-entity FK so deletion has no dependency on history volume.
-- Consumers that need current existence can LEFT JOIN accounts ON account_id.
ALTER TABLE request_logs DROP CONSTRAINT request_logs_account_id_fkey;
COMMENT ON COLUMN request_logs.account_id IS 'Historical account association at insertion; may refer to a deleted account. Use a LEFT JOIN to accounts for current existence.';
