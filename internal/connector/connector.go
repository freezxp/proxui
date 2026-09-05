// Package connector is the port every platform integration implements. It is
// the boundary that keeps ProxUI platform-independent: the core depends only on
// these interfaces and never imports a concrete connector.
//
// Adding a platform means writing a package that implements Connector plus the
// capability interfaces the platform supports, registering a factory in init(),
// and adding one blank import to cmd/proxui/connectors.go. Nothing in domain,
// app, sync or transport changes (docs/09-connector-architecture.md).
package connector

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// Capability names an optional behaviour a connector supports. The core asks
// for capabilities rather than type-switching on platform names, so the UI can
// hide a console button for a platform that has no consoles.
type Capability string

const (
	CapabilityVM                Capability = "vm"
	CapabilityHost              Capability = "host"
	CapabilityStorage           Capability = "storage"
	CapabilityNetwork           Capability = "network"
	CapabilityMetrics           Capability = "metrics"
	CapabilityMetricsBackfill   Capability = "metrics_backfill"
	CapabilityConsole           Capability = "console"
	CapabilitySerialConsole     Capability = "serial_console"
	CapabilityPower             Capability = "power"
	CapabilityNodeAddress       Capability = "node_address"
	CapabilityEndpointDiscovery Capability = "endpoint_discovery"
	CapabilityProvision         Capability = "provision"
	CapabilityDestroy           Capability = "destroy"
	CapabilityTemplateBuild     Capability = "template_build"
)

// Info identifies a connector implementation.
type Info struct {
	Type        string // registry key, e.g. "proxmox"
	DisplayName string
	Version     string
	// Schema drives the platform form in the UI. A connector that declares it
	// needs no UI change to be configurable, which is the point of the plugin
	// framework (docs/09-connector-architecture.md).
	Schema ConfigSchema
}

// Config is the non-secret platform configuration supplied by an administrator.
type Config struct {
	Endpoint   string
	Datacenter string
	TLS        TLSPolicy
	// Failover lists further addresses the same platform answers on, tried in
	// order when Endpoint is unreachable and never otherwise (ADR 0009).
	// Endpoint stays the address an administrator typed; these are cluster
	// facts the sync engine discovers and rewrites.
	Failover []Endpoint
	// Extra carries connector-specific settings declared by the connector's
	// config schema, so new platforms need no core schema changes.
	Extra map[string]any
}

// TLSMode selects how an upstream certificate is verified.
type TLSMode string

const (
	TLSVerify      TLSMode = "verify"      // system roots
	TLSCustomCA    TLSMode = "custom_ca"   // operator-supplied CA bundle
	TLSFingerprint TLSMode = "fingerprint" // SHA-256 pin, for self-signed clusters
	TLSInsecure    TLSMode = "insecure"    // audited and warned about in the UI
)

// TLSPolicy describes how to trust the platform endpoint.
type TLSPolicy struct {
	Mode        TLSMode
	CAPEM       string
	Fingerprint string
}

// Endpoint is one address a platform can be reached at.
//
// Fingerprint pins this address specifically, because the members of a cluster
// each present their own certificate: a pin that is correct for one is
// correctly rejected by the next. It is only consulted under TLSFingerprint,
// where it replaces TLSPolicy.Fingerprint for this address alone; the other
// modes already trust every member through system roots or a cluster CA.
type Endpoint struct {
	Address     string
	Fingerprint string
}

// EndpointDiscoverer reports the other addresses a platform answers on.
//
// The contract is deliberately narrow about trust: an implementation must learn
// each address, and each address's fingerprint, over the connection it is
// already using — one whose certificate has already been verified. Reading a
// certificate from the address being added would be trust-on-first-use under
// exactly the conditions that make it weakest, since failover happens when the
// network is already misbehaving (ADR 0009).
//
// Returning fewer endpoints than the platform has is always safe. Returning one
// whose fingerprint could not be established is not, and must not be done.
type EndpointDiscoverer interface {
	DiscoverEndpoints(ctx context.Context) ([]Endpoint, error)
}

// Credentials are decrypted secrets, scoped to a single connector instance.
// They are never logged and never persisted by a connector.
type Credentials struct {
	Kind     string // api_token | userpass
	TokenID  string
	Secret   string
	Username string
	Password string
}

// Options carries cross-cutting knobs the core controls.
type Options struct {
	Timeout        time.Duration
	MaxConcurrency int
	RequestsPerSec float64
}

// Connector is the root interface every platform implements.
type Connector interface {
	Info() Info
	// ValidateConfig checks configuration statically, before anything is saved.
	ValidateConfig(cfg Config) error
	// TestConnection proves reachability, authentication and privilege, and
	// reports precisely what is missing when it fails.
	TestConnection(ctx context.Context) (TestReport, error)
	// Capabilities lists what this instance supports. It must agree with the
	// interfaces the connector actually implements; the conformance suite
	// enforces that.
	Capabilities() []Capability
	Close() error
}

// VirtualMachineCollector lists virtual machines.
type VirtualMachineCollector interface {
	ListVMs(ctx context.Context) ([]VMRecord, error)
}

// HostCollector lists hypervisor hosts or nodes.
type HostCollector interface {
	ListHosts(ctx context.Context) ([]HostRecord, error)
}

// StorageCollector lists storage pools or datastores.
type StorageCollector interface {
	ListStoragePools(ctx context.Context) ([]StorageRecord, error)
}

// NetworkCollector lists networks, bridges and bonds.
type NetworkCollector interface {
	ListNetworks(ctx context.Context) ([]NetworkRecord, error)
}

// HealthCollector reports platform health. It must stay cheap: it runs far more
// often than inventory collection and drives the circuit breaker.
type HealthCollector interface {
	Health(ctx context.Context) (HealthReport, error)
}

// MetricsCollector samples performance counters.
type MetricsCollector interface {
	CollectMetrics(ctx context.Context, scope MetricScope) ([]Sample, error)
}

// MetricsBackfiller imports historical samples, so charts are useful the moment
// a platform is registered.
type MetricsBackfiller interface {
	BackfillMetrics(ctx context.Context, vm VMRef, from time.Time) ([]Sample, error)
}

// NodeAddresser reports the management address of each node, keyed by the
// node's external ID.
//
// It exists because some hardware readings are not in any platform's API and
// have to be fetched from the node itself (ADR 0007). The address comes from
// the platform rather than from a request, which is what keeps the reachable
// set the platform's own inventory.
type NodeAddresser interface {
	NodeAddresses(ctx context.Context) (map[string]string, error)
}

// Provisioner creates guests from a platform's own templates.
//
// The steps are separate because the platform performs them separately and at
// different speeds: cloning is a long asynchronous task returning a TaskRef,
// while configuring and resizing answer immediately. A caller that ran them as
// one call would have nothing to resume from when the slow half timed out
// (ADR 0010).
type Provisioner interface {
	// ListTemplates reports the images a guest can be cloned from. Templates
	// are deliberately absent from the VM inventory, so this is the only way
	// the core learns they exist.
	ListTemplates(ctx context.Context) ([]TemplateRecord, error)
	// NextID reserves nothing; it reports an identifier that was free when
	// asked. Two callers can be handed the same one, so Clone must be prepared
	// to be refused and to ask again.
	NextID(ctx context.Context) (string, error)
	Clone(ctx context.Context, spec CloneSpec) (TaskRef, error)
	Configure(ctx context.Context, vm VMRef, spec CloudInitSpec) error
	// ResizeDisk grows a disk by growBytes. Shrinking is not offered because
	// platforms do not implement it safely; a negative or zero value is an
	// error rather than a no-op.
	ResizeDisk(ctx context.Context, vm VMRef, disk string, growBytes int64) error
}

// TemplateBuilder builds the image everything else is cloned from.
//
// It exists because the alternative is a sentence in the UI telling an operator
// to go and run four commands on a node — which is the situation provisioning
// was built to remove, one step earlier (ADR 0010).
//
// The image is fetched by the platform, not by the portal: the node has the
// bandwidth, the storage and the internet access, and a portal that streamed
// hundreds of megabytes through itself would be the slowest possible way to do
// it.
type TemplateBuilder interface {
	// ImageExists reports whether the image is already on the storage, so a
	// second template built from the same image does not fetch it again.
	ImageExists(ctx context.Context, node, storage, filename string) (bool, error)
	DownloadImage(ctx context.Context, spec ImageDownloadSpec) (TaskRef, error)
	CreateGuest(ctx context.Context, spec GuestCreateSpec) (TaskRef, error)
	ImportDisk(ctx context.Context, vm VMRef, spec DiskImportSpec) (TaskRef, error)
	ConvertToTemplate(ctx context.Context, vm VMRef) (TaskRef, error)
}

// TaskWatcher reports how an asynchronous platform task ended.
//
// The Proxmox connector has answered this question since power actions were
// written and nothing declared the interface, so nothing could ask it. Anything
// that hands work to a platform and needs to know how it went — a power action
// whose audit entry should record the real outcome, a clone the provisioner is
// waiting on — goes through here.
type TaskWatcher interface {
	// TaskState reports whether the task finished, whether it succeeded, and
	// the platform's own description of the outcome.
	TaskState(ctx context.Context, task TaskRef) (done bool, ok bool, detail string, err error)
}

// Destroyer removes a guest and its disks.
//
// Separate from Provisioner so a connector can offer creation without
// destruction, and so the capability an operator is granting is legible one
// interface at a time.
type Destroyer interface {
	Destroy(ctx context.Context, vm VMRef, opts DestroyOptions) (TaskRef, error)
}

// ConsoleKind selects a console protocol.
type ConsoleKind string

const (
	ConsoleVNC    ConsoleKind = "vnc"
	ConsoleSerial ConsoleKind = "serial"
)

// ConsoleProvider opens interactive console sessions.
type ConsoleProvider interface {
	CreateConsoleSession(ctx context.Context, vm VMRef, kind ConsoleKind) (ConsoleEndpoint, error)
}

// ConsoleEndpoint is a dialable upstream console. The portal proxies bytes
// without interpreting the protocol, which keeps the bridge independent of VNC,
// SPICE or serial specifics.
type ConsoleEndpoint interface {
	DialContext(ctx context.Context) (net.Conn, error)
	// ExpiresAt is when the upstream ticket stops being usable.
	ExpiresAt() time.Time
}

// WebsocketConsole is a console reached over WebSocket, which is how Proxmox
// and most modern platforms expose one. The bridge relays frames between the
// browser and this endpoint without reading them, so it stays protocol-neutral.
type WebsocketConsole interface {
	ConsoleEndpoint
	// WebsocketURL is the full upstream URL including any ticket parameters.
	WebsocketURL() string
	// TLSClientConfig is the trust policy for the upstream connection, so a
	// pinned or self-signed cluster is honoured on the console path too.
	TLSClientConfig() *tls.Config
	// RequestHeader carries any headers the upstream handshake needs.
	RequestHeader() http.Header
}

// PowerAction is a lifecycle operation on a VM.
type PowerAction string

const (
	PowerStart    PowerAction = "start"
	PowerStop     PowerAction = "stop"
	PowerShutdown PowerAction = "shutdown"
	PowerReboot   PowerAction = "reboot"
)

// Valid reports whether a is a known power action.
func (a PowerAction) Valid() bool {
	switch a {
	case PowerStart, PowerStop, PowerShutdown, PowerReboot:
		return true
	}
	return false
}

// PowerManager performs power actions.
type PowerManager interface {
	Power(ctx context.Context, vm VMRef, action PowerAction) (TaskRef, error)
}

// VMRef identifies a VM on its platform without dragging the whole record around.
type VMRef struct {
	ExternalID string
	HostID     string // platform-side host/node identifier
	Type       string // qemu | lxc | ...
}

// TaskRef is a handle to an asynchronous platform task.
type TaskRef struct {
	ID   string
	Node string
}

// TestReport is the outcome of TestConnection. It is written for an
// administrator debugging a registration, so it is deliberately specific.
type TestReport struct {
	Reachable          bool
	Authenticated      bool
	Version            string
	NodeCount          int
	MissingPermissions []string
	Warnings           []string
	// ProvisioningAvailable reports whether the credential may create and
	// destroy guests. It is a capability, not a requirement: a token without
	// those privileges syncs exactly as before and simply cannot provision, so
	// an absent capability is reported rather than treated as a failure
	// (PROV-01, ADR 0010).
	ProvisioningAvailable         bool
	MissingProvisioningPrivileges []string
	// TemplateBuildAvailable is reported apart from provisioning: cloning from
	// a template somebody else built needs strictly less than building one.
	TemplateBuildAvailable    bool
	MissingTemplatePrivileges []string
}

// HealthState summarises platform reachability.
type HealthState string

const (
	HealthUnknown     HealthState = "unknown"
	HealthHealthy     HealthState = "healthy"
	HealthDegraded    HealthState = "degraded"
	HealthUnreachable HealthState = "unreachable"
)

// HealthReport is the result of a health probe.
type HealthReport struct {
	State   HealthState
	Detail  string
	Version string
}

// MetricScope narrows a metrics collection run.
type MetricScope struct {
	// VMs limits collection to specific VMs; empty means every VM.
	VMs []VMRef
	// Hosts limits collection to specific hosts; empty means every host.
	Hosts []string
}
