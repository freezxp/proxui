package publish

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrExposureNotAcknowledged means nobody has confirmed they intend to put
// this service on the public internet (PUB-43).
var ErrExposureNotAcknowledged = errors.New(
	"publish: publishing without an identity check in front requires acknowledging that the service will be reachable by anyone")

// App is a hostname the portal has published.
type App struct {
	ID         uuid.UUID
	ProviderID uuid.UUID

	Hostname string
	Path     string
	// ServiceURL is the resolved target. Resolved rather than a VM reference
	// so the rule the portal would write is knowable without the inventory —
	// including after the VM is gone, which is when it matters most.
	ServiceURL string

	VMID   *uuid.UUID
	VMPort int

	OriginRequest map[string]any

	DNSZoneID   string
	DNSRecordID string

	IsEnabled bool

	ExposureAckBy *uuid.UUID
	ExposureAckAt time.Time

	LastAppliedAt time.Time
	LastError     string

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt time.Time
}

// Rule renders the app as the routing rule it becomes.
func (a *App) Rule() Rule {
	return Rule{Hostname: a.Hostname, Path: a.Path, Service: a.ServiceURL}
}

// RouteKey identifies the route this app occupies.
func (a *App) RouteKey() string {
	return strings.ToLower(strings.TrimSpace(a.Hostname)) + strings.TrimSpace(a.Path)
}

// Validate checks what must hold for any app.
func (a *App) Validate() error {
	if err := ValidateHostname(a.Hostname); err != nil {
		return err
	}
	if err := ValidateService(a.ServiceURL); err != nil {
		return err
	}
	// A catch-all is the table's terminator, not something anyone publishes.
	// Allowing it would let a published app swallow every other hostname.
	if strings.HasPrefix(strings.TrimSpace(a.ServiceURL), "http_status:") {
		return fmt.Errorf("%w: a published app cannot target a status literal", ErrInvalidService)
	}
	if a.Path != "" && !strings.HasPrefix(a.Path, "/") {
		return fmt.Errorf("%w: a path must start with /", ErrInvalidService)
	}
	if a.VMPort < 0 || a.VMPort > 65535 {
		return fmt.Errorf("%w: port %d is out of range", ErrInvalidService, a.VMPort)
	}
	return nil
}

// TargetFor builds a service URL for a machine and port.
func TargetFor(scheme, address string, port int) string {
	if scheme == "" {
		scheme = "http"
	}
	return Target{Scheme: scheme, Host: address, Port: port}.String()
}

// ApplyTo returns the routing table that results from adding or replacing this
// app's rule in the current one.
//
// New rules go immediately before the catch-all, so they are reachable without
// disturbing the order of anything already there. That matters because order
// decides which rule wins, and quietly reshuffling somebody else's rules is
// the kind of change nobody notices until traffic goes somewhere unexpected.
func ApplyTo(current Table, app *App) Table {
	rule := app.Rule()
	key := ruleKey(rule)

	out := make(Table, 0, len(current)+1)
	replaced := false
	for _, r := range current {
		if r.IsCatchAll() {
			continue
		}
		if ruleKey(r) == key {
			out = append(out, rule)
			replaced = true
			continue
		}
		out = append(out, r)
	}
	if !replaced {
		out = append(out, rule)
	}
	return EnsureCatchAll(append(out, catchAllOf(current)))
}

// RemoveFrom returns the routing table with this app's rule taken out.
func RemoveFrom(current Table, app *App) Table {
	key := ruleKey(app.Rule())

	out := make(Table, 0, len(current))
	for _, r := range current {
		if r.IsCatchAll() || ruleKey(r) == key {
			continue
		}
		out = append(out, r)
	}
	return EnsureCatchAll(append(out, catchAllOf(current)))
}

// catchAllOf returns the table's terminator, or the conventional one when it
// has none. Preserved rather than replaced: a catch-all pointing somewhere
// other than a 404 was chosen deliberately by somebody.
func catchAllOf(t Table) Rule {
	for _, r := range t {
		if r.IsCatchAll() {
			return r
		}
	}
	return CatchAll()
}
