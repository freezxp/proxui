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

// TemplateRecord is an image a guest can be cloned from.
//
// It is not a VMRecord: templates are excluded from the inventory on purpose,
// because counting them as stopped machines misleads anyone reading a fleet
// total (see the filter in the Proxmox connector's ListVMs). They are listed
// only where they are being chosen from.
type TemplateRecord struct {
	ExternalID string
	Name       string
	Type       string // qemu | lxc
	HostID     string
	DiskBytes  int64
	// HasCloudInit reports whether the template carries a cloud-init drive.
	// One that does not cannot take a user or an SSH key, and the UI says so
	// rather than letting an operator provision an unreachable machine.
	HasCloudInit bool
	Notes        string
	Attrs        map[string]any
}

// CloneSpec describes the copy to make.
type CloneSpec struct {
	Template VMRef
	NewID    string
	Name     string
	// FullClone copies the disks instead of referencing the template's. A
	// linked clone is fast and cheap but keeps the template undeletable and
	// ties the guest's fate to it.
	FullClone   bool
	Storage     string
	TargetNode  string
	Description string
}

// CloudInitSpec is the guest configuration handed to cloud-init.
//
// There is no password field, and that is a decision rather than an omission:
// guest credentials are never stored (ADR 0005), and a password would pass
// through a form, a request body, a job payload and a state row on its way to
// the platform — four places it could come to rest. A type with nowhere to put
// one cannot grow the habit later (PROV-04, ADR 0010).
type CloudInitSpec struct {
	User    string
	SSHKeys []string
	// IPConfig is the platform's own notation, e.g. "ip=dhcp" or
	// "ip=10.0.30.50/24,gw=10.0.30.1". Empty leaves the template's setting.
	IPConfig     string
	Nameserver   string
	SearchDomain string
	Cores        int
	MemoryMB     int
	Bridge       string
	VLAN         int
	// UpgradePackages runs the distribution's upgrade on first boot. Default
	// on at the platform, so this is a pointer: nil keeps the platform's
	// default rather than silently choosing for the operator.
	UpgradePackages *bool
	StartOnBoot     bool
}

// DestroyOptions controls how thoroughly a guest is removed.
type DestroyOptions struct {
	// PurgeReferences removes the guest from backup jobs and HA resources as
	// well. Without it the guest goes but the jobs naming it remain and fail.
	PurgeReferences bool
	// DestroyUnreferencedDisks removes disks belonging to the guest that its
	// configuration no longer mentions, which is where the disks of a
	// half-finished provisioning run end up.
	DestroyUnreferencedDisks bool
}

// ImageDownloadSpec asks the platform to fetch a published cloud image.
//
// Checksum and ChecksumAlgorithm have no defaults and no "skip" flag. An
// unverified download is therefore something a caller states by leaving both
// empty, rather than something that happens because a field was forgotten —
// and the layer that states it is the layer that audits it (ADR 0010).
type ImageDownloadSpec struct {
	Node     string
	Storage  string
	URL      string
	Filename string
	// Checksum is the digest as the distribution publishes it; algorithm is one
	// of the platform's accepted names, e.g. "sha512".
	Checksum          string
	ChecksumAlgorithm string
	// VerifyTLS defaults on at the platform. It is here so that turning it off
	// has to be written down.
	SkipTLSVerify bool
}

// GuestCreateSpec is the shell of a guest, before it has a disk.
type GuestCreateSpec struct {
	Node        string
	VMID        string
	Name        string
	Cores       int
	MemoryMB    int
	Bridge      string
	Description string
}

// DiskImportSpec attaches a downloaded image to a guest as its boot disk, and
// adds the cloud-init drive beside it.
type DiskImportSpec struct {
	// Disk is the bus and index the image becomes, e.g. "scsi0".
	Disk string
	// Storage is where the imported disk lands — a storage that holds images,
	// which is rarely the same one the downloaded file sits on.
	Storage string
	// SourceVolume is the platform-side reference to the downloaded file.
	SourceVolume string
	// CloudInitDrive is the bus the generated drive takes, e.g. "ide2". Empty
	// leaves the guest without one, which makes it unusable as a cloud-init
	// template and is therefore never what a caller wants.
	CloudInitDrive string
}
