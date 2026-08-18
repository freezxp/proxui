-- +goose Up

-- TOTP enrolment (AUTH-04, docs/15-security-design.md §15.1).
--
-- The seed is envelope-encrypted exactly like a platform credential: sealed
-- with a per-row data key, the data key wrapped by the master key, so a key
-- rotation rewraps totp_dek_wrapped and leaves the ciphertext alone. That needs
-- five columns, which is why the single `totp_secret_enc` bytea the original
-- schema reserved is dropped here rather than used — it was never written to.
ALTER TABLE users
    DROP COLUMN IF EXISTS totp_secret_enc,
    ADD COLUMN totp_ciphertext  bytea,
    ADD COLUMN totp_nonce       bytea,
    ADD COLUMN totp_dek_wrapped bytea,
    ADD COLUMN totp_dek_nonce   bytea,
    ADD COLUMN totp_key_version int NOT NULL DEFAULT 1,
    -- The last time step a code was accepted for. RFC 6238 codes stay valid
    -- for a whole step and the portal accepts one step either side, so without
    -- this a code read over someone's shoulder — or off a phished form — is
    -- replayable for up to 90 seconds. Refusing a step that has already been
    -- used makes every code good exactly once.
    ADD COLUMN totp_last_step   bigint;

-- A seed present with totp_enabled false is an enrolment nobody has confirmed
-- yet: it must not be demanded at login, and it must not survive as a
-- half-state that a later read mistakes for protection.
COMMENT ON COLUMN users.totp_ciphertext IS
    'Sealed TOTP seed. Present with totp_enabled=false means enrolment started but unconfirmed.';
COMMENT ON COLUMN users.totp_last_step IS
    'Highest RFC 6238 time step already accepted; refuses replay of a code within its window.';

-- +goose Down
ALTER TABLE users
    DROP COLUMN IF EXISTS totp_ciphertext,
    DROP COLUMN IF EXISTS totp_nonce,
    DROP COLUMN IF EXISTS totp_dek_wrapped,
    DROP COLUMN IF EXISTS totp_dek_nonce,
    DROP COLUMN IF EXISTS totp_key_version,
    DROP COLUMN IF EXISTS totp_last_step,
    ADD COLUMN totp_secret_enc bytea;
