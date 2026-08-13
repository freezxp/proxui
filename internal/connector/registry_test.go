package connector

import (
	"context"
	"errors"
	"testing"
)

func stubFactory(Config, Credentials, Options) (Connector, error) { return nil, nil }

func TestRegisterAndBuild(t *testing.T) {
	resetRegistry()
	t.Cleanup(resetRegistry)

	Register(Info{Type: "alpha", DisplayName: "Alpha"}, stubFactory)
	Register(Info{Type: "beta", DisplayName: "Beta"}, stubFactory)

	if !IsRegistered("alpha") || !IsRegistered("beta") {
		t.Fatal("registered types are not reported as registered")
	}
	if IsRegistered("gamma") {
		t.Error("unregistered type reported as registered")
	}

	// Sorted output keeps the API and UI listing stable.
	got := Registered()
	if len(got) != 2 || got[0].Type != "alpha" || got[1].Type != "beta" {
		t.Errorf("Registered() = %+v, want alpha then beta", got)
	}
}

func TestNewRejectsUnknownType(t *testing.T) {
	resetRegistry()
	t.Cleanup(resetRegistry)

	_, err := New("nosuchplatform", Config{}, Credentials{}, Options{})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("error = %v, want ErrInvalidConfig", err)
	}
}

// Duplicate registration means two packages claim the same platform type. That
// is a build-time mistake, so it must fail loudly at startup rather than let
// one implementation silently shadow the other.
func TestDuplicateRegistrationPanics(t *testing.T) {
	resetRegistry()
	t.Cleanup(resetRegistry)

	Register(Info{Type: "alpha"}, stubFactory)
	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate type did not panic")
		}
	}()
	Register(Info{Type: "alpha"}, stubFactory)
}

func TestRegisterRejectsIncompleteRegistration(t *testing.T) {
	tests := []struct {
		name    string
		info    Info
		factory Factory
	}{
		{"empty type", Info{}, stubFactory},
		{"nil factory", Info{Type: "alpha"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetRegistry()
			t.Cleanup(resetRegistry)
			defer func() {
				if recover() == nil {
					t.Error("invalid registration did not panic")
				}
			}()
			Register(tt.info, tt.factory)
		})
	}
}

func TestErrorClassification(t *testing.T) {
	err := Errorf(ErrAuth, "list_vms", "token rejected")

	if !errors.Is(err, ErrAuth) {
		t.Error("classified error does not match its class sentinel")
	}
	if errors.Is(err, ErrUnreachable) {
		t.Error("error matched a class it does not belong to")
	}
	if Retryable(err) {
		t.Error("auth failures must not be retried; retrying burns quota and delays the alert")
	}
	if !Retryable(Errorf(ErrUnreachable, "list_vms", "connection refused")) {
		t.Error("network failures should be retryable")
	}
	if !Retryable(Errorf(ErrThrottled, "list_vms", "429")) {
		t.Error("throttling should be retryable after backoff")
	}
}

func TestErrorUnwrapsUnderlyingCause(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	err := Wrap(ErrUnreachable, "health", cause)

	if !errors.Is(err, ErrUnreachable) {
		t.Error("wrapped error lost its class")
	}
	if !errors.Is(err, cause) {
		t.Error("wrapped error lost its cause")
	}
}

func TestFingerprintExcludesVolatileFields(t *testing.T) {
	vm := VMRecord{ExternalID: "100", Name: "web-01", State: "running", CPUCores: 4, UptimeS: 100}

	busier := vm
	busier.UptimeS = 999999
	if string(vm.Fingerprint()) != string(busier.Fingerprint()) {
		t.Error("uptime affects the VM fingerprint; every sync would log a change")
	}

	resized := vm
	resized.CPUCores = 8
	if string(vm.Fingerprint()) == string(resized.Fingerprint()) {
		t.Error("resizing CPU does not affect the fingerprint; real changes would be missed")
	}

	storage := StorageRecord{ExternalID: "s1", Name: "pool", TotalBytes: 1000, UsedBytes: 10}
	filling := storage
	filling.UsedBytes = 900
	if string(storage.Fingerprint()) != string(filling.Fingerprint()) {
		t.Error("storage consumption affects the fingerprint; it belongs in metrics, not history")
	}
}

// Length prefixing prevents adjacent fields from being confused for one
// another, e.g. a VM named "ab" on host "c" versus one named "a" on host "bc".
func TestFingerprintAvoidsFieldBoundaryCollisions(t *testing.T) {
	a := VMRecord{ExternalID: "1", Name: "ab", HostID: "c"}
	b := VMRecord{ExternalID: "1", Name: "a", HostID: "bc"}
	if string(a.Fingerprint()) == string(b.Fingerprint()) {
		t.Error("field boundaries collide in the fingerprint")
	}
}

func TestNaturalKeysDisambiguateScope(t *testing.T) {
	// The same storage id on two nodes is two different pools.
	local1 := StorageRecord{ExternalID: "local", HostID: "node1"}
	local2 := StorageRecord{ExternalID: "local", HostID: "node2"}
	if local1.NaturalKey() == local2.NaturalKey() {
		t.Error("per-host storage pools collide on natural key")
	}

	// The same interface name on two nodes is two different interfaces.
	net1 := NetworkRecord{ExternalID: "vmbr0", HostID: "node1"}
	net2 := NetworkRecord{ExternalID: "vmbr0", HostID: "node2"}
	if net1.NaturalKey() == net2.NaturalKey() {
		t.Error("per-host networks collide on natural key")
	}
}

func TestSupportsReportsDeclaredCapabilities(t *testing.T) {
	c := capabilityStub{caps: []Capability{CapabilityVM, CapabilityConsole}}
	if !Supports(c, CapabilityVM) || !Supports(c, CapabilityConsole) {
		t.Error("declared capability not reported")
	}
	if Supports(c, CapabilityPower) {
		t.Error("undeclared capability reported as supported")
	}
}

type capabilityStub struct{ caps []Capability }

func (capabilityStub) Info() Info                   { return Info{Type: "stub"} }
func (capabilityStub) ValidateConfig(Config) error  { return nil }
func (capabilityStub) Close() error                 { return nil }
func (s capabilityStub) Capabilities() []Capability { return s.caps }
func (capabilityStub) TestConnection(context.Context) (TestReport, error) {
	return TestReport{}, nil
}
