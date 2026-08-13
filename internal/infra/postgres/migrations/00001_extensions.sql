-- +goose Up
-- Extensions the schema depends on:
--   citext    case-insensitive usernames/emails and unique names
--   pg_trgm   substring search on VM names (docs/07: ix_vms_name_trgm)
--   timescaledb  hypertables for the metrics pipeline (docs/07 §7.8)
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- +goose Down
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS citext;
-- timescaledb is intentionally left installed: dropping it would destroy
-- hypertable metadata, and it is harmless when unused.
