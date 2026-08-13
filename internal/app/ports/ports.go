// Package ports declares the interfaces the application layer depends on.
// Infrastructure implements them; the application never imports infrastructure.
// This is the seam that keeps handlers testable with in-memory fakes.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/identity"
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
