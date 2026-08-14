package command

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	appsync "github.com/freezxp/proxui/internal/app/sync"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/console"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// OpenConsoleInput asks for console access to a VM.
type OpenConsoleInput struct {
	Actor Actor
	Role  identity.Role
	VMID  uuid.UUID
	Kind  console.Kind
}

// OpenConsoleOutput is the handle the browser uses to connect.
type OpenConsoleOutput struct {
	TicketID  uuid.UUID
	SessionID uuid.UUID
	ExpiresIn time.Duration
}

// OpenConsole authorizes console access and mints a one-time ticket.
//
// It deliberately does not talk to the platform yet: a Proxmox VNC ticket is
// valid for seconds, so it is fetched when the browser actually connects. What
// happens here is the part that must not be skipped - authorization and the
// audit record (CONS-01..04).
type OpenConsole struct {
	Inventory ports.InventoryReader
	Sessions  ports.ConsoleRepository
	Tickets   ports.TicketStore
	Audit     ports.AuditWriter
	Clock     ports.Clock
}

// Handle authorizes and records a console request.
func (h *OpenConsole) Handle(ctx context.Context, in OpenConsoleInput) (OpenConsoleOutput, error) {
	if in.Kind == "" {
		in.Kind = console.KindVNC
	}
	if !in.Kind.Valid() {
		return OpenConsoleOutput{}, console.ErrUnsupported
	}

	// Scoping, not just the role gate: an operator may open consoles on the
	// VMs they were granted, not on every VM in the estate.
	allowed, err := h.Inventory.CanAccessVM(ctx, in.VMID, in.Role, in.Actor.UserID)
	if err != nil {
		return OpenConsoleOutput{}, fmt.Errorf("open console: %w", err)
	}
	if !allowed {
		h.auditDenied(ctx, in)
		return OpenConsoleOutput{}, console.ErrNotPermitted
	}

	// A stopped guest has no console. Checking here costs one read and turns
	// what would otherwise surface as a dropped WebSocket into a plain answer
	// before any session or audit record is created.
	vm, err := h.Inventory.GetVM(ctx, in.VMID, identity.RoleAdmin, uuid.Nil)
	if err != nil {
		return OpenConsoleOutput{}, err
	}
	if vm.State != "running" {
		return OpenConsoleOutput{}, console.ErrNotRunning
	}

	now := h.Clock.Now()
	session := &console.Session{
		ID: uuid.New(), UserID: in.Actor.UserID, VMID: in.VMID, Kind: in.Kind,
		ClientIP: in.Actor.IP, UserAgent: in.Actor.UserAgent, StartedAt: now,
	}
	if err := h.Sessions.Create(ctx, session); err != nil {
		return OpenConsoleOutput{}, err
	}

	ticket := console.NewTicket(session.ID, in.Actor.UserID, in.VMID, in.Kind, now)
	if err := h.Tickets.Issue(ctx, ticket); err != nil {
		return OpenConsoleOutput{}, err
	}

	writeAudit(ctx, h.Audit, in.Actor, now, AuditCategoryConsole, "console_session_created",
		"vm", in.VMID.String(), "", map[string]any{
			"session_id": session.ID.String(), "kind": string(in.Kind),
		})

	return OpenConsoleOutput{
		TicketID: ticket.ID, SessionID: session.ID, ExpiresIn: console.TicketTTL,
	}, nil
}

func (h *OpenConsole) auditDenied(ctx context.Context, in OpenConsoleInput) {
	actorID := in.Actor.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: h.Clock.Now(), ActorUserID: &actorID, ActorName: in.Actor.Username,
		Category: AuditCategoryConsole, Action: "console_denied",
		TargetType: "vm", TargetID: in.VMID.String(),
		SourceIP: in.Actor.IP, UserAgent: in.Actor.UserAgent, RequestID: in.Actor.RequestID,
		Outcome: ports.OutcomeDenied,
	})
}

// AuditCategoryConsole labels console access in the audit trail.
const AuditCategoryConsole = "console"

// ConsoleEndpointResolver turns a redeemed ticket into a live upstream console.
type ConsoleEndpointResolver struct {
	Inventory ports.InventoryReader
	Platforms ports.PlatformRepository
	Sync      *appsync.Service
	Clock     ports.Clock
}

// Resolve fetches the platform-side console for a VM. This is where the
// short-lived upstream ticket is minted, moments before it is used.
func (r *ConsoleEndpointResolver) Resolve(ctx context.Context, vmID uuid.UUID, kind console.Kind) (connector.ConsoleEndpoint, func(), error) {
	// Read as an admin: authorization already happened when the ticket was
	// issued, and the browser presenting it proves nothing more.
	vm, err := r.Inventory.GetVM(ctx, vmID, identity.RoleAdmin, uuid.Nil)
	if err != nil {
		return nil, nil, err
	}

	platform, err := r.Platforms.Get(ctx, vm.PlatformID)
	if err != nil {
		return nil, nil, err
	}
	conn, err := r.Sync.Connect(ctx, platform)
	if err != nil {
		return nil, nil, err
	}

	provider, ok := conn.(connector.ConsoleProvider)
	if !ok {
		conn.Close()
		return nil, nil, console.ErrUnsupported
	}

	hostExternalID := ""
	if vm.HostName != "" {
		hostExternalID = vm.HostName
	}
	endpoint, err := provider.CreateConsoleSession(ctx, connector.VMRef{
		ExternalID: vm.ExternalID, HostID: hostExternalID, Type: vm.VMType,
	}, connector.ConsoleKind(kind))
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return endpoint, func() { conn.Close() }, nil
}
