package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/provisioner"
	appsync "github.com/freezxp/proxui/internal/app/sync"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/domain/provision"
)

// Provisioning and destruction (ADR 0010).
//
// These are the first writes the portal makes that a platform token could not
// previously perform at all. What used to be guaranteed by arithmetic — the
// credential simply could not create or delete — is now guaranteed by the
// checks in this file, which is why they are here rather than in a handler:
// every caller goes through them, and they are testable without HTTP.

// ErrProvisionNotPermitted means the caller may not provision or destroy.
var ErrProvisionNotPermitted = errors.New("command: provisioning is not permitted")

// ErrNameMismatch means a destroy request did not name the guest correctly.
var ErrNameMismatch = errors.New("command: the confirmation does not match the guest's name")

// ErrTemplateProtected means the target of a destroy is a template.
var ErrTemplateProtected = errors.New("command: templates cannot be destroyed")

// AuditCategoryProvision labels create-and-destroy actions in the audit trail.
// They sit under the security category rather than getting their own: what an
// auditor is looking for is the set of things that changed what exists, and
// splitting them off would hide them from that search.
const AuditCategoryProvision = ports.AuditCategorySecurity

// ProvisionInput asks for a new guest.
type ProvisionInput struct {
	Actor      Actor
	Role       identity.Role
	PlatformID uuid.UUID
	TemplateID string
	Name       string
	TargetNode string
	VMGroupID  *uuid.UUID
	Spec       provision.Spec
}

// ProvisionOutput identifies the request that will carry the work out.
type ProvisionOutput struct {
	RequestID uuid.UUID
	State     string
}

// Provision creates guests from templates.
type Provision struct {
	Requests  ports.ProvisionRepository
	Platforms ports.PlatformRepository
	Sync      *appsync.Service
	Queue     ports.ProvisionEnqueuer
	Audit     ports.AuditWriter
	Clock     ports.Clock
}

// Handle authorizes the request, records it, and hands it to the driver.
//
// It deliberately does not perform any part of the work: cloning takes minutes,
// and a request that only exists inside an HTTP handler is a request that
// disappears when the handler's context is cancelled. The row is the promise.
func (h *Provision) Handle(ctx context.Context, in ProvisionInput) (ProvisionOutput, error) {
	if in.Role != identity.RoleAdmin {
		h.audit(ctx, in.Actor, "vm.provision.request", in.Name, ports.OutcomeDenied, nil)
		return ProvisionOutput{}, ErrProvisionNotPermitted
	}

	platform, err := h.Platforms.Get(ctx, in.PlatformID)
	if err != nil {
		return ProvisionOutput{}, err
	}
	if !platform.IsEnabled || platform.IsDeleted() {
		return ProvisionOutput{}, fmt.Errorf("%w: platform %s is not in service",
			connector.ErrRefused, platform.Name)
	}

	// Ask the connector whether it can do this at all before writing a request
	// that could never be carried out. A platform whose token was never widened
	// is a correctly configured portal, not a broken one, so this is a refusal
	// with a reason rather than an internal error.
	conn, err := h.Sync.Connect(ctx, platform)
	if err != nil {
		return ProvisionOutput{}, err
	}
	defer conn.Close()
	if !connector.Supports(conn, connector.CapabilityProvision) {
		return ProvisionOutput{}, connector.Errorf(connector.ErrNotSupported, "provision",
			"platform %s cannot create guests", platform.Name)
	}

	now := h.Clock.Now()
	actorID := in.Actor.UserID
	req := &provision.Request{
		ID:                 uuid.New(),
		PlatformID:         in.PlatformID,
		Kind:               provision.KindProvision,
		State:              provision.StatePending,
		RequestedBy:        &actorID,
		RequestedByName:    in.Actor.Username,
		TemplateExternalID: in.TemplateID,
		TargetNode:         in.TargetNode,
		GuestName:          strings.TrimSpace(in.Name),
		VMGroupID:          in.VMGroupID,
		Spec:               in.Spec,
		Created:            now,
		Updated:            now,
	}
	if err := req.Validate(); err != nil {
		return ProvisionOutput{}, fmt.Errorf("%w: %s", connector.ErrInvalidConfig, err)
	}
	if err := h.Requests.CreateRequest(ctx, req); err != nil {
		return ProvisionOutput{}, err
	}

	if err := h.Queue.EnqueueProvisionStep(ctx, req.ID, 0); err != nil {
		// The request is stored, so this is recoverable: the sweep that resumes
		// open requests after a restart will pick it up. Saying so beats
		// failing a request that will in fact run.
		h.audit(ctx, in.Actor, "vm.provision.request", req.GuestName, ports.OutcomeFailure,
			map[string]any{"request_id": req.ID.String(), "error": err.Error()})
		return ProvisionOutput{RequestID: req.ID, State: string(req.State)}, nil
	}

	h.audit(ctx, in.Actor, "vm.provision.request", req.GuestName, ports.OutcomeSuccess, map[string]any{
		"request_id": req.ID.String(), "platform": platform.Name,
		"template": in.TemplateID, "node": in.TargetNode,
	})
	return ProvisionOutput{RequestID: req.ID, State: string(req.State)}, nil
}

// DestroyInput asks for a guest to be removed.
type DestroyInput struct {
	Actor Actor
	Role  identity.Role
	VMID  uuid.UUID
	// ConfirmName must equal the guest's name. The browser asks for it too, but
	// this is the control: a client is not in a position to enforce anything.
	ConfirmName string
}

// Destroy removes guests.
type Destroy struct {
	Requests  ports.ProvisionRepository
	Inventory ports.InventoryReader
	Platforms ports.PlatformRepository
	Sync      *appsync.Service
	Queue     ports.ProvisionEnqueuer
	Audit     ports.AuditWriter
	Clock     ports.Clock
}

// Handle authorizes and records a destruction, then hands it to the driver.
func (h *Destroy) Handle(ctx context.Context, in DestroyInput) (ProvisionOutput, error) {
	if in.Role != identity.RoleAdmin {
		h.audit(ctx, in.Actor, in.VMID.String(), "", ports.OutcomeDenied, nil)
		return ProvisionOutput{}, ErrProvisionNotPermitted
	}

	vm, err := h.Inventory.GetVM(ctx, in.VMID, identity.RoleAdmin, uuid.Nil)
	if err != nil {
		return ProvisionOutput{}, err
	}

	// Typing the name is the last check between an administrator and an
	// irreversible action, so it is made here where it cannot be skipped.
	if strings.TrimSpace(in.ConfirmName) != vm.Name {
		h.audit(ctx, in.Actor, in.VMID.String(), vm.Name, ports.OutcomeDenied,
			map[string]any{"reason": "name confirmation did not match"})
		return ProvisionOutput{}, ErrNameMismatch
	}
	// Templates are what every other guest is cloned from; losing one costs far
	// more than the guest that asked to go.
	if isTemplate(vm) {
		h.audit(ctx, in.Actor, in.VMID.String(), vm.Name, ports.OutcomeDenied,
			map[string]any{"reason": "target is a template"})
		return ProvisionOutput{}, ErrTemplateProtected
	}

	platform, err := h.Platforms.Get(ctx, vm.PlatformID)
	if err != nil {
		return ProvisionOutput{}, err
	}
	conn, err := h.Sync.Connect(ctx, platform)
	if err != nil {
		return ProvisionOutput{}, err
	}
	defer conn.Close()
	if !connector.Supports(conn, connector.CapabilityDestroy) {
		return ProvisionOutput{}, connector.Errorf(connector.ErrNotSupported, "destroy",
			"platform %s cannot destroy guests", platform.Name)
	}

	now := h.Clock.Now()
	actorID := in.Actor.UserID
	req := &provision.Request{
		ID:              uuid.New(),
		PlatformID:      vm.PlatformID,
		Kind:            provision.KindDestroy,
		State:           provision.StatePending,
		RequestedBy:     &actorID,
		RequestedByName: in.Actor.Username,
		TargetNode:      vm.HostName,
		GuestName:       vm.Name,
		VMID:            vm.ExternalID,
		Spec:            provision.Spec{TemplateType: vm.VMType},
		Created:         now,
		Updated:         now,
	}
	if err := req.Validate(); err != nil {
		return ProvisionOutput{}, fmt.Errorf("%w: %s", connector.ErrInvalidConfig, err)
	}
	if err := h.Requests.CreateRequest(ctx, req); err != nil {
		return ProvisionOutput{}, err
	}
	if err := h.Queue.EnqueueProvisionStep(ctx, req.ID, 0); err != nil {
		h.audit(ctx, in.Actor, in.VMID.String(), vm.Name, ports.OutcomeFailure,
			map[string]any{"request_id": req.ID.String(), "error": err.Error()})
		return ProvisionOutput{RequestID: req.ID, State: string(req.State)}, nil
	}

	h.audit(ctx, in.Actor, in.VMID.String(), vm.Name, ports.OutcomeSuccess, map[string]any{
		"request_id": req.ID.String(), "platform": platform.Name, "vmid": vm.ExternalID,
	})
	return ProvisionOutput{RequestID: req.ID, State: string(req.State)}, nil
}

// isTemplate reports whether the inventory row describes a template. Templates
// are normally filtered out of the inventory entirely, so this only fires on a
// platform configured to include them — which is exactly the configuration
// where an administrator could reach one by accident.
func isTemplate(vm ports.VMDetail) bool {
	if vm.Attrs == nil {
		return false
	}
	switch v := vm.Attrs["template"].(type) {
	case bool:
		return v
	case float64:
		return v == 1
	case string:
		return v == "1" || v == "true"
	}
	return false
}

func (h *Provision) audit(ctx context.Context, actor Actor, action, name, outcome string, details map[string]any) {
	writeProvisionAudit(ctx, h.Audit, h.Clock, actor, action, "", name, outcome, details)
}

func (h *Destroy) audit(ctx context.Context, actor Actor, targetID, name, outcome string, details map[string]any) {
	writeProvisionAudit(ctx, h.Audit, h.Clock, actor, "vm.destroy", targetID, name, outcome, details)
}

func writeProvisionAudit(ctx context.Context, w ports.AuditWriter, clock ports.Clock,
	actor Actor, action, targetID, name, outcome string, details map[string]any,
) {
	if w == nil {
		return
	}
	actorID := actor.UserID
	now := time.Now().UTC()
	if clock != nil {
		now = clock.Now()
	}
	_ = w.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actorID, ActorName: actor.Username,
		Category: AuditCategoryProvision, Action: action,
		TargetType: "vm", TargetID: targetID, TargetName: name,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: outcome, Details: details,
	})
}

// BuildTemplateInput asks for a cloud-init template to be built.
type BuildTemplateInput struct {
	Actor      Actor
	Role       identity.Role
	PlatformID uuid.UUID
	Name       string
	Node       string
	// ImageStorage holds the downloaded file; DiskStorage holds the disk it
	// becomes. They are rarely the same: a directory storage takes the image,
	// a block storage takes the disk.
	ImageStorage string
	DiskStorage  string
	ImageURL     string
	ImageFile    string
	Checksum     string
	ChecksumAlgo string
	// SkipChecksum has to be stated. Leaving the digest blank is not enough,
	// because the difference between "I decided not to verify" and "I forgot"
	// is the whole point of asking.
	SkipChecksum bool
	Cores        int
	MemoryMB     int
	Bridge       string
	// CPU is the processor model to give the template, for an image whose
	// distribution needs more than the conservative default. The catalogue
	// carries it for the images the portal ships; anything else can say so
	// here.
	CPU string
}

// BuildTemplate creates the image everything else is cloned from.
type BuildTemplate struct {
	Requests  ports.ProvisionRepository
	Platforms ports.PlatformRepository
	Sync      *appsync.Service
	Queue     ports.ProvisionEnqueuer
	Audit     ports.AuditWriter
	Clock     ports.Clock
}

// Handle authorizes the build, records it, and hands it to the driver.
func (h *BuildTemplate) Handle(ctx context.Context, in BuildTemplateInput) (ProvisionOutput, error) {
	if in.Role != identity.RoleAdmin {
		h.audit(ctx, in, "template.build.request", ports.OutcomeDenied, nil)
		return ProvisionOutput{}, ErrProvisionNotPermitted
	}

	// Unverified is allowed and deliberately awkward: it must be asked for, and
	// asking for it is written down. This image becomes the ancestor of every
	// guest cloned from it, so the one thing worse than no checksum is no
	// record that there was none.
	checksum, algo := strings.TrimSpace(in.Checksum), strings.TrimSpace(in.ChecksumAlgo)
	if in.SkipChecksum {
		checksum, algo = "", ""
		// Recorded here, before anything else can go wrong, because the
		// decision was made whether or not the platform turns out to be
		// reachable — and a decision to skip verification that goes unrecorded
		// because a later step failed is exactly the one nobody would notice.
		h.audit(ctx, in, "template.build.unverified", ports.OutcomeSuccess, map[string]any{
			"image_url": strings.TrimSpace(in.ImageURL),
			"reason":    "verification was explicitly skipped by the requester",
		})
	} else if checksum == "" || algo == "" {
		return ProvisionOutput{}, fmt.Errorf(
			"%w: a checksum and its algorithm are required unless verification is explicitly skipped",
			connector.ErrInvalidConfig)
	}

	// Checked here rather than left to the platform: Proxmox refuses a name
	// whose extension it does not accept with "invalid filename or wrong
	// extension", which does not say which ones it would accept, and by then a
	// request has been recorded and a step has failed.
	if err := provisioner.ValidateImageFilename(in.ImageFile); err != nil {
		return ProvisionOutput{}, fmt.Errorf("%w: %s", connector.ErrInvalidConfig, err)
	}

	platform, err := h.Platforms.Get(ctx, in.PlatformID)
	if err != nil {
		return ProvisionOutput{}, err
	}
	if !platform.IsEnabled || platform.IsDeleted() {
		return ProvisionOutput{}, fmt.Errorf("%w: platform %s is not in service",
			connector.ErrRefused, platform.Name)
	}

	conn, err := h.Sync.Connect(ctx, platform)
	if err != nil {
		return ProvisionOutput{}, err
	}
	defer conn.Close()
	if !connector.Supports(conn, connector.CapabilityTemplateBuild) {
		return ProvisionOutput{}, connector.Errorf(connector.ErrNotSupported, "template",
			"platform %s cannot build templates", platform.Name)
	}

	now := h.Clock.Now()
	actorID := in.Actor.UserID
	req := &provision.Request{
		ID:              uuid.New(),
		PlatformID:      in.PlatformID,
		Kind:            provision.KindTemplate,
		State:           provision.StatePending,
		RequestedBy:     &actorID,
		RequestedByName: in.Actor.Username,
		TargetNode:      in.Node,
		GuestName:       strings.TrimSpace(in.Name),
		Spec: provision.Spec{
			TemplateType: "qemu",
			ImageURL:     strings.TrimSpace(in.ImageURL),
			ImageFile:    in.ImageFile,
			ImageStorage: in.ImageStorage,
			Storage:      in.DiskStorage,
			Checksum:     checksum,
			ChecksumAlgo: algo,
			Cores:        in.Cores,
			MemoryMB:     in.MemoryMB,
			Bridge:       in.Bridge,
			CPU:          strings.TrimSpace(in.CPU),
		},
		Created: now,
		Updated: now,
	}
	if err := req.Validate(); err != nil {
		return ProvisionOutput{}, fmt.Errorf("%w: %s", connector.ErrInvalidConfig, err)
	}
	if err := h.Requests.CreateRequest(ctx, req); err != nil {
		return ProvisionOutput{}, err
	}

	if err := h.Queue.EnqueueProvisionStep(ctx, req.ID, 0); err != nil {
		h.audit(ctx, in, "template.build.request", ports.OutcomeFailure,
			map[string]any{"request_id": req.ID.String(), "error": err.Error()})
		return ProvisionOutput{RequestID: req.ID, State: string(req.State)}, nil
	}

	h.audit(ctx, in, "template.build.request", ports.OutcomeSuccess, map[string]any{
		"request_id": req.ID.String(), "platform": platform.Name,
		"image_url": req.Spec.ImageURL, "node": in.Node, "verified": !in.SkipChecksum,
	})
	return ProvisionOutput{RequestID: req.ID, State: string(req.State)}, nil
}

func (h *BuildTemplate) audit(ctx context.Context, in BuildTemplateInput, action, outcome string, details map[string]any) {
	writeProvisionAudit(ctx, h.Audit, h.Clock, in.Actor, action, "", in.Name, outcome, details)
}
