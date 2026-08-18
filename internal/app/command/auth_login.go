// Package command holds application write operations. Each command owns one
// use case: it enforces domain rules, persists the result, and records audit
// and events. Queries live in the sibling query package (lightweight CQRS).
package command

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
	"github.com/freezxp/proxui/internal/infra/metrics"
)

// decoyHash returns a real argon2id hash of a random secret, computed once per
// process. Verifying against it when the username does not exist makes a
// missing account cost the same as a wrong password, so response timing cannot
// be used to enumerate usernames.
func (h *Login) decoyHash() string {
	h.decoyOnce.Do(func() {
		secret, _, err := crypto.NewOpaqueToken()
		if err != nil {
			secret = "decoy-password-fallback"
		}
		if hash, err := h.Hasher.Hash(secret); err == nil {
			h.decoy = hash
		}
	})
	return h.decoy
}

// LoginInput carries the credentials plus the request metadata that lands in
// the audit trail.
type LoginInput struct {
	Username  string
	Password  string
	IP        string
	UserAgent string
	RequestID string
}

// LoginOutput is the successful result of an authentication.
//
// It has two shapes. Either the sign-in is complete and it carries tokens, or
// a second factor is owed and it carries only a challenge id. The zero token
// fields in the second case are load-bearing: a caller that forgets to check
// MFARequired hands the browser no session rather than a session that skipped
// the factor.
type LoginOutput struct {
	AccessToken  string
	ExpiresIn    time.Duration
	RefreshToken string
	User         *identity.User

	// MFARequired means the password was right and a code is needed (AUTH-04).
	MFARequired bool
	// MFAToken is the challenge id to present to the verify endpoint. It is
	// worth nothing on its own: it names a pending sign-in, expires in
	// minutes, and only a code from the enrolled device completes it.
	MFAToken string
}

// Login authenticates a user and starts a session (AUTH-01, AUTH-02, AUTH-05).
type Login struct {
	Users    ports.UserRepository
	Sessions ports.SessionRepository
	Hasher   ports.PasswordHasher
	Tokens   ports.TokenIssuer
	Audit    ports.AuditWriter
	Clock    ports.Clock

	// Challenges holds sign-ins waiting for a second factor (AUTH-04). Nil in
	// a deployment or a test with no MFA wired, which is why an enrolled
	// account is refused rather than let through when it is missing.
	Challenges ports.MFAChallengeStore

	decoyOnce sync.Once
	decoy     string
}

// Handle performs the login. Every failure path returns ErrInvalidCredentials
// to the caller regardless of cause; the specific reason goes to the audit log.
func (h *Login) Handle(ctx context.Context, in LoginInput) (LoginOutput, error) {
	now := h.Clock.Now()

	user, err := h.Users.GetByUsername(ctx, in.Username)
	if err != nil {
		if !errors.Is(err, ports.ErrNotFound) {
			return LoginOutput{}, fmt.Errorf("login: %w", err)
		}
		_, _ = h.Hasher.Verify(in.Password, h.decoyHash())
		h.auditFailure(ctx, in, nil, "unknown_user", now)
		return LoginOutput{}, identity.ErrInvalidCredentials
	}

	// An account that signs in through a provider has no password here. Refuse
	// it on the provider rather than on the hash comparison: the stored hash is
	// empty, so a comparison would fail anyway, but as "wrong password" — which
	// invites guessing at a password that does not exist.
	if !user.UsesPassword() {
		h.auditFailure(ctx, in, user, "wrong_provider", now)
		return LoginOutput{}, identity.ErrInvalidCredentials
	}

	if err := user.CanAuthenticate(now); err != nil {
		reason := "inactive"
		if errors.Is(err, identity.ErrAccountLocked) {
			reason = "locked"
		}
		h.auditFailure(ctx, in, user, reason, now)
		return LoginOutput{}, err
	}

	ok, err := h.Hasher.Verify(in.Password, user.PasswordHash)
	if err != nil {
		return LoginOutput{}, fmt.Errorf("login: verify password: %w", err)
	}
	if !ok {
		lockedNow := user.RegisterFailedLogin(now)
		if err := h.Users.Update(ctx, user); err != nil {
			return LoginOutput{}, fmt.Errorf("login: record failure: %w", err)
		}
		h.auditFailure(ctx, in, user, "bad_password", now)
		if lockedNow {
			h.write(ctx, ports.AuditEntry{
				Time: now, ActorUserID: &user.ID, ActorName: user.Username,
				Category: ports.AuditCategorySecurity, Action: "account_locked",
				Outcome: ports.OutcomeFailure, SourceIP: in.IP, UserAgent: in.UserAgent,
				RequestID: in.RequestID,
				Details:   map[string]any{"until": user.LockedUntil, "threshold": identity.MaxFailedLogins},
			})
		}
		return LoginOutput{}, identity.ErrInvalidCredentials
	}

	// The password is right. If a second factor is enrolled, everything that
	// makes this a sign-in - the session, the token, the cleared failure
	// count, the last-login stamp - waits for the code. Recording success here
	// and demanding the code afterwards would leave an account that looks
	// signed in to every report while its owner is still holding their phone.
	if user.TOTPEnabled {
		if h.Challenges == nil {
			return LoginOutput{}, fmt.Errorf("login: %w", identity.ErrMFARequired)
		}
		challenge := identity.NewMFAChallenge(user.ID, in.IP, in.UserAgent, now)
		if err := h.Challenges.Issue(ctx, challenge); err != nil {
			return LoginOutput{}, fmt.Errorf("login: issue mfa challenge: %w", err)
		}
		h.write(ctx, ports.AuditEntry{
			Time: now, ActorUserID: &user.ID, ActorName: user.Username,
			Category: ports.AuditCategoryAuth, Action: "mfa_challenged",
			Outcome: ports.OutcomeSuccess, SourceIP: in.IP, UserAgent: in.UserAgent,
			RequestID: in.RequestID,
		})
		return LoginOutput{MFARequired: true, MFAToken: challenge.ID.String(), User: user}, nil
	}

	refreshToken, refreshHash, err := crypto.NewOpaqueToken()
	if err != nil {
		return LoginOutput{}, fmt.Errorf("login: %w", err)
	}
	session := identity.NewSession(user.ID, refreshHash, in.IP, in.UserAgent, now)
	if err := h.Sessions.Create(ctx, session); err != nil {
		return LoginOutput{}, fmt.Errorf("login: create session: %w", err)
	}

	accessToken, ttl, err := h.Tokens.Issue(user.ID, string(user.Role), user.Username, session.ID, now)
	if err != nil {
		return LoginOutput{}, fmt.Errorf("login: %w", err)
	}

	user.RegisterSuccessfulLogin(now)
	if err := h.Users.Update(ctx, user); err != nil {
		return LoginOutput{}, fmt.Errorf("login: record success: %w", err)
	}

	metrics.LoginSuccesses.Inc()
	h.write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &user.ID, ActorName: user.Username,
		Category: ports.AuditCategoryAuth, Action: "login_success",
		Outcome: ports.OutcomeSuccess, SourceIP: in.IP, UserAgent: in.UserAgent,
		RequestID: in.RequestID, Details: map[string]any{"session_id": session.ID.String()},
	})

	return LoginOutput{
		AccessToken:  accessToken,
		ExpiresIn:    ttl,
		RefreshToken: refreshToken,
		User:         user,
	}, nil
}

func (h *Login) auditFailure(ctx context.Context, in LoginInput, user *identity.User, reason string, now time.Time) {
	// A burst here is the shape of credential stuffing, which is why it is a
	// metric and not only an audit row (docs/16 §16.2).
	metrics.LoginFailures.Inc()
	e := ports.AuditEntry{
		Time: now, ActorName: in.Username,
		Category: ports.AuditCategoryAuth, Action: "login_failed",
		Outcome: ports.OutcomeFailure, SourceIP: in.IP, UserAgent: in.UserAgent,
		RequestID: in.RequestID, Details: map[string]any{"reason": reason},
	}
	if user != nil {
		e.ActorUserID = &user.ID
		e.ActorName = user.Username
	}
	h.write(ctx, e)
}

// write records an audit entry. A failing audit write must not mask the
// authentication result, but it must be visible, so it is returned as an error
// only when the caller has nothing else to report.
func (h *Login) write(ctx context.Context, e ports.AuditEntry) {
	_ = h.Audit.Write(ctx, e)
}
