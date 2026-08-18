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
	// RoleNewUser can sign in and nothing else. It is what a self-registered
	// account starts as: the account exists, but every part of the portal is
	// closed to it until an administrator says otherwise.
	RoleNewUser Role = "newuser"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleOperator, RoleReadOnly, RoleAuditor, RoleNewUser:
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
	// ErrCannotDeleteSelf refuses an administrator's request to delete their
	// own account. Not paternalism: the request would succeed, take their own
	// session with it, and leave them looking at a portal that no longer knows
	// who they are. Deactivating someone else and being deactivated in turn is
	// the two-person path this preserves.
	ErrCannotDeleteSelf = errors.New("identity: an administrator cannot delete their own account")
	// ErrLastAdmin refuses the deletion that would leave the portal with no
	// administrator who can sign in. Recovering from that state means the
	// first-run bootstrap (ADM-03), which only runs against an empty portal —
	// so this is a door that locks from the outside.
	ErrLastAdmin = errors.New("identity: the last active administrator cannot be deleted")
)

// User is the aggregate root for a portal account.
type User struct {
	ID uuid.UUID
	// AuthProvider names where this account signs in. A provider account has
	// no password of its own, so a password login against one must be refused
	// on the provider rather than on the hash comparison — otherwise the
	// failure looks like a wrong password and invites guessing.
	AuthProvider AuthProvider
	// ExternalID is the provider's own stable identifier, kept because an
	// email address can be reassigned while a subject identifier cannot.
	ExternalID         string
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

// AuthProvider is where an account authenticates.
type AuthProvider string

const (
	// ProviderLocal is a password held by this portal.
	ProviderLocal AuthProvider = "local"
	// ProviderGoogle is Google, via OpenID Connect.
	ProviderGoogle AuthProvider = "google"
)

// ErrWrongProvider is returned when someone tries to sign in with a password
// against an account that belongs to an identity provider.
var ErrWrongProvider = errors.New("identity: this account signs in with its provider")

// UsesPassword reports whether this account authenticates with a password.
func (u *User) UsesPassword() bool {
	return u.AuthProvider == "" || u.AuthProvider == ProviderLocal
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
	u.ClearLockout()
	u.LastLoginAt = now
}

// ClearLockout resets failure state without recording a login. An administrator
// resetting a password should unlock the account, not fake a sign-in.
func (u *User) ClearLockout() {
	u.FailedLoginCount = 0
	u.LastFailedAt = time.Time{}
	u.LockedUntil = time.Time{}
}

// Deactivate disables the account. Callers must also revoke its sessions
// (AUTH-06); the domain records intent, the application enforces the cascade.
func (u *User) Deactivate() { u.IsActive = false }
