CREATE TABLE groups (
 id text PRIMARY KEY, name text NOT NULL UNIQUE, description text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), version bigint NOT NULL DEFAULT 1 CHECK(version>0)
);
CREATE TABLE proxies (
 id text PRIMARY KEY, name text NOT NULL, type text NOT NULL CHECK(type IN ('http','socks5')),
 host text NOT NULL, port integer NOT NULL CHECK(port BETWEEN 1 AND 65535), username text NOT NULL DEFAULT '',
 password_cipher bytea NOT NULL DEFAULT '', enabled boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), version bigint NOT NULL DEFAULT 1 CHECK(version>0)
);
CREATE TABLE settings (
 key text PRIMARY KEY, value jsonb NOT NULL, version bigint NOT NULL DEFAULT 1, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE audit_logs (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now(),
 actor text NOT NULL DEFAULT 'admin', action text NOT NULL, entity_id text NOT NULL DEFAULT '', result text NOT NULL DEFAULT 'ok'
);
CREATE INDEX audit_logs_time_idx ON audit_logs(created_at DESC,id DESC);
