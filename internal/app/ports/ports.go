// Package ports declares the interfaces the application layer depends on.
// Infrastructure implements them; the application never imports infrastructure.
// This is the seam that keeps handlers testable with in-memory fakes.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// ErrNotFound is returned by repositories when a record does not exist.
var ErrNotFound = errors.New("ports: not found")

// ErrConflict is returned when a uniqueness constraint would be violated.
var ErrConflict = errors.New("ports: conflict")

// Clock supplies the current time. Injecting it keeps lockout windows, token
// expiry and cooldowns testable without sleeping.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// UserFilter narrows a user listing.
type UserFilter struct {
	Query  string // substring match on username, email or display name
	Role   string
	Active *bool
}

// UserRepository persists user accounts.
type UserRepository interface {
	Create(ctx context.Context, u *identity.User) error
	GetByID(ctx context.Context, id uuid.UUID) (*identity.User, error)
	GetByUsername(ctx context.Context, username string) (*identity.User, error)
	Update(ctx context.Context, u *identity.User) error
	CountAll(ctx context.Context) (int, error)
	List(ctx context.Context, f UserFilter) ([]*identity.User, error)
}

// AccessRepository persists the grouping and grant model.
type AccessRepository interface {
	CreateUserGroup(ctx context.Context, g *access.UserGroup) error
	ListUserGroups(ctx context.Context) ([]access.UserGroup, error)
	DeleteUserGroup(ctx context.Context, id uuid.UUID) error
	SetUserGroups(ctx context.Context, userID uuid.UUID, groupIDs []uuid.UUID) error
	UserGroupNames(ctx context.Context, userID uuid.UUID) ([]string, error)

	CreateVMGroup(ctx context.Context, g *access.VMGroup) error
	ListVMGroups(ctx context.Context) ([]access.VMGroup, error)
	DeleteVMGroup(ctx context.Context, id uuid.UUID) error

	CreateGrant(ctx context.Context, g *access.Grant) error
	ListGrants(ctx context.Context) ([]access.Grant, error)
	DeleteGrant(ctx context.Context, id uuid.UUID) error

	// VisibleVMGroupIDs resolves which VM groups a user reaches via grants.
	VisibleVMGroupIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// SessionRepository persists refresh-token sessions.
type SessionRepository interface {
	Create(ctx context.Context, s *identity.Session) error
	GetByTokenHash(ctx context.Context, hash []byte) (*identity.Session, error)
	// Rotate marks old spent and stores next in one transaction, so a crash
	// can never leave a token both spent and unusable.
	Rotate(ctx context.Context, old *identity.Session, next *identity.Session) error
	RevokeFamily(ctx context.Context, familyID uuid.UUID, at time.Time) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID, at time.Time) error
	IsSessionActive(ctx context.Context, sessionID uuid.UUID) (bool, error)
}

// PasswordHasher hashes and verifies passwords.
type PasswordHasher interface {
	Hash(password string) (string, error)
	Verify(password, encodedHash string) (bool, error)
}

// TokenIssuer mints and validates access tokens.
type TokenIssuer interface {
	Issue(userID uuid.UUID, role string, sessionID uuid.UUID, now time.Time) (string, time.Duration, error)
}

// AuditEntry is one append-only audit record (docs/03-frs.md §3.8).
type AuditEntry struct {
	Time        time.Time
	ActorUserID *uuid.UUID
	ActorName   string
	Category    string
	Action      string
	TargetType  string
	TargetID    string
	TargetName  string
	SourceIP    string
	UserAgent   string
	Outcome     string
	RequestID   string
	Details     map[string]any
}

// Audit categories and outcomes.
const (
	AuditCategoryAuth     = "auth"
	AuditCategoryUserMgmt = "user_mgmt"
	AuditCategorySecurity = "security"

	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeDenied  = "denied"
)

// AuditWriter appends audit entries. Writes must never block the caller's
// primary work from succeeding, but must never be silently dropped either.
type AuditWriter interface {
	Write(ctx context.Context, e AuditEntry) error
}

// SealedCredential is an encrypted platform secret ready for storage.
type SealedCredential struct {
	Kind    string
	TokenID string
	Sealed  crypto.SealedSecret
}

// PlainCredential is a decrypted secret, scoped to one call. It is never
// cached, logged or returned by the API.
type PlainCredential struct {
	Kind    string
	TokenID string
	Secret  string
}

// PlatformRepository persists platforms and their credentials.
type PlatformRepository interface {
	Create(ctx context.Context, p *inventory.Platform, cred SealedCredential) error
	Get(ctx context.Context, id uuid.UUID) (*inventory.Platform, error)
	List(ctx context.Context, includeDisabled bool) ([]*inventory.Platform, error)
	Update(ctx context.Context, p *inventory.Platform) error
	UpdateHealth(ctx context.Context, p *inventory.Platform) error
	SoftDelete(ctx context.Context, id uuid.UUID, at time.Time) error
	Credential(ctx context.Context, platformID uuid.UUID, vault *crypto.Vault) (PlainCredential, error)
	ReplaceCredential(ctx context.Context, platformID uuid.UUID, cred SealedCredential) error
}

// DomainEvent is something that happened, queued for reliable publication.
type DomainEvent struct {
	ID         int64
	OccurredAt time.Time
	Category   string
	Type       string
	Severity   string
	Payload    map[string]any
}

// Event categories and types (docs/12-domain-model.md §12.2).
const (
	EventCategorySyncFailure   = "sync_failure"
	EventCategoryVMStateChange = "vm_state_change"

	EventVMCreated      = "vm.created"
	EventVMStateChanged = "vm.state_changed"
	EventVMDeleted      = "vm.deleted"
	EventSyncFailed     = "sync.failed"
	EventSyncRecovered  = "sync.recovered"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// SyncRunSummary describes one completed synchronization attempt.
type SyncRunSummary struct {
	ID         int64
	Kind       string
	Status     string
	Trigger    string
	StartedAt  time.Time
	FinishedAt time.Time
	DurationMS int64
	Stats      map[string]any
	Error      string
}

// Querier is the subset of a database handle repositories need. It is declared
// here so the application layer can describe transactional work without
// importing a driver.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// StoredAsset is the subset of a persisted asset the reconciler compares
// against a fresh snapshot.
type StoredAsset struct {
	ID           uuid.UUID
	ContentHash  []byte
	SyncState    inventory.SyncState
	MissingCount int
	Name         string
	State        string
	HostID       string
	Extra        map[string]string
}

// SweptAsset is an asset the platform stopped reporting.
type SweptAsset struct {
	ID           uuid.UUID
	ExternalID   string
	Name         string
	SyncState    inventory.SyncState
	MissingCount int
}

// VMSample is one row of VM telemetry ready for storage.
type VMSample struct {
	Time          time.Time
	VMID          uuid.UUID
	CPUPct        *float64
	MemUsedBytes  *int64
	MemTotalBytes *int64
	DiskReadBps   *int64
	DiskWriteBps  *int64
	NetRxBps      *int64
	NetTxBps      *int64
	DiskUsedBytes *int64
}

// HostSample is one row of host telemetry.
type HostSample struct {
	Time          time.Time
	HostID        uuid.UUID
	CPUPct        *float64
	MemUsedBytes  *int64
	MemTotalBytes *int64
}

// CounterKey identifies a cumulative counter for one VM.
type CounterKey struct {
	VMID   uuid.UUID
	Metric string
}

// CounterValue is the last reading of a cumulative counter.
type CounterValue struct {
	Value float64
	Time  time.Time
}

// MetricPoint is one point on a chart.
type MetricPoint struct {
	Time          time.Time `json:"t"`
	CPUPct        float64   `json:"cpu_pct"`
	MemUsedBytes  int64     `json:"mem_used_bytes"`
	MemTotalBytes int64     `json:"mem_total_bytes"`
	DiskReadBps   int64     `json:"disk_read_bps"`
	DiskWriteBps  int64     `json:"disk_write_bps"`
	NetRxBps      int64     `json:"net_rx_bps"`
	NetTxBps      int64     `json:"net_tx_bps"`
	DiskUsedBytes int64     `json:"disk_used_bytes"`
}

// MetricSeries is a chart's worth of points plus the resolution they came from,
// so a client can label the granularity it is looking at.
type MetricSeries struct {
	Resolution string        `json:"resolution"`
	BucketS    int           `json:"bucket_seconds"`
	Points     []MetricPoint `json:"points"`
}

// MetricsRepository stores and reads telemetry.
type MetricsRepository interface {
	WriteVMSamples(ctx context.Context, rows []VMSample) (int64, error)
	WriteHostSamples(ctx context.Context, rows []HostSample) (int64, error)
	CounterState(ctx context.Context, vmIDs []uuid.UUID) (map[CounterKey]CounterValue, error)
	SaveCounterState(ctx context.Context, state map[CounterKey]CounterValue) error
	VMSeries(ctx context.Context, vmID uuid.UUID, from, to, now time.Time) (MetricSeries, error)
	LatestVMMetrics(ctx context.Context, since time.Time) (map[uuid.UUID]MetricPoint, error)
	LastSampleTime(ctx context.Context, platformID uuid.UUID) (time.Time, error)
	VMIDsByExternalID(ctx context.Context, platformID uuid.UUID) (map[string]uuid.UUID, error)
	HostIDsByExternalID(ctx context.Context, platformID uuid.UUID) (map[string]uuid.UUID, error)
}
