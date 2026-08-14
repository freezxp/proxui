-- +goose Up
-- Some settings are secrets. A Google client secret in the value column would
-- sit there in plain text, readable by anything with database access and
-- copied into every backup unencrypted.
--
-- Same envelope encryption as platform credentials and notification channels:
-- a per-secret data key, wrapped by the master key. The value column stays for
-- everything that is not a secret.
ALTER TABLE settings
    ADD COLUMN ciphertext  bytea,
    ADD COLUMN nonce       bytea,
    ADD COLUMN dek_wrapped bytea,
    ADD COLUMN dek_nonce   bytea,
    ADD COLUMN key_version int NOT NULL DEFAULT 1;

-- The value column is now optional: a secret setting stores its bytes in the
-- columns above and nothing in value.
ALTER TABLE settings ALTER COLUMN value DROP NOT NULL;

-- +goose Down
ALTER TABLE settings
    DROP COLUMN IF EXISTS ciphertext,
    DROP COLUMN IF EXISTS nonce,
    DROP COLUMN IF EXISTS dek_wrapped,
    DROP COLUMN IF EXISTS dek_nonce,
    DROP COLUMN IF EXISTS key_version;
