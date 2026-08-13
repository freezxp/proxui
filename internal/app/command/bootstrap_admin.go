package command

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// BootstrapAdminInput carries the first-run administrator credentials.
type BootstrapAdminInput struct {
	Username    string
	Email       string
	DisplayName string
	Password    string
}

// BootstrapAdmin creates the initial administrator on first run (ADM-03).
// It is a no-op once any account exists, so restarting the process can never
// resurrect or reset an administrator.
type BootstrapAdmin struct {
	Users  ports.UserRepository
	Hasher ports.PasswordHasher
	Audit  ports.AuditWriter
	Clock  ports.Clock
}

// Handle reports whether an administrator was created.
func (h *BootstrapAdmin) Handle(ctx context.Context, in BootstrapAdminInput) (bool, error) {
	count, err := h.Users.CountAll(ctx)
	if err != nil {
		return false, fmt.Errorf("bootstrap: %w", err)
	}
	if count > 0 {
		return false, nil
	}

	if err := identity.ValidatePassword(in.Password, in.Username, in.Email); err != nil {
		return false, fmt.Errorf("bootstrap: %w", err)
	}

	hash, err := h.Hasher.Hash(in.Password)
	if err != nil {
		return false, fmt.Errorf("bootstrap: hash password: %w", err)
	}

	now := h.Clock.Now()
	display := in.DisplayName
	if display == "" {
		display = in.Username
	}
	admin := &identity.User{
		ID:                 uuid.New(),
		Username:           in.Username,
		Email:              in.Email,
		DisplayName:        display,
		PasswordHash:       hash,
		Role:               identity.RoleAdmin,
		IsActive:           true,
		MustChangePassword: true, // the bootstrap secret is in the environment
		CreatedAt:          now,
	}
	if err := h.Users.Create(ctx, admin); err != nil {
		return false, fmt.Errorf("bootstrap: %w", err)
	}

	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorName: "system",
		Category: ports.AuditCategoryUserMgmt, Action: "bootstrap_admin_created",
		TargetType: "user", TargetID: admin.ID.String(), TargetName: admin.Username,
		Outcome: ports.OutcomeSuccess,
		Details: map[string]any{"role": string(identity.RoleAdmin)},
	})
	return true, nil
}
