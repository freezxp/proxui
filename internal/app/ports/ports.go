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
	"github.com/freezxp/proxui/internal/domain/console"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/domain/notify"
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
	// SetVMGroupMembers replaces a group's manually chosen members. Members
	// added by an auto rule are left alone: they belong to the rule, and a
	// manual edit must not silently undo synchronization.
	SetVMGroupMembers(ctx context.Context, groupID uuid.UUID, vmIDs []uuid.UUID) error
	VMGroupMemberIDs(ctx context.Context, groupID uuid.UUID) ([]uuid.UUID, error)

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
	Issue(userID uuid.UUID, role, username string, sessionID uuid.UUID, now time.Time) (string, time.Duration, error)
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
//
// The tags matter: this type is serialized onto the live event socket, so it is
// part of the public API and follows the same snake_case shape as every REST
// response rather than exposing Go field names.
type DomainEvent struct {
	ID         int64          `json:"id"`
	OccurredAt time.Time      `json:"occurred_at"`
	Category   string         `json:"category"`
	Type       string         `json:"type"`
	Severity   string         `json:"severity"`
	Payload    map[string]any `json:"payload"`
}

// Event categories and types (docs/12-domain-model.md §12.2).
const (
	EventCategorySyncFailure   = "sync_failure"
	EventCategoryVMStateChange = "vm_state_change"
	// EventCategoryPerformanceAlert carries threshold alerts (NOTIF-04). It
	// matches the category name notification rules route on.
	EventCategoryPerformanceAlert = "performance_alert"

	EventVMCreated      = "vm.created"
	EventVMStateChanged = "vm.state_changed"
	EventVMDeleted      = "vm.deleted"
	EventSyncFailed     = "sync.failed"
	EventSyncRecovered  = "sync.recovered"
	EventAlertFiring    = "alert.firing"
	EventAlertResolved  = "alert.resolved"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// SyncRunSummary describes one completed synchronization attempt.
type SyncRunSummary struct {
	ID         int64          `json:"id"`
	Kind       string         `json:"kind"`
	Status     string         `json:"status"`
	Trigger    string         `json:"trigger"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at,omitempty"`
	DurationMS int64          `json:"duration_ms"`
	Stats      map[string]any `json:"stats"`
	Error      string         `json:"error,omitempty"`
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

// VMFilter narrows an inventory listing. Role and UserID decide visibility.
type VMFilter struct {
	Role       identity.Role
	UserID     uuid.UUID
	Query      string
	State      string
	PlatformID uuid.UUID
	HostID     uuid.UUID
	GroupID    uuid.UUID
	Tag        string
	Sort       string
	Limit      int
	Offset     int
}

// VMListItem is a row in the inventory table.
type VMListItem struct {
	ID           uuid.UUID  `json:"id"`
	ExternalID   string     `json:"external_id"`
	Name         string     `json:"name"`
	VMType       string     `json:"vm_type"`
	State        string     `json:"state"`
	CPUCores     int        `json:"cpu_cores"`
	MemoryBytes  int64      `json:"memory_bytes"`
	DiskBytes    int64      `json:"disk_bytes"`
	UptimeS      int64      `json:"uptime_s"`
	IPAddresses  []string   `json:"ip_addresses"`
	PlatformTags []string   `json:"platform_tags"`
	PortalTags   []string   `json:"portal_tags"`
	Pool         string     `json:"platform_pool,omitempty"`
	SyncState    string     `json:"sync_state"`
	LastSeenAt   time.Time  `json:"last_seen_at"`
	PlatformID   uuid.UUID  `json:"platform_id"`
	PlatformName string     `json:"platform_name"`
	Datacenter   string     `json:"datacenter"`
	HostID       *uuid.UUID `json:"host_id,omitempty"`
	HostName     string     `json:"host_name,omitempty"`
	CPUPct       float64    `json:"cpu_pct"`
	MemPct       float64    `json:"mem_pct"`
}

// VMDetail is the VM detail page's read model.
type VMDetail struct {
	VMListItem
	Notes       string         `json:"notes"`
	Groups      []string       `json:"groups"`
	Attrs       map[string]any `json:"attrs,omitempty"`
	FirstSeenAt time.Time      `json:"first_seen_at"`
}

// VMPage is a page of inventory.
type VMPage struct {
	Items  []VMListItem
	Total  int
	Limit  int
	Offset int
}

// HistoryEntry is one recorded field change.
type HistoryEntry struct {
	ChangedAt time.Time `json:"changed_at"`
	Field     string    `json:"field"`
	OldValue  string    `json:"old_value"`
	NewValue  string    `json:"new_value"`
}

// PlatformHealth summarizes a platform for the dashboard.
type PlatformHealth struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Datacenter  string    `json:"datacenter"`
	Health      string    `json:"health"`
	Version     string    `json:"version,omitempty"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	BreakerOpen bool      `json:"breaker_open"`
	VMCount     int       `json:"vm_count"`
}

// TopConsumer is one entry in a busiest-VMs list.
type TopConsumer struct {
	VMID         uuid.UUID `json:"vm_id"`
	Name         string    `json:"name"`
	PlatformName string    `json:"platform_name"`
	Value        float64   `json:"value"`
}

// DashboardSummary is the dashboard's read model.
type DashboardSummary struct {
	TotalVMs   int              `json:"total_vms"`
	RunningVMs int              `json:"running_vms"`
	StoppedVMs int              `json:"stopped_vms"`
	OtherVMs   int              `json:"other_vms"`
	MissingVMs int              `json:"missing_vms"`
	Platforms  []PlatformHealth `json:"platforms"`
	TopCPU     []TopConsumer    `json:"top_cpu"`
	TopMemory  []TopConsumer    `json:"top_memory"`
}

// InventoryReader serves the inventory read model.
type InventoryReader interface {
	ListVMs(ctx context.Context, f VMFilter) (VMPage, error)
	GetVM(ctx context.Context, id uuid.UUID, role identity.Role, userID uuid.UUID) (VMDetail, error)
	CanAccessVM(ctx context.Context, id uuid.UUID, role identity.Role, userID uuid.UUID) (bool, error)
	VMHistory(ctx context.Context, id uuid.UUID, limit int) ([]HistoryEntry, error)
	SetPortalTags(ctx context.Context, id uuid.UUID, tags []string) error
	SetNotes(ctx context.Context, id uuid.UUID, notes string) error
	Dashboard(ctx context.Context, role identity.Role, userID uuid.UUID) (DashboardSummary, error)
}

// AuditFilter narrows an audit search.
type AuditFilter struct {
	From       time.Time
	To         time.Time
	ActorID    uuid.UUID
	Category   string
	Action     string
	Outcome    string
	TargetType string
	TargetID   string
	Query      string
	Limit      int
	Offset     int
}

// AuditRecord is one audit entry as returned to an auditor.
type AuditRecord struct {
	ID         int64          `json:"id"`
	Time       time.Time      `json:"ts"`
	ActorID    *uuid.UUID     `json:"actor_user_id,omitempty"`
	ActorName  string         `json:"actor_name"`
	Category   string         `json:"category"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type,omitempty"`
	TargetID   string         `json:"target_id,omitempty"`
	TargetName string         `json:"target_name,omitempty"`
	SourceIP   string         `json:"source_ip,omitempty"`
	Outcome    string         `json:"outcome"`
	RequestID  string         `json:"request_id,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

// AuditPage is a page of audit records.
type AuditPage struct {
	Items  []AuditRecord
	Total  int
	Limit  int
	Offset int
}

// AuditReader searches the audit log.
type AuditReader interface {
	Search(ctx context.Context, f AuditFilter) (AuditPage, error)
	Stream(ctx context.Context, f AuditFilter, fn func(AuditRecord) error) error
	Categories(ctx context.Context) (map[string][]string, error)
}

// ConsoleSessionRecord is a console session as shown to an administrator.
type ConsoleSessionRecord struct {
	ID          uuid.UUID `json:"id"`
	UserID      uuid.UUID `json:"user_id"`
	Username    string    `json:"username"`
	VMID        uuid.UUID `json:"vm_id"`
	VMName      string    `json:"vm_name"`
	Kind        string    `json:"kind"`
	ClientIP    string    `json:"client_ip,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	ConnectedAt time.Time `json:"connected_at,omitempty"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
	CloseReason string    `json:"close_reason,omitempty"`
	BytesTx     int64     `json:"bytes_tx"`
	BytesRx     int64     `json:"bytes_rx"`
	Active      bool      `json:"active"`
}

// ConsoleRepository records console sessions.
type ConsoleRepository interface {
	Create(ctx context.Context, s *console.Session) error
	MarkConnected(ctx context.Context, id uuid.UUID, at time.Time) error
	Close(ctx context.Context, id uuid.UUID, reason string, tx, rx int64, at time.Time) error
	Get(ctx context.Context, id uuid.UUID) (*console.Session, error)
	List(ctx context.Context, activeOnly bool, limit int) ([]ConsoleSessionRecord, error)
}

// TicketStore holds one-time console tickets.
type TicketStore interface {
	Issue(ctx context.Context, t console.Ticket) error
	Redeem(ctx context.Context, id string) (console.Ticket, error)
}

// DeliveryRecord is one row of the notification delivery log (NOTIF-03).
type DeliveryRecord struct {
	ID          int64      `json:"id"`
	ChannelID   uuid.UUID  `json:"channel_id"`
	ChannelName string     `json:"channel_name"`
	Subject     string     `json:"subject"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
}

// NotifyRepository stores channels, routing rules and deliveries.
type NotifyRepository interface {
	CreateChannel(ctx context.Context, ch *notify.Channel, sealed *crypto.SealedSecret) error
	UpdateChannel(ctx context.Context, ch *notify.Channel, sealed *crypto.SealedSecret) error
	ListChannels(ctx context.Context) ([]notify.Channel, error)
	GetChannel(ctx context.Context, id uuid.UUID) (notify.Channel, error)
	ChannelSecret(ctx context.Context, id uuid.UUID, vault *crypto.Vault) (string, error)
	DeleteChannel(ctx context.Context, id uuid.UUID, at time.Time) error

	CreateRule(ctx context.Context, rule *notify.Rule) error
	ListRules(ctx context.Context) ([]notify.Rule, error)
	DeleteRule(ctx context.Context, id uuid.UUID) error

	ClaimDelivery(ctx context.Context, outboxID int64, channelID uuid.UUID, subject string, now time.Time) (int64, bool, error)
	RecordDelivery(ctx context.Context, channelID uuid.UUID, subject string, now time.Time) (int64, error)
	FinishDelivery(ctx context.Context, id int64, sendErr error, now time.Time) error
	ListDeliveries(ctx context.Context, limit int) ([]DeliveryRecord, error)
}
