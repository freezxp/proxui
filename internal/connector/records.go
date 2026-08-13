package connector

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Records are the normalized shape every connector returns. Platform-specific
// extras live in Attrs so a new platform needs no schema change; anything the
// core filters or sorts on is a real field.
//
// Fingerprint drives change detection. It deliberately covers identity and
// configuration only — never uptime or counters — so an unchanged VM produces a
// stable fingerprint every cycle and only real changes are recorded as history.

// VMRecord is a normalized virtual machine.
type VMRecord struct {
	ExternalID  string
	Name        string
	Type        string // qemu | lxc
	State       string // running | stopped | paused | suspended | unknown
	HostID      string // platform host/node identifier, may be empty
	CPUCores    int
	MemoryBytes int64
	DiskBytes   int64
	UptimeS     int64 // volatile: excluded from the fingerprint
	IPAddresses []string
	Tags        []string
	Pool        string // platform grouping, used by VM group auto-rules
	Attrs       map[string]any
}

// NaturalKey is the platform-side identity used for upserts.
func (r VMRecord) NaturalKey() string { return r.ExternalID }

// Fingerprint hashes the fields whose change is worth recording.
func (r VMRecord) Fingerprint() []byte {
	return hashFields(
		r.ExternalID, r.Name, r.Type, r.State, r.HostID, r.Pool,
		itoa(int64(r.CPUCores)), itoa(r.MemoryBytes), itoa(r.DiskBytes),
		joinSorted(r.IPAddresses), joinSorted(r.Tags),
	)
}

// HostRecord is a normalized hypervisor host.
type HostRecord struct {
	ExternalID  string
	Name        string
	Status      string // online | offline | unknown
	CPUCores    int
	MemoryBytes int64
	Version     string
	UptimeS     int64 // volatile
	Attrs       map[string]any
}

// NaturalKey is the platform-side identity used for upserts.
func (r HostRecord) NaturalKey() string { return r.ExternalID }

// Fingerprint hashes the stable identity and configuration fields.
func (r HostRecord) Fingerprint() []byte {
	return hashFields(r.ExternalID, r.Name, r.Status, r.Version,
		itoa(int64(r.CPUCores)), itoa(r.MemoryBytes))
}

// StorageRecord is a normalized storage pool or datastore.
type StorageRecord struct {
	ExternalID  string
	Name        string
	StorageType string
	HostID      string // empty for cluster-wide/shared storage
	TotalBytes  int64
	UsedBytes   int64 // volatile: excluded from the fingerprint
	IsShared    bool
	Attrs       map[string]any
}

// NaturalKey is the platform-side identity used for upserts.
func (r StorageRecord) NaturalKey() string { return r.ExternalID + "@" + r.HostID }

// Fingerprint excludes UsedBytes: consumption changes constantly and belongs in
// metrics, not in change history.
func (r StorageRecord) Fingerprint() []byte {
	return hashFields(r.ExternalID, r.Name, r.StorageType, r.HostID,
		itoa(r.TotalBytes), btoa(r.IsShared))
}

// NetworkRecord is a normalized network interface, bridge or bond.
type NetworkRecord struct {
	ExternalID string
	Name       string
	NetType    string // bridge | bond | vlan | eth
	HostID     string
	CIDR       string
	VLANTag    int
	Attrs      map[string]any
}

// NaturalKey is the platform-side identity used for upserts.
func (r NetworkRecord) NaturalKey() string { return r.HostID + "/" + r.ExternalID }

// Fingerprint hashes the full configuration: networks have no volatile fields.
func (r NetworkRecord) Fingerprint() []byte {
	return hashFields(r.ExternalID, r.Name, r.NetType, r.HostID, r.CIDR, itoa(int64(r.VLANTag)))
}

// MetricKind names a sampled series.
type MetricKind string

const (
	MetricCPUPct        MetricKind = "cpu_pct"
	MetricMemUsedBytes  MetricKind = "mem_used_bytes"
	MetricMemTotalBytes MetricKind = "mem_total_bytes"
	MetricDiskReadBps   MetricKind = "disk_read_bps"
	MetricDiskWriteBps  MetricKind = "disk_write_bps"
	MetricNetRxBps      MetricKind = "net_rx_bps"
	MetricNetTxBps      MetricKind = "net_tx_bps"
	MetricDiskUsedBytes MetricKind = "disk_used_bytes"
)

// SubjectKind distinguishes what a sample measures.
type SubjectKind string

const (
	SubjectVM   SubjectKind = "vm"
	SubjectHost SubjectKind = "host"
)

// Sample is one measurement. Connectors emit rates already computed where the
// platform exposes counters, so the core never has to know which platforms
// report cumulative values.
type Sample struct {
	Time      time.Time
	Subject   SubjectKind
	SubjectID string // platform-side external id
	Kind      MetricKind
	Value     float64
	// Cumulative marks a counter reading the sync engine must convert to a
	// rate, tracking the previous value and handling resets.
	Cumulative bool
}

func hashFields(fields ...string) []byte {
	h := sha256.New()
	for _, f := range fields {
		// Length-prefixed so that ("ab","c") and ("a","bc") cannot collide.
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return h.Sum(nil)
}

func joinSorted(values []string) string {
	if len(values) == 0 {
		return ""
	}
	out := make([]string, len(values))
	copy(out, values)
	sort.Strings(out)
	return strings.Join(out, ",")
}

func itoa(v int64) string { return fmt.Sprintf("%d", v) }

func btoa(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
