-- +goose Up
-- Apps this portal has published through an edge provider (docs/28 P4).
--
-- The portal owns this table; the live routing rule and DNS record are owned
-- by Cloudflare. That split is deliberate and mirrors the platform rule: what
-- is actually serving traffic is read from the provider on every look, and
-- this table records only what the portal decided and needs in order to undo
-- it.
CREATE TABLE published_apps (
    id          uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES edge_providers(id) ON DELETE CASCADE,

    -- hostname + path is what makes a routing rule the same rule, so it is
    -- what makes an app the same app.
    hostname citext NOT NULL,
    path     text NOT NULL DEFAULT '',

    -- The resolved target, e.g. http://10.0.13.10:8080. Stored resolved rather
    -- than as a VM reference so that the rule the portal would write is
    -- knowable without reaching the inventory — including when the VM has
    -- since been deleted, which is exactly when you need to know.
    service_url text NOT NULL,

    -- Where the target came from, when it came from the inventory. Nullable
    -- and NOT cascading: deleting a VM must not silently delete an
    -- internet-facing route. It surfaces as orphaned and a human decides.
    vm_id   uuid REFERENCES vms(id) ON DELETE SET NULL,
    vm_port integer,

    -- Per-rule provider settings the portal wrote (noTLSVerify and friends).
    origin_request jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- The DNS record the portal created, so unpublishing removes that record
    -- and never one somebody else made on the same name (PUB-23). Null means
    -- the portal did not create one.
    dns_zone_id   text,
    dns_record_id text,

    is_enabled boolean NOT NULL DEFAULT true,

    -- Acknowledgement that this hostname is reachable by anyone, recorded
    -- because it is the most consequential thing this feature does (PUB-43).
    exposure_ack_by uuid REFERENCES users(id) ON DELETE SET NULL,
    exposure_ack_at timestamptz,

    last_applied_at timestamptz,
    last_error      text NOT NULL DEFAULT '',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

-- One live app per hostname+path on a provider. Scoped to live rows so an
-- unpublished hostname can be published again — the lesson from migration
-- 00007, where a deleted platform reserved its name forever.
CREATE UNIQUE INDEX published_apps_route_live
    ON published_apps (provider_id, hostname, path)
    WHERE deleted_at IS NULL;

CREATE INDEX published_apps_by_vm ON published_apps (vm_id) WHERE vm_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS published_apps;
