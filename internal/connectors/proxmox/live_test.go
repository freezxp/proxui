package proxmox_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/connector/connectortest"
	"github.com/freezxp/proxui/internal/connectors/proxmox"
)

// Live tests run against a real Proxmox cluster and are skipped unless
// credentials are supplied. CI never sets them: the fixture tests above are the
// hermetic gate, and this is the optional check against a lab cluster
// (docs/18-testing-strategy.md §18.2).
//
// Credentials come from the environment so no secret is ever written into the
// repository or a test file:
//
//	PROXUI_LIVE_PVE_URL=https://10.0.30.111:8006
//	PROXUI_LIVE_PVE_TOKEN_ID=proxui@pve!portal
//	PROXUI_LIVE_PVE_SECRET=...
//	PROXUI_LIVE_PVE_FINGERPRINT=40:40:...   (optional; pin instead of trusting)
//
// Every call here is read-only. Nothing is created, changed or powered.
func liveConnector(t *testing.T) connector.Connector {
	t.Helper()

	endpoint := os.Getenv("PROXUI_LIVE_PVE_URL")
	tokenID := os.Getenv("PROXUI_LIVE_PVE_TOKEN_ID")
	secret := os.Getenv("PROXUI_LIVE_PVE_SECRET")
	if endpoint == "" || tokenID == "" || secret == "" {
		t.Skip("live Proxmox credentials not set; skipping (set PROXUI_LIVE_PVE_URL, _TOKEN_ID and _SECRET to run)")
	}

	tls := connector.TLSPolicy{Mode: connector.TLSVerify}
	if fp := os.Getenv("PROXUI_LIVE_PVE_FINGERPRINT"); fp != "" {
		tls = connector.TLSPolicy{Mode: connector.TLSFingerprint, Fingerprint: fp}
	} else if os.Getenv("PROXUI_LIVE_PVE_INSECURE") == "true" {
		tls = connector.TLSPolicy{Mode: connector.TLSInsecure}
	}

	c, err := proxmox.New(
		connector.Config{Endpoint: endpoint, TLS: tls},
		connector.Credentials{Kind: "api_token", TokenID: tokenID, Secret: secret},
		connector.Options{Timeout: 20 * time.Second},
	)
	if err != nil {
		t.Fatalf("build connector: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestLiveTestConnection(t *testing.T) {
	c := liveConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := c.TestConnection(ctx)
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	t.Logf("reachable=%v authenticated=%v version=%q nodes=%d",
		report.Reachable, report.Authenticated, report.Version, report.NodeCount)
	for _, missing := range report.MissingPermissions {
		t.Logf("missing privilege: %s", missing)
	}
	for _, warning := range report.Warnings {
		t.Logf("warning: %s", warning)
	}

	if !report.Reachable || !report.Authenticated {
		t.Fatal("cluster did not accept the token")
	}
	if report.NodeCount == 0 {
		t.Error("no nodes reported; the token may lack Sys.Audit")
	}
}

func TestLiveInventory(t *testing.T) {
	c := liveConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	vms, err := c.(connector.VirtualMachineCollector).ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	t.Logf("%d virtual machines", len(vms))
	for i, vm := range vms {
		if i >= 10 {
			t.Logf("... and %d more", len(vms)-10)
			break
		}
		t.Logf("  vmid=%-6s %-24s %-8s node=%-10s %d vCPU %d MiB  ips=%v tags=%v",
			vm.ExternalID, vm.Name, vm.State, vm.HostID,
			vm.CPUCores, vm.MemoryBytes>>20, vm.IPAddresses, vm.Tags)
	}

	hosts, err := c.(connector.HostCollector).ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	t.Logf("%d hosts", len(hosts))
	for _, h := range hosts {
		t.Logf("  %-12s %-8s %d cores %d GiB", h.Name, h.Status, h.CPUCores, h.MemoryBytes>>30)
	}

	pools, err := c.(connector.StorageCollector).ListStoragePools(ctx)
	if err != nil {
		t.Fatalf("ListStoragePools: %v", err)
	}
	t.Logf("%d storage pools", len(pools))
	for _, p := range pools {
		shared := "local"
		if p.IsShared {
			shared = "shared"
		}
		t.Logf("  %-16s %-8s %-7s %d/%d GiB", p.Name, p.StorageType, shared,
			p.UsedBytes>>30, p.TotalBytes>>30)
	}

	nets, err := c.(connector.NetworkCollector).ListNetworks(ctx)
	if err != nil {
		t.Fatalf("ListNetworks: %v", err)
	}
	t.Logf("%d networks", len(nets))

	// Identity must be unique and stable, or upserts would collide.
	seen := map[string]bool{}
	for _, vm := range vms {
		if seen[vm.NaturalKey()] {
			t.Errorf("duplicate VM natural key %q on the real cluster", vm.NaturalKey())
		}
		seen[vm.NaturalKey()] = true
	}
}

// TestLiveConformance runs the full contract suite against the real cluster,
// proving the connector behaves the same way there as against the fixture.
func TestLiveConformance(t *testing.T) {
	if os.Getenv("PROXUI_LIVE_PVE_URL") == "" {
		t.Skip("live Proxmox credentials not set")
	}
	connectortest.Run(t, connectortest.Config{New: liveConnector})
}

func TestLiveHealthAndMetrics(t *testing.T) {
	c := liveConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	health, err := c.(connector.HealthCollector).Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	t.Logf("health=%s version=%s %s", health.State, health.Version, health.Detail)

	samples, err := c.(connector.MetricsCollector).CollectMetrics(ctx, connector.MetricScope{})
	if err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}
	t.Logf("%d samples collected", len(samples))

	kinds := map[connector.MetricKind]int{}
	for _, s := range samples {
		kinds[s.Kind]++
		if s.Time.IsZero() || s.SubjectID == "" {
			t.Errorf("malformed sample: %+v", s)
		}
		if s.Kind == connector.MetricCPUPct && (s.Value < 0 || s.Value > 100*64) {
			t.Errorf("cpu_pct %v is outside a plausible range; scaling may be wrong", s.Value)
		}
	}
	for kind, n := range kinds {
		t.Logf("  %-20s %d", kind, n)
	}
}
