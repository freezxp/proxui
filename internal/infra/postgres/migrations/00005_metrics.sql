-- +goose NO TRANSACTION
-- +goose Up

-- Metrics storage (docs/07-database-design.md §7.8).
--
-- One hypertable per subject holds raw samples; continuous aggregates roll them
-- up so a year-long chart never scans raw data. Retention differs per
-- resolution: raw samples answer "what is happening now" and are dropped in
-- days, while three-hour buckets answer "how has this grown" and are kept for
-- over a year. Timescale runs all of it as policies, so the application never
-- writes rollup code.

CREATE TABLE metrics_vm (
    time            timestamptz NOT NULL,
    vm_id           uuid NOT NULL,
    cpu_pct         real,
    mem_used_bytes  bigint,
    mem_total_bytes bigint,
    disk_read_bps   bigint,
    disk_write_bps  bigint,
    net_rx_bps      bigint,
    net_tx_bps      bigint,
    disk_used_bytes bigint
);
SELECT create_hypertable('metrics_vm', 'time', chunk_time_interval => interval '1 day');
CREATE INDEX ix_metrics_vm ON metrics_vm (vm_id, time DESC);

CREATE TABLE metrics_host (
    time            timestamptz NOT NULL,
    host_id         uuid NOT NULL,
    cpu_pct         real,
    mem_used_bytes  bigint,
    mem_total_bytes bigint,
    load1           real,
    rootfs_used_bytes bigint
);
SELECT create_hypertable('metrics_host', 'time', chunk_time_interval => interval '1 day');
CREATE INDEX ix_metrics_host ON metrics_host (host_id, time DESC);

-- Continuous aggregates. Each keeps avg and max: an average hides the spike
-- that woke someone up, and a max alone misleads about sustained load.
--
-- All three read the raw hypertable rather than stacking on each other.
-- Timescale rejects an aggregate bucketed on another aggregate's bucket
-- column, and at this scale the extra scan is irrelevant.
CREATE MATERIALIZED VIEW metrics_vm_5m
WITH (timescaledb.continuous) AS
SELECT time_bucket('5 minutes', time) AS bucket,
       vm_id,
       avg(cpu_pct)::real          AS cpu_pct_avg,
       max(cpu_pct)::real          AS cpu_pct_max,
       avg(mem_used_bytes)::bigint AS mem_used_bytes_avg,
       max(mem_used_bytes)::bigint AS mem_used_bytes_max,
       max(mem_total_bytes)::bigint AS mem_total_bytes,
       avg(disk_read_bps)::bigint  AS disk_read_bps_avg,
       avg(disk_write_bps)::bigint AS disk_write_bps_avg,
       avg(net_rx_bps)::bigint     AS net_rx_bps_avg,
       avg(net_tx_bps)::bigint     AS net_tx_bps_avg,
       max(disk_used_bytes)::bigint AS disk_used_bytes
FROM metrics_vm
GROUP BY bucket, vm_id
WITH NO DATA;

CREATE MATERIALIZED VIEW metrics_vm_30m
WITH (timescaledb.continuous) AS
SELECT time_bucket('30 minutes', time) AS bucket,
       vm_id,
       avg(cpu_pct)::real           AS cpu_pct_avg,
       max(cpu_pct)::real           AS cpu_pct_max,
       avg(mem_used_bytes)::bigint  AS mem_used_bytes_avg,
       max(mem_used_bytes)::bigint  AS mem_used_bytes_max,
       max(mem_total_bytes)::bigint AS mem_total_bytes,
       avg(disk_read_bps)::bigint   AS disk_read_bps_avg,
       avg(disk_write_bps)::bigint  AS disk_write_bps_avg,
       avg(net_rx_bps)::bigint      AS net_rx_bps_avg,
       avg(net_tx_bps)::bigint      AS net_tx_bps_avg,
       max(disk_used_bytes)::bigint AS disk_used_bytes
FROM metrics_vm
GROUP BY bucket, vm_id
WITH NO DATA;

CREATE MATERIALIZED VIEW metrics_vm_3h
WITH (timescaledb.continuous) AS
SELECT time_bucket('3 hours', time) AS bucket,
       vm_id,
       avg(cpu_pct)::real           AS cpu_pct_avg,
       max(cpu_pct)::real           AS cpu_pct_max,
       avg(mem_used_bytes)::bigint  AS mem_used_bytes_avg,
       max(mem_used_bytes)::bigint  AS mem_used_bytes_max,
       max(mem_total_bytes)::bigint AS mem_total_bytes,
       avg(disk_read_bps)::bigint   AS disk_read_bps_avg,
       avg(disk_write_bps)::bigint  AS disk_write_bps_avg,
       avg(net_rx_bps)::bigint      AS net_rx_bps_avg,
       avg(net_tx_bps)::bigint      AS net_tx_bps_avg,
       max(disk_used_bytes)::bigint AS disk_used_bytes
FROM metrics_vm
GROUP BY bucket, vm_id
WITH NO DATA;

CREATE MATERIALIZED VIEW metrics_host_5m
WITH (timescaledb.continuous) AS
SELECT time_bucket('5 minutes', time) AS bucket,
       host_id,
       avg(cpu_pct)::real           AS cpu_pct_avg,
       max(cpu_pct)::real           AS cpu_pct_max,
       avg(mem_used_bytes)::bigint  AS mem_used_bytes_avg,
       max(mem_total_bytes)::bigint AS mem_total_bytes
FROM metrics_host
GROUP BY bucket, host_id
WITH NO DATA;

-- +goose StatementBegin
DO $$
BEGIN
    -- Refresh policies. Each lags slightly behind real time so a bucket is
    -- only materialized once its samples have all arrived.
    PERFORM add_continuous_aggregate_policy('metrics_vm_5m',
        start_offset => interval '2 hours', end_offset => interval '5 minutes',
        schedule_interval => interval '5 minutes');
    -- start_offset stays inside the 48h raw retention: refreshing a window
    -- whose source rows have been dropped would quietly produce empty buckets.
    PERFORM add_continuous_aggregate_policy('metrics_vm_30m',
        start_offset => interval '1 day', end_offset => interval '30 minutes',
        schedule_interval => interval '30 minutes');
    PERFORM add_continuous_aggregate_policy('metrics_vm_3h',
        start_offset => interval '1 day', end_offset => interval '3 hours',
        schedule_interval => interval '1 hour');
    PERFORM add_continuous_aggregate_policy('metrics_host_5m',
        start_offset => interval '2 hours', end_offset => interval '5 minutes',
        schedule_interval => interval '5 minutes');

    -- Compression: raw samples older than a day are read rarely and compress
    -- roughly tenfold, which is what keeps a year of history on one disk.
    ALTER TABLE metrics_vm SET (timescaledb.compress, timescaledb.compress_segmentby = 'vm_id');
    ALTER TABLE metrics_host SET (timescaledb.compress, timescaledb.compress_segmentby = 'host_id');
    PERFORM add_compression_policy('metrics_vm', interval '1 day');
    PERFORM add_compression_policy('metrics_host', interval '1 day');

    -- Retention per resolution (docs/03-frs.md PERF-03).
    PERFORM add_retention_policy('metrics_vm', interval '48 hours');
    PERFORM add_retention_policy('metrics_host', interval '48 hours');
    PERFORM add_retention_policy('metrics_vm_5m', interval '30 days');
    PERFORM add_retention_policy('metrics_vm_30m', interval '180 days');
    PERFORM add_retention_policy('metrics_vm_3h', interval '400 days');
    PERFORM add_retention_policy('metrics_host_5m', interval '30 days');
EXCEPTION WHEN OTHERS THEN
    -- Policies are an optimization, not a correctness requirement: a Timescale
    -- build without the background worker (or a community edition difference)
    -- must not stop the schema from being usable.
    RAISE WARNING 'could not install all Timescale policies: %', SQLERRM;
END $$;
-- +goose StatementEnd

-- Tracks the last cumulative counter value per VM so the collector can turn
-- counters into rates and recognise a reset (docs/10-sync-engine.md §10.6).
CREATE TABLE metrics_counter_state (
    vm_id      uuid NOT NULL,
    metric     text NOT NULL,
    last_value double precision NOT NULL,
    last_time  timestamptz NOT NULL,
    PRIMARY KEY (vm_id, metric)
);

-- +goose Down
DROP TABLE IF EXISTS metrics_counter_state;
DROP MATERIALIZED VIEW IF EXISTS metrics_host_5m;
DROP MATERIALIZED VIEW IF EXISTS metrics_vm_3h;
DROP MATERIALIZED VIEW IF EXISTS metrics_vm_30m;
DROP MATERIALIZED VIEW IF EXISTS metrics_vm_5m;
DROP TABLE IF EXISTS metrics_host;
DROP TABLE IF EXISTS metrics_vm;
