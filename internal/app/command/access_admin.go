package command

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/access"
)

// ManageAccess owns the group and grant lifecycle. These are configuration
// changes, so every one of them is audited (AUD-01).
type ManageAccess struct {
	Access ports.AccessRepository
	Audit  ports.AuditWriter
	Clock  ports.Clock
}

// GroupInput describes a group to create.
type GroupInput struct {
	Actor       Actor
	Name        string
	Description string
	AutoRule    map[string]any // VM groups only
}

// CreateUserGroup adds a user group.
func (h *ManageAccess) CreateUserGroup(ctx context.Context, in GroupInput) (*access.UserGroup, error) {
	if err := access.ValidateName(in.Name); err != nil {
		return nil, err
	}
	g := &access.UserGroup{
		ID: uuid.New(), Name: in.Name, Description: in.Description, CreatedAt: h.Clock.Now(),
	}
	if err := h.Access.CreateUserGroup(ctx, g); err != nil {
		return nil, err
	}
	writeAudit(ctx, h.Audit, in.Actor, g.CreatedAt, ports.AuditCategoryUserMgmt, "user_group_created",
		"user_group", g.ID.String(), g.Name, nil)
	return g, nil
}

// DeleteUserGroup removes a user group and every grant that referenced it.
func (h *ManageAccess) DeleteUserGroup(ctx context.Context, actor Actor, id uuid.UUID) error {
	if err := h.Access.DeleteUserGroup(ctx, id); err != nil {
		return err
	}
	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), ports.AuditCategoryUserMgmt, "user_group_deleted",
		"user_group", id.String(), "", nil)
	return nil
}

// CreateVMGroup adds a VM group.
func (h *ManageAccess) CreateVMGroup(ctx context.Context, in GroupInput) (*access.VMGroup, error) {
	if err := access.ValidateName(in.Name); err != nil {
		return nil, err
	}
	g := &access.VMGroup{
		ID: uuid.New(), Name: in.Name, Description: in.Description, CreatedAt: h.Clock.Now(),
	}
	if in.AutoRule != nil {
		raw, err := json.Marshal(in.AutoRule)
		if err != nil {
			return nil, fmt.Errorf("create vm group: encode auto rule: %w", err)
		}
		g.AutoRule = raw
	}
	if err := h.Access.CreateVMGroup(ctx, g); err != nil {
		return nil, err
	}
	writeAudit(ctx, h.Audit, in.Actor, g.CreatedAt, ports.AuditCategoryUserMgmt, "vm_group_created",
		"vm_group", g.ID.String(), g.Name, map[string]any{"auto_rule": in.AutoRule})
	return g, nil
}

// SetVMGroupMembers replaces which VMs a group contains.
//
// This changes who can see what, because grants are expressed in terms of
// groups: it is audited with both the old and the new membership so that a
// later question of "when did this operator gain access to that VM" has an
// answer.
func (h *ManageAccess) SetVMGroupMembers(ctx context.Context, actor Actor, groupID uuid.UUID, vmIDs []uuid.UUID) error {
	before, err := h.Access.VMGroupMemberIDs(ctx, groupID)
	if err != nil {
		return err
	}
	if err := h.Access.SetVMGroupMembers(ctx, groupID, vmIDs); err != nil {
		return err
	}
	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), ports.AuditCategoryUserMgmt, "vm_group_members_set",
		"vm_group", groupID.String(), "", map[string]any{
			"before": idStrings(before), "after": idStrings(vmIDs),
			"added": len(vmIDs) - len(before),
		})
	return nil
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// DeleteVMGroup removes a VM group and every grant that referenced it.
func (h *ManageAccess) DeleteVMGroup(ctx context.Context, actor Actor, id uuid.UUID) error {
	if err := h.Access.DeleteVMGroup(ctx, id); err != nil {
		return err
	}
	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), ports.AuditCategoryUserMgmt, "vm_group_deleted",
		"vm_group", id.String(), "", nil)
	return nil
}

// GrantInput links a user group to a VM group.
type GrantInput struct {
	Actor       Actor
	UserGroupID uuid.UUID
	VMGroupID   uuid.UUID
}

// CreateGrant widens what a user group can see, so it is audited as a
// security-relevant configuration change.
func (h *ManageAccess) CreateGrant(ctx context.Context, in GrantInput) (*access.Grant, error) {
	grantedBy := in.Actor.UserID
	g := &access.Grant{
		ID:          uuid.New(),
		UserGroupID: in.UserGroupID,
		VMGroupID:   in.VMGroupID,
		GrantedBy:   &grantedBy,
		CreatedAt:   h.Clock.Now(),
	}
	if err := h.Access.CreateGrant(ctx, g); err != nil {
		return nil, err
	}
	writeAudit(ctx, h.Audit, in.Actor, g.CreatedAt, ports.AuditCategorySecurity, "grant_created",
		"grant", g.ID.String(), "", map[string]any{
			"user_group_id": in.UserGroupID.String(),
			"vm_group_id":   in.VMGroupID.String(),
		})
	return g, nil
}

// DeleteGrant revokes access.
func (h *ManageAccess) DeleteGrant(ctx context.Context, actor Actor, id uuid.UUID) error {
	if err := h.Access.DeleteGrant(ctx, id); err != nil {
		return err
	}
	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), ports.AuditCategorySecurity, "grant_deleted",
		"grant", id.String(), "", nil)
	return nil
}
