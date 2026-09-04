-- +goose Up

-- The other addresses a platform answers on (ADR 0009).
--
-- A platform is configured with one endpoint and reached through a list of
-- them. Until now it was reached through the one, and on 4 September 2026 that
-- turned one node's power switch into a twenty-minute outage of the whole
-- portal: `endpoint_url` pointed at the node that went down, so inventory,
-- metrics and consoles stopped for three nodes that were up and quorate the
-- entire time. Proxmox serves the same clustered API from every member; the
-- restriction was ours.
--
-- Its own table rather than an array column on `platforms`: these rows are
-- cluster facts, rewritten by the sync engine on its own cadence, while
-- `platforms.endpoint_url` is the address an administrator typed. Keeping the
-- two apart means discovery never edits configuration, and an operator reading
-- the platform form still sees what they entered.
CREATE TABLE platform_endpoints (
    platform_id  uuid NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    -- host or host:port, as the cluster reports it.
    address      text NOT NULL,
    -- This address's own certificate pin, empty under every TLS mode but
    -- `fingerprint`. Cluster members each present a different certificate, so
    -- one pin cannot cover them all -- and a member whose pin is unknown is
    -- never stored, because failing over to an address we cannot verify would
    -- relax trust at precisely the moment the network is misbehaving.
    fingerprint  text NOT NULL DEFAULT '',
    -- `configured` seeds the list from endpoint_url; `discovered` rows come
    -- from /cluster/status and are replaced wholesale by each refresh.
    source       text NOT NULL DEFAULT 'discovered',
    -- When discovery last saw this member, which is what makes the list
    -- refreshable on a slower cadence than the sync that fills it.
    refreshed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (platform_id, address)
);

CREATE INDEX ix_platform_endpoints ON platform_endpoints (platform_id, refreshed_at DESC);

-- +goose Down
DROP TABLE IF EXISTS platform_endpoints;
