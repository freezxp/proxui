-- +goose Up
-- Accounts can now come from somewhere other than an administrator typing
-- them in: self-registration, and Google (docs/adr/0003).

ALTER TABLE users
    ADD COLUMN auth_provider text NOT NULL DEFAULT 'local',
    ADD COLUMN external_id   text;

-- One portal account per external identity. Without this a second sign-in
-- from the same Google account could create a duplicate user.
CREATE UNIQUE INDEX users_external_identity_key
    ON users (auth_provider, external_id)
    WHERE external_id IS NOT NULL;

-- An account that signs in through a provider has no password of its own.
-- The column stays NOT NULL and holds an empty string, which no hash can
-- ever match, so a password login against such an account fails on the
-- comparison as well as on the provider check.
ALTER TABLE users ALTER COLUMN password_hash DROP DEFAULT;

-- +goose Down
DROP INDEX IF EXISTS users_external_identity_key;
ALTER TABLE users DROP COLUMN IF EXISTS external_id;
ALTER TABLE users DROP COLUMN IF EXISTS auth_provider;
