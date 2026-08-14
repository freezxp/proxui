package command

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// ChangePasswordInput is a user changing their own password.
type ChangePasswordInput struct {
	Actor           Actor
	UserID          uuid.UUID
	CurrentPassword string
	NewPassword     string
}

// ChangePassword lets a signed-in user set their own password.
//
// This is the only way MustChangePassword is ever cleared. Without it the flag
// set on every new account and on the bootstrap admin could be raised and never
// lowered, which is how it stood until now (AUTH-08).
type ChangePassword struct {
	Users    ports.UserRepository
	Sessions ports.SessionRepository
	Hasher   ports.PasswordHasher
	Audit    ports.AuditWriter
	Clock    ports.Clock
}

// Handle verifies the current password and replaces it.
func (h *ChangePassword) Handle(ctx context.Context, in ChangePasswordInput) error {
	user, err := h.Users.GetByID(ctx, in.UserID)
	if err != nil {
		return err
	}

	// Proving knowledge of the current password is what makes this safe to
	// expose to every role: a stolen access token alone cannot change the
	// password it was minted from, and so cannot lock the owner out.
	ok, err := h.Hasher.Verify(in.CurrentPassword, user.PasswordHash)
	if err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	if !ok {
		h.auditFailure(ctx, in, user)
		return identity.ErrInvalidCredentials
	}

	if err := identity.ValidatePassword(in.NewPassword, user.Username, user.Email); err != nil {
		return err
	}
	// Rejecting a "change" that changes nothing: a forced change satisfied by
	// re-entering the same password would defeat the point of forcing it.
	if same, _ := h.Hasher.Verify(in.NewPassword, user.PasswordHash); same {
		return identity.ErrPasswordUnchanged
	}

	hash, err := h.Hasher.Hash(in.NewPassword)
	if err != nil {
		return fmt.Errorf("change password: %w", err)
	}

	now := h.Clock.Now()
	user.PasswordHash = hash
	user.MustChangePassword = false
	user.ClearLockout()

	if err := h.Users.Update(ctx, user); err != nil {
		return err
	}

	// Every session goes, including this one. Someone changing their password
	// because they think it was seen expects that to end other people's access
	// immediately, and signing in again is a small price for that guarantee.
	if err := h.Sessions.RevokeAllForUser(ctx, user.ID, now); err != nil {
		return fmt.Errorf("change password: revoke sessions: %w", err)
	}

	writeAudit(ctx, h.Audit, in.Actor, now, ports.AuditCategoryAuth, "password_changed",
		"user", user.ID.String(), user.Username, nil)
	return nil
}

func (h *ChangePassword) auditFailure(ctx context.Context, in ChangePasswordInput, user *identity.User) {
	actorID := in.Actor.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: h.Clock.Now(), ActorUserID: &actorID, ActorName: user.Username,
		Category: ports.AuditCategoryAuth, Action: "password_change_denied",
		TargetType: "user", TargetID: user.ID.String(), TargetName: user.Username,
		SourceIP: in.Actor.IP, UserAgent: in.Actor.UserAgent, RequestID: in.Actor.RequestID,
		Outcome: ports.OutcomeDenied,
	})
}
