package sensor

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/shell"
	"github.com/freezxp/proxui/internal/domain/telemetry"
	"github.com/freezxp/proxui/internal/infra/sensors"
)

const twoSensors = `{"coretemp-isa-0000":{"Package id 0":{"temp1_input":47.0,"temp1_crit":100.0},
	"Core 0":{"temp2_input":45.0,"temp2_crit":100.0}}}`

type fakeHosts struct{ hosts []ports.SensorHost }

func (f *fakeHosts) OnlineHosts(context.Context, uuid.UUID) ([]ports.SensorHost, error) {
	return f.hosts, nil
}

type fakeStore struct {
	mu      sync.Mutex
	written []ports.SensorReadings
}

func (f *fakeStore) Write(_ context.Context, in ports.SensorReadings) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, in)
	return len(in.Readings), nil
}
func (f *fakeStore) Latest(context.Context, uuid.UUID) (ports.SensorReadings, error) {
	return ports.SensorReadings{}, nil
}
func (f *fakeStore) Summaries(context.Context, []uuid.UUID) (map[uuid.UUID]telemetry.SensorSummary, error) {
	return nil, nil
}
func (f *fakeStore) Series(context.Context, uuid.UUID, string, string, time.Time, time.Time, telemetry.Resolution) ([]ports.SensorPoint, error) {
	return nil, nil
}
func (f *fakeStore) HottestNow(context.Context, time.Time) (map[uuid.UUID]telemetry.Reading, error) {
	return nil, nil
}

// fakeSSH is shared by the goroutines that poll each node, so it locks like
// the real store's database does.
type fakeSSH struct {
	mu       sync.Mutex
	known    map[uuid.UUID]ports.NodeSSH
	pinned   []ports.NodeSSH
	failures map[uuid.UUID]string
}

func newFakeSSH() *fakeSSH {
	return &fakeSSH{known: map[uuid.UUID]ports.NodeSSH{}, failures: map[uuid.UUID]string{}}
}
func (f *fakeSSH) Get(_ context.Context, id uuid.UUID) (ports.NodeSSH, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.known[id]
	if !ok {
		return ports.NodeSSH{}, ports.ErrNotFound
	}
	return rec, nil
}
func (f *fakeSSH) Pin(_ context.Context, rec ports.NodeSSH) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinned = append(f.pinned, rec)
	f.known[rec.HostID] = rec
	return nil
}
func (f *fakeSSH) RecordAttempt(_ context.Context, id uuid.UUID, _ time.Time, failure string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[id] = failure
	return nil
}
func (f *fakeSSH) Forget(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.known, id)
	return nil
}

// fakeSource stands in for the node: it applies the host key policy the way a
// real connection would, and records what it was asked and with what.
type fakeSource struct {
	mu      sync.Mutex
	out     string
	err     error
	hostKey []byte
	reads   int
	targets []string
	creds   []ports.SSHCredential
}

func (f *fakeSource) Read(_ context.Context, target ports.SSHTarget, cred ports.SSHCredential,
	policy ports.HostKeyPolicy) ([]telemetry.Reading, error) {
	f.mu.Lock()
	f.reads++
	f.targets = append(f.targets, target.Address())
	f.creds = append(f.creds, cred)
	out, readErr, key := f.out, f.err, f.hostKey
	f.mu.Unlock()
	if key == nil {
		key = []byte("node-key")
	}
	if err := policy.Check(target.Host, "ssh-ed25519", "SHA256:abc", key); err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	return sensors.Parse([]byte(out))
}

type fakeKey struct{ key string }

func (f fakeKey) PrivateKey(context.Context) (string, error) { return f.key, nil }

// addresser is a connector that can name its nodes.
type addresser struct {
	connector.Connector
	addresses map[string]string
	err       error
}

func (a addresser) NodeAddresses(context.Context) (map[string]string, error) {
	return a.addresses, a.err
}

// plain is a connector that cannot.
type plain struct{ connector.Connector }

func newCollector(hosts *fakeHosts, store *fakeStore, ssh *fakeSSH, source *fakeSource) *Collector {
	return &Collector{
		Hosts: hosts, Store: store, SSH: ssh, Source: source,
		Key:   fakeKey{key: "PRIVATE KEY"},
		Log:   zerolog.New(io.Discard),
		Clock: func() time.Time { return time.Date(2026, 8, 19, 1, 0, 0, 0, time.UTC) },
	}
}

func oneHost() *fakeHosts {
	return &fakeHosts{hosts: []ports.SensorHost{
		{ID: uuid.New(), PlatformID: uuid.New(), ExternalID: "pve1", Name: "pve1"},
	}}
}

func TestCollectReadsEveryNodeItHasAnAddressFor(t *testing.T) {
	hosts := &fakeHosts{hosts: []ports.SensorHost{
		{ID: uuid.New(), ExternalID: "pve1", Name: "pve1"},
		{ID: uuid.New(), ExternalID: "pve2", Name: "pve2"},
		// No address: the platform did not name it, so there is nothing to dial.
		{ID: uuid.New(), ExternalID: "pve3", Name: "pve3"},
	}}
	store, ssh := &fakeStore{}, newFakeSSH()
	source := &fakeSource{out: twoSensors}
	c := newCollector(hosts, store, ssh, source)

	stats, err := c.Collect(context.Background(), uuid.New(), addresser{
		addresses: map[string]string{"pve1": "10.0.30.111", "pve2": "10.0.30.112"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if stats.Nodes != 2 || stats.Answered != 2 || stats.Readings != 4 {
		t.Errorf("stats = %+v, want 2 nodes, 2 answered, 4 readings", stats)
	}
	if len(store.written) != 2 {
		t.Fatalf("wrote %d node results, want 2", len(store.written))
	}
	// The address dialled is the platform's, and the port is not negotiable.
	for _, target := range source.targets {
		if !strings.HasSuffix(target, ":22") {
			t.Errorf("dialled %q, want port 22", target)
		}
	}
}

// The command is a constant. If this test ever has to change because a caller
// wants to pass something else, that is the moment ADR 0007's boundary moved.
func TestTheNodeIsReadWithThePortalKeyAsRoot(t *testing.T) {
	source := &fakeSource{out: twoSensors}
	c := newCollector(oneHost(), &fakeStore{}, newFakeSSH(), source)

	if _, err := c.Collect(context.Background(), uuid.New(),
		addresser{addresses: map[string]string{"pve1": "10.0.30.111"}}); err != nil {
		t.Fatal(err)
	}
	if source.reads != 1 {
		t.Errorf("read the node %d times, want once", source.reads)
	}
	// The portal's own key, and never a password.
	if source.creds[0].PrivateKey == "" || source.creds[0].Password != "" {
		t.Errorf("credential = %+v, want the portal key and no password", source.creds[0])
	}
	if source.creds[0].Username != "root" {
		t.Errorf("user = %q, want root", source.creds[0].Username)
	}
}

func TestFirstContactPinsTheKeyAndAChangeIsRefused(t *testing.T) {
	hosts := oneHost()
	id := hosts.hosts[0].ID
	ssh := newFakeSSH()
	source := &fakeSource{out: twoSensors, hostKey: []byte("first-key")}
	c := newCollector(hosts, &fakeStore{}, ssh, source)
	addrs := addresser{addresses: map[string]string{"pve1": "10.0.30.111"}}

	if _, err := c.Collect(context.Background(), uuid.New(), addrs); err != nil {
		t.Fatal(err)
	}
	if len(ssh.pinned) != 1 || ssh.pinned[0].Fingerprint != "SHA256:abc" {
		t.Fatalf("pinned = %+v, want the key the node presented", ssh.pinned)
	}

	// The node comes back as something else.
	source.hostKey = []byte("different-key")
	stats, err := c.Collect(context.Background(), uuid.New(), addrs)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Answered != 0 || stats.Silent != 1 {
		t.Errorf("stats = %+v, want the node refused", stats)
	}
	if !strings.Contains(ssh.failures[id], "different host key") {
		t.Errorf("recorded %q, want it to name the host key change", ssh.failures[id])
	}
}

// A node that never authenticated must not have its key pinned: pinning it
// would record whatever answered on port 22 as this node's identity.
func TestAFailedNodeIsNotPinned(t *testing.T) {
	ssh := newFakeSSH()
	source := &fakeSource{err: shell.ErrAuthFailed}
	c := newCollector(oneHost(), &fakeStore{}, ssh, source)

	if _, err := c.Collect(context.Background(), uuid.New(),
		addresser{addresses: map[string]string{"pve1": "10.0.30.111"}}); err != nil {
		t.Fatal(err)
	}
	if len(ssh.pinned) != 0 {
		t.Errorf("pinned %+v for a node that never authenticated", ssh.pinned)
	}
}

// Every failure an operator can actually fix has to say which fix it is.
func TestFailuresAreNamedInTermsOfTheFix(t *testing.T) {
	tests := []struct {
		name string
		err  error
		out  string
		want string
	}{
		{"no key installed", shell.ErrAuthFailed, "", "authorized_keys"},
		{"node down", shell.ErrUnreachable, "", "could not be reached"},
		{"host key changed", shell.ErrHostKeyMismatch, "", "clear the pin"},
		{"no lm-sensors", nil, "", "lm-sensors"},
		{"no sensors command", errors.New(`ssh: "sensors -j" failed: bash: sensors: command not found`), "", "install lm-sensors"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hosts := oneHost()
			ssh := newFakeSSH()
			c := newCollector(hosts, &fakeStore{}, ssh, &fakeSource{err: tt.err, out: tt.out})

			if _, err := c.Collect(context.Background(), uuid.New(),
				addresser{addresses: map[string]string{"pve1": "10.0.30.111"}}); err != nil {
				t.Fatal(err)
			}
			got := ssh.failures[hosts.hosts[0].ID]
			if !strings.Contains(got, tt.want) {
				t.Errorf("recorded %q, want it to mention %q", got, tt.want)
			}
		})
	}
}

// A platform whose connector cannot name node addresses has nothing to reach,
// and that is not a failure — most platforms will never implement it.
func TestAConnectorThatCannotNameNodesIsSkipped(t *testing.T) {
	source := &fakeSource{out: twoSensors}
	c := newCollector(oneHost(), &fakeStore{}, newFakeSSH(), source)

	stats, err := c.Collect(context.Background(), uuid.New(), plain{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if stats.Nodes != 0 || source.reads != 0 {
		t.Errorf("stats = %+v after %d reads, want nothing attempted", stats, source.reads)
	}
}

// Without a portal key there is nothing to authenticate with. That is a portal
// that has not been set up yet, not a collection failure.
func TestNoPortalKeyCollectsNothingQuietly(t *testing.T) {
	source := &fakeSource{out: twoSensors}
	c := newCollector(oneHost(), &fakeStore{}, newFakeSSH(), source)
	c.Key = fakeKey{key: ""}

	stats, err := c.Collect(context.Background(), uuid.New(),
		addresser{addresses: map[string]string{"pve1": "10.0.30.111"}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if source.reads != 0 || stats.Nodes != 0 {
		t.Errorf("attempted %d connections without a key", source.reads)
	}
}

// A success has to clear the last failure, or a node that was fixed keeps
// showing the reason it used to be broken.
func TestASuccessClearsTheRecordedFailure(t *testing.T) {
	hosts := oneHost()
	id := hosts.hosts[0].ID
	ssh := newFakeSSH()
	source := &fakeSource{err: shell.ErrAuthFailed}
	c := newCollector(hosts, &fakeStore{}, ssh, source)
	addrs := addresser{addresses: map[string]string{"pve1": "10.0.30.111"}}

	_, _ = c.Collect(context.Background(), uuid.New(), addrs)
	if ssh.failures[id] == "" {
		t.Fatal("the first failure was not recorded")
	}

	source.err, source.out = nil, twoSensors
	if _, err := c.Collect(context.Background(), uuid.New(), addrs); err != nil {
		t.Fatal(err)
	}
	if ssh.failures[id] != "" {
		t.Errorf("still recorded %q after a successful read", ssh.failures[id])
	}
}
