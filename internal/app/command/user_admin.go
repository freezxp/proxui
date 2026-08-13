package command

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// Actor identifies who is performing an administrative action, for the audit
// trail. Every admin command takes one.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	IP        string
	UserAgent string
	RequestID string
}

// CreateUserInput describes a new account. There is no self-registration:
// accounts exist because an administrator created them (ADM-01).
type CreateUserInput struct {
	Actor        Actor
	Username     string
	Email        string
	DisplayName  string
	Role         identity.Role
	TempPassword string
	GroupIDs     []uuid.UUID
}

// CreateUser provisions an account with a temporary password.
type CreateUser struct {
	Users  ports.UserRepository
	Access ports.AccessRepository
	Hasher ports.PasswordHasher
	Audit  ports.AuditWriter
	Clock  ports.Clock
}

// Handle creates the user and returns it.
func (h *CreateUser) Handle(ctx context.Context, in CreateUserInput) (*identity.User, error) {
	if !in.Role.Valid() {
		return nil, identity.ErrInvalidRole
	}
	if err := identity.ValidatePassword(in.TempPassword, in.Username, in.Email); err != nil {
		return nil, err
	}
	hash, err := h.Hasher.Hash(in.TempPassword)
	if err != nil {
		return nil, fmt.Errorf("create user: hash password: %w", err)
	}

	now := h.Clock.Now()
	display := in.DisplayName
	if display == "" {
		display = in.Username
	}
	user := &identity.User{
		ID:           uuid.New(),
		Username:     in.Username,
		Email:        in.Email,
		DisplayName:  display,
		PasswordHash: hash,
		Role:         in.Role,
		IsActive:     true,
		// The creating admin knows the temporary password, so the account is
		// not truly the user's until they change it.
		MustChangePassword: true,
		CreatedAt:          now,
	}
	if err := h.Users.Create(ctx, user); err != nil {
		return nil, err
	}
	if len(in.GroupIDs) > 0 {
		if err := h.Access.SetUserGroups(ctx, user.ID, in.GroupIDs); err != nil {
			return nil, err
		}
	}

	writeAudit(ctx, h.Audit, in.Actor, now, ports.AuditCategoryUserMgmt, "user_created",
		"user", user.ID.String(), user.Username, map[string]any{"role": string(user.Role)})
	return user, nil
}

// UpdateUserInput carries the mutable parts of an account. Nil fields are left
// unchanged.
type UpdateUserInput struct {
	Actor       Actor
	UserID      uuid.UUID
	DisplayName *string
	Email       *string
	Role        *identity.Role
	IsActive    *bool
}

// UpdateUser changes role, contact details or activation state.
type UpdateUser struct {
	Users    ports.UserRepository
	Sessions ports.SessionRepository
	Audit    ports.AuditWriter
	Clock    ports.Clock
}

// Handle applies the update. Deactivating a user immediately revokes their
// sessions (AUTH-06) rather than waiting for tokens to expire.
func (h *UpdateUser) Handle(ctx context.Context, in UpdateUserInput) (*identity.User, error) {
	user, err := h.Users.GetByID(ctx, in.UserID)
	if err != nil {
		return nil, err
	}

	changes := map[string]any{}
	if in.DisplayName != nil && *in.DisplayName != user.DisplayName {
		user.DisplayName = *in.DisplayName
		changes["display_name"] = *in.DisplayName
	}
	if in.Email != nil && *in.Email != user.Email {
		user.Email = *in.Email
		changes["email"] = *in.Email
	}
	if in.Role != nil && *in.Role != user.Role {
		if !in.Role.Valid() {
			return nil, identity.ErrInvalidRole
		}
		changes["role"] = map[string]any{"from": string(user.Role), "to": string(*in.Role)}
		user.Role = *in.Role
	}
	deactivated := false
	if in.IsActive != nil && *in.IsActive != user.IsActive {
		user.IsActive = *in.IsActive
		changes["is_active"] = *in.IsActive
		deactivated = !*in.IsActive
	}

	if len(changes) == 0 {
		return user, nil
	}

	now := h.Clock.Now()
	if err := h.Users.Update(ctx, user); err != nil {
		return nil, err
	}
	if deactivated {
		if err := h.Sessions.RevokeAllForUser(ctx, user.ID, now); err != nil {
			return nil, fmt.Errorf("update user: revoke sessions: %w", err)
		}
	}

	writeAudit(ctx, h.Audit, in.Actor, now, ports.AuditCategoryUserMgmt, "user_updated",
		"user", user.ID.String(), user.Username, changes)
	return user, nil
}

// ResetPasswordInput identifies whose password to reset.
type ResetPasswordInput struct {
	Actor        Actor
	UserID       uuid.UUID
	TempPassword string
}

// ResetPassword sets a temporary password and forces a change at next login.
type ResetPassword struct {
	Users    ports.UserRepository
	Sessions ports.SessionRepository
	Hasher   ports.PasswordHasher
	Audit    ports.AuditWriter
	Clock    ports.Clock
}

// Handle resets the password and revokes existing sessions, so a compromised
// session cannot outlive the reset that was meant to end it.
func (h *ResetPassword) Handle(ctx context.Context, in ResetPasswordInput) error {
	user, err := h.Users.GetByID(ctx, in.UserID)
	if err != nil {
		return err
	}
	if err := identity.ValidatePassword(in.TempPassword, user.Username, user.Email); err != nil {
		return err
	}
	hash, err := h.Hasher.Hash(in.TempPassword)
	if err != nil {
		return fmt.Errorf("reset password: %w", err)
	}

	now := h.Clock.Now()
	user.PasswordHash = hash
	user.MustChangePassword = true
	user.ClearLockout()

	if err := h.Users.Update(ctx, user); err != nil {
		return err
	}
	if err := h.Sessions.RevokeAllForUser(ctx, user.ID, now); err != nil {
		return fmt.Errorf("reset password: revoke sessions: %w", err)
	}

	writeAudit(ctx, h.Audit, in.Actor, now, ports.AuditCategoryUserMgmt, "user_password_reset",
		"user", user.ID.String(), user.Username, nil)
	return nil
}

// SetUserGroupsInput assigns a user's group memberships.
type SetUserGroupsInput struct {
	Actor    Actor
	UserID   uuid.UUID
	GroupIDs []uuid.UUID
}

// SetUserGroups replaces a user's group memberships, changing what they can see.
type SetUserGroups struct {
	Users  ports.UserRepository
	Access ports.AccessRepository
	Audit  ports.AuditWriter
	Clock  ports.Clock
}

// Handle replaces the memberships.
func (h *SetUserGroups) Handle(ctx context.Context, in SetUserGroupsInput) error {
	user, err := h.Users.GetByID(ctx, in.UserID)
	if err != nil {
		return err
	}
	if err := h.Access.SetUserGroups(ctx, in.UserID, in.GroupIDs); err != nil {
		return err
	}

	ids := make([]string, len(in.GroupIDs))
	for i, id := range in.GroupIDs {
		ids[i] = id.String()
	}
	writeAudit(ctx, h.Audit, in.Actor, h.Clock.Now(), ports.AuditCategoryUserMgmt, "user_groups_changed",
		"user", user.ID.String(), user.Username, map[string]any{"user_groups": ids})
	return nil
}

// writeAudit records an administrative action. Audit failures are swallowed:
// they must never roll back an action the operator already saw succeed, and
// the write itself is logged by the repository on error.
func writeAudit(ctx context.Context, w ports.AuditWriter, actor Actor, at time.Time, category, action, targetType, targetID, targetName string, details map[string]any) {
	id := actor.UserID
	_ = w.Write(ctx, ports.AuditEntry{
		Time: at, ActorUserID: &id, ActorName: actor.Username,
		Category: category, Action: action,
		TargetType: targetType, TargetID: targetID, TargetName: targetName,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: ports.OutcomeSuccess, Details: details,
	})
}
