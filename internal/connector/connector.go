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
	CapabilityVM              Capability = "vm"
	CapabilityHost            Capability = "host"
	CapabilityStorage         Capability = "storage"
	CapabilityNetwork         Capability = "network"
	CapabilityMetrics         Capability = "metrics"
	CapabilityMetricsBackfill Capability = "metrics_backfill"
	CapabilityConsole         Capability = "console"
	CapabilitySerialConsole   Capability = "serial_console"
	CapabilityPower           Capability = "power"
	CapabilityNodeAddress     Capability = "node_address"
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
