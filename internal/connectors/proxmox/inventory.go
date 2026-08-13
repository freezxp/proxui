package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/freezxp/proxui/internal/connector"
)

// clusterResource is one row of /cluster/resources — the single call that
// returns every VM, container, node and storage pool in the cluster with its
// current state. At the scale ProxUI targets this makes a full snapshot cheaper
// than any incremental bookkeeping would be (docs/10-sync-engine.md §10.1).
type clusterResource struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"` // qemu | lxc | node | storage | sdn | pool
	Node      string  `json:"node"`
	Name      string  `json:"name"`
	VMID      int64   `json:"vmid"`
	Status    string  `json:"status"`
	MaxCPU    float64 `json:"maxcpu"`
	CPU       float64 `json:"cpu"`
	MaxMem    int64   `json:"maxmem"`
	Mem       int64   `json:"mem"`
	MaxDisk   int64   `json:"maxdisk"`
	Disk      int64   `json:"disk"`
	Uptime    int64   `json:"uptime"`
	Template  int     `json:"template"`
	Pool      string  `json:"pool"`
	Tags      string  `json:"tags"`    // semicolon separated
	Storage   string  `json:"storage"` // storage rows only
	PlugType  string  `json:"plugintype"`
	Shared    int     `json:"shared"`
	Level     string  `json:"level"`
	DiskRead  int64   `json:"diskread"`
	DiskWrite int64   `json:"diskwrite"`
	NetIn     int64   `json:"netin"`
	NetOut    int64   `json:"netout"`
}

type nodeStatus struct {
	Node   string  `json:"node"`
	Status string  `json:"status"`
	MaxCPU float64 `json:"maxcpu"`
	MaxMem int64   `json:"maxmem"`
	Uptime int64   `json:"uptime"`
	Level  string  `json:"level"`
}

type networkIface struct {
	Iface       string `json:"iface"`
	Type        string `json:"type"`
	Address     string `json:"address"`
	Netmask     string `json:"netmask"`
	CIDR        string `json:"cidr"`
	Active      int    `json:"active"`
	Autostart   int    `json:"autostart"`
	BridgePorts string `json:"bridge_ports"`
	VLANTag     int    `json:"vlan-id"`
	Comments    string `json:"comments"`
}

type agentInterfaces struct {
	Result []struct {
		Name        string `json:"name"`
		IPAddresses []struct {
			Address string `json:"ip-address"`
			Type    string `json:"ip-address-type"`
		} `json:"ip-addresses"`
	} `json:"result"`
}

func (c *Connector) resources(ctx context.Context) ([]clusterResource, error) {
	var out []clusterResource
	if err := c.client.get(ctx, "/cluster/resources", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListVMs implements connector.VirtualMachineCollector.
func (c *Connector) ListVMs(ctx context.Context) ([]connector.VMRecord, error) {
	resources, err := c.resources(ctx)
	if err != nil {
		return nil, err
	}

	var records []connector.VMRecord
	for _, r := range resources {
		if r.Type != "qemu" && r.Type != "lxc" {
			continue
		}
		// Templates are not runnable machines; showing them as stopped VMs
		// would mislead operators counting their fleet.
		if r.Template == 1 {
			continue
		}
		records = append(records, connector.VMRecord{
			ExternalID:  fmt.Sprintf("%d", r.VMID),
			Name:        firstNonEmpty(r.Name, fmt.Sprintf("vm-%d", r.VMID)),
			Type:        r.Type,
			State:       normalizeVMState(r.Status),
			HostID:      r.Node,
			CPUCores:    int(r.MaxCPU),
			MemoryBytes: r.MaxMem,
			DiskBytes:   r.MaxDisk,
			UptimeS:     r.Uptime,
			Tags:        splitTags(r.Tags),
			Pool:        r.Pool,
			Attrs:       map[string]any{"proxmox_id": r.ID},
		})
	}

	// Guest agent lookups are per-VM and optional, so they run bounded and
	// tolerate failure: a VM without the agent simply has no IP addresses.
	c.enrichIPAddresses(ctx, records)

	sort.Slice(records, func(i, j int) bool { return records[i].ExternalID < records[j].ExternalID })
	return records, nil
}

func (c *Connector) enrichIPAddresses(ctx context.Context, records []connector.VMRecord) {
	type job struct{ idx int }
	jobs := make(chan job)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for j := range jobs {
			rec := records[j.idx]
			if rec.Type != "qemu" || rec.State != "running" || rec.HostID == "" {
				continue
			}
			var ifaces agentInterfaces
			path := fmt.Sprintf("/nodes/%s/qemu/%s/agent/network-get-interfaces", rec.HostID, rec.ExternalID)
			if err := c.client.get(ctx, path, &ifaces); err != nil {
				continue // no agent installed, or not permitted: not an error
			}
			records[j.idx].IPAddresses = extractIPs(ifaces)
		}
	}

	for i := 0; i < c.client.concurrency; i++ {
		wg.Add(1)
		go worker()
	}
	for i := range records {
		select {
		case jobs <- job{idx: i}:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func extractIPs(ifaces agentInterfaces) []string {
	var out []string
	for _, iface := range ifaces.Result {
		if iface.Name == "lo" {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.Address == "" || strings.HasPrefix(addr.Address, "fe80") || addr.Address == "127.0.0.1" {
				continue
			}
			out = append(out, addr.Address)
		}
	}
	sort.Strings(out)
	return out
}

// ListHosts implements connector.HostCollector.
func (c *Connector) ListHosts(ctx context.Context) ([]connector.HostRecord, error) {
	var nodes []nodeStatus
	if err := c.client.get(ctx, "/nodes", &nodes); err != nil {
		return nil, err
	}

	records := make([]connector.HostRecord, 0, len(nodes))
	for _, n := range nodes {
		records = append(records, connector.HostRecord{
			ExternalID:  n.Node,
			Name:        n.Node,
			Status:      normalizeNodeStatus(n.Status),
			CPUCores:    int(n.MaxCPU),
			MemoryBytes: n.MaxMem,
			UptimeS:     n.Uptime,
			Attrs:       map[string]any{"subscription_level": n.Level},
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ExternalID < records[j].ExternalID })
	return records, nil
}

// ListStoragePools implements connector.StorageCollector.
func (c *Connector) ListStoragePools(ctx context.Context) ([]connector.StorageRecord, error) {
	resources, err := c.resources(ctx)
	if err != nil {
		return nil, err
	}

	var records []connector.StorageRecord
	for _, r := range resources {
		if r.Type != "storage" {
			continue
		}
		shared := r.Shared == 1
		hostID := r.Node
		if shared {
			// Shared storage belongs to the cluster, not to whichever node
			// happened to report it; otherwise it appears once per node.
			hostID = ""
		}
		records = append(records, connector.StorageRecord{
			ExternalID:  firstNonEmpty(r.Storage, r.Name),
			Name:        firstNonEmpty(r.Storage, r.Name),
			StorageType: r.PlugType,
			HostID:      hostID,
			TotalBytes:  r.MaxDisk,
			UsedBytes:   r.Disk,
			IsShared:    shared,
			Attrs:       map[string]any{"proxmox_id": r.ID, "status": r.Status},
		})
	}

	records = dedupeStorage(records)
	sort.Slice(records, func(i, j int) bool { return records[i].NaturalKey() < records[j].NaturalKey() })
	return records, nil
}

// dedupeStorage collapses the per-node rows Proxmox reports for shared storage
// into one record, keeping listings idempotent as the conformance suite requires.
func dedupeStorage(in []connector.StorageRecord) []connector.StorageRecord {
	seen := map[string]bool{}
	out := in[:0]
	for _, r := range in {
		key := r.NaturalKey()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// ListNetworks implements connector.NetworkCollector.
func (c *Connector) ListNetworks(ctx context.Context) ([]connector.NetworkRecord, error) {
	hosts, err := c.ListHosts(ctx)
	if err != nil {
		return nil, err
	}

	var (
		mu       sync.Mutex
		records  []connector.NetworkRecord
		wg       sync.WaitGroup
		firstErr error
	)
	sem := make(chan struct{}, c.client.concurrency)

	for _, host := range hosts {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var ifaces []networkIface
			if err := c.client.get(ctx, "/nodes/"+node+"/network", &ifaces); err != nil {
				mu.Lock()
				// One unreachable node must not fail the whole listing: the
				// sync engine records a partial run instead.
				if firstErr == nil && !errors.Is(err, connector.ErrPermission) {
					firstErr = err
				}
				mu.Unlock()
				return
			}

			local := make([]connector.NetworkRecord, 0, len(ifaces))
			for _, i := range ifaces {
				local = append(local, connector.NetworkRecord{
					ExternalID: i.Iface,
					Name:       i.Iface,
					NetType:    normalizeNetType(i.Type),
					HostID:     node,
					CIDR:       firstNonEmpty(i.CIDR, cidrFrom(i.Address, i.Netmask)),
					VLANTag:    i.VLANTag,
					Attrs: map[string]any{
						"active":       i.Active == 1,
						"autostart":    i.Autostart == 1,
						"bridge_ports": i.BridgePorts,
					},
				})
			}
			mu.Lock()
			records = append(records, local...)
			mu.Unlock()
		}(host.ExternalID)
	}
	wg.Wait()

	if len(records) == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(records, func(i, j int) bool { return records[i].NaturalKey() < records[j].NaturalKey() })
	return records, nil
}

// Health implements connector.HealthCollector. It stays to two cheap calls
// because it runs every 30 seconds and drives the circuit breaker.
func (c *Connector) Health(ctx context.Context) (connector.HealthReport, error) {
	var version versionResponse
	if err := c.client.get(ctx, "/version", &version); err != nil {
		if errors.Is(err, connector.ErrAuth) || errors.Is(err, connector.ErrPermission) {
			return connector.HealthReport{State: connector.HealthDegraded, Detail: err.Error()}, nil
		}
		return connector.HealthReport{State: connector.HealthUnreachable, Detail: err.Error()}, nil
	}

	report := connector.HealthReport{State: connector.HealthHealthy, Version: version.Version}

	var status []struct {
		Type    string `json:"type"`
		Quorate int    `json:"quorate"`
		Online  int    `json:"online"`
		Name    string `json:"name"`
	}
	if err := c.client.get(ctx, "/cluster/status", &status); err != nil {
		// A standalone node has no cluster endpoint; that is healthy, not broken.
		return report, nil
	}
	for _, s := range status {
		if s.Type == "cluster" && s.Quorate == 0 {
			report.State = connector.HealthDegraded
			report.Detail = "cluster has lost quorum"
		}
	}
	return report, nil
}

func normalizeVMState(status string) string {
	switch strings.ToLower(status) {
	case "running":
		return "running"
	case "stopped":
		return "stopped"
	case "paused":
		return "paused"
	case "suspended":
		return "suspended"
	default:
		return "unknown"
	}
}

func normalizeNodeStatus(status string) string {
	switch strings.ToLower(status) {
	case "online":
		return "online"
	case "offline":
		return "offline"
	default:
		return "unknown"
	}
}

func normalizeNetType(t string) string {
	switch {
	case strings.Contains(t, "bridge"):
		return "bridge"
	case strings.Contains(t, "bond"):
		return "bond"
	case strings.Contains(t, "vlan"):
		return "vlan"
	default:
		return "eth"
	}
}

func splitTags(tags string) []string {
	if tags == "" {
		return nil
	}
	parts := strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func cidrFrom(address, netmask string) string {
	if address == "" || netmask == "" {
		return ""
	}
	return address + "/" + netmask
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// decodeInto is a small helper for endpoints that return loosely typed data.
func decodeInto(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
