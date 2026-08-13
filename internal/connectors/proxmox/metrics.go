package proxmox

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/freezxp/proxui/internal/connector"
)

// CollectMetrics implements connector.MetricsCollector.
//
// /cluster/resources already carries current CPU, memory and disk for every
// guest, so a full metrics cycle is one API call rather than one per VM. Disk
// and network counters are cumulative; they are emitted as such and the sync
// engine converts them to rates, which is where reset handling belongs.
func (c *Connector) CollectMetrics(ctx context.Context, scope connector.MetricScope) ([]connector.Sample, error) {
	resources, err := c.resources(ctx)
	if err != nil {
		return nil, err
	}

	wanted := map[string]bool{}
	for _, vm := range scope.VMs {
		wanted[vm.ExternalID] = true
	}
	wantedHosts := map[string]bool{}
	for _, h := range scope.Hosts {
		wantedHosts[h] = true
	}

	now := time.Now().UTC()
	samples := make([]connector.Sample, 0, len(resources)*4)

	for _, r := range resources {
		switch r.Type {
		case "qemu", "lxc":
			if r.Template == 1 || normalizeVMState(r.Status) != "running" {
				continue
			}
			id := fmt.Sprintf("%d", r.VMID)
			if len(wanted) > 0 && !wanted[id] {
				continue
			}
			samples = append(samples,
				// Proxmox reports CPU as a 0..1 fraction of allocated cores.
				gauge(now, connector.SubjectVM, id, connector.MetricCPUPct, r.CPU*100),
				gauge(now, connector.SubjectVM, id, connector.MetricMemUsedBytes, float64(r.Mem)),
				gauge(now, connector.SubjectVM, id, connector.MetricMemTotalBytes, float64(r.MaxMem)),
				gauge(now, connector.SubjectVM, id, connector.MetricDiskUsedBytes, float64(r.Disk)),
				counter(now, connector.SubjectVM, id, connector.MetricDiskReadBps, float64(r.DiskRead)),
				counter(now, connector.SubjectVM, id, connector.MetricDiskWriteBps, float64(r.DiskWrite)),
				counter(now, connector.SubjectVM, id, connector.MetricNetRxBps, float64(r.NetIn)),
				counter(now, connector.SubjectVM, id, connector.MetricNetTxBps, float64(r.NetOut)),
			)

		case "node":
			if len(wantedHosts) > 0 && !wantedHosts[r.Node] {
				continue
			}
			if normalizeNodeStatus(r.Status) != "online" {
				continue
			}
			samples = append(samples,
				gauge(now, connector.SubjectHost, r.Node, connector.MetricCPUPct, r.CPU*100),
				gauge(now, connector.SubjectHost, r.Node, connector.MetricMemUsedBytes, float64(r.Mem)),
				gauge(now, connector.SubjectHost, r.Node, connector.MetricMemTotalBytes, float64(r.MaxMem)),
			)
		}
	}
	return samples, nil
}

// rrdPoint is one row of Proxmox RRD history.
type rrdPoint struct {
	Time      int64   `json:"time"`
	CPU       float64 `json:"cpu"`
	MaxCPU    float64 `json:"maxcpu"`
	Mem       float64 `json:"mem"`
	MaxMem    float64 `json:"maxmem"`
	Disk      float64 `json:"disk"`
	DiskRead  float64 `json:"diskread"`
	DiskWrite float64 `json:"diskwrite"`
	NetIn     float64 `json:"netin"`
	NetOut    float64 `json:"netout"`
}

// BackfillMetrics implements connector.MetricsBackfiller by importing Proxmox's
// own RRD history, so charts are useful the moment a platform is registered
// instead of a year from now (PERF-04).
func (c *Connector) BackfillMetrics(ctx context.Context, vm connector.VMRef, from time.Time) ([]connector.Sample, error) {
	if vm.HostID == "" || vm.ExternalID == "" {
		return nil, connector.Errorf(connector.ErrInvalidConfig, "backfill",
			"backfill needs both the node and the VMID")
	}
	kind := vm.Type
	if kind == "" {
		kind = "qemu"
	}

	path := fmt.Sprintf("/nodes/%s/%s/%s/rrddata", vm.HostID, kind, vm.ExternalID)
	query := url.Values{"timeframe": {timeframeFor(from)}, "cf": {"AVERAGE"}}

	var points []rrdPoint
	if err := c.client.getQuery(ctx, path, query, &points); err != nil {
		return nil, err
	}

	samples := make([]connector.Sample, 0, len(points)*3)
	for _, p := range points {
		ts := time.Unix(p.Time, 0).UTC()
		if ts.Before(from) {
			continue
		}
		// RRD rows exist for every slot; unpopulated ones carry zero counters
		// and would otherwise be stored as real "idle" measurements.
		if p.MaxMem == 0 && p.CPU == 0 && p.Mem == 0 {
			continue
		}
		samples = append(samples,
			gauge(ts, connector.SubjectVM, vm.ExternalID, connector.MetricCPUPct, p.CPU*100),
			gauge(ts, connector.SubjectVM, vm.ExternalID, connector.MetricMemUsedBytes, p.Mem),
			gauge(ts, connector.SubjectVM, vm.ExternalID, connector.MetricMemTotalBytes, p.MaxMem),
		)
	}
	return samples, nil
}

// timeframeFor picks the coarsest RRD window that still covers the requested
// range: asking for a year of hourly data when a day is wanted wastes both
// sides' time.
func timeframeFor(from time.Time) string {
	age := time.Since(from)
	switch {
	case age <= 24*time.Hour:
		return "day"
	case age <= 7*24*time.Hour:
		return "week"
	case age <= 31*24*time.Hour:
		return "month"
	default:
		return "year"
	}
}

func gauge(ts time.Time, subject connector.SubjectKind, id string, kind connector.MetricKind, value float64) connector.Sample {
	return connector.Sample{Time: ts, Subject: subject, SubjectID: id, Kind: kind, Value: value}
}

func counter(ts time.Time, subject connector.SubjectKind, id string, kind connector.MetricKind, value float64) connector.Sample {
	return connector.Sample{Time: ts, Subject: subject, SubjectID: id, Kind: kind, Value: value, Cumulative: true}
}
