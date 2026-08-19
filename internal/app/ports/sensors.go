package ports

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// NodeCommandRunner runs one command on a node and returns its standard
// output.
//
// Separate from SSHDialer, and returning bytes rather than a connection, so
// that no node connection can become a terminal or a file browser. The
// boundary ADR 0007 relaxed SSH-02 to is held by this signature.
type NodeCommandRunner interface {
	RunCommand(ctx context.Context, target SSHTarget, cred SSHCredential,
		policy HostKeyPolicy, command string) ([]byte, error)
}

// NodeSensorReader gets one node's current readings.
//
// The app layer knows which nodes to poll and what to do with a pin; how a
// reading is fetched — SSH, one fixed command, and the format that comes back
// — belongs to infrastructure, and this is the seam between the two.
type NodeSensorReader interface {
	Read(ctx context.Context, target SSHTarget, cred SSHCredential,
		policy HostKeyPolicy) ([]telemetry.Reading, error)
}

// NodeSSH is what the portal knows about reaching one node.
type NodeSSH struct {
	HostID      uuid.UUID `json:"-"`
	Address     string    `json:"address"`
	SSHUser     string    `json:"ssh_user"`
	Algorithm   string    `json:"algorithm"`
	Fingerprint string    `json:"fingerprint"`
	PublicKey   []byte    `json:"-"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastTriedAt time.Time `json:"last_tried_at,omitempty"`
	LastOKAt    time.Time `json:"last_ok_at,omitempty"`
	// LastError is the most recent failure in the operator's words: no key
	// installed, no lm-sensors, host key changed. Empty after a success.
	LastError string `json:"last_error,omitempty"`
}

// NodeSSHStore holds the pinned key and the last outcome per node.
type NodeSSHStore interface {
	Get(ctx context.Context, hostID uuid.UUID) (NodeSSH, error)
	// Pin records the key a node presented the first time it was met.
	Pin(ctx context.Context, rec NodeSSH) error
	// RecordAttempt saves how the last poll went, success or not.
	RecordAttempt(ctx context.Context, hostID uuid.UUID, at time.Time, failure string) error
	// Forget drops the pin so the next poll meets the node afresh. Admin-only
	// and audited, exactly like clearing a guest's pin.
	Forget(ctx context.Context, hostID uuid.UUID) error
}

// SensorReadings is one node's sensors as of one moment.
type SensorReadings struct {
	HostID   uuid.UUID           `json:"host_id"`
	At       time.Time           `json:"at"`
	Readings []telemetry.Reading `json:"readings"`
}

// SensorPoint is one reading in a series.
type SensorPoint struct {
	Time  time.Time `json:"t"`
	Value float64   `json:"v"`
	Max   float64   `json:"max,omitempty"`
}

// HostSensorStore persists and reads node sensor readings.
type HostSensorStore interface {
	Write(ctx context.Context, in SensorReadings) (int, error)
	// Latest is every sensor's most recent reading for one node.
	Latest(ctx context.Context, hostID uuid.UUID) (SensorReadings, error)
	// Summaries is what a list of hosts needs: the hottest reading each, in
	// one query rather than one per row.
	Summaries(ctx context.Context, hostIDs []uuid.UUID) (map[uuid.UUID]telemetry.SensorSummary, error)
	// Series is one sensor's history, bucketed by the resolution the window
	// calls for.
	Series(ctx context.Context, hostID uuid.UUID, chip, label string,
		from, to time.Time, res telemetry.Resolution) ([]SensorPoint, error)
	// HottestNow is what the alert evaluator reads: one value per host.
	HottestNow(ctx context.Context, since time.Time) (map[uuid.UUID]telemetry.Reading, error)
}

// SensorHost is a node the collector should poll.
type SensorHost struct {
	ID         uuid.UUID
	PlatformID uuid.UUID
	ExternalID string
	Name       string
}

// SensorHostLister names the online hosts of a platform.
type SensorHostLister interface {
	OnlineHosts(ctx context.Context, platformID uuid.UUID) ([]SensorHost, error)
}

// PortalKeyReader hands out the portal's own private key, which is the only
// credential a node connection uses (ADR 0006, ADR 0007).
type PortalKeyReader interface {
	PrivateKey(ctx context.Context) (string, error)
}
