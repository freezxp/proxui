-- +goose NO TRANSACTION
-- +goose Up

-- Node hardware sensors (SENSOR-01…SENSOR-05, docs/adr/0007).
--
-- Readings are stored as the hardware reports them — chip, label, and the
-- thresholds the chip declares for itself — rather than reduced to one number
-- per host on the way in. Which sensor is hot is the question that follows
-- "something is hot", and a maximum cannot answer it.
--
-- Its own table rather than columns on metrics_host: that table is wide and
-- fixed, one row per host per interval, and the set of sensors differs per
-- machine and changes when hardware does.
CREATE TABLE host_sensors (
    time     timestamptz NOT NULL,
    host_id  uuid NOT NULL,
    -- As `sensors` prints them: "coretemp-isa-0000", "Package id 0".
    chip     text NOT NULL,
    label    text NOT NULL,
    kind     text NOT NULL,            -- temp_c | fan_rpm
    value    real NOT NULL,
    -- The chip's own limits, absent on hardware that declares none. 80°C means
    -- something different on a package than on a VRM, and only the chip knows.
    high     real,
    crit     real
);
SELECT create_hypertable('host_sensors', 'time', chunk_time_interval => interval '1 day');
CREATE INDEX ix_host_sensors ON host_sensors (host_id, time DESC);
CREATE INDEX ix_host_sensors_series ON host_sensors (host_id, chip, label, time DESC);

CREATE MATERIALIZED VIEW host_sensors_5m
WITH (timescaledb.continuous) AS
SELECT time_bucket('5 minutes', time) AS bucket,
       host_id, chip, label, kind,
       avg(value)::real AS value_avg,
       max(value)::real AS value_max,
       max(crit)::real  AS crit
FROM host_sensors
GROUP BY bucket, host_id, chip, label, kind
WITH NO DATA;

-- Where a node is reached and which key it presented.
--
-- The key is pinned the first time the collector connects and a change is
-- refused from then on. Nobody is present at that first connection to compare
-- a fingerprint — the collector runs on the scheduler — so this is
-- trust-on-first-use, weaker than the operator-confirmed pinning SSH-04 gives
-- a guest. What it buys is the part that matters afterwards: the node cannot
-- be swapped underneath a portal that has already met it. The fingerprint is
-- shown on the host page so the comparison can still be made, late, by hand.
CREATE TABLE host_ssh (
    host_id       uuid PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    address       text NOT NULL,
    ssh_user      text NOT NULL DEFAULT 'root',
    algorithm     text NOT NULL,
    fingerprint   text NOT NULL,
    public_key    bytea NOT NULL,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    -- The last attempt, whatever came of it. A node with no key installed is
    -- the normal starting state, not an error, and the host page says which
    -- of the two it is looking at.
    last_tried_at timestamptz,
    last_ok_at    timestamptz,
    last_error    text
);

-- Alert rules gain a subject. Every rule until now was over a VM, and a rule
-- about a node's hardware has no VM to name.
ALTER TABLE alert_rules ADD COLUMN subject text NOT NULL DEFAULT 'vm';
ALTER TABLE alert_states ADD COLUMN host_id uuid REFERENCES hosts(id) ON DELETE CASCADE;

-- The primary key was (rule, vm). It has to admit a host row now, and exactly
-- one of the two subjects must be present. The key goes first: Postgres
-- refuses to drop NOT NULL from a column while a primary key still needs it.
ALTER TABLE alert_states DROP CONSTRAINT alert_states_pkey;
ALTER TABLE alert_states ALTER COLUMN vm_id DROP NOT NULL;
ALTER TABLE alert_states ADD CONSTRAINT alert_states_subject
    CHECK ((vm_id IS NULL) <> (host_id IS NULL));
CREATE UNIQUE INDEX alert_states_vm_key   ON alert_states (alert_rule_id, vm_id)   WHERE vm_id IS NOT NULL;
CREATE UNIQUE INDEX alert_states_host_key ON alert_states (alert_rule_id, host_id) WHERE host_id IS NOT NULL;

-- +goose StatementBegin
DO $$
BEGIN
    PERFORM add_continuous_aggregate_policy('host_sensors_5m',
        start_offset => interval '2 hours', end_offset => interval '5 minutes',
        schedule_interval => interval '5 minutes');

    -- Raw readings match the raw metrics retention; the rollup matches the
    -- 5-minute one. A temperature nobody looked at within two days is a
    -- temperature the rollup answers for.
    PERFORM add_retention_policy('host_sensors', interval '48 hours');
    PERFORM add_retention_policy('host_sensors_5m', interval '30 days');
    ALTER TABLE host_sensors SET (timescaledb.compress, timescaledb.compress_segmentby = 'host_id');
    PERFORM add_compression_policy('host_sensors', interval '1 day');
EXCEPTION WHEN OTHERS THEN
    -- Policies are an optimization, not a correctness requirement, and the
    -- metrics migration makes the same accommodation.
    RAISE WARNING 'could not install all Timescale policies: %', SQLERRM;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS alert_states_host_key;
DROP INDEX IF EXISTS alert_states_vm_key;
ALTER TABLE alert_states DROP CONSTRAINT IF EXISTS alert_states_subject;
ALTER TABLE alert_states DROP COLUMN IF EXISTS host_id;
ALTER TABLE alert_rules DROP COLUMN IF EXISTS subject;
DROP TABLE IF EXISTS host_ssh;
DROP MATERIALIZED VIEW IF EXISTS host_sensors_5m;
DROP TABLE IF EXISTS host_sensors;
