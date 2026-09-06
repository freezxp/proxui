-- +goose Up

-- Installing an application into an LXC container on a node (APP-01…APP-06,
-- ADR 0012).
--
-- A separate table rather than a `kind` on provision_requests, because that
-- table is guest-shaped: it carries a template, a cloud-init spec and a VM
-- group, and its state machine ends in `verifying`, which waits for a guest
-- agent an LXC does not have. Sharing it would mean a final state that could
-- never be reached.
--
-- The record is durable for the same reason a provisioning request is: the work
-- is minutes long and outlives the HTTP call that asked for it, and a portal
-- restarted halfway must be able to pick the answer back up. Here it can do
-- more than resume — the script's log lives on the node, so the outcome is
-- still there to be read even if nothing in the portal remembers asking.
CREATE TABLE container_deployments (
    id           uuid PRIMARY KEY,
    platform_id  uuid NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    node         text NOT NULL,

    -- The catalogue identifier, and the name as it was at deploy time. The name
    -- is denormalized on purpose: bumping the pinned upstream can rename an
    -- application, and the record should say what was deployed rather than what
    -- that identifier means today.
    app_id       text NOT NULL,
    app_name     text NOT NULL DEFAULT '',

    -- pending → deploying → ready | failed
    state        text NOT NULL DEFAULT 'pending',

    -- The container the script created. Empty until the script has said which:
    -- it allocates the id, not the portal.
    --
    -- Named container_id rather than ctid, which is what Proxmox calls it and
    -- what the Go field is called: `ctid` is a Postgres system column that
    -- every table already has, and CREATE TABLE refuses to shadow it.
    container_id text NOT NULL DEFAULT '',

    requested_by uuid REFERENCES users(id) ON DELETE SET NULL,
    -- Denormalized like the audit trail's actor_name: who asked is part of the
    -- record, and deleting the account must not erase it.
    requested_by_name text NOT NULL DEFAULT '',

    -- What the operator chose: hostname, cores, memory, disk, storage, bridge.
    -- Every field is validated before it reaches the node and every one is
    -- optional, an empty one meaning the script's own default.
    spec         jsonb NOT NULL DEFAULT '{}',

    -- What the script printed, truncated to its last 256 kB. Kept because a
    -- deploy that fails halfway cannot be explained by a state name, and
    -- nothing else in the portal has ever kept the output of a non-interactive
    -- command run on a node.
    log          text NOT NULL DEFAULT '',
    -- The script's exit status, once it has one.
    exit_code    integer,
    error        text NOT NULL DEFAULT '',

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ix_container_deployments_platform
    ON container_deployments (platform_id, created_at DESC);

-- The resume sweep asks only for deployments still in motion, which is a small
-- fraction of the table forever after.
CREATE INDEX ix_container_deployments_open
    ON container_deployments (state, updated_at)
    WHERE state NOT IN ('ready', 'failed');

-- +goose Down
DROP TABLE IF EXISTS container_deployments;
