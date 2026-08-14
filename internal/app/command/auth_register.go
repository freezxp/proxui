package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// ErrRegistrationClosed is returned when self-registration is switched off,
// which is how a portal ships.
var ErrRegistrationClosed = errors.New("identity: self-registration is not enabled")

// RegistrationPolicy answers whether a new account may be created, and is read
// per request so an administrator turning registration off takes effect at
// once rather than at the next restart.
type RegistrationPolicy interface {
	SelfRegistrationEnabled(ctx context.Context) bool
}

// NewAccountRole is what every self-provisioned account starts as.
//
// Not read-only: that role can see the hosts, storage and networks the estate
// is made of, which is more than someone who has just signed up should get.
// A new user reaches one page telling it to ask for access, and nothing else
// (docs/adr/0003).
const NewAccountRole = identity.RoleNewUser

// RegisterInput is someone creating their own account.
type RegisterInput struct {
	Actor       Actor
	Username    string
	Email       string
	DisplayName string
	Password    string
}

// Register creates an account for someone who is not signed in.
type Register struct {
	Users  ports.UserRepository
	Policy RegistrationPolicy
	Hasher ports.PasswordHasher
	Audit  ports.AuditWriter
	Clock  ports.Clock
}

// Handle validates and creates the account.
func (h *Register) Handle(ctx context.Context, in RegisterInput) (*identity.User, error) {
	if !h.Policy.SelfRegistrationEnabled(ctx) {
		return nil, ErrRegistrationClosed
	}

	username := strings.ToLower(strings.TrimSpace(in.Username))
	email := strings.ToLower(strings.TrimSpace(in.Email))

	if err := identity.ValidateUsername(username); err != nil {
		return nil, err
	}
	if err := identity.ValidateEmail(email); err != nil {
		return nil, err
	}
	if err := identity.ValidatePassword(in.Password, username, email); err != nil {
		return nil, err
	}

	hash, err := h.Hasher.Hash(in.Password)
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}

	now := h.Clock.Now()
	user := &identity.User{
		ID: uuid.New(), AuthProvider: identity.ProviderLocal,
		Username: username, Email: email,
		DisplayName: strings.TrimSpace(in.DisplayName), PasswordHash: hash,
		Role: NewAccountRole, IsActive: true,
		// They chose this password a moment ago; forcing an immediate change
		// would be theatre.
		MustChangePassword: false,
		CreatedAt:          now,
	}

	if err := h.Users.Create(ctx, user); err != nil {
		// A taken username and a taken email are the same answer to the
		// caller. Distinguishing them turns registration into an account
		// enumeration oracle.
		if errors.Is(err, ports.ErrConflict) {
			return nil, ports.ErrConflict
		}
		return nil, err
	}

	writeAudit(ctx, h.Audit, in.Actor, now, ports.AuditCategoryUserMgmt, "user_registered",
		"user", user.ID.String(), user.Username,
		map[string]any{"provider": string(identity.ProviderLocal), "role": string(user.Role)})
	return user, nil
}

// ExternalIdentity is what an identity provider told us about someone.
type ExternalIdentity struct {
	Provider    identity.AuthProvider
	Subject     string // the provider's stable identifier for this person
	Email       string
	DisplayName string
	// EmailVerified is refused when false: an unverified address could belong
	// to someone else, and the email is what links a provider account to a
	// portal account.
	EmailVerified bool
}

// SignInExternal finds or creates the account behind a provider identity.
type SignInExternal struct {
	Users  ports.UserRepository
	Policy RegistrationPolicy
	Audit  ports.AuditWriter
	Clock  ports.Clock
}

// Handle resolves a provider identity to a portal account.
//
// Lookup is by the provider's subject first and the email address second. The
// subject is stable where an email is not — a person who changes their address
// is still the same account, and an address reassigned to someone else must
// not hand them the previous holder's access.
func (h *SignInExternal) Handle(ctx context.Context, in ExternalIdentity, actor Actor) (*identity.User, error) {
	if in.Subject == "" || in.Email == "" {
		return nil, fmt.Errorf("%w: the provider returned no identity", identity.ErrInvalidCredentials)
	}
	if !in.EmailVerified {
		h.auditDenied(ctx, actor, in, "email_unverified")
		return nil, fmt.Errorf("%w: the provider has not verified that address", identity.ErrInvalidCredentials)
	}

	email := strings.ToLower(strings.TrimSpace(in.Email))
	now := h.Clock.Now()

	user, err := h.Users.GetByExternalID(ctx, in.Provider, in.Subject)
	switch {
	case err == nil:
		return h.admit(ctx, user, actor, now)
	case !errors.Is(err, ports.ErrNotFound):
		return nil, err
	}

	// First time with this subject. An account may still exist under the same
	// address — created by an administrator, or registered with a password —
	// in which case this links the two rather than creating a duplicate.
	user, err = h.Users.GetByEmail(ctx, email)
	if err == nil {
		// An account created before providers existed carries the zero value
		// rather than "local"; UsesPassword already treats the two alike and
		// this must as well, or such an account can never be linked.
		if user.UsesPassword() && user.ExternalID == "" {
			user.AuthProvider = in.Provider
			user.ExternalID = in.Subject
			// The stored password is now unreachable: the account signs in
			// through the provider, and a provider account is refused on the
			// password path.
			if err := h.Users.Update(ctx, user); err != nil {
				return nil, err
			}
			writeAudit(ctx, h.Audit, actor, now, ports.AuditCategoryUserMgmt, "user_linked_provider",
				"user", user.ID.String(), user.Username,
				map[string]any{"provider": string(in.Provider)})
		}
		return h.admit(ctx, user, actor, now)
	}
	if !errors.Is(err, ports.ErrNotFound) {
		return nil, err
	}

	if !h.Policy.SelfRegistrationEnabled(ctx) {
		h.auditDenied(ctx, actor, in, "registration_closed")
		return nil, ErrRegistrationClosed
	}

	user = &identity.User{
		ID: uuid.New(), AuthProvider: in.Provider, ExternalID: in.Subject,
		Username: usernameFromEmail(email), Email: email,
		DisplayName: strings.TrimSpace(in.DisplayName),
		// No password: this account cannot use the password path at all.
		PasswordHash: "", Role: NewAccountRole, IsActive: true,
		MustChangePassword: false, CreatedAt: now,
	}
	if err := h.Users.Create(ctx, user); err != nil {
		return nil, err
	}
	writeAudit(ctx, h.Audit, actor, now, ports.AuditCategoryUserMgmt, "user_registered",
		"user", user.ID.String(), user.Username,
		map[string]any{"provider": string(in.Provider), "role": string(user.Role)})

	return h.admit(ctx, user, actor, now)
}

// admit applies the same account checks the password path applies, so a
// disabled or locked account cannot be sidestepped by signing in with Google.
func (h *SignInExternal) admit(ctx context.Context, user *identity.User, actor Actor, now time.Time) (*identity.User, error) {
	if err := user.CanAuthenticate(now); err != nil {
		h.auditDenied(ctx, actor, ExternalIdentity{Email: user.Email}, "account_unavailable")
		return nil, err
	}
	return user, nil
}

func (h *SignInExternal) auditDenied(ctx context.Context, actor Actor, in ExternalIdentity, reason string) {
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: h.Clock.Now(), ActorName: in.Email,
		Category: ports.AuditCategoryAuth, Action: "login_failed",
		Outcome: ports.OutcomeFailure, SourceIP: actor.IP, UserAgent: actor.UserAgent,
		RequestID: actor.RequestID,
		Details:   map[string]any{"reason": reason, "provider": string(in.Provider)},
	})
}

// usernameFromEmail derives a username, since a provider gives an address
// rather than one. Collisions are possible between different domains, so the
// caller's Create is what actually settles uniqueness — this only proposes.
func usernameFromEmail(email string) string {
	local, _, found := strings.Cut(email, "@")
	if !found || local == "" {
		return email
	}
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return -1
	}, local)
	if cleaned == "" {
		return email
	}
	return cleaned
}
