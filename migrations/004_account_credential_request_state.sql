-- Current credential observations are separate from durable last-request audit.
-- Old unversioned logs remain historical and are intentionally not backfilled.
CREATE TABLE account_credential_request_state (
    account_ref text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    credential_version bigint NOT NULL CHECK (credential_version > 0),
    last_at timestamptz NOT NULL,
    last_log_id bigint NOT NULL,
    record jsonb NOT NULL
);
