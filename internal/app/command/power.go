package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	appsync "github.com/freezxp/proxui/internal/app/sync"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// ErrPowerNotPermitted means the caller may not act on this VM.
var ErrPowerNotPermitted = errors.New("command: power action not permitted")

// PowerInput requests a lifecycle action on a VM.
type PowerInput struct {
	Actor  Actor
	Role   identity.Role
	VMID   uuid.UUID
	Action connector.PowerAction
}

// PowerOutput identifies the platform task that will carry out the action.
type PowerOutput struct {
	TaskID string
	Node   string
}

// Power performs start, stop, shutdown and reboot.
//
// This is the only write the portal makes to a platform, and the token it uses
// cannot create or delete anything - that ceiling is enforced at Proxmox, not
// here (docs/15-security-design.md §15.4). Every attempt is audited whether it
// succeeds or not.
type Power struct {
	Inventory ports.InventoryReader
	Platforms ports.PlatformRepository
	Sync      *appsync.Service
	Audit     ports.AuditWriter
	Clock     ports.Clock
}

// Handle authorizes and performs the action.
func (h *Power) Handle(ctx context.Context, in PowerInput) (PowerOutput, error) {
	if !in.Action.Valid() {
		return PowerOutput{}, connector.Errorf(connector.ErrNotSupported, "power",
			"unknown power action %q", in.Action)
	}

	allowed, err := h.Inventory.CanAccessVM(ctx, in.VMID, in.Role, in.Actor.UserID)
	if err != nil {
		return PowerOutput{}, fmt.Errorf("power: %w", err)
	}
	if !allowed {
		h.audit(ctx, in, "", ports.OutcomeDenied, nil)
		return PowerOutput{}, ErrPowerNotPermitted
	}

	vm, err := h.Inventory.GetVM(ctx, in.VMID, identity.RoleAdmin, uuid.Nil)
	if err != nil {
		return PowerOutput{}, err
	}
	platform, err := h.Platforms.Get(ctx, vm.PlatformID)
	if err != nil {
		return PowerOutput{}, err
	}

	conn, err := h.Sync.Connect(ctx, platform)
	if err != nil {
		h.audit(ctx, in, vm.Name, ports.OutcomeFailure, map[string]any{"error": err.Error()})
		return PowerOutput{}, err
	}
	defer conn.Close()

	manager, ok := conn.(connector.PowerManager)
	if !ok {
		return PowerOutput{}, connector.Errorf(connector.ErrNotSupported, "power",
			"platform %s does not support power actions", platform.Name)
	}

	task, err := manager.Power(ctx, connector.VMRef{
		ExternalID: vm.ExternalID, HostID: vm.HostName, Type: vm.VMType,
	}, in.Action)
	if err != nil {
		// A refused action is exactly what an operator needs in the record:
		// "I tried to stop it and the platform said no" is the useful entry.
		h.audit(ctx, in, vm.Name, ports.OutcomeFailure, map[string]any{"error": err.Error()})
		return PowerOutput{}, err
	}

	h.audit(ctx, in, vm.Name, ports.OutcomeSuccess, map[string]any{
		"task_id": task.ID, "node": task.Node, "state_before": vm.State,
	})
	return PowerOutput{TaskID: task.ID, Node: task.Node}, nil
}

func (h *Power) audit(ctx context.Context, in PowerInput, vmName, outcome string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	details["action"] = string(in.Action)

	actorID := in.Actor.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: h.Clock.Now(), ActorUserID: &actorID, ActorName: in.Actor.Username,
		Category: AuditCategoryPower, Action: "vm_power_" + string(in.Action),
		TargetType: "vm", TargetID: in.VMID.String(), TargetName: vmName,
		SourceIP: in.Actor.IP, UserAgent: in.Actor.UserAgent, RequestID: in.Actor.RequestID,
		Outcome: outcome, Details: details,
	})
}

// AuditCategoryPower labels lifecycle actions in the audit trail.
const AuditCategoryPower = "power"
