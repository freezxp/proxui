-- +goose Up

-- Platforms, their credentials, the assets synced from them, and the tables
-- that record how synchronization went (docs/07-database-design.md §7.3-7.7).

CREATE TYPE platform_health AS ENUM ('unknown', 'healthy', 'degraded', 'unreachable');
CREATE TYPE sync_state AS ENUM ('active', 'missing', 'deleted');
CREATE TYPE vm_state AS ENUM ('running', 'stopped', 'paused', 'suspended', 'unknown');
CREATE TYPE sync_kind AS ENUM ('inventory', 'metrics', 'health', 'backfill');
CREATE TYPE sync_status AS ENUM ('running', 'success', 'partial', 'failed');

CREATE TABLE platforms (
    id               uuid PRIMARY KEY,
    name             citext NOT NULL UNIQUE,
    type             text NOT NULL,              -- connector registry key
    endpoint_url     text NOT NULL,
    datacenter       text NOT NULL DEFAULT 'default',
    is_enabled       boolean NOT NULL DEFAULT true,
    tls_mode         text NOT NULL DEFAULT 'verify',
    tls_ca_pem       text,
    tls_fingerprint  text,
    config           jsonb NOT NULL DEFAULT '{}',
    sync_intervals   jsonb NOT NULL DEFAULT '{"inventory":60,"metrics":60,"health":30}',
    health           platform_health NOT NULL DEFAULT 'unknown',
    health_detail    text NOT NULL DEFAULT '',
    detected_version text NOT NULL DEFAULT '',
    last_seen_at     timestamptz,
    -- Circuit breaker state: repeated failures stop the platform being polled
    -- until a probe succeeds (docs/10-sync-engine.md §10.5).
    consecutive_failures int NOT NULL DEFAULT 0,
    breaker_open_until   timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    deleted_at       timestamptz
);
CREATE INDEX ix_platforms_enabled ON platforms(is_enabled) WHERE deleted_at IS NULL;

-- Envelope encryption: the secret is sealed with a per-credential data key,
-- which is itself sealed with the master key held outside the database. Master
-- key rotation rewraps dek_wrapped only; the ciphertext is untouched.
CREATE TABLE platform_credentials (
    id          uuid PRIMARY KEY,
    platform_id uuid NOT NULL UNIQUE REFERENCES platforms(id) ON DELETE CASCADE,
    kind        text NOT NULL DEFAULT 'api_token',
    token_id    text NOT NULL DEFAULT '',        -- non-secret half of the token
    ciphertext  bytea NOT NULL,
    nonce       bytea NOT NULL,
    dek_wrapped bytea NOT NULL,
    dek_nonce   bytea NOT NULL,
    key_version int NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    rotated_at  timestamptz
);

CREATE TABLE hosts (
    id            uuid PRIMARY KEY,
    platform_id   uuid NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    external_id   text NOT NULL,
    name          text NOT NULL,
    status        text NOT NULL DEFAULT 'unknown',
    cpu_cores     int,
    memory_bytes  bigint,
    version       text NOT NULL DEFAULT '',
    uptime_s      bigint,
    content_hash  bytea NOT NULL,
    sync_state    sync_state NOT NULL DEFAULT 'active',
    missing_count int NOT NULL DEFAULT 0,
    attrs         jsonb NOT NULL DEFAULT '{}',
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    UNIQUE (platform_id, external_id)
);

CREATE TABLE vms (
    id            uuid PRIMARY KEY,
    platform_id   uuid NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    host_id       uuid REFERENCES hosts(id) ON DELETE SET NULL,
    external_id   text NOT NULL,
    name          text NOT NULL,
    vm_type       text NOT NULL DEFAULT 'qemu',
    state         vm_state NOT NULL DEFAULT 'unknown',
    cpu_cores     int,
    memory_bytes  bigint,
    disk_bytes    bigint,
    uptime_s      bigint,
    ip_addresses  jsonb NOT NULL DEFAULT '[]',
    platform_tags text[] NOT NULL DEFAULT '{}',  -- synced, read-only
    platform_pool text NOT NULL DEFAULT '',
    portal_tags   text[] NOT NULL DEFAULT '{}',  -- portal-owned, never sync-written
    notes         text NOT NULL DEFAULT '',      -- portal-owned
    content_hash  bytea NOT NULL,
    sync_state    sync_state NOT NULL DEFAULT 'active',
    missing_count int NOT NULL DEFAULT 0,
    attrs         jsonb NOT NULL DEFAULT '{}',
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    UNIQUE (platform_id, external_id)
);
CREATE INDEX ix_vms_state     ON vms(state) WHERE deleted_at IS NULL;
CREATE INDEX ix_vms_host      ON vms(host_id) WHERE deleted_at IS NULL;
CREATE INDEX ix_vms_platform  ON vms(platform_id) WHERE deleted_at IS NULL;
CREATE INDEX ix_vms_name_trgm ON vms USING gin (name gin_trgm_ops);
CREATE INDEX ix_vms_ptags     ON vms USING gin (portal_tags);

CREATE TABLE storage_pools (
    id            uuid PRIMARY KEY,
    platform_id   uuid NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    host_id       uuid REFERENCES hosts(id) ON DELETE SET NULL,
    external_id   text NOT NULL,
    natural_key   text NOT NULL,                 -- external_id@host, from the connector
    name          text NOT NULL,
    storage_type  text NOT NULL DEFAULT '',
    total_bytes   bigint,
    used_bytes    bigint,
    is_shared     boolean NOT NULL DEFAULT false,
    content_hash  bytea NOT NULL,
    sync_state    sync_state NOT NULL DEFAULT 'active',
    missing_count int NOT NULL DEFAULT 0,
    attrs         jsonb NOT NULL DEFAULT '{}',
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    UNIQUE (platform_id, natural_key)
);

CREATE TABLE networks (
    id            uuid PRIMARY KEY,
    platform_id   uuid NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    host_id       uuid REFERENCES hosts(id) ON DELETE CASCADE,
    external_id   text NOT NULL,
    natural_key   text NOT NULL,                 -- host/iface, from the connector
    name          text NOT NULL,
    net_type      text NOT NULL DEFAULT '',
    cidr          text NOT NULL DEFAULT '',
    vlan_tag      int,
    content_hash  bytea NOT NULL,
    sync_state    sync_state NOT NULL DEFAULT 'active',
    missing_count int NOT NULL DEFAULT 0,
    attrs         jsonb NOT NULL DEFAULT '{}',
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    UNIQUE (platform_id, natural_key)
);

-- VM group membership could not exist before the vms table did.
CREATE TABLE vm_group_members (
    vm_group_id uuid NOT NULL REFERENCES vm_groups(id) ON DELETE CASCADE,
    vm_id       uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
    added_by    text NOT NULL DEFAULT 'manual',  -- manual | auto
    added_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (vm_group_id, vm_id)
);
CREATE INDEX ix_vgm_vm ON vm_group_members(vm_id);

CREATE TABLE sync_runs (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    platform_id uuid NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    kind        sync_kind NOT NULL,
    status      sync_status NOT NULL DEFAULT 'running',
    trigger     text NOT NULL DEFAULT 'schedule',
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    stats       jsonb NOT NULL DEFAULT '{}',
    error       text NOT NULL DEFAULT ''
);
CREATE INDEX ix_syncruns_platform ON sync_runs(platform_id, started_at DESC);

CREATE TABLE sync_errors (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sync_run_id bigint NOT NULL REFERENCES sync_runs(id) ON DELETE CASCADE,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    scope       text NOT NULL DEFAULT '',
    message     text NOT NULL,
    detail      jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX ix_syncerrors_run ON sync_errors(sync_run_id);

-- Field-level change history, partitioned monthly like the audit log.
CREATE TABLE asset_state_history (
    id          bigint GENERATED ALWAYS AS IDENTITY,
    changed_at  timestamptz NOT NULL DEFAULT now(),
    asset_type  text NOT NULL,
    asset_id    uuid NOT NULL,
    platform_id uuid NOT NULL,
    sync_run_id bigint,
    field       text NOT NULL,
    old_value   text,
    new_value   text,
    PRIMARY KEY (changed_at, id)
) PARTITION BY RANGE (changed_at);
CREATE INDEX ix_ash_asset ON asset_state_history(asset_id, changed_at DESC);

-- +goose StatementBegin
DO $$
DECLARE
    start_month date := date_trunc('month', now() - interval '1 month')::date;
    m           date;
BEGIN
    FOR i IN 0..24 LOOP
        m := (start_month + (i || ' month')::interval)::date;
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS asset_state_history_%s PARTITION OF asset_state_history FOR VALUES FROM (%L) TO (%L)',
            to_char(m, 'YYYYMM'), m, (m + interval '1 month')::date
        );
    END LOOP;
END $$;
-- +goose StatementEnd

-- Transactional outbox: events are written in the same transaction as the
-- state change that produced them, so a crash between the two is impossible
-- (docs/10-sync-engine.md §10.8).
CREATE TABLE events_outbox (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at  timestamptz NOT NULL DEFAULT now(),
    category     text NOT NULL,
    event_type   text NOT NULL,
    severity     text NOT NULL DEFAULT 'info',
    payload      jsonb NOT NULL DEFAULT '{}',
    published_at timestamptz
);
CREATE INDEX ix_outbox_unpublished ON events_outbox(id) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS events_outbox;
DROP TABLE IF EXISTS asset_state_history;
DROP TABLE IF EXISTS sync_errors;
DROP TABLE IF EXISTS sync_runs;
DROP TABLE IF EXISTS vm_group_members;
DROP TABLE IF EXISTS networks;
DROP TABLE IF EXISTS storage_pools;
DROP TABLE IF EXISTS vms;
DROP TABLE IF EXISTS hosts;
DROP TABLE IF EXISTS platform_credentials;
DROP TABLE IF EXISTS platforms;
DROP TYPE IF EXISTS sync_status;
DROP TYPE IF EXISTS sync_kind;
DROP TYPE IF EXISTS vm_state;
DROP TYPE IF EXISTS sync_state;
DROP TYPE IF EXISTS platform_health;
