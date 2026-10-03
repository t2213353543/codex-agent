-- One current retirement operation per existing account, never request history.
CREATE TABLE IF NOT EXISTS account_drains (
    account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    operation_id text NOT NULL CHECK (operation_id ~ '^[0-9a-f]{64}$'),
    owner_ref_sha text NOT NULL CHECK (owner_ref_sha ~ '^[0-9a-f]{64}$'),
    workspace_sha256 text NOT NULL CHECK (workspace_sha256 ~ '^[0-9a-f]{64}$'),
    user_sha256 text NOT NULL CHECK (user_sha256 ~ '^[0-9a-f]{64}$'),
    credential_version bigint NOT NULL CHECK (credential_version > 0),
    drain_token text NOT NULL CHECK (drain_token ~ '^[0-9a-f]{64}$'),
    created_at bigint NOT NULL,
    zero_observed_at_ms bigint NOT NULL DEFAULT 0 CHECK (zero_observed_at_ms >= 0)
);
