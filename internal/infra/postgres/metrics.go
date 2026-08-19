package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// MetricsRepository writes samples and reads them back at the resolution a
// query needs.
type MetricsRepository struct{ db *Pool }

// NewMetricsRepository builds the repository.
func NewMetricsRepository(db *Pool) *MetricsRepository { return &MetricsRepository{db: db} }

// WriteVMSamples inserts a batch of VM samples with COPY. One collection cycle
// is a few hundred rows, and COPY keeps that a single round trip.
func (r *MetricsRepository) WriteVMSamples(ctx context.Context, rows []ports.VMSample) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	n, err := r.db.CopyFrom(ctx,
		pgx.Identifier{"metrics_vm"},
		[]string{"time", "vm_id", "cpu_pct", "mem_used_bytes", "mem_total_bytes",
			"disk_read_bps", "disk_write_bps", "net_rx_bps", "net_tx_bps", "disk_used_bytes"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			s := rows[i]
			return []any{s.Time, s.VMID, s.CPUPct, s.MemUsedBytes, s.MemTotalBytes,
				s.DiskReadBps, s.DiskWriteBps, s.NetRxBps, s.NetTxBps, s.DiskUsedBytes}, nil
		}))
	if err != nil {
		return 0, fmt.Errorf("write vm samples: %w", err)
	}
	return n, nil
}

// WriteHostSamples inserts a batch of host samples.
func (r *MetricsRepository) WriteHostSamples(ctx context.Context, rows []ports.HostSample) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	n, err := r.db.CopyFrom(ctx,
		pgx.Identifier{"metrics_host"},
		[]string{"time", "host_id", "cpu_pct", "mem_used_bytes", "mem_total_bytes"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			s := rows[i]
			return []any{s.Time, s.HostID, s.CPUPct, s.MemUsedBytes, s.MemTotalBytes}, nil
		}))
	if err != nil {
		return 0, fmt.Errorf("write host samples: %w", err)
	}
	return n, nil
}

// CounterState loads the previous cumulative readings for a platform's VMs, so
// the collector can turn counters into rates.
func (r *MetricsRepository) CounterState(ctx context.Context, vmIDs []uuid.UUID) (map[ports.CounterKey]ports.CounterValue, error) {
	if len(vmIDs) == 0 {
		return map[ports.CounterKey]ports.CounterValue{}, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT vm_id, metric, last_value, last_time FROM metrics_counter_state WHERE vm_id = ANY($1)`, vmIDs)
	if err != nil {
		return nil, fmt.Errorf("load counter state: %w", err)
	}
	defer rows.Close()

	out := map[ports.CounterKey]ports.CounterValue{}
	for rows.Next() {
		var (
			key ports.CounterKey
			val ports.CounterValue
		)
		if err := rows.Scan(&key.VMID, &key.Metric, &val.Value, &val.Time); err != nil {
			return nil, fmt.Errorf("scan counter state: %w", err)
		}
		out[key] = val
	}
	return out, rows.Err()
}

// SaveCounterState records the latest cumulative readings.
func (r *MetricsRepository) SaveCounterState(ctx context.Context, state map[ports.CounterKey]ports.CounterValue) error {
	if len(state) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for key, val := range state {
		batch.Queue(`
			INSERT INTO metrics_counter_state (vm_id, metric, last_value, last_time)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (vm_id, metric) DO UPDATE SET
				last_value = EXCLUDED.last_value, last_time = EXCLUDED.last_time`,
			key.VMID, key.Metric, val.Value, val.Time)
	}
	results := r.db.SendBatch(ctx, batch)
	defer results.Close()
	for range state {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("save counter state: %w", err)
		}
	}
	return nil
}

// resolutionSource maps a resolution onto the table or aggregate that holds it,
// and the column names that differ between raw samples and rollups.
type resolutionSource struct {
	table      string
	timeColumn string
	cpu        string
	memUsed    string
	memTotal   string
	diskRead   string
	diskWrite  string
	netRx      string
	netTx      string
	diskUsed   string
}

func sourceFor(res telemetry.Resolution) resolutionSource {
	switch res {
	case telemetry.ResolutionRaw:
		return resolutionSource{
			table: "metrics_vm", timeColumn: "time",
			cpu: "cpu_pct", memUsed: "mem_used_bytes", memTotal: "mem_total_bytes",
			diskRead: "disk_read_bps", diskWrite: "disk_write_bps",
			netRx: "net_rx_bps", netTx: "net_tx_bps", diskUsed: "disk_used_bytes",
		}
	default:
		table := map[telemetry.Resolution]string{
			telemetry.Resolution5m:  "metrics_vm_5m",
			telemetry.Resolution30m: "metrics_vm_30m",
			telemetry.Resolution3h:  "metrics_vm_3h",
		}[res]
		return resolutionSource{
			table: table, timeColumn: "bucket",
			cpu: "cpu_pct_avg", memUsed: "mem_used_bytes_avg", memTotal: "mem_total_bytes",
			diskRead: "disk_read_bps_avg", diskWrite: "disk_write_bps_avg",
			netRx: "net_rx_bps_avg", netTx: "net_tx_bps_avg", diskUsed: "disk_used_bytes",
		}
	}
}

// HostSeries reads a node's metrics for a window. Nodes report only CPU and
// memory (the Proxmox connector emits those per node), and the rollups stop at
// five minutes — there is no 30-minute or 3-hour host aggregate — so a window
// wider than the raw retention is answered from metrics_host_5m rather than a
// coarser table that does not exist. The other MetricPoint fields stay zero:
// a node has no per-VM disk or network series to report.
func (r *MetricsRepository) HostSeries(ctx context.Context, hostID uuid.UUID, from, to, now time.Time) (ports.MetricSeries, error) {
	res := telemetry.SelectResolution(from, to, now)

	table, timeCol, cpu, memUsed, memTotal := "metrics_host", "time", "cpu_pct", "mem_used_bytes", "mem_total_bytes"
	if res != telemetry.ResolutionRaw {
		// Every non-raw resolution collapses to the one host rollup there is.
		res = telemetry.Resolution5m
		table, timeCol = "metrics_host_5m", "bucket"
		cpu, memUsed = "cpu_pct_avg", "mem_used_bytes_avg"
	}

	query := fmt.Sprintf(`
		SELECT %s AS ts, %s, %s, %s
		FROM %s WHERE host_id = $1 AND %s >= $2 AND %s <= $3
		ORDER BY ts`,
		timeCol, cpu, memUsed, memTotal, table, timeCol, timeCol)

	rows, err := r.db.Query(ctx, query, hostID, from, to)
	if err != nil {
		return ports.MetricSeries{}, fmt.Errorf("read host series: %w", err)
	}
	defer rows.Close()

	series := ports.MetricSeries{
		Resolution: string(res),
		BucketS:    int(telemetry.Bucket(res).Seconds()),
		Points:     []ports.MetricPoint{},
	}
	for rows.Next() {
		var (
			p                 ports.MetricPoint
			cpuVal            *float64
			memUsedV, memTotV *int64
		)
		if err := rows.Scan(&p.Time, &cpuVal, &memUsedV, &memTotV); err != nil {
			return ports.MetricSeries{}, fmt.Errorf("scan host sample: %w", err)
		}
		p.CPUPct = derefFloat(cpuVal)
		p.MemUsedBytes = derefInt64(memUsedV)
		p.MemTotalBytes = derefInt64(memTotV)
		series.Points = append(series.Points, p)
	}
	return series, rows.Err()
}

// VMSeries reads a VM's metrics for a window, choosing the resolution that
// balances detail against cost. The caller never names a table: which one holds
// the answer is a storage detail (docs/03-frs.md PERF-02).
func (r *MetricsRepository) VMSeries(ctx context.Context, vmID uuid.UUID, from, to, now time.Time) (ports.MetricSeries, error) {
	res := telemetry.SelectResolution(from, to, now)
	src := sourceFor(res)

	query := fmt.Sprintf(`
		SELECT %s AS ts, %s, %s, %s, %s, %s, %s, %s, %s
		FROM %s WHERE vm_id = $1 AND %s >= $2 AND %s <= $3
		ORDER BY ts`,
		src.timeColumn, src.cpu, src.memUsed, src.memTotal, src.diskRead,
		src.diskWrite, src.netRx, src.netTx, src.diskUsed,
		src.table, src.timeColumn, src.timeColumn)

	rows, err := r.db.Query(ctx, query, vmID, from, to)
	if err != nil {
		return ports.MetricSeries{}, fmt.Errorf("read vm series: %w", err)
	}
	defer rows.Close()

	series := ports.MetricSeries{
		Resolution: string(res),
		BucketS:    int(telemetry.Bucket(res).Seconds()),
		// An empty series must serialize as [] rather than null: a chart
		// iterating the response should find nothing to draw, not crash.
		Points: []ports.MetricPoint{},
	}
	for rows.Next() {
		var (
			p                           ports.MetricPoint
			cpu                         *float64
			memUsed, memTotal, diskUsed *int64
			diskRead, diskWrite, rx, tx *int64
		)
		if err := rows.Scan(&p.Time, &cpu, &memUsed, &memTotal, &diskRead, &diskWrite, &rx, &tx, &diskUsed); err != nil {
			return ports.MetricSeries{}, fmt.Errorf("scan sample: %w", err)
		}
		p.CPUPct = derefFloat(cpu)
		p.MemUsedBytes = derefInt64(memUsed)
		p.MemTotalBytes = derefInt64(memTotal)
		p.DiskReadBps = derefInt64(diskRead)
		p.DiskWriteBps = derefInt64(diskWrite)
		p.NetRxBps = derefInt64(rx)
		p.NetTxBps = derefInt64(tx)
		p.DiskUsedBytes = derefInt64(diskUsed)
		series.Points = append(series.Points, p)
	}
	return series, rows.Err()
}

// LatestVMMetrics returns the most recent sample per VM, for dashboard gauges
// and the inventory list. DISTINCT ON is the cheap way to do "latest per group"
// in Postgres.
func (r *MetricsRepository) LatestVMMetrics(ctx context.Context, since time.Time) (map[uuid.UUID]ports.MetricPoint, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT ON (vm_id) vm_id, time, cpu_pct, mem_used_bytes, mem_total_bytes,
			disk_read_bps, disk_write_bps, net_rx_bps, net_tx_bps, disk_used_bytes
		FROM metrics_vm WHERE time >= $1
		ORDER BY vm_id, time DESC`, since)
	if err != nil {
		return nil, fmt.Errorf("read latest metrics: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]ports.MetricPoint{}
	for rows.Next() {
		var (
			id                          uuid.UUID
			p                           ports.MetricPoint
			cpu                         *float64
			memUsed, memTotal, diskUsed *int64
			diskRead, diskWrite, rx, tx *int64
		)
		if err := rows.Scan(&id, &p.Time, &cpu, &memUsed, &memTotal, &diskRead, &diskWrite, &rx, &tx, &diskUsed); err != nil {
			return nil, fmt.Errorf("scan latest sample: %w", err)
		}
		p.CPUPct = derefFloat(cpu)
		p.MemUsedBytes = derefInt64(memUsed)
		p.MemTotalBytes = derefInt64(memTotal)
		p.DiskReadBps = derefInt64(diskRead)
		p.DiskWriteBps = derefInt64(diskWrite)
		p.NetRxBps = derefInt64(rx)
		p.NetTxBps = derefInt64(tx)
		p.DiskUsedBytes = derefInt64(diskUsed)
		out[id] = p
	}
	return out, rows.Err()
}

// LastSampleTime reports when a platform's VMs were last sampled, which the
// collector uses to decide whether a gap needs backfilling.
func (r *MetricsRepository) LastSampleTime(ctx context.Context, platformID uuid.UUID) (time.Time, error) {
	var t *time.Time
	err := r.db.QueryRow(ctx, `
		SELECT max(m.time) FROM metrics_vm m
		JOIN vms v ON v.id = m.vm_id WHERE v.platform_id = $1`, platformID).Scan(&t)
	if err != nil {
		return time.Time{}, fmt.Errorf("read last sample time: %w", err)
	}
	return derefTime(t), nil
}

// VMIDsByExternalID maps a platform's external ids onto portal VM ids, so
// samples from a connector can be attached to stored assets.
func (r *MetricsRepository) VMIDsByExternalID(ctx context.Context, platformID uuid.UUID) (map[string]uuid.UUID, error) {
	rows, err := r.db.Query(ctx,
		`SELECT external_id, id FROM vms WHERE platform_id = $1 AND sync_state <> 'deleted'`, platformID)
	if err != nil {
		return nil, fmt.Errorf("map vm ids: %w", err)
	}
	defer rows.Close()

	out := map[string]uuid.UUID{}
	for rows.Next() {
		var (
			external string
			id       uuid.UUID
		)
		if err := rows.Scan(&external, &id); err != nil {
			return nil, fmt.Errorf("scan vm id: %w", err)
		}
		out[external] = id
	}
	return out, rows.Err()
}

// HostIDsByExternalID maps a platform's node names onto portal host ids.
func (r *MetricsRepository) HostIDsByExternalID(ctx context.Context, platformID uuid.UUID) (map[string]uuid.UUID, error) {
	rows, err := r.db.Query(ctx,
		`SELECT external_id, id FROM hosts WHERE platform_id = $1 AND sync_state <> 'deleted'`, platformID)
	if err != nil {
		return nil, fmt.Errorf("map host ids: %w", err)
	}
	defer rows.Close()

	out := map[string]uuid.UUID{}
	for rows.Next() {
		var (
			external string
			id       uuid.UUID
		)
		if err := rows.Scan(&external, &id); err != nil {
			return nil, fmt.Errorf("scan host id: %w", err)
		}
		out[external] = id
	}
	return out, rows.Err()
}

func derefFloat(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
