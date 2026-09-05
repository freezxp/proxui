// Package provision holds the lifecycle of a request to create or destroy a
// guest (ADR 0010).
//
// The state machine lives here rather than in the job that drives it because
// the ordering rules are the whole substance of the feature: a guest must be
// configured before it is started, or cloud-init has nothing to read on first
// boot; a disk must be grown before the guest boots, or the filesystem inside
// will not have been resized to match. Those are invariants, and invariants
// belong where they can be tested without a database or a hypervisor.
package provision

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Kind separates the two things a request can be.
type Kind string

const (
	KindProvision Kind = "provision"
	KindDestroy   Kind = "destroy"
	// KindTemplate builds the image everything else is cloned from. It runs on
	// the same machinery because it is the same shape: four long platform
	// operations that must happen in order and survive a restart.
	KindTemplate Kind = "template"
)

// State is where a request has got to.
type State string

const (
	StatePending State = "pending"

	// Provisioning, in order.
	StateCloning     State = "cloning"
	StateConfiguring State = "configuring"
	StateResizing    State = "resizing"
	StateStarting    State = "starting"
	// StateVerifying waits for the guest's agent to answer after the portal
	// started it. It is the difference between "the platform accepted every
	// call" and "the machine came up": a guest that panics before init leaves
	// every step reporting success, and the only evidence anything is wrong is
	// an agent that never speaks (PROV-16).
	StateVerifying State = "verifying"
	StateReady     State = "ready"

	// Destruction.
	StateDeleting State = "deleting"
	StateDeleted  State = "deleted"

	// Template building, in order. StateReady ends this path too.
	StateDownloading State = "downloading"
	StateCreating    State = "creating"
	StateImporting   State = "importing"
	// StatePreparing installs the guest agent and clears the identity a clone
	// must not inherit. Between importing and converting because a template
	// cannot be modified once it is one (PROV-14).
	StatePreparing  State = "preparing"
	StateConverting State = "converting"

	// Either, at any point.
	StateFailed State = "failed"
)

// Terminal reports whether a request has finished moving.
func (s State) Terminal() bool {
	return s == StateReady || s == StateDeleted || s == StateFailed
}

// Spec is the non-secret input to a provisioning run.
//
// It deliberately has no password field. Guest credentials are never stored
// (ADR 0005), and this struct is serialized into a jsonb column — the most
// durable of the several places a password would otherwise come to rest
// (PROV-04).
type Spec struct {
	TemplateNode string `json:"template_node"`
	TemplateType string `json:"template_type"`
	FullClone    bool   `json:"full_clone"`
	Storage      string `json:"storage,omitempty"`

	CIUser       string   `json:"ci_user,omitempty"`
	SSHKeys      []string `json:"ssh_keys,omitempty"`
	IPConfig     string   `json:"ip_config,omitempty"`
	Nameserver   string   `json:"nameserver,omitempty"`
	SearchDomain string   `json:"search_domain,omitempty"`

	Cores    int `json:"cores,omitempty"`
	MemoryMB int `json:"memory_mb,omitempty"`

	Bridge string `json:"bridge,omitempty"`
	VLAN   int    `json:"vlan,omitempty"`

	// CPU is the processor model a built template gets. Only a template build
	// uses it: a clone inherits whatever its template was given.
	CPU string `json:"cpu,omitempty"`

	// DiskName and DiskGrowBytes are a pair: growing needs to know which disk.
	// Zero growth skips the resize step entirely rather than sending a no-op
	// the platform would reject.
	DiskName      string `json:"disk_name,omitempty"`
	DiskGrowBytes int64  `json:"disk_grow_bytes,omitempty"`

	UpgradePackages  *bool `json:"upgrade_packages,omitempty"`
	StartOnBoot      bool  `json:"start_on_boot,omitempty"`
	StartAfterCreate bool  `json:"start_after_create,omitempty"`

	// Template building (KindTemplate).
	ImageURL string `json:"image_url,omitempty"`
	// ImageFile is the name the image is stored under, which is also how a
	// later build recognises that it is already there.
	ImageFile    string `json:"image_file,omitempty"`
	ImageStorage string `json:"image_storage,omitempty"`
	Checksum     string `json:"checksum,omitempty"`
	ChecksumAlgo string `json:"checksum_algo,omitempty"`
	// ImagePresent is set by the driver when it finds the image already
	// downloaded, so the state machine can skip a step it would otherwise
	// spend several minutes on.
	ImagePresent bool `json:"image_present,omitempty"`
}

// Request is one create-or-destroy job, durable across restarts.
type Request struct {
	ID         uuid.UUID
	PlatformID uuid.UUID
	Kind       Kind
	State      State
	// Step records which step is running, and after a failure which one failed.
	// Separate from Error because "where" and "why" are different questions.
	Step string

	RequestedBy     *uuid.UUID
	RequestedByName string

	TemplateExternalID string
	TargetNode         string
	GuestName          string
	VMID               string
	VMGroupID          *uuid.UUID

	Spec   Spec
	TaskID string
	Error  string
	// VerifyUntil is when the portal stops waiting for a started guest's agent
	// and records that it never answered. Held on the request rather than
	// inferred from a timestamp so that "how long is it still going to wait"
	// is a fact the row states, not arithmetic somebody has to redo.
	VerifyUntil time.Time
	Created     time.Time
	Updated     time.Time
}

// ErrTerminal reports an attempt to move a request that has already finished.
var ErrTerminal = errors.New("provision: request has already finished")

// NextState reports the state that follows the current one.
//
// It consults the spec rather than walking a fixed list, because two of the
// steps are conditional: a request that asked for no extra disk must not enter
// resizing, and one that asked for the guest to stay off must not enter
// starting. Encoding that here keeps the driver a loop rather than a nest of
// conditions, and keeps the skipping testable.
func (r *Request) NextState() State {
	switch r.Kind {
	case KindDestroy:
		switch r.State {
		case StatePending:
			return StateDeleting
		case StateDeleting:
			return StateDeleted
		default:
			return StateFailed
		}

	case KindTemplate:
		switch r.State {
		case StatePending:
			// An image already on the storage is not downloaded again: it is
			// hundreds of megabytes, and the check that finds it is one call.
			if r.Spec.ImagePresent {
				return StateCreating
			}
			return StateDownloading
		case StateDownloading:
			return StateCreating
		case StateCreating:
			return StateImporting
		case StateImporting:
			return StatePreparing
		case StatePreparing:
			return StateConverting
		case StateConverting:
			return StateReady
		default:
			return StateFailed
		}
	}

	switch r.State {
	case StatePending:
		return StateCloning
	case StateCloning:
		return StateConfiguring
	case StateConfiguring:
		if r.Spec.DiskGrowBytes > 0 {
			return StateResizing
		}
		fallthrough
	case StateResizing:
		if r.Spec.StartAfterCreate {
			return StateStarting
		}
		return StateReady
	case StateStarting:
		return StateVerifying
	case StateVerifying:
		return StateReady
	default:
		return StateFailed
	}
}

// Advance moves the request on one step. It is the only way a request changes
// state on success, so the transition table has one implementation.
func (r *Request) Advance(now time.Time) error {
	if r.State.Terminal() {
		return ErrTerminal
	}
	next := r.NextState()
	r.State = next
	r.Step = string(next)
	// A task handle belongs to the step that started it; carrying it forward
	// would have the next step poll the previous step's task and conclude it
	// had already finished.
	r.TaskID = ""
	r.Updated = now
	return nil
}

// Fail records why a request stopped, keeping the step it stopped on.
//
// It does not clear VMID: a run that failed after cloning has left a guest on
// the platform, and the identifier is how an administrator finds it. Cleaning
// up automatically would mean destroying a machine on the strength of an error
// that may have been misread (PROV-06).
func (r *Request) Fail(now time.Time, cause error) {
	if r.State.Terminal() {
		return
	}
	r.Step = string(r.State)
	r.State = StateFailed
	if cause != nil {
		r.Error = cause.Error()
	}
	r.Updated = now
}

// Validate checks what must hold before a request is stored.
func (r *Request) Validate() error {
	if r.PlatformID == uuid.Nil {
		return errors.New("provision: a request needs a platform")
	}
	switch r.Kind {
	case KindProvision:
		if r.TemplateExternalID == "" {
			return errors.New("provision: a template is required")
		}
		if r.GuestName == "" {
			return errors.New("provision: a name for the new guest is required")
		}
		if r.Spec.DiskGrowBytes > 0 && r.Spec.DiskName == "" {
			return errors.New("provision: growing a disk requires naming which one")
		}
		if r.Spec.DiskGrowBytes < 0 {
			return errors.New("provision: disks cannot be shrunk")
		}
	case KindDestroy:
		if r.VMID == "" {
			return errors.New("provision: destroying needs the guest's platform id")
		}
	case KindTemplate:
		if r.GuestName == "" {
			return errors.New("provision: a name for the template is required")
		}
		if r.Spec.ImageURL == "" {
			return errors.New("provision: an image URL is required")
		}
		if r.TargetNode == "" {
			return errors.New("provision: building a template needs a node to build it on")
		}
		if r.Spec.Storage == "" || r.Spec.ImageStorage == "" {
			return errors.New("provision: building a template needs both a disk and an image storage")
		}
		// A checksum without an algorithm cannot be checked, and an algorithm
		// without a digest checks nothing. Neither is a refusal to verify —
		// that is stated by leaving both empty, and audited where it is stated.
		if (r.Spec.Checksum == "") != (r.Spec.ChecksumAlgo == "") {
			return errors.New("provision: a checksum needs both a digest and an algorithm")
		}
	default:
		return errors.New("provision: unknown request kind")
	}
	return nil
}
