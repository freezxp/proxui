// Package mock is an in-process platform used by the dev environment, the test
// suite and the load-test fixture. It is the proof that the core is platform
// independent: the entire stack runs against it with no Proxmox in sight, and
// CI needs nothing but Docker (docs/09-connector-architecture.md §9.5).
//
// It can also misbehave on demand — auth failures, timeouts, throttling,
// disappearing VMs — so failure handling is exercised deliberately rather than
// hoped for.
package mock

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/freezxp/proxui/internal/connector"
)

// Type is the registry key for this connector.
const Type = "mock"

func init() {
	connector.Register(connector.Info{
		Type:        Type,
		DisplayName: "Mock Platform",
		Version:     "1.0.0",
		// The mock declares a schema too, so the form-rendering path is
		// exercised by something other than the one real connector.
		Schema: connector.ConfigSchema{
			EndpointLabel: "Mock endpoint",
			EndpointHelp:  "Any value; the mock never dials it.",
			Fields: []connector.Field{
				{
					Key: "vm_count", Label: "Simulated VMs", Kind: connector.FieldNumber,
					Default: 25, Help: "How many guests the fake platform reports.",
				},
				{
					Key: "fault", Label: "Injected fault", Kind: connector.FieldSelect,
					Help: "Forces a failure mode, for exercising error handling.",
					Options: []connector.FieldOption{
						{Value: "", Label: "None"},
						{Value: "unreachable", Label: "Unreachable"},
						{Value: "auth", Label: "Authentication failure"},
						{Value: "permission", Label: "Missing permission"},
						{Value: "throttled", Label: "Throttled"},
					},
				},
			},
			Credentials: []connector.CredentialForm{{
				Kind: "api_token", Label: "API token",
				Fields: []connector.Field{
					{Key: "token_id", Label: "Token ID", Kind: connector.FieldText},
					{Key: "secret", Label: "Secret", Kind: connector.FieldSecret},
				},
			}},
		},
	}, New)
}

// Fault injects a specific failure so the sync engine's error handling can be
// tested on purpose.
type Fault string

const (
	FaultNone        Fault = ""
	FaultAuth        Fault = "auth"        // credentials rejected: must not be retried
	FaultUnreachable Fault = "unreachable" // network failure: retryable
	FaultThrottled   Fault = "throttled"   // platform asks us to slow down
	FaultPermission  Fault = "permission"  // authenticated but under-privileged
	FaultSlow        Fault = "slow"        // exceeds deadlines
)

// Options configure the simulated fleet. They are read from Config.Extra so an
// operator can point a dev install at a synthetic 2,000-VM estate.
type Options struct {
	VMCount      int
	HostCount    int
	StorageCount int
	NetworkCount int
	Fault        Fault
	// MutationRate is the fraction of VMs that flip state on each listing,
	// exercising change detection.
	MutationRate float64
	Latency      time.Duration
	Seed         int64
}

func defaults() Options {
	return Options{VMCount: 12, HostCount: 3, StorageCount: 2, NetworkCount: 2, Seed: 1}
}

// Connector is the simulated platform.
type Connector struct {
	opts   Options
	cfg    connector.Config
	rnd    *rand.Rand
	mu     sync.Mutex
	vms    []connector.VMRecord
	hosts  []connector.HostRecord
	closed bool
	// tick advances only when the fleet actually mutates, so repeated listings
	// are stable — the conformance suite requires that.
	tick int64
	// counters accumulate like the cumulative disk and network counters real
	// platforms expose, so the collector's rate conversion is exercised
	// without a hypervisor.
	counters map[string]float64
}

// New builds a mock connector. It satisfies connector.Factory.
func New(cfg connector.Config, _ connector.Credentials, _ connector.Options) (connector.Connector, error) {
	opts := defaults()
	if err := applyExtra(&opts, cfg.Extra); err != nil {
		return nil, err
	}
	c := &Connector{
		opts: opts, cfg: cfg,
		rnd:      rand.New(rand.NewSource(opts.Seed)),
		counters: map[string]float64{},
	}
	c.seed()
	return c, nil
}

func applyExtra(opts *Options, extra map[string]any) error {
	for key, raw := range extra {
		switch key {
		case "vm_count":
			v, ok := toInt(raw)
			if !ok || v < 0 {
				return connector.Errorf(connector.ErrInvalidConfig, "validate_config", "vm_count must be a non-negative number")
			}
			opts.VMCount = v
		case "host_count":
			v, ok := toInt(raw)
			if !ok || v < 1 {
				return connector.Errorf(connector.ErrInvalidConfig, "validate_config", "host_count must be at least 1")
			}
			opts.HostCount = v
		case "fault":
			s, ok := raw.(string)
			if !ok {
				return connector.Errorf(connector.ErrInvalidConfig, "validate_config", "fault must be a string")
			}
			opts.Fault = Fault(s)
		case "mutation_rate":
			f, ok := toFloat(raw)
			if !ok || f < 0 || f > 1 {
				return connector.Errorf(connector.ErrInvalidConfig, "validate_config", "mutation_rate must be between 0 and 1")
			}
			opts.MutationRate = f
		case "latency_ms":
			v, ok := toInt(raw)
			if !ok || v < 0 {
				return connector.Errorf(connector.ErrInvalidConfig, "validate_config", "latency_ms must be non-negative")
			}
			opts.Latency = time.Duration(v) * time.Millisecond
		case "seed":
			v, ok := toInt(raw)
			if !ok {
				return connector.Errorf(connector.ErrInvalidConfig, "validate_config", "seed must be a number")
			}
			opts.Seed = int64(v)
		default:
			return connector.Errorf(connector.ErrInvalidConfig, "validate_config", "unknown option %q", key)
		}
	}
	return nil
}

func (c *Connector) seed() {
	c.seedLocked()
}

func (c *Connector) seedLocked() {
	states := []string{"running", "running", "running", "stopped", "paused"}

	c.hosts = make([]connector.HostRecord, 0, c.opts.HostCount)
	for i := 0; i < c.opts.HostCount; i++ {
		c.hosts = append(c.hosts, connector.HostRecord{
			ExternalID:  fmt.Sprintf("node%02d", i+1),
			Name:        fmt.Sprintf("mock-node-%02d", i+1),
			Status:      "online",
			CPUCores:    32,
			MemoryBytes: 256 << 30,
			Version:     "mock 8.2",
			UptimeS:     int64(86400 * (i + 3)),
		})
	}

	c.vms = make([]connector.VMRecord, 0, c.opts.VMCount)
	for i := 0; i < c.opts.VMCount; i++ {
		host := c.hosts[i%max(1, len(c.hosts))]
		c.vms = append(c.vms, connector.VMRecord{
			ExternalID:  fmt.Sprintf("%d", 100+i),
			Name:        fmt.Sprintf("mock-vm-%03d", i+1),
			Type:        "qemu",
			State:       states[i%len(states)],
			HostID:      host.ExternalID,
			CPUCores:    2 + i%6,
			MemoryBytes: int64(2+i%8) << 30,
			DiskBytes:   int64(32+i%64) << 30,
			UptimeS:     int64(3600 * (i + 1)),
			// Every third guest has neither tags nor a reported address, which
			// is what real fleets look like and what the NOT NULL columns must
			// tolerate.
			IPAddresses: ipsFor(i),
			Tags:        tagsFor(i),
			Pool:        []string{"prod", "staging", "lab"}[i%3],
			Attrs:       map[string]any{"os": "linux", "agent": true},
		})
	}
}

func ipsFor(i int) []string {
	if i%3 == 0 {
		return nil
	}
	return []string{fmt.Sprintf("10.10.%d.%d", i/250, 10+i%240)}
}

func tagsFor(i int) []string {
	if i%3 == 0 {
		return nil
	}
	return []string{"env:mock"}
}

// Info implements connector.Connector.
func (c *Connector) Info() connector.Info {
	return connector.Info{Type: Type, DisplayName: "Mock Platform", Version: "1.0.0"}
}

// ValidateConfig implements connector.Connector.
func (c *Connector) ValidateConfig(cfg connector.Config) error {
	opts := defaults()
	return applyExtra(&opts, cfg.Extra)
}

// Capabilities implements connector.Connector. The mock supports everything so
// that every core code path has something to run against.
func (c *Connector) Capabilities() []connector.Capability {
	return []connector.Capability{
		connector.CapabilityVM,
		connector.CapabilityHost,
		connector.CapabilityStorage,
		connector.CapabilityNetwork,
		connector.CapabilityMetrics,
		connector.CapabilityMetricsBackfill,
		connector.CapabilityConsole,
		connector.CapabilityPower,
	}
}

// TestConnection implements connector.Connector.
func (c *Connector) TestConnection(ctx context.Context) (connector.TestReport, error) {
	if err := c.gate(ctx); err != nil {
		return connector.TestReport{Reachable: false}, err
	}
	return connector.TestReport{
		Reachable:     true,
		Authenticated: true,
		Version:       "mock 8.2",
		NodeCount:     len(c.hosts),
		Warnings:      []string{"this is a simulated platform; no real workloads are shown"},
	}, nil
}

// Close implements connector.Connector and is safe to call repeatedly.
func (c *Connector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// ListVMs implements connector.VirtualMachineCollector.
func (c *Connector) ListVMs(ctx context.Context) ([]connector.VMRecord, error) {
	if err := c.gate(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.mutateLocked()
	out := make([]connector.VMRecord, len(c.vms))
	copy(out, c.vms)
	return out, nil
}

// ListHosts implements connector.HostCollector.
func (c *Connector) ListHosts(ctx context.Context) ([]connector.HostRecord, error) {
	if err := c.gate(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]connector.HostRecord, len(c.hosts))
	copy(out, c.hosts)
	return out, nil
}

// ListStoragePools implements connector.StorageCollector.
func (c *Connector) ListStoragePools(ctx context.Context) ([]connector.StorageRecord, error) {
	if err := c.gate(ctx); err != nil {
		return nil, err
	}
	out := make([]connector.StorageRecord, 0, c.opts.StorageCount)
	for i := 0; i < c.opts.StorageCount; i++ {
		out = append(out, connector.StorageRecord{
			ExternalID:  fmt.Sprintf("store%d", i+1),
			Name:        fmt.Sprintf("mock-store-%d", i+1),
			StorageType: []string{"zfs", "nfs"}[i%2],
			TotalBytes:  int64(4+i) << 40,
			UsedBytes:   int64(1+i) << 40,
			IsShared:    i%2 == 1,
		})
	}
	return out, nil
}

// ListNetworks implements connector.NetworkCollector.
func (c *Connector) ListNetworks(ctx context.Context) ([]connector.NetworkRecord, error) {
	if err := c.gate(ctx); err != nil {
		return nil, err
	}
	out := make([]connector.NetworkRecord, 0, c.opts.NetworkCount*len(c.hosts))
	for _, host := range c.hosts {
		for i := 0; i < c.opts.NetworkCount; i++ {
			out = append(out, connector.NetworkRecord{
				ExternalID: fmt.Sprintf("vmbr%d", i),
				Name:       fmt.Sprintf("vmbr%d", i),
				NetType:    "bridge",
				HostID:     host.ExternalID,
				CIDR:       fmt.Sprintf("10.10.%d.0/24", i),
			})
		}
	}
	return out, nil
}

// Health implements connector.HealthCollector.
func (c *Connector) Health(ctx context.Context) (connector.HealthReport, error) {
	if err := c.gate(ctx); err != nil {
		// Health reports unreachability rather than failing: the circuit
		// breaker needs a state, not an exception.
		return connector.HealthReport{State: connector.HealthUnreachable, Detail: err.Error()}, nil
	}
	return connector.HealthReport{State: connector.HealthHealthy, Version: "mock 8.2"}, nil
}

// CollectMetrics implements connector.MetricsCollector.
func (c *Connector) CollectMetrics(ctx context.Context, scope connector.MetricScope) ([]connector.Sample, error) {
	if err := c.gate(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	vms := make([]connector.VMRecord, len(c.vms))
	copy(vms, c.vms)
	hosts := make([]connector.HostRecord, len(c.hosts))
	copy(hosts, c.hosts)
	c.mu.Unlock()

	now := time.Now().UTC()
	out := make([]connector.Sample, 0, len(vms)*4+len(hosts))
	for i, vm := range vms {
		if vm.State != "running" {
			continue
		}
		phase := float64(now.Unix()%3600)/3600*2*math.Pi + float64(i)
		cpu := 20 + 15*math.Sin(phase)
		out = append(out,
			sample(now, connector.SubjectVM, vm.ExternalID, connector.MetricCPUPct, cpu),
			sample(now, connector.SubjectVM, vm.ExternalID, connector.MetricMemUsedBytes, float64(vm.MemoryBytes)*0.6),
			sample(now, connector.SubjectVM, vm.ExternalID, connector.MetricMemTotalBytes, float64(vm.MemoryBytes)),
			// Real platforms report traffic as an ever-growing byte count, not
			// a rate. Modelling that here is what lets the collector's counter
			// handling be tested with no hypervisor present.
			counterSample(now, vm.ExternalID, connector.MetricNetRxBps, c.advanceCounter(vm.ExternalID+":rx", 1e6+float64(i)*1e4)),
			counterSample(now, vm.ExternalID, connector.MetricNetTxBps, c.advanceCounter(vm.ExternalID+":tx", 5e5+float64(i)*1e3)),
			counterSample(now, vm.ExternalID, connector.MetricDiskReadBps, c.advanceCounter(vm.ExternalID+":rd", 2e5)),
			counterSample(now, vm.ExternalID, connector.MetricDiskWriteBps, c.advanceCounter(vm.ExternalID+":wr", 1e5)),
		)
	}
	for _, host := range hosts {
		out = append(out, sample(now, connector.SubjectHost, host.ExternalID, connector.MetricCPUPct, 35))
	}
	return out, nil
}

// BackfillMetrics implements connector.MetricsBackfiller, returning hourly
// history so charts are populated the moment a platform is registered.
func (c *Connector) BackfillMetrics(ctx context.Context, vm connector.VMRef, from time.Time) ([]connector.Sample, error) {
	if err := c.gate(ctx); err != nil {
		return nil, err
	}
	var out []connector.Sample
	for ts := from.UTC().Truncate(time.Hour); ts.Before(time.Now().UTC()); ts = ts.Add(time.Hour) {
		out = append(out, sample(ts, connector.SubjectVM, vm.ExternalID, connector.MetricCPUPct,
			25+10*math.Sin(float64(ts.Unix()))))
		if len(out) > 24*400 {
			break // a year of hourly points is the documented ceiling
		}
	}
	return out, nil
}

// CreateConsoleSession implements connector.ConsoleProvider. The endpoint is a
// local echo server, which is enough to prove the proxy pipes bytes correctly.
func (c *Connector) CreateConsoleSession(ctx context.Context, vm connector.VMRef, kind connector.ConsoleKind) (connector.ConsoleEndpoint, error) {
	if err := c.gate(ctx); err != nil {
		return nil, err
	}
	if kind != connector.ConsoleVNC && kind != connector.ConsoleSerial {
		return nil, connector.Errorf(connector.ErrNotSupported, "console", "console kind %q is not supported", kind)
	}
	return newEchoEndpoint()
}

// Power implements connector.PowerManager.
func (c *Connector) Power(ctx context.Context, vm connector.VMRef, action connector.PowerAction) (connector.TaskRef, error) {
	if err := c.gate(ctx); err != nil {
		return connector.TaskRef{}, err
	}
	if !action.Valid() {
		return connector.TaskRef{}, connector.Errorf(connector.ErrNotSupported, "power", "unknown action %q", action)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.vms {
		if c.vms[i].ExternalID != vm.ExternalID {
			continue
		}
		switch action {
		case connector.PowerStart, connector.PowerReboot:
			c.vms[i].State = "running"
		case connector.PowerStop, connector.PowerShutdown:
			c.vms[i].State = "stopped"
		}
		return connector.TaskRef{ID: fmt.Sprintf("UPID:mock:%s:%s", vm.ExternalID, action), Node: c.vms[i].HostID}, nil
	}
	return connector.TaskRef{}, connector.Errorf(connector.ErrNotSupported, "power", "unknown vm %q", vm.ExternalID)
}

// SetFault changes the injected failure at runtime, for tests that need a
// platform to break midway through a run.
func (c *Connector) SetFault(f Fault) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.Fault = f
}

// RemoveVM deletes a VM from the simulated fleet so deleted-asset detection can
// be exercised.
func (c *Connector) RemoveVM(externalID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, vm := range c.vms {
		if vm.ExternalID == externalID {
			c.vms = append(c.vms[:i], c.vms[i+1:]...)
			return
		}
	}
}

// RestoreVMs puts the full simulated fleet back, so tests can exercise an
// asset reappearing after being reported missing.
func (c *Connector) RestoreVMs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seedLocked()
}

// gate applies latency, context rules and the injected fault to every call.
func (c *Connector) gate(ctx context.Context) error {
	c.mu.Lock()
	fault, latency, closed := c.opts.Fault, c.opts.Latency, c.closed
	c.mu.Unlock()

	if closed {
		return connector.Errorf(connector.ErrUnreachable, "call", "connector is closed")
	}
	if err := ctx.Err(); err != nil {
		return connector.Wrap(connector.ErrUnreachable, "call", err)
	}

	if fault == FaultSlow {
		latency = 30 * time.Second
	}
	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			return connector.Wrap(connector.ErrUnreachable, "call", ctx.Err())
		}
	}

	switch fault {
	case FaultAuth:
		return connector.Errorf(connector.ErrAuth, "call", "simulated credential rejection")
	case FaultUnreachable:
		return connector.Errorf(connector.ErrUnreachable, "call", "simulated network failure")
	case FaultThrottled:
		return connector.Errorf(connector.ErrThrottled, "call", "simulated rate limit")
	case FaultPermission:
		return connector.Errorf(connector.ErrPermission, "call", "simulated missing privilege")
	}
	return nil
}

// mutateLocked flips a share of VM states, so change detection has something to
// detect. With MutationRate zero the fleet is stable and listings are identical.
func (c *Connector) mutateLocked() {
	if c.opts.MutationRate <= 0 || len(c.vms) == 0 {
		return
	}
	count := int(float64(len(c.vms)) * c.opts.MutationRate)
	if count == 0 {
		count = 1
	}
	for i := 0; i < count; i++ {
		idx := c.rnd.Intn(len(c.vms))
		if c.vms[idx].State == "running" {
			c.vms[idx].State = "stopped"
		} else {
			c.vms[idx].State = "running"
		}
	}
	c.tick++
}

func sample(ts time.Time, subject connector.SubjectKind, id string, kind connector.MetricKind, value float64) connector.Sample {
	return connector.Sample{Time: ts, Subject: subject, SubjectID: id, Kind: kind, Value: value}
}

func counterSample(ts time.Time, id string, kind connector.MetricKind, value float64) connector.Sample {
	return connector.Sample{
		Time: ts, Subject: connector.SubjectVM, SubjectID: id,
		Kind: kind, Value: value, Cumulative: true,
	}
}

// advanceCounter grows a monotonic counter by the given amount and returns the
// new total, mirroring how platforms report cumulative traffic.
func (c *Connector) advanceCounter(key string, delta float64) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counters[key] += delta
	return c.counters[key]
}

// ResetCounters simulates a guest reboot, where counters restart from zero.
// The collector must drop that sample rather than draw an enormous spike.
func (c *Connector) ResetCounters() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counters = map[string]float64{}
}

// echoEndpoint is a loopback console served over WebSocket, mirroring how real
// platforms expose one. Speaking the same protocol as Proxmox is what lets the
// console bridge be tested end to end with no hypervisor present.
type echoEndpoint struct {
	server  *httptest.Server
	expires time.Time
}

var echoUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func newEchoEndpoint() (connector.ConsoleEndpoint, error) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := echoUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(kind, data); err != nil {
				return
			}
		}
	}))
	return &echoEndpoint{server: srv, expires: time.Now().Add(60 * time.Second)}, nil
}

// DialContext opens a raw connection, for callers that want bytes rather than
// frames.
func (e *echoEndpoint) DialContext(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", strings.TrimPrefix(e.server.URL, "http://"))
	if err != nil {
		return nil, connector.Wrap(connector.ErrUnreachable, "console_dial", err)
	}
	return conn, nil
}

func (e *echoEndpoint) ExpiresAt() time.Time { return e.expires }

// WebsocketURL implements connector.WebsocketConsole.
func (e *echoEndpoint) WebsocketURL() string {
	return "ws://" + strings.TrimPrefix(e.server.URL, "http://")
}

// TLSClientConfig implements connector.WebsocketConsole; the echo server is
// plain HTTP, so no trust policy applies.
func (e *echoEndpoint) TLSClientConfig() *tls.Config { return nil }

// RequestHeader implements connector.WebsocketConsole.
func (e *echoEndpoint) RequestHeader() http.Header { return http.Header{} }

// Close shuts down the echo server.
func (e *echoEndpoint) Close() { e.server.Close() }

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
