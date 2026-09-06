// Package deployment is the record of the portal installing one application
// into an LXC container on a node (ADR 0012).
//
// It is deliberately not part of `provision`: that package is guest-shaped —
// clone a template, write cloud-init, wait for a guest agent — and an LXC has
// no guest agent, so its final state could never be reached here. What the two
// share is the shape of the record rather than the steps.
package deployment

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// State is where a deployment has got to.
type State string

const (
	StatePending   State = "pending"
	StateDeploying State = "deploying"
	StateReady     State = "ready"
	StateFailed    State = "failed"
)

// Terminal reports whether nothing further will happen.
func (s State) Terminal() bool { return s == StateReady || s == StateFailed }

// ErrTerminal reports an attempt to move a deployment that has finished.
var ErrTerminal = errors.New("deployment: already finished")

// Limits on what may be asked for.
//
// These are not the platform's limits and are not trying to be. They are the
// range in which a number is obviously a number, because every one of these is
// placed into the environment of a command running as root and the cheapest
// place to be sure of that is before it leaves the portal.
const (
	MinCores    = 1
	MaxCores    = 128
	MinMemoryMB = 128
	MaxMemoryMB = 512 << 10
	MinDiskGB   = 1
	MaxDiskGB   = 2048
	MaxLogBytes = 256 << 10
)

// hostnamePattern is a DNS label: what `pct` will accept and what the container
// will answer to. namePattern covers a storage or a bridge, which are Proxmox
// identifiers rather than free text.
var (
	hostnamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	namePattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
)

// Spec is what an operator chose. Every field is optional: an empty one means
// the script's own default, which is what the catalogue displays.
type Spec struct {
	Hostname string `json:"hostname,omitempty"`
	Cores    int    `json:"cores,omitempty"`
	MemoryMB int    `json:"memory_mb,omitempty"`
	DiskGB   int    `json:"disk_gb,omitempty"`
	Storage  string `json:"storage,omitempty"`
	Bridge   string `json:"bridge,omitempty"`
	// Unprivileged is a pointer because "not chosen" and "chosen as privileged"
	// are different: the first leaves the script's default alone, and most of
	// these scripts default to unprivileged for a reason.
	Unprivileged *bool `json:"unprivileged,omitempty"`
}

// Deployment is the durable record of one install.
type Deployment struct {
	ID         uuid.UUID
	PlatformID uuid.UUID
	Node       string
	AppID      string
	AppName    string
	// CTID is the container the script created, once it has said which. Read
	// out of the log, because the script allocates it rather than the portal.
	CTID string

	State           State
	RequestedBy     *uuid.UUID
	RequestedByName string

	Spec Spec
	// Log is what the script printed, truncated to the last MaxLogBytes. It is
	// kept because a deploy that fails halfway cannot be explained by a state
	// name, and nothing else in the portal has ever kept the output of a
	// non-interactive command.
	Log string
	// ExitCode is the script's, once it has finished. Nil while it runs.
	ExitCode *int
	Error    string

	Created time.Time
	Updated time.Time
}

// Validate checks a deployment before anything is dialled.
//
// This is the boundary. Everything here ends up in the environment of a command
// that runs as root on a hypervisor, so a value that is not obviously a number
// or an identifier does not get to travel any further than this function.
func (d *Deployment) Validate() error {
	if d.PlatformID == uuid.Nil {
		return errors.New("a deployment needs a platform")
	}
	if !namePattern.MatchString(d.Node) {
		return fmt.Errorf("%q is not a node name", d.Node)
	}
	if strings.TrimSpace(d.AppID) == "" {
		return errors.New("a deployment needs an application")
	}
	return d.Spec.Validate()
}

// Validate checks the chosen settings.
func (s *Spec) Validate() error {
	if s.Hostname != "" && !hostnamePattern.MatchString(s.Hostname) {
		return fmt.Errorf("%q is not a hostname: lowercase letters, digits and hyphens", s.Hostname)
	}
	if s.Storage != "" && !namePattern.MatchString(s.Storage) {
		return fmt.Errorf("%q is not a storage name", s.Storage)
	}
	if s.Bridge != "" && !namePattern.MatchString(s.Bridge) {
		return fmt.Errorf("%q is not a bridge name", s.Bridge)
	}
	if err := inRange("cores", s.Cores, MinCores, MaxCores); err != nil {
		return err
	}
	if err := inRange("memory", s.MemoryMB, MinMemoryMB, MaxMemoryMB); err != nil {
		return err
	}
	return inRange("disk", s.DiskGB, MinDiskGB, MaxDiskGB)
}

// inRange treats zero as "not chosen", which is what leaves the script's own
// default in place rather than overriding it with a guess.
func inRange(what string, v, low, high int) error {
	if v == 0 {
		return nil
	}
	if v < low || v > high {
		return fmt.Errorf("%s must be between %d and %d, not %d", what, low, high, v)
	}
	return nil
}

// Advance moves a deployment on one step.
func (d *Deployment) Advance(next State, now time.Time) error {
	if d.State.Terminal() {
		return ErrTerminal
	}
	d.State = next
	d.Updated = now
	return nil
}

// Finish records how the script ended.
//
// A non-zero exit is a failure of the deployment, not of the portal: the
// container may well exist and be half-built, which is why the log is kept and
// nothing is cleaned up (the same reasoning as PROV-06).
//
// A zero exit is not on its own a success. These scripts exit 0 when a person
// closes their menu, so a deploy that stopped at a question nobody was there to
// answer would otherwise be recorded as ready with nothing installed — which is
// exactly the shape of failure the provisioning verification step exists to
// catch. The container is the evidence: no container, no deployment.
func (d *Deployment) Finish(code int, now time.Time) {
	if d.State.Terminal() {
		return
	}
	d.ExitCode = &code
	d.Updated = now
	switch {
	case code != 0:
		d.State = StateFailed
		d.Error = fmt.Sprintf("the install script exited %d; the log below is what it printed", code)
	case d.CTID == "":
		d.State = StateFailed
		d.Error = "the install script finished without creating a container. It usually means it " +
			"stopped at a question — the log below ends at the one it asked — and the answer " +
			"belongs in this deployment's settings rather than in a prompt nobody can see."
	default:
		d.State = StateReady
	}
}

// Fail records that the deployment could not be carried out at all.
func (d *Deployment) Fail(cause error, now time.Time) {
	if d.State.Terminal() {
		return
	}
	d.State = StateFailed
	d.Updated = now
	if cause != nil {
		d.Error = cause.Error()
	}
}

// TruncateLog keeps the end of a transcript, which is where a failure is.
func TruncateLog(s string) string {
	if len(s) <= MaxLogBytes {
		return s
	}
	cut := s[len(s)-MaxLogBytes:]
	// Start at a line boundary so the first line shown is a whole one.
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < 4096 {
		cut = cut[i+1:]
	}
	return "… earlier output dropped …\n" + cut
}
