-- +goose Up

-- Access context (docs/07-database-design.md §7.5): roles say what a user may
-- do, groups say which VMs they may do it to. VM groups are created here; the
-- vms table itself arrives with the inventory sprint, so vm_group_members is
-- added then to keep the foreign key honest.

CREATE TABLE user_groups (
    id          uuid PRIMARY KEY,
    name        citext NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_group_members (
    user_group_id uuid NOT NULL REFERENCES user_groups(id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    added_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_group_id, user_id)
);
CREATE INDEX ix_ugm_user ON user_group_members(user_id);

CREATE TABLE vm_groups (
    id          uuid PRIMARY KEY,
    name        citext NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    -- auto_rule maps a platform pool or tag onto this group so newly synced
    -- VMs are grouped without manual work (RBAC-06).
    auto_rule   jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE access_grants (
    id            uuid PRIMARY KEY,
    user_group_id uuid NOT NULL REFERENCES user_groups(id) ON DELETE CASCADE,
    vm_group_id   uuid NOT NULL REFERENCES vm_groups(id) ON DELETE CASCADE,
    granted_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_group_id, vm_group_id)
);
CREATE INDEX ix_grants_vm_group ON access_grants(vm_group_id);

-- +goose Down
DROP TABLE IF EXISTS access_grants;
DROP TABLE IF EXISTS vm_groups;
DROP TABLE IF EXISTS user_group_members;
DROP TABLE IF EXISTS user_groups;
