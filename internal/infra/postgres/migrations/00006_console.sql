-- +goose Up

-- Console sessions (docs/07-database-design.md §7.7, CONS-04).
--
-- Every console a user opens is recorded: who, which VM, when, for how long,
-- and why it ended. This is the audit surface for the portal's most sensitive
-- capability - direct keyboard access to a machine - so the row is written when
-- the session starts, not when it ends, and survives a crashed bridge.
CREATE TABLE console_sessions (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    vm_id        uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
    kind         text NOT NULL DEFAULT 'vnc',
    client_ip    inet,
    user_agent   text,
    started_at   timestamptz NOT NULL DEFAULT now(),
    connected_at timestamptz,
    ended_at     timestamptz,
    close_reason text NOT NULL DEFAULT '',
    bytes_tx     bigint NOT NULL DEFAULT 0,
    bytes_rx     bigint NOT NULL DEFAULT 0
);
CREATE INDEX ix_console_vm    ON console_sessions(vm_id, started_at DESC);
CREATE INDEX ix_console_user  ON console_sessions(user_id, started_at DESC);
CREATE INDEX ix_console_open  ON console_sessions(started_at DESC) WHERE ended_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS console_sessions;
