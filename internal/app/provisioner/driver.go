// Package provisioner drives a provisioning or destruction request from one
// state to the next (ADR 0010).
//
// The work is split into steps because the platform splits it: cloning and
// destroying are tasks that run for minutes and are polled, while configuring
// and resizing answer immediately. A driver invocation does as much as it can
// without waiting — running the synchronous steps back to back — and stops as
// soon as it has started something it would have to wait for. The job layer
// calls it again.
//
// Nothing is held in memory between invocations. Everything the next one needs
// is in the request row, which is what makes a portal restart mid-clone
// survivable rather than a guest nobody is watching.
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/domain/provision"
)

// ErrStillRunning reports that the platform has not finished the step being
// waited on. It is not a failure: the caller re-schedules and asks again.
var ErrStillRunning = errors.New("provisioner: platform task still running")

// groupAssignmentGrace bounds how long a finished guest waits to be filed into
// its VM group. The guest only becomes visible to the portal when a sync brings
// it in, and a sync can be slow or a platform briefly unreachable; past this
// the request completes anyway and records that the filing did not happen. A
// working guest that is merely unfiled is a much better outcome than a request
// that never finishes.
const groupAssignmentGrace = 5 * time.Minute

// PlatformConnector opens a connection to a platform.
//
// An interface rather than the sync service itself, because the driver needs
// exactly one thing from it and because a test that wants a platform whose
// state survives between steps — which every real platform's does — has to be
// able to supply one.
type PlatformConnector interface {
	Connect(ctx context.Context, p *inventory.Platform) (connector.Connector, error)
}

// Driver advances requests.
type Driver struct {
	Requests  ports.ProvisionRepository
	Platforms ports.PlatformRepository
	Access    ports.AccessRepository
	Platform  PlatformConnector
	Queue     ports.ProvisionEnqueuer
	Audit     ports.AuditWriter
	Clock     ports.Clock
	Log       zerolog.Logger
}

// Step advances one request as far as it can without waiting.
//
// It returns ErrStillRunning when the platform is mid-task, which the job layer
// turns into another attempt later. Any other error is transient — a database
// hiccup, an unreachable platform — and is worth retrying as-is; a failure the
// platform reported is recorded on the request and returns nil, because the
// request has reached a conclusion even though the work did not.
func (d *Driver) Step(ctx context.Context, requestID uuid.UUID) error {
	req, err := d.Requests.GetRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if req.State.Terminal() {
		return nil
	}

	platform, err := d.Platforms.Get(ctx, req.PlatformID)
	if err != nil {
		return err
	}
	conn, err := d.Platform.Connect(ctx, platform)
	if err != nil {
		return err
	}
	defer conn.Close()

	now := d.Clock.Now()

	// Waiting on the platform? Find out how it went before doing anything else.
	if req.TaskID != "" {
		done, ok, detail, err := d.pollTask(ctx, conn, req)
		if err != nil {
			return err
		}
		if !done {
			return ErrStillRunning
		}
		if !ok {
			return d.fail(ctx, req, now, fmt.Errorf("%s failed on the platform: %s", req.State, detail))
		}
		if err := req.Advance(now); err != nil {
			return err
		}
	} else if req.State == provision.StatePending {
		// A template build looks first: the image is hundreds of megabytes and
		// a second template from the same one should not fetch it again. The
		// answer is written to the request so the decision is visible in the
		// record rather than re-derived on every turn.
		if req.Kind == provision.KindTemplate {
			if b, ok := conn.(connector.TemplateBuilder); ok {
				present, err := b.ImageExists(ctx, req.TargetNode, req.Spec.ImageStorage, req.Spec.ImageFile)
				if err != nil {
					return err
				}
				req.Spec.ImagePresent = present
			}
		}
		if err := req.Advance(now); err != nil {
			return err
		}
	}

	// Run whatever can be done without waiting, stopping at the first step that
	// hands work to the platform.
	for {
		async, err := d.begin(ctx, conn, req, now)
		if errors.Is(err, ErrStillRunning) {
			// Deliberately without saving: the request stays where it was, and
			// the next invocation replays from a step that is safe to repeat.
			return ErrStillRunning
		}
		if err != nil {
			return d.fail(ctx, req, now, err)
		}
		if async || req.State.Terminal() {
			break
		}
		if err := req.Advance(now); err != nil {
			return err
		}
	}

	return d.Requests.SaveRequest(ctx, req)
}

// begin starts the work for the request's current state. It reports whether it
// handed something to the platform that must be waited on.
func (d *Driver) begin(ctx context.Context, conn connector.Connector, req *provision.Request, now time.Time) (bool, error) {
	switch req.State {
	case provision.StateCloning:
		return true, d.clone(ctx, conn, req)

	case provision.StateConfiguring:
		p, ok := conn.(connector.Provisioner)
		if !ok {
			return false, errNotSupported("configure")
		}
		return false, p.Configure(ctx, d.guestRef(req), cloudInitSpec(req))

	case provision.StateResizing:
		p, ok := conn.(connector.Provisioner)
		if !ok {
			return false, errNotSupported("resize")
		}
		return false, p.ResizeDisk(ctx, d.guestRef(req), req.Spec.DiskName, req.Spec.DiskGrowBytes)

	case provision.StateStarting:
		pm, ok := conn.(connector.PowerManager)
		if !ok {
			return false, errNotSupported("start")
		}
		task, err := pm.Power(ctx, d.guestRef(req), connector.PowerStart)
		if err != nil {
			return false, err
		}
		req.TaskID = task.ID
		return true, nil

	case provision.StateDeleting:
		dz, ok := conn.(connector.Destroyer)
		if !ok {
			return false, errNotSupported("destroy")
		}
		task, err := dz.Destroy(ctx, d.guestRef(req), connector.DestroyOptions{
			PurgeReferences:          true,
			DestroyUnreferencedDisks: true,
		})
		if err != nil {
			return false, err
		}
		req.TaskID = task.ID
		return true, nil

	case provision.StateDownloading:
		b, ok := conn.(connector.TemplateBuilder)
		if !ok {
			return false, errNotSupported("download an image for")
		}
		task, err := b.DownloadImage(ctx, connector.ImageDownloadSpec{
			Node: req.TargetNode, Storage: req.Spec.ImageStorage,
			URL: req.Spec.ImageURL, Filename: req.Spec.ImageFile,
			Checksum: req.Spec.Checksum, ChecksumAlgorithm: req.Spec.ChecksumAlgo,
		})
		if err != nil {
			return false, err
		}
		req.TaskID = task.ID
		return true, nil

	case provision.StateCreating:
		return true, d.createTemplateShell(ctx, conn, req)

	case provision.StateImporting:
		b, ok := conn.(connector.TemplateBuilder)
		if !ok {
			return false, errNotSupported("import a disk for")
		}
		task, err := b.ImportDisk(ctx, d.guestRef(req), connector.DiskImportSpec{
			Disk: "scsi0", Storage: req.Spec.Storage,
			SourceVolume:   req.Spec.ImageStorage + ":" + importVolumePath + req.Spec.ImageFile,
			CloudInitDrive: "ide2",
		})
		if err != nil {
			return false, err
		}
		req.TaskID = task.ID
		// A config write that had nothing slow to do answers without a task.
		// Treating that as "wait for a task that does not exist" would hang the
		// request until somebody looked at it.
		return task.ID != "", nil

	case provision.StateConverting:
		b, ok := conn.(connector.TemplateBuilder)
		if !ok {
			return false, errNotSupported("convert to a template")
		}
		task, err := b.ConvertToTemplate(ctx, d.guestRef(req))
		if err != nil {
			return false, err
		}
		req.TaskID = task.ID
		return task.ID != "", nil

	case provision.StateReady:
		return false, d.finish(ctx, req, now)

	case provision.StateDeleted:
		// The guest is gone on the platform; the portal learns at the next
		// sync, which marks it missing and then deleted (SYNC-03).
		d.enqueueSync(ctx, req)
		d.auditOutcome(ctx, req, ports.OutcomeSuccess, nil)
		return false, nil

	default:
		return false, fmt.Errorf("provisioner: nothing to do in state %q", req.State)
	}
}

// clone reserves an identifier and asks the platform to copy the template.
//
// The identifier is only free at the moment it was asked for — Proxmox reserves
// nothing — so a rejection is treated as ordinary and the caller comes back for
// another. That is also why the id is stored before the clone is requested: a
// clone that succeeds and a portal that dies before recording the id would
// leave a guest nobody can find.
func (d *Driver) clone(ctx context.Context, conn connector.Connector, req *provision.Request) error {
	p, ok := conn.(connector.Provisioner)
	if !ok {
		return errNotSupported("clone")
	}
	if req.VMID == "" {
		id, err := p.NextID(ctx)
		if err != nil {
			return err
		}
		req.VMID = id
		if err := d.Requests.SaveRequest(ctx, req); err != nil {
			return err
		}
	}

	task, err := p.Clone(ctx, connector.CloneSpec{
		Template: connector.VMRef{
			ExternalID: req.TemplateExternalID,
			HostID:     req.Spec.TemplateNode,
			Type:       req.Spec.TemplateType,
		},
		NewID:       req.VMID,
		Name:        req.GuestName,
		FullClone:   req.Spec.FullClone,
		Storage:     req.Spec.Storage,
		TargetNode:  req.TargetNode,
		Description: "Created by ProxUI for " + req.RequestedByName,
	})
	if err != nil {
		return err
	}
	req.TaskID = task.ID
	// The clone lands wherever it was targeted, and every later step addresses
	// the guest by node. Without this they would keep addressing the template's.
	if req.TargetNode == "" {
		req.TargetNode = req.Spec.TemplateNode
	}
	return nil
}

// importVolumePath is the path segment Proxmox files downloaded images under.
// A stored image is addressed as "<storage>:import/<file>" — a colon after the
// storage, not a slash, which the platform rejects outright.
const importVolumePath = "import/"

// createTemplateShell makes the guest the image will be imported into.
//
// It reserves an identifier the same way cloning does, and for the same reason:
// the platform reserves nothing, so the id is written down before it is used.
func (d *Driver) createTemplateShell(ctx context.Context, conn connector.Connector, req *provision.Request) error {
	b, ok := conn.(connector.TemplateBuilder)
	if !ok {
		return errNotSupported("create a guest for")
	}
	if req.VMID == "" {
		p, ok := conn.(connector.Provisioner)
		if !ok {
			return errNotSupported("allocate an identifier for")
		}
		id, err := p.NextID(ctx)
		if err != nil {
			return err
		}
		req.VMID = id
		if err := d.Requests.SaveRequest(ctx, req); err != nil {
			return err
		}
	}

	task, err := b.CreateGuest(ctx, connector.GuestCreateSpec{
		Node: req.TargetNode, VMID: req.VMID, Name: req.GuestName,
		Cores: req.Spec.Cores, MemoryMB: req.Spec.MemoryMB, Bridge: req.Spec.Bridge,
		Description: "Cloud-init template built by ProxUI from " + req.Spec.ImageURL,
	})
	if err != nil {
		return err
	}
	req.TaskID = task.ID
	return nil
}

// finish files a finished guest into the VM group that was chosen for it.
//
// A newly created guest belongs to no group, and a group is what makes a VM
// visible to anyone but an administrator — so without this the person who asked
// for the machine cannot see it. The guest only becomes addressable once a sync
// has brought it in, which is why this waits, and why the wait is bounded.
func (d *Driver) finish(ctx context.Context, req *provision.Request, now time.Time) error {
	d.enqueueSync(ctx, req)

	// A template is not a guest: it belongs to no VM group, and it is excluded
	// from the inventory by design, so there is nothing to wait for or file.
	if req.Kind == provision.KindTemplate {
		// Nothing to file and nothing to tidy: a guest that a sync caught
		// mid-build is closed out by the sweep itself, which can tell a
		// conversion from a disappearance whenever it happens (ADR 0010).
		d.auditOutcome(ctx, req, ports.OutcomeSuccess, nil)
		return nil
	}

	if req.VMGroupID == nil {
		d.auditOutcome(ctx, req, ports.OutcomeSuccess, nil)
		return nil
	}

	vmID, err := d.Requests.FindVMByExternalID(ctx, req.PlatformID, req.VMID)
	if err != nil || vmID == uuid.Nil {
		if now.Sub(req.Created) < groupAssignmentGrace {
			return ErrStillRunning
		}
		// Out of patience. The guest exists and works; say plainly that the
		// filing did not happen rather than failing a request that succeeded.
		req.Error = "the guest was created but did not appear in inventory in time to be added to its group"
		d.Log.Warn().Str("request", req.ID.String()).Str("vmid", req.VMID).
			Msg("provisioned guest not filed into its group")
		d.auditOutcome(ctx, req, ports.OutcomeSuccess, map[string]any{"group_assigned": false})
		return nil
	}

	// Append rather than replace: SetVMGroupMembers rewrites a group's manual
	// membership, so passing only the new guest would empty the group of
	// everything else in it.
	members, err := d.Access.VMGroupMemberIDs(ctx, *req.VMGroupID)
	if err != nil {
		return err
	}
	for _, id := range members {
		if id == vmID {
			d.auditOutcome(ctx, req, ports.OutcomeSuccess, map[string]any{"group_assigned": true})
			return nil
		}
	}
	if err := d.Access.SetVMGroupMembers(ctx, *req.VMGroupID, append(members, vmID)); err != nil {
		return err
	}
	d.auditOutcome(ctx, req, ports.OutcomeSuccess, map[string]any{"group_assigned": true})
	return nil
}

func (d *Driver) pollTask(ctx context.Context, conn connector.Connector, req *provision.Request) (done, ok bool, detail string, err error) {
	watcher, isWatcher := conn.(connector.TaskWatcher)
	if !isWatcher {
		// Nothing to poll with: treat the task as finished rather than waiting
		// forever on an answer that will never come.
		return true, true, "", nil
	}
	return watcher.TaskState(ctx, connector.TaskRef{ID: req.TaskID, Node: req.TargetNode})
}

func (d *Driver) guestRef(req *provision.Request) connector.VMRef {
	guestType := req.Spec.TemplateType
	if guestType == "" {
		guestType = "qemu"
	}
	return connector.VMRef{ExternalID: req.VMID, HostID: req.TargetNode, Type: guestType}
}

func cloudInitSpec(req *provision.Request) connector.CloudInitSpec {
	return connector.CloudInitSpec{
		User:            req.Spec.CIUser,
		SSHKeys:         req.Spec.SSHKeys,
		IPConfig:        req.Spec.IPConfig,
		Nameserver:      req.Spec.Nameserver,
		SearchDomain:    req.Spec.SearchDomain,
		Cores:           req.Spec.Cores,
		MemoryMB:        req.Spec.MemoryMB,
		Bridge:          req.Spec.Bridge,
		VLAN:            req.Spec.VLAN,
		UpgradePackages: req.Spec.UpgradePackages,
		StartOnBoot:     req.Spec.StartOnBoot,
	}
}

// fail records a conclusion the platform reported and returns nil: the request
// is finished, even though the work is not, and retrying the job would only
// re-read the same answer.
func (d *Driver) fail(ctx context.Context, req *provision.Request, now time.Time, cause error) error {
	req.Fail(now, cause)
	d.Log.Warn().Str("request", req.ID.String()).Str("step", req.Step).Err(cause).
		Msg("provisioning request failed")
	d.auditOutcome(ctx, req, ports.OutcomeFailure, map[string]any{"error": cause.Error()})
	return d.Requests.SaveRequest(ctx, req)
}

func (d *Driver) enqueueSync(ctx context.Context, req *provision.Request) {
	if d.Queue == nil {
		return
	}
	if err := d.Queue.EnqueueInventorySync(ctx, req.PlatformID, "provision"); err != nil {
		d.Log.Warn().Err(err).Msg("could not request a sync after provisioning")
	}
}

func (d *Driver) auditOutcome(ctx context.Context, req *provision.Request, outcome string, details map[string]any) {
	if d.Audit == nil {
		return
	}
	action := "vm.provision.complete"
	if outcome == ports.OutcomeFailure {
		action = "vm.provision.fail"
	}
	switch req.Kind {
	case provision.KindDestroy:
		action = "vm.destroy.complete"
		if outcome == ports.OutcomeFailure {
			action = "vm.destroy.fail"
		}
	case provision.KindTemplate:
		action = "template.build.complete"
		if outcome == ports.OutcomeFailure {
			action = "template.build.fail"
		}
	}
	if details == nil {
		details = map[string]any{}
	}
	details["request_id"] = req.ID.String()
	details["vmid"] = req.VMID
	details["step"] = req.Step

	_ = d.Audit.Write(ctx, ports.AuditEntry{
		Time:        d.Clock.Now(),
		ActorUserID: req.RequestedBy,
		ActorName:   req.RequestedByName,
		Category:    ports.AuditCategorySecurity,
		Action:      action,
		TargetType:  "vm",
		TargetID:    req.VMID,
		TargetName:  req.GuestName,
		Outcome:     outcome,
		Details:     details,
	})
}

func errNotSupported(step string) error {
	return connector.Errorf(connector.ErrNotSupported, step,
		"this platform's connector cannot %s guests", step)
}
