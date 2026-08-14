-- +goose Up
-- Alert rules and their per-VM state (docs/07-database-design.md §7.9,
-- NOTIF-04, NOTIF-05).

CREATE TABLE alert_rules (
    id          uuid PRIMARY KEY,
    name        citext NOT NULL,
    metric      text NOT NULL,                  -- cpu_pct | mem_pct | disk_read_bps | ...
    op          text NOT NULL DEFAULT '>',
    threshold   real NOT NULL,
    -- How long the breach must persist before it fires. A VM at 100% for one
    -- sample is a build starting; at 100% for ten minutes it is a problem.
    duration_s  int NOT NULL DEFAULT 600,
    vm_group_id uuid REFERENCES vm_groups(id) ON DELETE CASCADE,  -- NULL = every VM
    severity    text NOT NULL DEFAULT 'warning',
    -- Suppresses repeat notifications while a rule stays firing (NOTIF-05).
    cooldown_s  int NOT NULL DEFAULT 1800,
    is_enabled  boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);

CREATE UNIQUE INDEX alert_rules_name_live_key
    ON alert_rules (name)
    WHERE deleted_at IS NULL;

CREATE TABLE alert_states (
    alert_rule_id    uuid NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    vm_id            uuid NOT NULL REFERENCES vms(id) ON DELETE CASCADE,
    state            text NOT NULL DEFAULT 'ok',   -- ok | pending | firing
    -- When the current state began. For a pending breach this is when the
    -- metric first crossed the threshold, which is what the sustained
    -- duration is measured against.
    since            timestamptz NOT NULL DEFAULT now(),
    last_value       real,
    last_notified_at timestamptz,
    PRIMARY KEY (alert_rule_id, vm_id)
);

CREATE INDEX ix_alert_states_firing ON alert_states(alert_rule_id) WHERE state = 'firing';

-- +goose Down
DROP TABLE IF EXISTS alert_states;
DROP TABLE IF EXISTS alert_rules;
