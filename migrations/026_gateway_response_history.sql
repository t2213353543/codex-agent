-- Permanent continuation bodies belong on durable storage, not in Redis RAM.
-- Ciphertext is the existing authenticated AES-GCM value; no request, output,
-- response identifier or account identity is stored in plaintext columns.
CREATE TABLE IF NOT EXISTS gateway_response_history (
    namespace text NOT NULL,
    history_key text NOT NULL,
    ciphertext bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, history_key)
);
