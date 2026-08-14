package command

import (
	"context"
	"fmt"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
	"github.com/freezxp/proxui/internal/infra/metrics"
)

// IssueSession mints a session for an account that has already established who
// it is by some route other than a password: registering, or signing in
// through an identity provider.
//
// Deliberately separate from Login rather than a flag on it. Login's job is to
// decide whether a password is correct, and a path that skips that decision
// should not run through the same function — the risk is a future edit making
// the skip reachable from the password path.
type IssueSession struct {
	Users    ports.UserRepository
	Sessions ports.SessionRepository
	Tokens   ports.TokenIssuer
	Audit    ports.AuditWriter
	Clock    ports.Clock
}

// Issue creates the session and records the sign-in.
func (h *IssueSession) Issue(ctx context.Context, user *identity.User, ip, userAgent, requestID string) (LoginOutput, error) {
	now := h.Clock.Now()

	// The same account checks the password path applies. An account disabled
	// a moment ago must not get in through a different door.
	if err := user.CanAuthenticate(now); err != nil {
		return LoginOutput{}, err
	}

	refreshToken, refreshHash, err := crypto.NewOpaqueToken()
	if err != nil {
		return LoginOutput{}, fmt.Errorf("issue session: %w", err)
	}
	session := identity.NewSession(user.ID, refreshHash, ip, userAgent, now)
	if err := h.Sessions.Create(ctx, session); err != nil {
		return LoginOutput{}, fmt.Errorf("issue session: %w", err)
	}

	accessToken, ttl, err := h.Tokens.Issue(user.ID, string(user.Role), user.Username, session.ID, now)
	if err != nil {
		return LoginOutput{}, fmt.Errorf("issue session: %w", err)
	}

	user.RegisterSuccessfulLogin(now)
	if err := h.Users.Update(ctx, user); err != nil {
		return LoginOutput{}, fmt.Errorf("issue session: %w", err)
	}

	metrics.LoginSuccesses.Inc()
	actorID := user.ID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actorID, ActorName: user.Username,
		Category: ports.AuditCategoryAuth, Action: "login_success",
		Outcome: ports.OutcomeSuccess, SourceIP: ip, UserAgent: userAgent,
		RequestID: requestID,
		Details: map[string]any{
			"session_id": session.ID.String(),
			// Which door was used matters when reading the trail later.
			"provider": string(providerOf(user)),
		},
	})

	return LoginOutput{
		AccessToken: accessToken, ExpiresIn: ttl,
		RefreshToken: refreshToken, User: user,
	}, nil
}

func providerOf(u *identity.User) identity.AuthProvider {
	if u.AuthProvider == "" {
		return identity.ProviderLocal
	}
	return u.AuthProvider
}
