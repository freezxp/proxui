-- +goose Up

-- SSH sessions (SSH-06, docs/29-ssh-terminal.md).
--
-- The same shape as console_sessions and deliberately a separate table: an SSH
-- session records things a console session has no notion of - which account on
-- the guest was used, and at which address - and a console session records a
-- console kind that SSH has no notion of. Merging them would mean a table where
-- half the columns are null depending on a discriminator.
--
-- What is NOT here is the credential. It is typed per session and held only in
-- the memory of the process serving it (SSH-03); the row records who connected
-- as whom, which is the part an audit needs and the part that is not a secret.
CREATE TABLE ssh_sessions (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    vm_id        uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
    ssh_user     text NOT NULL,
    address      text NOT NULL,
    client_ip    inet,
    user_agent   text,
    started_at   timestamptz NOT NULL DEFAULT now(),
    connected_at timestamptz,
    ended_at     timestamptz,
    close_reason text NOT NULL DEFAULT '',
    bytes_tx     bigint NOT NULL DEFAULT 0,
    bytes_rx     bigint NOT NULL DEFAULT 0
);
CREATE INDEX ix_ssh_vm   ON ssh_sessions(vm_id, started_at DESC);
CREATE INDEX ix_ssh_user ON ssh_sessions(user_id, started_at DESC);
CREATE INDEX ix_ssh_open ON ssh_sessions(started_at DESC) WHERE ended_at IS NULL;

-- Pinned host keys (SSH-04).
--
-- One row per VM rather than per address: a guest that gets a new DHCP lease is
-- the same machine, and treating it as a new trust decision would train
-- operators to click past the one warning that matters. The address is kept for
-- display - "trusted at 192.168.100.40 on 16 August" is what makes a later
-- mismatch legible.
--
-- Nothing here is secret: a host key is public by construction. It is pinned so
-- that a *change* is detectable, which is the only property that matters.
CREATE TABLE ssh_known_hosts (
    vm_id         uuid PRIMARY KEY REFERENCES vms(id) ON DELETE CASCADE,
    address       text NOT NULL,
    algorithm     text NOT NULL,
    fingerprint   text NOT NULL,
    public_key    bytea NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    trusted_by    uuid REFERENCES users(id) ON DELETE SET NULL
);

-- +goose Down
DROP TABLE IF EXISTS ssh_known_hosts;
DROP TABLE IF EXISTS ssh_sessions;
