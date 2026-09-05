-- +goose Up

-- Favourites and personal folders (INV-16…INV-19).
--
-- The portal's first per-user state. `settings` is a global key-value table and
-- nothing until now stored a preference against a person, so these three tables
-- establish the shape: everything is keyed by the user it belongs to, and
-- nothing here is visible to anybody else.
--
-- Deliberately NOT vm_groups. A VM group is what a user group is granted
-- (`access_grants`), so it is part of the permission model; if arranging your
-- own view meant editing those, tidying your sidebar would change who can see
-- what. These tables touch nothing in the access model and are read only ever
-- for the user who wrote them.

-- A favourite is an opinion, not a fact about the machine, so two people can
-- disagree and both be right.
CREATE TABLE vm_favourites (
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    vm_id      uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, vm_id)
);

-- Listing one user's favourites, which is what every inventory query does.
CREATE INDEX ix_vm_favourites_user ON vm_favourites (user_id);

CREATE TABLE vm_folders (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       text NOT NULL,
    -- Where the folder sits in the user's own ordering. Names sort
    -- alphabetically by default, which is rarely the order someone wants their
    -- own folders in.
    position   integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Two folders with the same name would be indistinguishable in a picker.
    UNIQUE (user_id, name)
);

CREATE INDEX ix_vm_folders_user ON vm_folders (user_id, position, name);

-- Which folder a user has filed a VM into.
--
-- The primary key is the whole design. `(user_id, vm_id)` makes "one folder per
-- VM per user" a constraint rather than a convention, so "where is this VM"
-- always has exactly one answer. That is why user_id is carried here rather
-- than reached through folder_id: the key could not say it otherwise.
CREATE TABLE vm_folder_members (
    user_id   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    vm_id     uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
    -- Deleting a folder frees the VMs that were in it rather than removing
    -- them: they become unfiled, which is where they started.
    folder_id uuid NOT NULL REFERENCES vm_folders(id) ON DELETE CASCADE,
    filed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, vm_id)
);

CREATE INDEX ix_vm_folder_members_folder ON vm_folder_members (folder_id);

-- +goose Down
DROP TABLE IF EXISTS vm_folder_members;
DROP TABLE IF EXISTS vm_folders;
DROP TABLE IF EXISTS vm_favourites;
