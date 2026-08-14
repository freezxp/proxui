-- +goose Up
-- Platform deletion is a soft delete, but the name uniqueness constraint did
-- not know that: once a platform was deleted its name stayed reserved forever,
-- and re-adding it failed with "A platform with that name already exists" —
-- naming a row the administrator can no longer see.
--
-- Replacing the constraint with a partial index scopes uniqueness to live
-- platforms, which is what it always meant.
ALTER TABLE platforms DROP CONSTRAINT platforms_name_key;

CREATE UNIQUE INDEX platforms_name_live_key
    ON platforms (name)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX platforms_name_live_key;

ALTER TABLE platforms ADD CONSTRAINT platforms_name_key UNIQUE (name);
