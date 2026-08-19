package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	appsync "github.com/freezxp/proxui/internal/app/sync"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/telemetry"
	"github.com/freezxp/proxui/internal/infra/postgres"
)

func TestMetricsRoundTrip(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewMetricsRepository(f.pool)
	ids, err := repo.VMIDsByExternalID(ctx, f.platform.ID)
	if err != nil {
		t.Fatalf("VMIDsByExternalID: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("mapped %d VM ids, want 3", len(ids))
	}

	var vmID uuid.UUID
	for _, id := range ids {
		vmID = id
		break
	}

	now := time.Now().UTC().Truncate(time.Second)
	cpu := 42.5
	mem := int64(1 << 30)
	rows := []ports.VMSample{
		{Time: now.Add(-2 * time.Minute), VMID: vmID, CPUPct: &cpu, MemUsedBytes: &mem},
		{Time: now.Add(-time.Minute), VMID: vmID, CPUPct: &cpu, MemUsedBytes: &mem},
		{Time: now, VMID: vmID, CPUPct: &cpu, MemUsedBytes: &mem},
	}
	written, err := repo.WriteVMSamples(ctx, rows)
	if err != nil {
		t.Fatalf("WriteVMSamples: %v", err)
	}
	if written != 3 {
		t.Errorf("wrote %d samples, want 3", written)
	}

	series, err := repo.VMSeries(ctx, vmID, now.Add(-time.Hour), now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("VMSeries: %v", err)
	}
	if series.Resolution != string(telemetry.ResolutionRaw) {
		t.Errorf("resolution = %q, want raw for a one-hour window", series.Resolution)
	}
	if len(series.Points) != 3 {
		t.Fatalf("got %d points, want 3", len(series.Points))
	}
	if series.Points[0].CPUPct != 42.5 {
		t.Errorf("cpu = %v, want 42.5", series.Points[0].CPUPct)
	}
	// Points must come back in chronological order or a chart draws backwards.
	for i := 1; i < len(series.Points); i++ {
		if !series.Points[i].Time.After(series.Points[i-1].Time) {
			t.Error("points are not in ascending time order")
		}
	}
}

// An empty range must serialize as an empty list, not null: a chart iterating
// the response should find nothing to draw rather than fail.
func TestEmptySeriesIsNeverNull(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 1})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewMetricsRepository(f.pool)
	ids, _ := repo.VMIDsByExternalID(ctx, f.platform.ID)
	var vmID uuid.UUID
	for _, id := range ids {
		vmID = id
	}

	now := time.Now().UTC()
	series, err := repo.VMSeries(ctx, vmID, now.Add(-time.Hour), now, now)
	if err != nil {
		t.Fatalf("VMSeries: %v", err)
	}
	if series.Points == nil {
		t.Error("Points is nil; it must be an empty slice so JSON renders []")
	}
}

func TestCounterStateRoundTrip(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 2})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewMetricsRepository(f.pool)
	ids, _ := repo.VMIDsByExternalID(ctx, f.platform.ID)
	var vmID uuid.UUID
	for _, id := range ids {
		vmID = id
		break
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	key := ports.CounterKey{VMID: vmID, Metric: "net_rx_bps"}
	err := repo.SaveCounterState(ctx, map[ports.CounterKey]ports.CounterValue{
		key: {Value: 12345, Time: now},
	})
	if err != nil {
		t.Fatalf("SaveCounterState: %v", err)
	}

	state, err := repo.CounterState(ctx, []uuid.UUID{vmID})
	if err != nil {
		t.Fatalf("CounterState: %v", err)
	}
	if got := state[key]; got.Value != 12345 {
		t.Errorf("counter value = %v, want 12345", got.Value)
	}

	// Saving again must overwrite, not accumulate rows.
	if err := repo.SaveCounterState(ctx, map[ports.CounterKey]ports.CounterValue{
		key: {Value: 99999, Time: now.Add(time.Minute)},
	}); err != nil {
		t.Fatalf("SaveCounterState (update): %v", err)
	}
	state, _ = repo.CounterState(ctx, []uuid.UUID{vmID})
	if got := state[key]; got.Value != 99999 {
		t.Errorf("counter value = %v after update, want 99999", got.Value)
	}
}

// The first collection has no baseline, so counters cannot yield rates; the
// second must. This is the behaviour that keeps fictional spikes out of charts.
func TestCollectorConvertsCountersOnSecondCycle(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 4})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewMetricsRepository(f.pool)
	collector := &appsync.MetricsCollector{Metrics: repo, Clock: ports.SystemClock{}, Log: zerolog.Nop()}

	first, err := collector.Collect(ctx, f.platform.ID, f.conn)
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if first.VMSamples == 0 {
		t.Fatal("first cycle stored no samples")
	}
	if first.Dropped == 0 {
		t.Error("first cycle produced rates with no previous reading to compare against")
	}

	second, err := collector.Collect(ctx, f.platform.ID, f.conn)
	if err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if second.VMSamples == 0 {
		t.Fatal("second cycle stored no samples")
	}

	var withRates int
	err = f.pool.QueryRow(ctx, `
		SELECT count(*) FROM metrics_vm m JOIN vms v ON v.id=m.vm_id
		WHERE v.platform_id=$1 AND m.net_rx_bps IS NOT NULL`, f.platform.ID).Scan(&withRates)
	if err != nil {
		t.Fatalf("count rates: %v", err)
	}
	if withRates == 0 {
		t.Error("no counter was converted to a rate on the second cycle")
	}
}

// Samples for a VM the inventory has not seen are counted, not stored: they
// have nothing to attach to, and inventing an asset from a metric would create
// inventory that no platform reported.
func TestCollectorSkipsUnknownSubjects(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	ctx := context.Background()
	// Deliberately no reconcile: the VMs do not exist in the portal yet.

	repo := postgres.NewMetricsRepository(f.pool)
	collector := &appsync.MetricsCollector{Metrics: repo, Clock: ports.SystemClock{}, Log: zerolog.Nop()}

	stats, err := collector.Collect(ctx, f.platform.ID, f.conn)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if stats.VMSamples != 0 {
		t.Errorf("stored %d samples for unknown VMs, want 0", stats.VMSamples)
	}
	if stats.Unknown == 0 {
		t.Error("unknown subjects were not counted")
	}
}

func TestBackfillImportsHistory(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 2})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewMetricsRepository(f.pool)
	collector := &appsync.MetricsCollector{Metrics: repo, Clock: ports.SystemClock{}, Log: zerolog.Nop()}

	written, err := collector.Backfill(ctx, f.platform.ID, f.conn, time.Now().Add(-6*time.Hour))
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if written == 0 {
		t.Fatal("backfill imported nothing")
	}

	var oldest time.Time
	err = f.pool.QueryRow(ctx, `
		SELECT min(m.time) FROM metrics_vm m JOIN vms v ON v.id=m.vm_id
		WHERE v.platform_id=$1`, f.platform.ID).Scan(&oldest)
	if err != nil {
		t.Fatalf("read oldest sample: %v", err)
	}
	if time.Since(oldest) < 3*time.Hour {
		t.Errorf("oldest sample is only %v old; history was not imported", time.Since(oldest))
	}
}

func TestLastSampleTimeTracksCollection(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 2})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewMetricsRepository(f.pool)
	if last, err := repo.LastSampleTime(ctx, f.platform.ID); err != nil {
		t.Fatalf("LastSampleTime: %v", err)
	} else if !last.IsZero() {
		t.Errorf("last sample = %v before any collection, want zero", last)
	}

	collector := &appsync.MetricsCollector{Metrics: repo, Clock: ports.SystemClock{}, Log: zerolog.Nop()}
	if _, err := collector.Collect(ctx, f.platform.ID, f.conn); err != nil {
		t.Fatalf("collect: %v", err)
	}

	last, err := repo.LastSampleTime(ctx, f.platform.ID)
	if err != nil {
		t.Fatalf("LastSampleTime: %v", err)
	}
	if time.Since(last) > time.Minute {
		t.Errorf("last sample = %v, want a fresh timestamp", last)
	}
}

func TestLatestVMMetricsForDashboard(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewMetricsRepository(f.pool)
	collector := &appsync.MetricsCollector{Metrics: repo, Clock: ports.SystemClock{}, Log: zerolog.Nop()}
	if _, err := collector.Collect(ctx, f.platform.ID, f.conn); err != nil {
		t.Fatalf("collect: %v", err)
	}

	latest, err := repo.LatestVMMetrics(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("LatestVMMetrics: %v", err)
	}
	if len(latest) == 0 {
		t.Fatal("no latest metrics returned for the dashboard")
	}
	for id, point := range latest {
		if point.Time.IsZero() {
			t.Errorf("VM %s has a sample with no timestamp", id)
		}
	}
}

var _ = connector.MetricCPUPct

// HostSeries reads a node's CPU and memory back for a chart. Nodes report only
// those two, and the rollups stop at five minutes, so a wide window must fall
// to the 5-minute source rather than a coarser one that does not exist.
func TestHostSeriesRoundTrip(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 1})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewMetricsRepository(f.pool)
	var hostID uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM hosts WHERE platform_id=$1 ORDER BY name LIMIT 1`, f.platform.ID).Scan(&hostID); err != nil {
		t.Fatalf("no host: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	cpu := 33.0
	used, total := int64(8<<30), int64(16<<30)
	rows := []ports.HostSample{
		{Time: now.Add(-2 * time.Minute), HostID: hostID, CPUPct: &cpu, MemUsedBytes: &used, MemTotalBytes: &total},
		{Time: now, HostID: hostID, CPUPct: &cpu, MemUsedBytes: &used, MemTotalBytes: &total},
	}
	if _, err := repo.WriteHostSamples(ctx, rows); err != nil {
		t.Fatalf("WriteHostSamples: %v", err)
	}

	series, err := repo.HostSeries(ctx, hostID, now.Add(-time.Hour), now.Add(time.Minute), now)
	if err != nil {
		t.Fatalf("HostSeries: %v", err)
	}
	if series.Resolution != string(telemetry.ResolutionRaw) {
		t.Errorf("resolution = %q, want raw for a one-hour window", series.Resolution)
	}
	if len(series.Points) != 2 {
		t.Fatalf("got %d points, want 2", len(series.Points))
	}
	if series.Points[0].CPUPct != 33 || series.Points[0].MemUsedBytes != used {
		t.Errorf("point = %+v, want cpu 33 and mem %d", series.Points[0], used)
	}
	// A wide window has no 30-minute host rollup to read, so it must resolve to
	// the five-minute one rather than error or return nothing.
	wide, err := repo.HostSeries(ctx, hostID, now.Add(-200*24*time.Hour), now, now)
	if err != nil {
		t.Fatalf("HostSeries wide: %v", err)
	}
	if wide.Resolution != string(telemetry.Resolution5m) {
		t.Errorf("wide-window resolution = %q, want 5m (no coarser host rollup exists)", wide.Resolution)
	}
}
