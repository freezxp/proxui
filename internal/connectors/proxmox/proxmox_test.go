package proxmox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/connector/connectortest"
	"github.com/freezxp/proxui/internal/connectors/proxmox"
)

const testToken = "proxui@pve!portal"
const testSecret = "00000000-1111-2222-3333-444444444444"

// fakePVE is a fixture Proxmox API. It lets the connector be tested against
// realistic payloads with no cluster present, which is what keeps CI hermetic.
type fakePVE struct {
	*httptest.Server
	mu          sync.Mutex
	requests    []string
	authHeaders []string
	status      map[string]int // path -> forced status
	permissions map[string]map[string]int
}

func newFakePVE(t *testing.T) *fakePVE {
	t.Helper()
	f := &fakePVE{
		status: map[string]int{},
		permissions: map[string]map[string]int{
			"/": {"VM.Audit": 1, "Sys.Audit": 1, "Datastore.Audit": 1, "VM.Console": 1, "VM.PowerMgmt": 1, "VM.GuestAgent.Audit": 1},
		},
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/api2/json/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")

		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+path)
		f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
		forced, hasForced := f.status[path]
		perms := f.permissions
		f.mu.Unlock()

		if hasForced {
			w.WriteHeader(forced)
			return
		}
		if r.Header.Get("Authorization") != "PVEAPIToken="+testToken+"="+testSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case path == "/version":
			writeData(w, map[string]any{"version": "8.2.4", "release": "8.2"})
		case path == "/cluster/status":
			writeData(w, []map[string]any{{"type": "cluster", "name": "test", "quorate": 1}})
		case path == "/access/permissions":
			writeData(w, perms)
		case path == "/nodes":
			writeData(w, []map[string]any{
				{"node": "pve1", "status": "online", "maxcpu": 16, "maxmem": 68719476736, "uptime": 864000},
				{"node": "pve2", "status": "online", "maxcpu": 32, "maxmem": 137438953472, "uptime": 432000},
			})
		case path == "/cluster/resources":
			writeData(w, clusterResourcesFixture())
		case strings.HasSuffix(path, "/network"):
			writeData(w, []map[string]any{
				{"iface": "vmbr0", "type": "bridge", "cidr": "10.0.30.0/24", "active": 1, "bridge_ports": "eno1"},
				{"iface": "eno1", "type": "eth", "active": 1},
			})
		case strings.HasSuffix(path, "/agent/network-get-interfaces"):
			writeData(w, map[string]any{"result": []map[string]any{
				{"name": "lo", "ip-addresses": []map[string]any{{"ip-address": "127.0.0.1", "ip-address-type": "ipv4"}}},
				{"name": "eth0", "ip-addresses": []map[string]any{
					{"ip-address": "10.0.30.50", "ip-address-type": "ipv4"},
					{"ip-address": "fe80::1", "ip-address-type": "ipv6"},
				}},
			}})
		case strings.HasSuffix(path, "/vncproxy"):
			writeData(w, map[string]any{"ticket": "PVEVNC:ticket-value", "port": "5900", "user": testToken})
		case strings.HasSuffix(path, "/rrddata"):
			writeData(w, rrdFixture())
		case strings.Contains(path, "/status/"):
			writeData(w, "UPID:pve1:00001234:mock:qmstart:100:proxui@pve!portal:")
		case strings.Contains(path, "/tasks/"):
			writeData(w, map[string]any{"status": "stopped", "exitstatus": "OK"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakePVE) forceStatus(path string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[path] = code
}

func (f *fakePVE) setPermissions(perms map[string]map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permissions = perms
}

func (f *fakePVE) sawRequest(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if strings.HasPrefix(req, prefix) {
			return true
		}
	}
	return false
}

func (f *fakePVE) requestCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if strings.HasPrefix(req, prefix) {
			n++
		}
	}
	return n
}

func writeData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func clusterResourcesFixture() []map[string]any {
	return []map[string]any{
		{"id": "qemu/100", "type": "qemu", "node": "pve1", "name": "web-01", "vmid": 100,
			"status": "running", "maxcpu": 4, "cpu": 0.12, "maxmem": 8589934592, "mem": 5153960755,
			"maxdisk": 34359738368, "disk": 0, "uptime": 432000, "tags": "prod;web", "pool": "production",
			"diskread": 1024, "diskwrite": 2048, "netin": 4096, "netout": 8192},
		{"id": "qemu/101", "type": "qemu", "node": "pve2", "name": "db-01", "vmid": 101,
			"status": "stopped", "maxcpu": 8, "maxmem": 17179869184, "maxdisk": 107374182400},
		{"id": "lxc/200", "type": "lxc", "node": "pve1", "name": "proxy-01", "vmid": 200,
			"status": "running", "maxcpu": 2, "cpu": 0.05, "maxmem": 2147483648, "mem": 1073741824},
		// A template must not appear as a VM.
		{"id": "qemu/900", "type": "qemu", "node": "pve1", "name": "ubuntu-template", "vmid": 900,
			"status": "stopped", "template": 1, "maxcpu": 2, "maxmem": 2147483648},
		{"id": "node/pve1", "type": "node", "node": "pve1", "status": "online", "maxcpu": 16,
			"cpu": 0.35, "maxmem": 68719476736, "mem": 34359738368},
		{"id": "node/pve2", "type": "node", "node": "pve2", "status": "online", "maxcpu": 32,
			"cpu": 0.1, "maxmem": 137438953472, "mem": 17179869184},
		{"id": "storage/pve1/local", "type": "storage", "node": "pve1", "storage": "local",
			"plugintype": "dir", "maxdisk": 107374182400, "disk": 21474836480, "shared": 0, "status": "available"},
		// Shared storage is reported once per node and must collapse to one record.
		{"id": "storage/pve1/ceph", "type": "storage", "node": "pve1", "storage": "ceph",
			"plugintype": "rbd", "maxdisk": 10995116277760, "disk": 3298534883328, "shared": 1},
		{"id": "storage/pve2/ceph", "type": "storage", "node": "pve2", "storage": "ceph",
			"plugintype": "rbd", "maxdisk": 10995116277760, "disk": 3298534883328, "shared": 1},
	}
}

func rrdFixture() []map[string]any {
	now := time.Now().Unix()
	out := make([]map[string]any, 0, 24)
	for i := 24; i > 0; i-- {
		out = append(out, map[string]any{
			"time": now - int64(i)*3600, "cpu": 0.2, "maxcpu": 4,
			"mem": 4294967296, "maxmem": 8589934592,
		})
	}
	// RRD pads unpopulated slots with zeroes; those must not become samples.
	out = append(out, map[string]any{"time": now, "cpu": 0, "mem": 0, "maxmem": 0})
	return out
}

func newConnector(t *testing.T, f *fakePVE) connector.Connector {
	t.Helper()
	c, err := proxmox.New(
		connector.Config{Endpoint: f.URL},
		connector.Credentials{Kind: "api_token", TokenID: testToken, Secret: testSecret},
		connector.Options{},
	)
	if err != nil {
		t.Fatalf("build connector: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestConformance runs the shared contract suite against the Proxmox connector,
// exactly as the mock connector does.
func TestConformance(t *testing.T) {
	connectortest.Run(t, connectortest.Config{
		New: func(t *testing.T) connector.Connector {
			return newConnector(t, newFakePVE(t))
		},
		SampleVM: connector.VMRef{ExternalID: "100", HostID: "pve1", Type: "qemu"},
	})
}

func TestRegisteredInRegistry(t *testing.T) {
	if !connector.IsRegistered(proxmox.Type) {
		t.Fatal("proxmox connector did not register itself")
	}
}

func TestListVMsMapsFixture(t *testing.T) {
	f := newFakePVE(t)
	c := newConnector(t, f)

	vms, err := c.(connector.VirtualMachineCollector).ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 3 {
		t.Fatalf("got %d VMs, want 3 (templates excluded)", len(vms))
	}

	byID := map[string]connector.VMRecord{}
	for _, vm := range vms {
		byID[vm.ExternalID] = vm
	}

	web := byID["100"]
	if web.Name != "web-01" || web.State != "running" || web.HostID != "pve1" {
		t.Errorf("VM 100 = %+v, want web-01/running/pve1", web)
	}
	if web.CPUCores != 4 || web.MemoryBytes != 8589934592 {
		t.Errorf("VM 100 sizing = %d cores / %d bytes", web.CPUCores, web.MemoryBytes)
	}
	if want := []string{"prod", "web"}; strings.Join(web.Tags, ",") != strings.Join(want, ",") {
		t.Errorf("tags = %v, want %v", web.Tags, want)
	}
	if web.Pool != "production" {
		t.Errorf("pool = %q, want production (VM group auto-rules depend on it)", web.Pool)
	}
	// Loopback and link-local addresses are noise, not inventory.
	if len(web.IPAddresses) != 1 || web.IPAddresses[0] != "10.0.30.50" {
		t.Errorf("IPs = %v, want only 10.0.30.50", web.IPAddresses)
	}

	if byID["101"].State != "stopped" {
		t.Errorf("VM 101 state = %q, want stopped", byID["101"].State)
	}
	if byID["200"].Type != "lxc" {
		t.Errorf("container 200 type = %q, want lxc", byID["200"].Type)
	}
	if _, found := byID["900"]; found {
		t.Error("template 900 was returned as a VM")
	}
}

func TestStoppedVMsSkipAgentLookup(t *testing.T) {
	f := newFakePVE(t)
	c := newConnector(t, f)

	if _, err := c.(connector.VirtualMachineCollector).ListVMs(context.Background()); err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	// Only the two running guests may be probed, and LXC has no qemu agent:
	// exactly one agent call.
	if got := f.requestCount("GET /nodes/pve1/qemu/100/agent"); got != 1 {
		t.Errorf("agent calls for running qemu VM = %d, want 1", got)
	}
	if f.sawRequest("GET /nodes/pve2/qemu/101/agent") {
		t.Error("agent was queried for a stopped VM")
	}
}

func TestSharedStorageCollapsesToOneRecord(t *testing.T) {
	c := newConnector(t, newFakePVE(t))

	pools, err := c.(connector.StorageCollector).ListStoragePools(context.Background())
	if err != nil {
		t.Fatalf("ListStoragePools: %v", err)
	}

	ceph := 0
	for _, p := range pools {
		if p.Name == "ceph" {
			ceph++
			if p.HostID != "" {
				t.Errorf("shared pool bound to host %q; it belongs to the cluster", p.HostID)
			}
			if !p.IsShared {
				t.Error("ceph pool not marked shared")
			}
		}
	}
	if ceph != 1 {
		t.Errorf("ceph appeared %d times, want 1 despite being reported per node", ceph)
	}
}

func TestListHostsAndNetworks(t *testing.T) {
	c := newConnector(t, newFakePVE(t))
	ctx := context.Background()

	hosts, err := c.(connector.HostCollector).ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if len(hosts) != 2 || hosts[0].ExternalID != "pve1" {
		t.Fatalf("hosts = %+v, want pve1 and pve2 sorted", hosts)
	}
	if hosts[0].Status != "online" || hosts[0].CPUCores != 16 {
		t.Errorf("host pve1 = %+v", hosts[0])
	}

	nets, err := c.(connector.NetworkCollector).ListNetworks(ctx)
	if err != nil {
		t.Fatalf("ListNetworks: %v", err)
	}
	// Two interfaces on each of two nodes, keyed per host so vmbr0 on pve1 is
	// distinct from vmbr0 on pve2.
	if len(nets) != 4 {
		t.Fatalf("got %d networks, want 4", len(nets))
	}
	keys := map[string]bool{}
	for _, n := range nets {
		if keys[n.NaturalKey()] {
			t.Errorf("duplicate network key %q", n.NaturalKey())
		}
		keys[n.NaturalKey()] = true
	}
}

func TestTestConnectionReportsVersionAndPrivileges(t *testing.T) {
	f := newFakePVE(t)
	c := newConnector(t, f)

	report, err := c.TestConnection(context.Background())
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !report.Reachable || !report.Authenticated {
		t.Fatalf("report = %+v, want reachable and authenticated", report)
	}
	if !strings.Contains(report.Version, "8.2") {
		t.Errorf("version = %q, want the platform version", report.Version)
	}
	if report.NodeCount != 2 {
		t.Errorf("node count = %d, want 2", report.NodeCount)
	}
	if len(report.MissingPermissions) != 0 {
		t.Errorf("missing permissions = %v, want none for a fully privileged token", report.MissingPermissions)
	}
}

// A half-privileged token is the most common registration mistake, so the
// report must name what is missing rather than failing vaguely later.
func TestTestConnectionNamesMissingPrivileges(t *testing.T) {
	f := newFakePVE(t)
	f.setPermissions(map[string]map[string]int{
		"/": {"VM.Audit": 1, "Sys.Audit": 1, "Datastore.Audit": 1},
	})
	c := newConnector(t, f)

	report, err := c.TestConnection(context.Background())
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	joined := strings.Join(report.MissingPermissions, " ")
	if !strings.Contains(joined, "VM.Console") {
		t.Errorf("missing permissions = %v, want VM.Console named", report.MissingPermissions)
	}
	if !strings.Contains(joined, "opening consoles") {
		t.Error("missing permission does not explain the consequence")
	}
	if len(report.Warnings) == 0 {
		t.Error("reduced functionality was not warned about")
	}
}

func TestErrorClassificationFromStatusCodes(t *testing.T) {
	tests := []struct {
		name   string
		status int
		class  error
	}{
		{"unauthorized", http.StatusUnauthorized, connector.ErrAuth},
		{"forbidden", http.StatusForbidden, connector.ErrPermission},
		{"rate limited", http.StatusTooManyRequests, connector.ErrThrottled},
		{"server error", http.StatusInternalServerError, connector.ErrUnreachable},
		{"not found", http.StatusNotFound, connector.ErrNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePVE(t)
			f.forceStatus("/cluster/resources", tt.status)
			c := newConnector(t, f)

			_, err := c.(connector.VirtualMachineCollector).ListVMs(context.Background())
			if !errors.Is(err, tt.class) {
				t.Fatalf("HTTP %d produced %v, want class %v", tt.status, err, tt.class)
			}
		})
	}
}

func TestAuthFailureIsNotRetryable(t *testing.T) {
	f := newFakePVE(t)
	f.forceStatus("/cluster/resources", http.StatusUnauthorized)
	c := newConnector(t, f)

	_, err := c.(connector.VirtualMachineCollector).ListVMs(context.Background())
	if connector.Retryable(err) {
		t.Error("a rejected token is reported as retryable; the sync engine would hammer the cluster")
	}
}

func TestHealthDegradesOnLostQuorum(t *testing.T) {
	f := newFakePVE(t)
	c := newConnector(t, f)

	report, err := c.(connector.HealthCollector).Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if report.State != connector.HealthHealthy {
		t.Fatalf("state = %q, want healthy", report.State)
	}

	f.forceStatus("/version", http.StatusInternalServerError)
	report, err = c.(connector.HealthCollector).Health(context.Background())
	if err != nil {
		t.Fatalf("Health after failure returned an error instead of a state: %v", err)
	}
	if report.State != connector.HealthUnreachable {
		t.Errorf("state = %q, want unreachable", report.State)
	}
}

func TestMetricsFromSingleClusterCall(t *testing.T) {
	f := newFakePVE(t)
	c := newConnector(t, f)

	samples, err := c.(connector.MetricsCollector).CollectMetrics(context.Background(), connector.MetricScope{})
	if err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}
	if got := f.requestCount("GET /cluster/resources"); got != 1 {
		t.Errorf("cluster/resources called %d times; a metrics cycle should be one call", got)
	}

	var cpuForVM100 *connector.Sample
	counters := 0
	for i := range samples {
		s := samples[i]
		if s.Subject == connector.SubjectVM && s.SubjectID == "100" && s.Kind == connector.MetricCPUPct {
			cpuForVM100 = &samples[i]
		}
		if s.Cumulative {
			counters++
		}
	}
	if cpuForVM100 == nil {
		t.Fatal("no CPU sample for the running VM")
	}
	// Proxmox reports CPU as a fraction of allocated cores.
	if got := cpuForVM100.Value; got < 11.9 || got > 12.1 {
		t.Errorf("cpu_pct = %v, want ~12 (0.12 scaled to percent)", got)
	}
	if counters == 0 {
		t.Error("no counters marked cumulative; the engine cannot compute rates safely")
	}

	// Stopped VMs would otherwise contribute a misleading flat zero series.
	for _, s := range samples {
		if s.SubjectID == "101" {
			t.Error("metrics were emitted for a stopped VM")
		}
	}
}

func TestBackfillSkipsEmptyRRDSlots(t *testing.T) {
	c := newConnector(t, newFakePVE(t))

	samples, err := c.(connector.MetricsBackfiller).BackfillMetrics(context.Background(),
		connector.VMRef{ExternalID: "100", HostID: "pve1", Type: "qemu"},
		time.Now().Add(-25*time.Hour))
	if err != nil {
		t.Fatalf("BackfillMetrics: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no history returned")
	}
	for _, s := range samples {
		if s.Kind == connector.MetricMemTotalBytes && s.Value == 0 {
			t.Error("an unpopulated RRD slot was stored as a real measurement")
		}
	}
}

func TestConsoleTicketFlow(t *testing.T) {
	f := newFakePVE(t)
	c := newConnector(t, f)

	endpoint, err := c.(connector.ConsoleProvider).CreateConsoleSession(context.Background(),
		connector.VMRef{ExternalID: "100", HostID: "pve1", Type: "qemu"}, connector.ConsoleVNC)
	if err != nil {
		t.Fatalf("CreateConsoleSession: %v", err)
	}
	if !f.sawRequest("POST /nodes/pve1/qemu/100/vncproxy") {
		t.Error("vncproxy was not requested")
	}
	if !endpoint.ExpiresAt().After(time.Now()) {
		t.Error("console endpoint is already expired")
	}
	if endpoint.ExpiresAt().After(time.Now().Add(2 * time.Minute)) {
		t.Error("console ticket lifetime is too generous; tickets are single-session")
	}

	conn, err := endpoint.DialContext(context.Background())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	conn.Close()
}

func TestConsoleRequiresNodeAndVMID(t *testing.T) {
	c := newConnector(t, newFakePVE(t))

	_, err := c.(connector.ConsoleProvider).CreateConsoleSession(context.Background(),
		connector.VMRef{ExternalID: "100"}, connector.ConsoleVNC)
	if !errors.Is(err, connector.ErrInvalidConfig) {
		t.Errorf("error = %v, want ErrInvalidConfig when the node is unknown", err)
	}
}

func TestPowerActionsReturnTaskRef(t *testing.T) {
	f := newFakePVE(t)
	c := newConnector(t, f)
	vm := connector.VMRef{ExternalID: "100", HostID: "pve1", Type: "qemu"}

	task, err := c.(connector.PowerManager).Power(context.Background(), vm, connector.PowerStart)
	if err != nil {
		t.Fatalf("Power(start): %v", err)
	}
	if !strings.HasPrefix(task.ID, "UPID:") || task.Node != "pve1" {
		t.Errorf("task = %+v, want a UPID on pve1", task)
	}
	if !f.sawRequest("POST /nodes/pve1/qemu/100/status/start") {
		t.Error("start was not requested on the node")
	}

	if _, err := c.(connector.PowerManager).Power(context.Background(), vm, connector.PowerAction("selfdestruct")); !errors.Is(err, connector.ErrNotSupported) {
		t.Errorf("unknown action error = %v, want ErrNotSupported", err)
	}
}

func TestCredentialValidation(t *testing.T) {
	tests := []struct {
		name  string
		creds connector.Credentials
	}{
		{"missing secret", connector.Credentials{Kind: "api_token", TokenID: testToken}},
		{"missing token id", connector.Credentials{Kind: "api_token", Secret: testSecret}},
		{"malformed token id", connector.Credentials{Kind: "api_token", TokenID: "roottoken", Secret: testSecret}},
		{"unsupported kind", connector.Credentials{Kind: "certificate", TokenID: testToken, Secret: testSecret}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := proxmox.New(connector.Config{Endpoint: "https://10.0.0.1:8006"}, tt.creds, connector.Options{})
			if !errors.Is(err, connector.ErrInvalidConfig) {
				t.Errorf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestTLSPolicyValidation(t *testing.T) {
	creds := connector.Credentials{Kind: "api_token", TokenID: testToken, Secret: testSecret}
	tests := []struct {
		name    string
		tls     connector.TLSPolicy
		wantErr bool
	}{
		{"default verify", connector.TLSPolicy{}, false},
		{"insecure allowed", connector.TLSPolicy{Mode: connector.TLSInsecure}, false},
		{"valid fingerprint", connector.TLSPolicy{Mode: connector.TLSFingerprint,
			Fingerprint: "40:40:C5:AC:7F:A1:E6:FE:0A:98:EF:CB:56:A2:D8:F3:0D:6A:78:6B:38:62:36:D8:2D:85:0B:D7:4B:35:E5:AD"}, false},
		{"short fingerprint", connector.TLSPolicy{Mode: connector.TLSFingerprint, Fingerprint: "AB:CD"}, true},
		{"custom CA without bundle", connector.TLSPolicy{Mode: connector.TLSCustomCA}, true},
		{"unknown mode", connector.TLSPolicy{Mode: connector.TLSMode("whatever")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := proxmox.New(connector.Config{Endpoint: "https://10.0.0.1:8006", TLS: tt.tls}, creds, connector.Options{})
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// A pinned fingerprint that does not match must fail closed, otherwise pinning
// would be decoration rather than protection.
func TestFingerprintMismatchIsRejected(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeData(w, map[string]any{"version": "8.2.4"})
	}))
	defer tlsServer.Close()

	wrong := strings.Repeat("ab", 32)
	c, err := proxmox.New(
		connector.Config{Endpoint: tlsServer.URL, TLS: connector.TLSPolicy{Mode: connector.TLSFingerprint, Fingerprint: wrong}},
		connector.Credentials{Kind: "api_token", TokenID: testToken, Secret: testSecret},
		connector.Options{},
	)
	if err != nil {
		t.Fatalf("build connector: %v", err)
	}
	defer c.Close()

	_, err = c.TestConnection(context.Background())
	if err == nil {
		t.Fatal("connection succeeded despite a mismatched pinned certificate")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("error = %v, want a fingerprint mismatch explanation", err)
	}
}

func TestEndpointNormalization(t *testing.T) {
	creds := connector.Credentials{Kind: "api_token", TokenID: testToken, Secret: testSecret}
	// A bare host should become https on the Proxmox port rather than failing.
	if _, err := proxmox.New(connector.Config{Endpoint: "10.0.30.111"}, creds, connector.Options{}); err != nil {
		t.Errorf("bare host endpoint rejected: %v", err)
	}
	if _, err := proxmox.New(connector.Config{Endpoint: "ftp://10.0.30.111"}, creds, connector.Options{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Errorf("non-http scheme error = %v, want ErrInvalidConfig", err)
	}
}

func TestSecretNeverAppearsInErrors(t *testing.T) {
	f := newFakePVE(t)
	f.forceStatus("/cluster/resources", http.StatusUnauthorized)
	c := newConnector(t, f)

	_, err := c.(connector.VirtualMachineCollector).ListVMs(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Error("the token secret leaked into an error message")
	}
	if strings.Contains(fmt.Sprintf("%+v", err), testSecret) {
		t.Error("the token secret leaked into the verbose error rendering")
	}
}
