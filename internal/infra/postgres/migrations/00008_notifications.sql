-- +goose Up
-- Notification delivery (docs/07-database-design.md §7.9, NOTIF-01..03).

CREATE TABLE notification_channels (
    id         uuid PRIMARY KEY,
    name       citext NOT NULL,
    kind       text NOT NULL,          -- email | slack | webhook
    -- Non-secret configuration: recipient addresses, SMTP host, webhook URL.
    config     jsonb NOT NULL DEFAULT '{}',
    -- Envelope-encrypted, exactly like platform credentials: an SMTP password,
    -- a Slack webhook URL (which is itself a bearer secret), or the HMAC key
    -- a webhook receiver verifies signatures with.
    --
    -- The design sketched this as a single secret_enc column. Envelope
    -- encryption needs five fields, and platform_credentials already stores
    -- them in exactly this shape; one representation for encrypted secrets is
    -- worth more than matching the sketch.
    ciphertext  bytea,
    nonce       bytea,
    dek_wrapped bytea,
    dek_nonce   bytea,
    key_version int NOT NULL DEFAULT 1,
    is_enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

-- Uniqueness scoped to live rows, so a deleted channel's name can be reused.
-- The platforms table learned this the hard way in migration 00007.
CREATE UNIQUE INDEX notification_channels_name_live_key
    ON notification_channels (name)
    WHERE deleted_at IS NULL;

CREATE TABLE notification_rules (
    id           uuid PRIMARY KEY,
    category     text NOT NULL,        -- sync_failure | vm_state_change | performance_alert | security
    min_severity text NOT NULL DEFAULT 'info',
    platform_id  uuid REFERENCES platforms(id) ON DELETE CASCADE,   -- NULL = every platform
    vm_group_id  uuid REFERENCES vm_groups(id) ON DELETE CASCADE,   -- NULL = every VM
    channel_id   uuid NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    is_enabled   boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ix_notification_rules_category ON notification_rules(category) WHERE is_enabled;

CREATE TABLE notification_deliveries (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    outbox_id   bigint,
    channel_id  uuid NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    -- Kept alongside the reference so the delivery log still reads sensibly
    -- after a channel is renamed, and survives as history in its own right.
    subject     text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'pending',   -- pending | sent | failed
    attempts    int  NOT NULL DEFAULT 0,
    last_error  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    sent_at     timestamptz
);

CREATE INDEX ix_deliveries_recent ON notification_deliveries(created_at DESC);
CREATE INDEX ix_deliveries_channel ON notification_deliveries(channel_id, created_at DESC);

-- One delivery per event per channel. Retries update the row rather than
-- adding another, and a relay that re-publishes after a crash cannot produce
-- a second message to the same channel (NOTIF-03).
CREATE UNIQUE INDEX notification_deliveries_event_channel_key
    ON notification_deliveries (outbox_id, channel_id)
    WHERE outbox_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notification_rules;
DROP TABLE IF EXISTS notification_channels;
