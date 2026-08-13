// Package identity is the Identity bounded context: users, roles, sessions and
// the invariants that govern authentication. It depends on nothing outside the
// standard library — all mechanism (hashing, storage, clocks) is injected.
package identity

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Role is the portal permission role. A user holds exactly one (RBAC-01).
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleReadOnly Role = "readonly"
	RoleAuditor  Role = "auditor"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleOperator, RoleReadOnly, RoleAuditor:
		return true
	}
	return false
}

// Lockout policy (AUTH-05): five failures inside the window lock the account
// for the lock duration. Both numbers are domain policy, not configuration, so
// that the rule is testable in one place.
const (
	MaxFailedLogins = 5
	FailureWindow   = 15 * time.Minute
	LockoutDuration = 15 * time.Minute
)

// Domain errors. Transport maps these to status codes; the domain does not
// know about HTTP.
var (
	ErrInvalidCredentials = errors.New("identity: invalid credentials")
	ErrAccountLocked      = errors.New("identity: account locked")
	ErrAccountInactive    = errors.New("identity: account inactive")
	ErrInvalidRole        = errors.New("identity: invalid role")
)

// User is the aggregate root for a portal account.
type User struct {
	ID                 uuid.UUID
	Username           string
	Email              string
	DisplayName        string
	PasswordHash       string
	Role               Role
	IsActive           bool
	MustChangePassword bool
	TOTPEnabled        bool
	FailedLoginCount   int
	LastFailedAt       time.Time
	LockedUntil        time.Time
	LastLoginAt        time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// IsLocked reports whether the account is currently locked out.
func (u *User) IsLocked(now time.Time) bool {
	return !u.LockedUntil.IsZero() && now.Before(u.LockedUntil)
}

// CanAuthenticate checks the preconditions that apply before a password is
// even verified. Callers must still verify the password itself.
func (u *User) CanAuthenticate(now time.Time) error {
	if !u.IsActive {
		return ErrAccountInactive
	}
	if u.IsLocked(now) {
		return ErrAccountLocked
	}
	return nil
}

// RegisterFailedLogin records an authentication failure and locks the account
// once the threshold is reached inside the failure window. It reports whether
// this failure caused a lockout, which the caller turns into a security event.
func (u *User) RegisterFailedLogin(now time.Time) (lockedNow bool) {
	// Failures older than the window do not accumulate: a single typo months
	// ago should not combine with today's to lock the account.
	if u.LastFailedAt.IsZero() || now.Sub(u.LastFailedAt) > FailureWindow {
		u.FailedLoginCount = 0
	}
	u.FailedLoginCount++
	u.LastFailedAt = now

	if u.FailedLoginCount >= MaxFailedLogins {
		u.LockedUntil = now.Add(LockoutDuration)
		u.FailedLoginCount = 0
		return true
	}
	return false
}

// RegisterSuccessfulLogin clears failure state after a valid authentication.
func (u *User) RegisterSuccessfulLogin(now time.Time) {
	u.FailedLoginCount = 0
	u.LastFailedAt = time.Time{}
	u.LockedUntil = time.Time{}
	u.LastLoginAt = now
}

// Deactivate disables the account. Callers must also revoke its sessions
// (AUTH-06); the domain records intent, the application enforces the cascade.
func (u *User) Deactivate() { u.IsActive = false }
