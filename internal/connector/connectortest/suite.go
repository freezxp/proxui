// Package connectortest is the shared conformance suite every connector must
// pass. It encodes the contract prose in docs/09-connector-architecture.md as
// executable checks, so a new platform integration is verified against the same
// rules as the built-in ones.
package connectortest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/connector"
)

// Config parameterises a conformance run.
type Config struct {
	// New builds a connector instance under test. It is called per subtest so
	// state cannot leak between checks.
	New func(t *testing.T) connector.Connector

	// SampleVM is a VM the connector can act on, for console and power checks.
	// Leave zero to skip those checks.
	SampleVM connector.VMRef
}

// Run executes the full conformance suite.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.New == nil {
		t.Fatal("connectortest: Config.New is required")
	}

	t.Run("Info", func(t *testing.T) { testInfo(t, cfg) })
	t.Run("CapabilitiesMatchInterfaces", func(t *testing.T) { testCapabilities(t, cfg) })
	t.Run("ListsAreIdempotent", func(t *testing.T) { testIdempotentLists(t, cfg) })
	t.Run("NaturalKeysAreUnique", func(t *testing.T) { testNaturalKeys(t, cfg) })
	t.Run("FingerprintsAreStable", func(t *testing.T) { testFingerprints(t, cfg) })
	t.Run("ContextCancellationIsHonoured", func(t *testing.T) { testCancellation(t, cfg) })
	t.Run("ErrorsAreClassified", func(t *testing.T) { testErrorClasses(t, cfg) })
	t.Run("HealthIsCheap", func(t *testing.T) { testHealth(t, cfg) })
	t.Run("CloseIsIdempotent", func(t *testing.T) { testClose(t, cfg) })
}

func testInfo(t *testing.T, cfg Config) {
	c := cfg.New(t)
	defer c.Close()

	info := c.Info()
	if info.Type == "" {
		t.Error("Info().Type is empty; the registry key must be stable")
	}
	if info.DisplayName == "" {
		t.Error("Info().DisplayName is empty; the UI has nothing to show")
	}
}

// testCapabilities is the heart of the contract: a connector that claims a
// capability must implement the matching interface, and vice versa. Otherwise
// the core would offer buttons that cannot work.
func testCapabilities(t *testing.T, cfg Config) {
	c := cfg.New(t)
	defer c.Close()

	checks := []struct {
		capability  connector.Capability
		implemented bool
	}{
		{connector.CapabilityVM, isVMCollector(c)},
		{connector.CapabilityHost, isHostCollector(c)},
		{connector.CapabilityStorage, isStorageCollector(c)},
		{connector.CapabilityNetwork, isNetworkCollector(c)},
		{connector.CapabilityMetrics, isMetricsCollector(c)},
		{connector.CapabilityMetricsBackfill, isBackfiller(c)},
		{connector.CapabilityPower, isPowerManager(c)},
		{connector.CapabilityProvision, isProvisioner(c)},
		{connector.CapabilityDestroy, isDestroyer(c)},
		{connector.CapabilityTemplateBuild, isTemplateBuilder(c)},
	}
	for _, check := range checks {
		declared := connector.Supports(c, check.capability)
		if declared && !check.implemented {
			t.Errorf("capability %q is declared but the interface is not implemented", check.capability)
		}
		if !declared && check.implemented {
			t.Errorf("interface for %q is implemented but the capability is not declared", check.capability)
		}
	}

	// Console has two capability flags over one interface.
	console := isConsoleProvider(c)
	declaresConsole := connector.Supports(c, connector.CapabilityConsole) ||
		connector.Supports(c, connector.CapabilitySerialConsole)
	if declaresConsole != console {
		t.Errorf("console capability declared=%v but ConsoleProvider implemented=%v", declaresConsole, console)
	}
}

// testIdempotentLists checks that listing twice without platform changes gives
// the same result. The reconciler assumes this; a connector that returns
// unstable ordering or synthesised ids would produce phantom change history.
func testIdempotentLists(t *testing.T, cfg Config) {
	c := cfg.New(t)
	defer c.Close()
	ctx := context.Background()

	if vc, ok := c.(connector.VirtualMachineCollector); ok {
		first, err := vc.ListVMs(ctx)
		if err != nil {
			t.Fatalf("ListVMs: %v", err)
		}
		second, err := vc.ListVMs(ctx)
		if err != nil {
			t.Fatalf("ListVMs (second call): %v", err)
		}
		if len(first) != len(second) {
			t.Fatalf("ListVMs returned %d then %d records without platform changes", len(first), len(second))
		}
		firstByKey := map[string][]byte{}
		for _, r := range first {
			firstByKey[r.NaturalKey()] = r.Fingerprint()
		}
		for _, r := range second {
			want, ok := firstByKey[r.NaturalKey()]
			if !ok {
				t.Errorf("VM %q appeared only in the second listing", r.NaturalKey())
				continue
			}
			if string(want) != string(r.Fingerprint()) {
				t.Errorf("VM %q changed fingerprint between identical listings", r.NaturalKey())
			}
		}
	}

	if hc, ok := c.(connector.HostCollector); ok {
		if _, err := hc.ListHosts(ctx); err != nil {
			t.Errorf("ListHosts: %v", err)
		}
	}
	if sc, ok := c.(connector.StorageCollector); ok {
		if _, err := sc.ListStoragePools(ctx); err != nil {
			t.Errorf("ListStoragePools: %v", err)
		}
	}
	if nc, ok := c.(connector.NetworkCollector); ok {
		if _, err := nc.ListNetworks(ctx); err != nil {
			t.Errorf("ListNetworks: %v", err)
		}
	}
}

// testNaturalKeys enforces that identity is unique and non-empty: the whole
// upsert model rests on (platform, natural key).
func testNaturalKeys(t *testing.T, cfg Config) {
	c := cfg.New(t)
	defer c.Close()
	ctx := context.Background()

	if vc, ok := c.(connector.VirtualMachineCollector); ok {
		vms, err := vc.ListVMs(ctx)
		if err != nil {
			t.Fatalf("ListVMs: %v", err)
		}
		seen := map[string]bool{}
		for _, r := range vms {
			key := r.NaturalKey()
			if key == "" {
				t.Errorf("VM %q has an empty natural key", r.Name)
			}
			if seen[key] {
				t.Errorf("duplicate VM natural key %q", key)
			}
			seen[key] = true
			if r.Name == "" {
				t.Errorf("VM %q has an empty name", key)
			}
		}
	}

	if hc, ok := c.(connector.HostCollector); ok {
		hosts, err := hc.ListHosts(ctx)
		if err != nil {
			t.Fatalf("ListHosts: %v", err)
		}
		seen := map[string]bool{}
		for _, r := range hosts {
			key := r.NaturalKey()
			if key == "" {
				t.Errorf("host %q has an empty natural key", r.Name)
			}
			if seen[key] {
				t.Errorf("duplicate host natural key %q", key)
			}
			seen[key] = true
		}
	}
}

// testFingerprints checks that volatile fields stay out of the fingerprint.
// If uptime changed the fingerprint, every sync would write change history for
// every VM forever.
func testFingerprints(t *testing.T, cfg Config) {
	c := cfg.New(t)
	defer c.Close()

	vc, ok := c.(connector.VirtualMachineCollector)
	if !ok {
		t.Skip("connector does not collect VMs")
	}
	vms, err := vc.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) == 0 {
		t.Skip("no VMs to compare")
	}

	original := vms[0]
	bumped := original
	bumped.UptimeS = original.UptimeS + 3600
	if string(original.Fingerprint()) != string(bumped.Fingerprint()) {
		t.Error("uptime changes the fingerprint; every sync would record spurious changes")
	}

	renamed := original
	renamed.Name = original.Name + "-renamed"
	if string(original.Fingerprint()) == string(renamed.Fingerprint()) {
		t.Error("renaming a VM does not change the fingerprint; real changes would be missed")
	}

	restarted := original
	if restarted.State == "running" {
		restarted.State = "stopped"
	} else {
		restarted.State = "running"
	}
	if string(original.Fingerprint()) == string(restarted.Fingerprint()) {
		t.Error("state changes do not affect the fingerprint; state history would never fire")
	}
}

func testCancellation(t *testing.T, cfg Config) {
	c := cfg.New(t)
	defer c.Close()

	vc, ok := c.(connector.VirtualMachineCollector)
	if !ok {
		t.Skip("connector does not collect VMs")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := vc.ListVMs(ctx); err == nil {
		t.Error("ListVMs ignored a cancelled context; the sync engine could not enforce timeouts")
	}
}

// testErrorClasses checks that failures carry a class the engine can act on.
func testErrorClasses(t *testing.T, cfg Config) {
	c := cfg.New(t)
	defer c.Close()

	err := c.ValidateConfig(connector.Config{})
	if err == nil {
		return // a connector may legitimately accept empty configuration
	}
	if !errors.Is(err, connector.ErrInvalidConfig) {
		t.Errorf("ValidateConfig error %v is not classified as ErrInvalidConfig", err)
	}
}

func testHealth(t *testing.T, cfg Config) {
	c := cfg.New(t)
	defer c.Close()

	hc, ok := c.(connector.HealthCollector)
	if !ok {
		t.Skip("connector does not report health")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	report, err := hc.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	switch report.State {
	case connector.HealthHealthy, connector.HealthDegraded,
		connector.HealthUnreachable, connector.HealthUnknown:
	default:
		t.Errorf("Health returned unknown state %q", report.State)
	}
}

func testClose(t *testing.T, cfg Config) {
	c := cfg.New(t)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v; Close must be safe to call twice", err)
	}
}

func isVMCollector(c connector.Connector) bool {
	_, ok := c.(connector.VirtualMachineCollector)
	return ok
}
func isHostCollector(c connector.Connector) bool {
	_, ok := c.(connector.HostCollector)
	return ok
}
func isStorageCollector(c connector.Connector) bool {
	_, ok := c.(connector.StorageCollector)
	return ok
}
func isNetworkCollector(c connector.Connector) bool {
	_, ok := c.(connector.NetworkCollector)
	return ok
}
func isMetricsCollector(c connector.Connector) bool {
	_, ok := c.(connector.MetricsCollector)
	return ok
}
func isBackfiller(c connector.Connector) bool {
	_, ok := c.(connector.MetricsBackfiller)
	return ok
}
func isConsoleProvider(c connector.Connector) bool {
	_, ok := c.(connector.ConsoleProvider)
	return ok
}
func isProvisioner(c connector.Connector) bool {
	_, ok := c.(connector.Provisioner)
	return ok
}

func isTemplateBuilder(c connector.Connector) bool {
	_, ok := c.(connector.TemplateBuilder)
	return ok
}

func isDestroyer(c connector.Connector) bool {
	_, ok := c.(connector.Destroyer)
	return ok
}

func isPowerManager(c connector.Connector) bool {
	_, ok := c.(connector.PowerManager)
	return ok
}
