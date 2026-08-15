-- +goose Up
-- Edge providers: an external control plane the portal configures, as opposed
-- to a platform it reads (ADR 0004, docs/28-published-apps.md).
--
-- Separate from `platforms` rather than a new row type in it. They share a
-- shape — an endpoint, a sealed credential, health, a circuit breaker — and
-- almost nothing else: a platform has a connector, a datacenter, sync
-- intervals for inventory and metrics, and VMs pointing at it. Reusing the
-- table would mean half its columns being null for one kind of row, and a
-- foreign key from vms that must never resolve to an edge provider.
CREATE TABLE edge_providers (
    id       uuid PRIMARY KEY,
    name     citext NOT NULL,
    -- The provider implementation, matching an internal/edge registry key.
    kind     text NOT NULL DEFAULT 'cloudflare_tunnel',

    -- Cloudflare's account. Never discovered from the token: a resource-scoped
    -- token reports zero accounts while reading that account's tunnels
    -- perfectly well, so the administrator supplies it (docs/28 §28.10).
    account_id  text NOT NULL,

    -- The selected tunnel. Nullable because a provider is registered before a
    -- tunnel is chosen: the credential has to work before its tunnels can be
    -- listed to choose from.
    tunnel_id   text,
    tunnel_name text NOT NULL DEFAULT '',

    -- The write boundary (PUB-04). DNS:Edit reaches an entire zone, so the
    -- portal refuses zones outside this list even when the token would allow
    -- them. Empty means no zone may be written, which is the safe default for
    -- a provider that has been registered but not yet scoped.
    allowed_zone_ids jsonb NOT NULL DEFAULT '[]'::jsonb,

    is_enabled boolean NOT NULL DEFAULT true,

    -- Health, mirroring platforms so the same breaker logic applies.
    health           platform_health NOT NULL DEFAULT 'unknown',
    health_detail    text NOT NULL DEFAULT '',
    last_seen_at     timestamptz,
    consecutive_failures integer NOT NULL DEFAULT 0,
    breaker_open_until   timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

-- Scoped to live rows, the lesson from migration 00007: a plain UNIQUE keeps a
-- deleted provider's name reserved forever, and the operator who deletes one
-- and recreates it with the same name gets a conflict they cannot see the
-- cause of.
CREATE UNIQUE INDEX edge_providers_name_live ON edge_providers (name)
    WHERE deleted_at IS NULL;

-- One row per provider, so the credential cannot outlive it or be orphaned.
-- Split from the provider for the same reason platform_credentials is: the
-- provider is read constantly for listings and health, and the sealed secret
-- has no business being on those result sets.
CREATE TABLE edge_credentials (
    id          uuid PRIMARY KEY,
    provider_id uuid NOT NULL UNIQUE REFERENCES edge_providers(id) ON DELETE CASCADE,

    -- AES-256-GCM envelope, identical to platform_credentials: a per-secret
    -- data key wrapped by the master key.
    ciphertext  bytea NOT NULL,
    nonce       bytea NOT NULL,
    dek_wrapped bytea NOT NULL,
    dek_nonce   bytea NOT NULL,
    key_version integer NOT NULL,

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The previous routing table, kept before every mutation (PUB-34).
--
-- This is the revert path, and it exists from the first migration rather than
-- being added when it is needed, because the moment it is needed is the moment
-- nobody can reach the portal to add it. The portal is published through the
-- tunnel it edits (docs/28 §28.10).
CREATE TABLE edge_config_snapshots (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES edge_providers(id) ON DELETE CASCADE,
    tunnel_id   text NOT NULL,
    -- The provider's own revision marker at the time of the read. Cloudflare
    -- increments one per configuration write, which makes a stale write
    -- detectable exactly rather than by diffing arrays (docs/28 §28.10).
    version     integer NOT NULL DEFAULT 0,
    -- The complete ingress array as read, including rules the portal did not
    -- create. A partial snapshot would restore a routing table that never
    -- existed.
    ingress     jsonb NOT NULL,
    taken_at    timestamptz NOT NULL DEFAULT now(),
    taken_by    uuid REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX edge_config_snapshots_recent
    ON edge_config_snapshots (provider_id, taken_at DESC);

-- +goose Down
DROP TABLE IF EXISTS edge_config_snapshots;
DROP TABLE IF EXISTS edge_credentials;
DROP TABLE IF EXISTS edge_providers;
