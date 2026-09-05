-- +goose Up

-- Provisioning and destruction requests (PROV-05, PROV-06, ADR 0010).
--
-- A clone is a platform task that runs for minutes, followed by three more
-- steps that must happen in order and only if the one before succeeded. Doing
-- that inside the HTTP request that asked for it would time out, and would
-- leave nothing to resume from when the portal restarts halfway through. So
-- the request is a row, and a job advances it.
--
-- Destruction shares the table rather than getting its own. It is a single
-- asynchronous task where provisioning is four, but it wants the same things:
-- durability across a restart, one place to look up "what happened to that
-- request", and one status endpoint. `kind` tells them apart.
CREATE TABLE provision_requests (
    id           uuid PRIMARY KEY,
    platform_id  uuid NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
    kind         text NOT NULL,                    -- provision | destroy
    -- provision: pending → cloning → configuring → resizing → starting → ready
    -- destroy:   pending → deleting → deleted
    -- either:    failed
    state        text NOT NULL DEFAULT 'pending',
    -- The step that is running, or the one that failed. Kept separately from
    -- `error` because "which step" and "what went wrong" answer different
    -- questions and an operator reading a failed request needs both.
    step         text NOT NULL DEFAULT '',

    requested_by uuid REFERENCES users(id) ON DELETE SET NULL,
    -- Denormalized like the audit trail's actor_name: who asked is part of the
    -- record, and deleting the account must not erase it.
    requested_by_name text NOT NULL DEFAULT '',

    template_external_id text NOT NULL DEFAULT '',
    target_node  text NOT NULL DEFAULT '',
    guest_name   text NOT NULL DEFAULT '',
    -- The platform-side id, empty until the clone has been given one.
    vmid         text NOT NULL DEFAULT '',
    -- Where the finished guest should land so the person who asked for it can
    -- see it. A newly created guest belongs to no group, and a group is what
    -- makes a VM visible to anyone but an administrator.
    vm_group_id  uuid REFERENCES vm_groups(id) ON DELETE SET NULL,

    -- The cloud-init and sizing input. There is no secret in here and there
    -- cannot be: the spec type has no password field, by decision (PROV-04).
    spec         jsonb NOT NULL DEFAULT '{}',
    -- The UPID of the platform task currently being waited on.
    task_id      text NOT NULL DEFAULT '',
    error        text NOT NULL DEFAULT '',

    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ix_provision_requests_platform ON provision_requests (platform_id, created_at DESC);

-- The resume sweep asks only for requests still in motion, which is a small
-- fraction of the table forever after.
CREATE INDEX ix_provision_requests_open ON provision_requests (state, updated_at)
    WHERE state NOT IN ('ready', 'deleted', 'failed');

-- +goose Down
DROP TABLE IF EXISTS provision_requests;
