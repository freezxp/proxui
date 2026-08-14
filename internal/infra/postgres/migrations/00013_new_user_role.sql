-- +goose NO TRANSACTION
-- +goose Up
-- A role for an account that exists but has been granted nothing: it can sign
-- in, change its own password, and see a page telling it to ask for access.
--
-- Self-registration previously landed people in "readonly", which sees an
-- empty inventory but also the hosts, storage and networks that make up the
-- estate. This separates "someone an administrator trusts to look" from
-- "someone who has just signed up".
--
-- ALTER TYPE ... ADD VALUE cannot run inside a transaction block, hence the
-- NO TRANSACTION above. It is also why the new value cannot be used until
-- this statement has committed, which is why no data change accompanies it.
ALTER TYPE user_role ADD VALUE IF NOT EXISTS 'newuser';

-- +goose Down
-- Postgres cannot remove a value from an enum. Reversing this means moving
-- any account holding it back to readonly and rebuilding the type, which is a
-- data decision rather than a schema one — so the down migration deliberately
-- does nothing rather than pretending otherwise.
SELECT 1;
