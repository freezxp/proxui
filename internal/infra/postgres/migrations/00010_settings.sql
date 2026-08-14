-- +goose Up
-- Runtime settings (docs/07-database-design.md §7.10, ADM-02). Values are
-- JSON so a setting can be a number, a string or a flag without a schema
-- change per key.
CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL
);

-- +goose Down
DROP TABLE IF EXISTS settings;
