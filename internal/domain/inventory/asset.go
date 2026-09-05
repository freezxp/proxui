package inventory

import (
	"time"

	"github.com/google/uuid"
)

// FieldChange is one field that differed between the stored asset and the
// snapshot the platform just returned. These become asset_state_history rows
// and, for state changes, notification events.
type FieldChange struct {
	Field string
	Old   string
	New   string
}

// Pseudo-fields recorded when an asset appears or disappears, so a VM's history
// tab reads as a complete story rather than starting mid-sentence.
const (
	FieldCreated = "_created"
	FieldDeleted = "_deleted"
	FieldMissing = "_missing"
	// FieldConverted marks a guest that left the inventory because it became a
	// template. It is not FieldDeleted and it is certainly not FieldMissing:
	// the machine did not vanish, it was deliberately turned into the thing
	// other machines are cloned from, and an operator reading the trail needs
	// to be able to tell those apart.
	FieldConverted = "_converted_to_template"
	// FieldRestored marks a VM the platform started reporting again after it
	// had been swept away. It is distinct from _created: the row, its history,
	// its portal tags and its notes all survived, and an operator reading the
	// tab needs to see the same machine returning rather than a new one.
	FieldRestored = "_restored"
)

// VM is a synced virtual machine. Platform-derived fields are overwritten on
// every sync; portal-owned fields (tags, notes) are never touched by sync, so
// the two sets are disjoint and conflict resolution is trivial by construction
// (docs/10-sync-engine.md §10.4).
type VM struct {
	ID           uuid.UUID
	PlatformID   uuid.UUID
	HostID       *uuid.UUID
	ExternalID   string
	Name         string
	VMType       string
	State        string
	CPUCores     int
	MemoryBytes  int64
	DiskBytes    int64
	UptimeS      int64
	IPAddresses  []string
	PlatformTags []string
	PlatformPool string
	PortalTags   []string // portal-owned
	Notes        string   // portal-owned
	ContentHash  []byte
	SyncState    SyncState
	MissingCount int
	Attrs        map[string]any
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	DeletedAt    time.Time
}

// Host is a synced hypervisor node.
type Host struct {
	ID           uuid.UUID
	PlatformID   uuid.UUID
	ExternalID   string
	Name         string
	Status       string
	CPUCores     int
	MemoryBytes  int64
	Version      string
	UptimeS      int64
	ContentHash  []byte
	SyncState    SyncState
	MissingCount int
	Attrs        map[string]any
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	DeletedAt    time.Time
}

// StoragePool is a synced storage pool.
type StoragePool struct {
	ID           uuid.UUID
	PlatformID   uuid.UUID
	HostID       *uuid.UUID
	ExternalID   string
	NaturalKey   string
	Name         string
	StorageType  string
	TotalBytes   int64
	UsedBytes    int64
	IsShared     bool
	ContentHash  []byte
	SyncState    SyncState
	MissingCount int
	Attrs        map[string]any
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	DeletedAt    time.Time
}

// Network is a synced network interface.
type Network struct {
	ID           uuid.UUID
	PlatformID   uuid.UUID
	HostID       *uuid.UUID
	ExternalID   string
	NaturalKey   string
	Name         string
	NetType      string
	CIDR         string
	VLANTag      int
	ContentHash  []byte
	SyncState    SyncState
	MissingCount int
	Attrs        map[string]any
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	DeletedAt    time.Time
}

// MarkMissing advances the mark-and-sweep counter for an asset that was absent
// from a snapshot, and reports whether it has now been absent long enough to
// count as deleted.
//
// The counter exists because a single missed appearance is far more often an
// API hiccup or a node timing out than a real deletion; three consecutive
// misses is evidence. Until then the asset shows as missing in the UI, so
// operators see the anomaly without a false alarm.
func MarkMissing(state SyncState, missingCount int, now time.Time) (newState SyncState, newCount int, deleted bool) {
	newCount = missingCount + 1
	if newCount >= MissingThreshold {
		return SyncDeleted, newCount, true
	}
	return SyncMissing, newCount, false
}

// SoftDeleteRetention is how long deleted assets are kept before the janitor
// purges them, so audit entries and history stay resolvable.
const SoftDeleteRetention = 90 * 24 * time.Hour
