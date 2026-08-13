-- +goose Up

-- Identity context (docs/07-database-design.md §7.2).
CREATE TYPE user_role AS ENUM ('admin', 'operator', 'readonly', 'auditor');

CREATE TABLE users (
    id                   uuid PRIMARY KEY,
    username             citext NOT NULL UNIQUE,
    email                citext NOT NULL UNIQUE,
    display_name         text NOT NULL,
    password_hash        text NOT NULL,
    role                 user_role NOT NULL DEFAULT 'readonly',
    is_active            boolean NOT NULL DEFAULT true,
    must_change_password boolean NOT NULL DEFAULT true,
    totp_secret_enc      bytea,
    totp_enabled         boolean NOT NULL DEFAULT false,
    failed_login_count   int NOT NULL DEFAULT 0,
    last_failed_at       timestamptz,
    locked_until         timestamptz,
    last_login_at        timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- Refresh-token sessions. family_id groups a rotation chain so that presenting
-- an already-rotated token can revoke every descendant at once (AUTH-03).
CREATE TABLE sessions (
    id                 uuid PRIMARY KEY,
    user_id            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id          uuid NOT NULL,
    refresh_token_hash bytea NOT NULL,
    ip                 inet,
    user_agent         text,
    expires_at         timestamptz NOT NULL,
    rotated_at         timestamptz,
    revoked_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ux_sessions_token ON sessions(refresh_token_hash);
CREATE INDEX ix_sessions_user   ON sessions(user_id) WHERE revoked_at IS NULL;
CREATE INDEX ix_sessions_family ON sessions(family_id);

-- Audit log: append-only, partitioned monthly so retention is a DROP PARTITION
-- (docs/07 §7.7). actor_name is denormalized so entries outlive their users.
CREATE TABLE audit_logs (
    id            bigint GENERATED ALWAYS AS IDENTITY,
    ts            timestamptz NOT NULL DEFAULT now(),
    actor_user_id uuid,
    actor_name    text NOT NULL,
    category      text NOT NULL,
    action        text NOT NULL,
    target_type   text,
    target_id     text,
    target_name   text,
    source_ip     inet,
    user_agent    text,
    outcome       text NOT NULL DEFAULT 'success',
    request_id    text,
    details       jsonb NOT NULL DEFAULT '{}',
    PRIMARY KEY (ts, id)
) PARTITION BY RANGE (ts);

CREATE INDEX ix_audit_actor    ON audit_logs(actor_user_id, ts DESC);
CREATE INDEX ix_audit_category ON audit_logs(category, ts DESC);
CREATE INDEX ix_audit_action   ON audit_logs(action, ts DESC);

-- Pre-create partitions covering the previous month through the next two
-- years; the janitor job extends the window as time passes.
-- +goose StatementBegin
DO $$
DECLARE
    start_month date := date_trunc('month', now() - interval '1 month')::date;
    m           date;
BEGIN
    FOR i IN 0..24 LOOP
        m := (start_month + (i || ' month')::interval)::date;
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS audit_logs_%s PARTITION OF audit_logs FOR VALUES FROM (%L) TO (%L)',
            to_char(m, 'YYYYMM'), m, (m + interval '1 month')::date
        );
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP TYPE IF EXISTS user_role;
