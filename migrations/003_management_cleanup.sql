-- The target set freezes the exact MVCC-visible preview, including concurrent
-- inserts whose sequence IDs were allocated before the preview started.
-- No FK to request_logs: another cleanup may delete a target before this job
-- reaches it, and that must be accounted for without adding new targets.
CREATE TABLE management_cleanup_targets (
    preview_id text NOT NULL,
    log_id bigint NOT NULL,
    expires_at timestamptz,
    PRIMARY KEY (preview_id, log_id)
);
CREATE INDEX management_cleanup_targets_expiry_idx
    ON management_cleanup_targets (expires_at) WHERE expires_at IS NOT NULL;
