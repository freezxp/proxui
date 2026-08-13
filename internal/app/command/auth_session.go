package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// RefreshInput carries the presented refresh token and request metadata.
type RefreshInput struct {
	RefreshToken string
	IP           string
	UserAgent    string
	RequestID    string
}

// Refresh exchanges a refresh token for a new token pair, rotating the old one
// and detecting reuse (AUTH-02, AUTH-03).
type Refresh struct {
	Users    ports.UserRepository
	Sessions ports.SessionRepository
	Tokens   ports.TokenIssuer
	Audit    ports.AuditWriter
	Clock    ports.Clock
}

// Handle rotates the session. Any failure returns an identity error; the
// caller maps every one of them to 401 without distinguishing causes.
func (h *Refresh) Handle(ctx context.Context, in RefreshInput) (LoginOutput, error) {
	now := h.Clock.Now()

	session, err := h.Sessions.GetByTokenHash(ctx, crypto.HashToken(in.RefreshToken))
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return LoginOutput{}, identity.ErrSessionRevoked
		}
		return LoginOutput{}, fmt.Errorf("refresh: %w", err)
	}

	if err := session.Validate(now); err != nil {
		// Reuse means the token leaked: kill the whole rotation chain so the
		// legitimate user and the attacker both lose access, then make noise.
		if errors.Is(err, identity.ErrRefreshTokenReuse) {
			if rerr := h.Sessions.RevokeFamily(ctx, session.FamilyID, now); rerr != nil {
				return LoginOutput{}, fmt.Errorf("refresh: revoke family: %w", rerr)
			}
			h.auditReuse(ctx, session, in, now)
		}
		return LoginOutput{}, err
	}

	user, err := h.Users.GetByID(ctx, session.UserID)
	if err != nil {
		return LoginOutput{}, fmt.Errorf("refresh: %w", err)
	}
	if err := user.CanAuthenticate(now); err != nil {
		return LoginOutput{}, err
	}

	newToken, newHash, err := crypto.NewOpaqueToken()
	if err != nil {
		return LoginOutput{}, fmt.Errorf("refresh: %w", err)
	}
	next := session.Rotate(newHash, in.IP, in.UserAgent, now)
	if err := h.Sessions.Rotate(ctx, session, next); err != nil {
		if errors.Is(err, identity.ErrRefreshTokenReuse) {
			// Lost a concurrent rotation race: treat exactly like reuse.
			if rerr := h.Sessions.RevokeFamily(ctx, session.FamilyID, now); rerr != nil {
				return LoginOutput{}, fmt.Errorf("refresh: revoke family: %w", rerr)
			}
			h.auditReuse(ctx, session, in, now)
			return LoginOutput{}, identity.ErrRefreshTokenReuse
		}
		return LoginOutput{}, fmt.Errorf("refresh: %w", err)
	}

	accessToken, ttl, err := h.Tokens.Issue(user.ID, string(user.Role), next.ID, now)
	if err != nil {
		return LoginOutput{}, fmt.Errorf("refresh: %w", err)
	}
	return LoginOutput{AccessToken: accessToken, ExpiresIn: ttl, RefreshToken: newToken, User: user}, nil
}

func (h *Refresh) auditReuse(ctx context.Context, s *identity.Session, in RefreshInput, now time.Time) {
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &s.UserID, ActorName: "unknown",
		Category: ports.AuditCategorySecurity, Action: "refresh_token_reuse",
		Outcome: ports.OutcomeFailure, SourceIP: in.IP, UserAgent: in.UserAgent,
		RequestID: in.RequestID,
		Details: map[string]any{
			"session_id": s.ID.String(),
			"family_id":  s.FamilyID.String(),
			"note":       "session family revoked",
		},
	})
}

// LogoutInput identifies the session to end.
type LogoutInput struct {
	RefreshToken string
	UserID       uuid.UUID
	Username     string
	AllSessions  bool
	IP           string
	UserAgent    string
	RequestID    string
}

// Logout revokes the current session family, or every session the user holds
// (AUTH-07).
type Logout struct {
	Sessions ports.SessionRepository
	Audit    ports.AuditWriter
	Clock    ports.Clock
}

// Handle revokes sessions. Logging out is best-effort by design: an unknown or
// already-revoked token still yields success so clients can always clear state.
func (h *Logout) Handle(ctx context.Context, in LogoutInput) error {
	now := h.Clock.Now()
	action := "logout"

	switch {
	case in.AllSessions:
		action = "logout_all"
		if err := h.Sessions.RevokeAllForUser(ctx, in.UserID, now); err != nil {
			return fmt.Errorf("logout: %w", err)
		}
	case in.RefreshToken != "":
		session, err := h.Sessions.GetByTokenHash(ctx, crypto.HashToken(in.RefreshToken))
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("logout: %w", err)
		}
		if err := h.Sessions.RevokeFamily(ctx, session.FamilyID, now); err != nil {
			return fmt.Errorf("logout: %w", err)
		}
	default:
		return nil
	}

	actor := in.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actor, ActorName: in.Username,
		Category: ports.AuditCategoryAuth, Action: action,
		Outcome: ports.OutcomeSuccess, SourceIP: in.IP, UserAgent: in.UserAgent,
		RequestID: in.RequestID,
	})
	return nil
}
