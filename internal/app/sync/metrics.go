package sync

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// MetricsCollector turns a connector's samples into stored telemetry.
//
// Connectors report what their platform reports: gauges for CPU and memory,
// cumulative counters for disk and network. Converting counters into rates
// needs the previous reading, which is state, so it belongs here rather than in
// a connector (docs/10-sync-engine.md §10.6).
type MetricsCollector struct {
	Metrics ports.MetricsRepository
	Clock   ports.Clock
	Log     zerolog.Logger
}

// MetricsStats counts what a collection cycle produced.
type MetricsStats struct {
	VMSamples   int `json:"vm_samples"`
	HostSamples int `json:"host_samples"`
	Dropped     int `json:"dropped"`
	Unknown     int `json:"unknown_subjects"`
}

// Collect samples a platform once and stores the result.
func (c *MetricsCollector) Collect(ctx context.Context, platformID uuid.UUID, conn connector.Connector) (MetricsStats, error) {
	var stats MetricsStats

	mc, ok := conn.(connector.MetricsCollector)
	if !ok {
		return stats, nil
	}
	samples, err := mc.CollectMetrics(ctx, connector.MetricScope{})
	if err != nil {
		return stats, err
	}
	if len(samples) == 0 {
		return stats, nil
	}

	vmIDs, err := c.Metrics.VMIDsByExternalID(ctx, platformID)
	if err != nil {
		return stats, err
	}
	hostIDs, err := c.Metrics.HostIDsByExternalID(ctx, platformID)
	if err != nil {
		return stats, err
	}

	ids := make([]uuid.UUID, 0, len(vmIDs))
	for _, id := range vmIDs {
		ids = append(ids, id)
	}
	counters, err := c.Metrics.CounterState(ctx, ids)
	if err != nil {
		return stats, err
	}

	vmRows := map[uuid.UUID]*ports.VMSample{}
	hostRows := map[uuid.UUID]*ports.HostSample{}
	newCounters := map[ports.CounterKey]ports.CounterValue{}

	for _, s := range samples {
		switch s.Subject {
		case connector.SubjectVM:
			id, known := vmIDs[s.SubjectID]
			if !known {
				// A VM the inventory sync has not seen yet: its samples have
				// nowhere to attach, and will be collected on the next cycle.
				stats.Unknown++
				continue
			}
			row := vmRows[id]
			if row == nil {
				row = &ports.VMSample{Time: s.Time, VMID: id}
				vmRows[id] = row
			}
			c.applyVMSample(row, s, id, counters, newCounters, &stats)

		case connector.SubjectHost:
			id, known := hostIDs[s.SubjectID]
			if !known {
				stats.Unknown++
				continue
			}
			row := hostRows[id]
			if row == nil {
				row = &ports.HostSample{Time: s.Time, HostID: id}
				hostRows[id] = row
			}
			applyHostSample(row, s)
		}
	}

	vmSlice := make([]ports.VMSample, 0, len(vmRows))
	for _, row := range vmRows {
		vmSlice = append(vmSlice, *row)
	}
	hostSlice := make([]ports.HostSample, 0, len(hostRows))
	for _, row := range hostRows {
		hostSlice = append(hostSlice, *row)
	}

	written, err := c.Metrics.WriteVMSamples(ctx, vmSlice)
	if err != nil {
		return stats, err
	}
	stats.VMSamples = int(written)

	writtenHosts, err := c.Metrics.WriteHostSamples(ctx, hostSlice)
	if err != nil {
		return stats, err
	}
	stats.HostSamples = int(writtenHosts)

	if err := c.Metrics.SaveCounterState(ctx, newCounters); err != nil {
		return stats, err
	}
	return stats, nil
}

func (c *MetricsCollector) applyVMSample(row *ports.VMSample, s connector.Sample, vmID uuid.UUID,
	previous map[ports.CounterKey]ports.CounterValue, next map[ports.CounterKey]ports.CounterValue, stats *MetricsStats) {

	if !s.Cumulative {
		switch s.Kind {
		case connector.MetricCPUPct:
			row.CPUPct = floatPtr(s.Value)
		case connector.MetricMemUsedBytes:
			row.MemUsedBytes = int64Ptr(s.Value)
		case connector.MetricMemTotalBytes:
			row.MemTotalBytes = int64Ptr(s.Value)
		case connector.MetricDiskUsedBytes:
			row.DiskUsedBytes = int64Ptr(s.Value)
		}
		return
	}

	// Record the raw counter regardless, so the next cycle has a baseline even
	// when this one cannot produce a rate.
	key := ports.CounterKey{VMID: vmID, Metric: string(s.Kind)}
	prev := previous[key]
	next[key] = ports.CounterValue{Value: s.Value, Time: s.Time}

	rate, ok := telemetry.RateWithinGap(telemetry.CounterReading{
		Previous: prev.Value, PreviousTime: prev.Time,
		Current: s.Value, CurrentTime: s.Time,
	})
	if !ok {
		// A reboot, a first sighting or a long gap. Leaving the field nil
		// stores a gap; inventing a value would draw a spike that never
		// happened.
		stats.Dropped++
		return
	}

	switch s.Kind {
	case connector.MetricDiskReadBps:
		row.DiskReadBps = int64Ptr(rate)
	case connector.MetricDiskWriteBps:
		row.DiskWriteBps = int64Ptr(rate)
	case connector.MetricNetRxBps:
		row.NetRxBps = int64Ptr(rate)
	case connector.MetricNetTxBps:
		row.NetTxBps = int64Ptr(rate)
	}
}

func applyHostSample(row *ports.HostSample, s connector.Sample) {
	switch s.Kind {
	case connector.MetricCPUPct:
		row.CPUPct = floatPtr(s.Value)
	case connector.MetricMemUsedBytes:
		row.MemUsedBytes = int64Ptr(s.Value)
	case connector.MetricMemTotalBytes:
		row.MemTotalBytes = int64Ptr(s.Value)
	}
}

// Backfill imports history for a platform's VMs so charts are useful
// immediately after registration rather than a year later (PERF-04).
func (c *MetricsCollector) Backfill(ctx context.Context, platformID uuid.UUID, conn connector.Connector, from time.Time) (int, error) {
	bf, ok := conn.(connector.MetricsBackfiller)
	if !ok {
		return 0, nil
	}
	vmIDs, err := c.Metrics.VMIDsByExternalID(ctx, platformID)
	if err != nil {
		return 0, err
	}

	total := 0
	for external, id := range vmIDs {
		samples, err := bf.BackfillMetrics(ctx, connector.VMRef{ExternalID: external}, from)
		if err != nil {
			// History is a nicety: one VM refusing it must not abort the rest.
			c.Log.Debug().Err(err).Str("vm", external).Msg("backfill skipped")
			continue
		}

		byTime := map[time.Time]*ports.VMSample{}
		for _, s := range samples {
			row := byTime[s.Time]
			if row == nil {
				row = &ports.VMSample{Time: s.Time, VMID: id}
				byTime[s.Time] = row
			}
			switch s.Kind {
			case connector.MetricCPUPct:
				row.CPUPct = floatPtr(s.Value)
			case connector.MetricMemUsedBytes:
				row.MemUsedBytes = int64Ptr(s.Value)
			case connector.MetricMemTotalBytes:
				row.MemTotalBytes = int64Ptr(s.Value)
			}
		}

		rows := make([]ports.VMSample, 0, len(byTime))
		for _, row := range byTime {
			rows = append(rows, *row)
		}
		written, err := c.Metrics.WriteVMSamples(ctx, rows)
		if err != nil {
			return total, err
		}
		total += int(written)
	}
	return total, nil
}

func floatPtr(v float64) *float64 { return &v }

func int64Ptr(v float64) *int64 {
	i := int64(v)
	return &i
}
