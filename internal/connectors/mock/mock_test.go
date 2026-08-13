package mock_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/connector/connectortest"
	"github.com/freezxp/proxui/internal/connectors/mock"
)

func newMock(t *testing.T, extra map[string]any) connector.Connector {
	t.Helper()
	c, err := mock.New(connector.Config{Endpoint: "mock://local", Extra: extra}, connector.Credentials{}, connector.Options{})
	if err != nil {
		t.Fatalf("build mock connector: %v", err)
	}
	return c
}

// TestConformance is the contract check every connector must pass. Running it
// here proves the framework's rules are satisfiable and gives the Proxmox
// connector a worked example to match.
func TestConformance(t *testing.T) {
	connectortest.Run(t, connectortest.Config{
		New: func(t *testing.T) connector.Connector {
			return newMock(t, nil)
		},
		SampleVM: connector.VMRef{ExternalID: "100", Type: "qemu"},
	})
}

func TestRegisteredInRegistry(t *testing.T) {
	if !connector.IsRegistered(mock.Type) {
		t.Fatal("mock connector did not register itself via init()")
	}
	c, err := connector.New(mock.Type, connector.Config{}, connector.Credentials{}, connector.Options{})
	if err != nil {
		t.Fatalf("connector.New: %v", err)
	}
	defer c.Close()
	if c.Info().Type != mock.Type {
		t.Errorf("Info().Type = %q, want %q", c.Info().Type, mock.Type)
	}
}

func TestFleetSizeIsConfigurable(t *testing.T) {
	c := newMock(t, map[string]any{"vm_count": 50, "host_count": 5})
	defer c.Close()

	vms, err := c.(connector.VirtualMachineCollector).ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(vms) != 50 {
		t.Errorf("got %d VMs, want 50", len(vms))
	}

	hosts, err := c.(connector.HostCollector).ListHosts(context.Background())
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if len(hosts) != 5 {
		t.Errorf("got %d hosts, want 5", len(hosts))
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]any
	}{
		{"unknown option", map[string]any{"nonsense": 1}},
		{"negative vm count", map[string]any{"vm_count": -5}},
		{"mutation rate above one", map[string]any{"mutation_rate": 2.0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mock.New(connector.Config{Extra: tt.extra}, connector.Credentials{}, connector.Options{})
			if !errors.Is(err, connector.ErrInvalidConfig) {
				t.Errorf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// TestFaultClassification is why the mock exists: the sync engine's retry and
// circuit-breaker decisions depend on these classes being right.
func TestFaultClassification(t *testing.T) {
	tests := []struct {
		fault     mock.Fault
		class     error
		retryable bool
	}{
		{mock.FaultAuth, connector.ErrAuth, false},
		{mock.FaultUnreachable, connector.ErrUnreachable, true},
		{mock.FaultThrottled, connector.ErrThrottled, true},
		{mock.FaultPermission, connector.ErrPermission, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.fault), func(t *testing.T) {
			c := newMock(t, map[string]any{"fault": string(tt.fault)})
			defer c.Close()

			_, err := c.(connector.VirtualMachineCollector).ListVMs(context.Background())
			if !errors.Is(err, tt.class) {
				t.Fatalf("error = %v, want class %v", err, tt.class)
			}
			if got := connector.Retryable(err); got != tt.retryable {
				t.Errorf("Retryable(%v) = %v, want %v", tt.class, got, tt.retryable)
			}
		})
	}
}

func TestSlowFaultRespectsDeadline(t *testing.T) {
	c := newMock(t, map[string]any{"fault": string(mock.FaultSlow)})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.(connector.VirtualMachineCollector).ListVMs(ctx)
	if err == nil {
		t.Fatal("a slow platform did not produce an error before the deadline")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("call took %v; the connector ignored the context deadline", elapsed)
	}
}

func TestHealthReportsUnreachableInsteadOfFailing(t *testing.T) {
	c := newMock(t, map[string]any{"fault": string(mock.FaultUnreachable)})
	defer c.Close()

	report, err := c.(connector.HealthCollector).Health(context.Background())
	if err != nil {
		t.Fatalf("Health returned an error instead of a state: %v", err)
	}
	if report.State != connector.HealthUnreachable {
		t.Errorf("State = %q, want unreachable", report.State)
	}
}

func TestMutationDrivesChangeDetection(t *testing.T) {
	c := newMock(t, map[string]any{"vm_count": 20, "mutation_rate": 0.5})
	defer c.Close()
	vc := c.(connector.VirtualMachineCollector)
	ctx := context.Background()

	first, err := vc.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	second, err := vc.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}

	changed := 0
	before := map[string]string{}
	for _, vm := range first {
		before[vm.ExternalID] = vm.State
	}
	for _, vm := range second {
		if before[vm.ExternalID] != vm.State {
			changed++
		}
	}
	if changed == 0 {
		t.Error("mutation_rate 0.5 produced no state changes; change detection has nothing to detect")
	}
}

func TestRemovedVMDisappears(t *testing.T) {
	c := newMock(t, nil)
	defer c.Close()
	m := c.(*mock.Connector)
	ctx := context.Background()

	before, err := m.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	m.RemoveVM(before[0].ExternalID)

	after, err := m.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(after) != len(before)-1 {
		t.Fatalf("got %d VMs after removal, want %d", len(after), len(before)-1)
	}
	for _, vm := range after {
		if vm.ExternalID == before[0].ExternalID {
			t.Error("removed VM is still listed")
		}
	}
}

func TestPowerActionsChangeState(t *testing.T) {
	c := newMock(t, nil)
	defer c.Close()
	m := c.(*mock.Connector)
	ctx := context.Background()

	vms, _ := m.ListVMs(ctx)
	target := connector.VMRef{ExternalID: vms[0].ExternalID, Type: "qemu"}

	if _, err := m.Power(ctx, target, connector.PowerStop); err != nil {
		t.Fatalf("Power(stop): %v", err)
	}
	after, _ := m.ListVMs(ctx)
	for _, vm := range after {
		if vm.ExternalID == target.ExternalID && vm.State != "stopped" {
			t.Errorf("state = %q after stop, want stopped", vm.State)
		}
	}

	if _, err := m.Power(ctx, target, connector.PowerAction("explode")); !errors.Is(err, connector.ErrNotSupported) {
		t.Errorf("unknown action error = %v, want ErrNotSupported", err)
	}
}

// TestConsoleEndpointPipesBytes proves the console contract the WebSocket proxy
// relies on: dial the endpoint, exchange bytes, protocol-agnostically.
func TestConsoleEndpointPipesBytes(t *testing.T) {
	c := newMock(t, nil)
	defer c.Close()
	ctx := context.Background()

	endpoint, err := c.(connector.ConsoleProvider).CreateConsoleSession(ctx,
		connector.VMRef{ExternalID: "100", Type: "qemu"}, connector.ConsoleVNC)
	if err != nil {
		t.Fatalf("CreateConsoleSession: %v", err)
	}
	if !endpoint.ExpiresAt().After(time.Now()) {
		t.Error("console endpoint is already expired")
	}

	conn, err := endpoint.DialContext(ctx)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()

	want := []byte("RFB 003.008\n")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("echoed %q, want %q", got, want)
	}
}

func TestMetricsCoverRunningVMs(t *testing.T) {
	c := newMock(t, map[string]any{"vm_count": 10})
	defer c.Close()

	samples, err := c.(connector.MetricsCollector).CollectMetrics(context.Background(), connector.MetricScope{})
	if err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("no samples collected")
	}
	kinds := map[connector.MetricKind]bool{}
	for _, s := range samples {
		kinds[s.Kind] = true
		if s.Time.IsZero() || s.SubjectID == "" {
			t.Errorf("malformed sample: %+v", s)
		}
	}
	for _, want := range []connector.MetricKind{connector.MetricCPUPct, connector.MetricMemUsedBytes} {
		if !kinds[want] {
			t.Errorf("no %q samples were produced", want)
		}
	}
}

func TestBackfillProducesHistory(t *testing.T) {
	c := newMock(t, nil)
	defer c.Close()

	from := time.Now().Add(-48 * time.Hour)
	samples, err := c.(connector.MetricsBackfiller).BackfillMetrics(context.Background(),
		connector.VMRef{ExternalID: "100"}, from)
	if err != nil {
		t.Fatalf("BackfillMetrics: %v", err)
	}
	if len(samples) < 40 {
		t.Errorf("got %d backfilled samples for 48h, want roughly hourly coverage", len(samples))
	}
	for _, s := range samples {
		if s.Time.Before(from.Add(-time.Hour)) {
			t.Errorf("sample %v predates the requested start %v", s.Time, from)
		}
	}
}
