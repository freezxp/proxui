package deploy

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
	"github.com/freezxp/proxui/internal/connectors/mock"
	"github.com/freezxp/proxui/internal/domain/deployment"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

// --- doubles ---------------------------------------------------------------

type memDeployments struct {
	mu   sync.Mutex
	byID map[uuid.UUID]*deployment.Deployment
}

func newMemDeployments() *memDeployments {
	return &memDeployments{byID: map[uuid.UUID]*deployment.Deployment{}}
}

func (m *memDeployments) Create(_ context.Context, d *deployment.Deployment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *d
	m.byID[d.ID] = &copied
	return nil
}

func (m *memDeployments) Save(_ context.Context, d *deployment.Deployment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := *d
	copied.Log = deployment.TruncateLog(copied.Log)
	m.byID[d.ID] = &copied
	return nil
}

func (m *memDeployments) Get(_ context.Context, id uuid.UUID) (*deployment.Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	copied := *rec
	return &copied, nil
}

func (m *memDeployments) List(context.Context, uuid.UUID, int) ([]*deployment.Deployment, error) {
	return nil, nil
}

func (m *memDeployments) ListOpen(context.Context) ([]*deployment.Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*deployment.Deployment{}
	for _, d := range m.byID {
		if !d.State.Terminal() {
			copied := *d
			out = append(out, &copied)
		}
	}
	return out, nil
}

type memPlatforms struct{ p *inventory.Platform }

func (m memPlatforms) Get(context.Context, uuid.UUID) (*inventory.Platform, error) {
	return m.p, nil
}

type fakeHosts struct{ hosts []ports.SensorHost }

func (f fakeHosts) OnlineHosts(context.Context, uuid.UUID) ([]ports.SensorHost, error) {
	return f.hosts, nil
}

type fakeKey struct{ key string }

func (f fakeKey) PrivateKey(context.Context) (string, error) { return f.key, nil }

type fakeSSH struct{ known map[uuid.UUID]ports.NodeSSH }

func (f fakeSSH) Get(_ context.Context, id uuid.UUID) (ports.NodeSSH, error) {
	rec, ok := f.known[id]
	if !ok {
		return ports.NodeSSH{}, ports.ErrNotFound
	}
	return rec, nil
}
func (f fakeSSH) Pin(context.Context, ports.NodeSSH) error { return nil }
func (f fakeSSH) RecordAttempt(context.Context, uuid.UUID, string, time.Time, string) error {
	return nil
}
func (f fakeSSH) Forget(context.Context, uuid.UUID) error { return nil }

// fakeRunner records what was asked of the node and answers with a script that
// the test drives.
type fakeRunner struct {
	mu       sync.Mutex
	commands []string
	// launched is what the launch command answers; polls is a queue of poll
	// answers, the last repeating.
	launched string
	polls    []string
	fail     error
}

func (f *fakeRunner) RunCommand(_ context.Context, target ports.SSHTarget, _ ports.SSHCredential,
	policy ports.HostKeyPolicy, command string) ([]byte, error) {

	if err := policy.Check(target.Host, "ssh-ed25519", "SHA256:node", []byte("node-key")); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	if f.fail != nil {
		return nil, f.fail
	}
	if strings.Contains(command, launchedMarker) {
		if f.launched != "" {
			return []byte(f.launched), nil
		}
		return []byte(launchedMarker + "\n"), nil
	}
	if len(f.polls) == 0 {
		return []byte("running\nctid \n" + pollMarker + "\n"), nil
	}
	answer := f.polls[0]
	if len(f.polls) > 1 {
		f.polls = f.polls[1:]
	}
	return []byte(answer), nil
}

func (f *fakeRunner) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.commands))
	copy(out, f.commands)
	return out
}

type nopAudit struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
}

func (n *nopAudit) Write(_ context.Context, e ports.AuditEntry) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.entries = append(n.entries, e)
	return nil
}
func (n *nopAudit) all() []ports.AuditEntry {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]ports.AuditEntry, len(n.entries))
	copy(out, n.entries)
	return out
}

type nopQueue struct{ enqueued int }

func (q *nopQueue) EnqueueDeployStep(context.Context, uuid.UUID, time.Duration) error {
	q.enqueued++
	return nil
}

type nopSync struct{ calls int }

func (s *nopSync) EnqueueInventorySync(context.Context, uuid.UUID, string) error {
	s.calls++
	return nil
}

type movingClock struct{ at *time.Time }

func (m movingClock) Now() time.Time { return *m.at }

type sharedPlatform struct{ conn *mock.Connector }

func (s sharedPlatform) Connect(context.Context, *inventory.Platform) (connector.Connector, error) {
	return nodeAddresses{s.conn}, nil
}

// nodeAddresses is the mock with the one capability the deployer looks for
// forwarded explicitly. Embedding the interface would hide it: a type assertion
// sees the wrapper's method set, and NodeAddresser would silently come back
// unsupported — which would make every test pass while testing nothing.
type nodeAddresses struct{ c *mock.Connector }

func (n nodeAddresses) Info() connector.Info                    { return n.c.Info() }
func (n nodeAddresses) ValidateConfig(c connector.Config) error { return n.c.ValidateConfig(c) }
func (n nodeAddresses) Capabilities() []connector.Capability    { return n.c.Capabilities() }
func (n nodeAddresses) Close() error                            { return nil }
func (n nodeAddresses) TestConnection(ctx context.Context) (connector.TestReport, error) {
	return n.c.TestConnection(ctx)
}
func (n nodeAddresses) NodeAddresses(ctx context.Context) (map[string]string, error) {
	return n.c.NodeAddresses(ctx)
}

// --- fixture ---------------------------------------------------------------

var nodeID = uuid.New()

func newDeployer(t *testing.T, store *memDeployments, runner *fakeRunner,
	audit *nopAudit, queue *nopQueue, sync *nopSync, at *time.Time) *Deployer {
	t.Helper()

	platform := &inventory.Platform{
		ID: uuid.New(), Name: "mock", Type: mock.Type,
		EndpointURL: "mock://local", IsEnabled: true, TLSMode: "verify",
	}
	built, err := mock.New(connector.Config{Endpoint: platform.EndpointURL},
		connector.Credentials{}, connector.Options{})
	if err != nil {
		t.Fatalf("mock.New: %v", err)
	}
	conn := built.(*mock.Connector)
	t.Cleanup(func() { _ = conn.Close() })

	addresses, err := conn.NodeAddresses(context.Background())
	if err != nil {
		t.Fatalf("NodeAddresses: %v", err)
	}
	var node string
	for name := range addresses {
		node = name
		break
	}
	if node == "" {
		t.Fatal("the mock platform reports no node addresses")
	}
	return &Deployer{
		Deployments: store,
		Platforms:   memPlatforms{p: platform},
		Platform:    sharedPlatform{conn: conn},
		Hosts: fakeHosts{hosts: []ports.SensorHost{
			{ID: nodeID, PlatformID: platform.ID, ExternalID: node, Name: node},
		}},
		SSH: fakeSSH{known: map[uuid.UUID]ports.NodeSSH{nodeID: {
			HostID: nodeID, Algorithm: "ssh-ed25519",
			Fingerprint: "SHA256:node", PublicKey: []byte("node-key"),
		}}},
		Key:    fakeKey{key: "PRIVATE"},
		Runner: runner, Queue: queue, Sync: sync, Audit: audit,
		Clock: movingClock{at}, Log: zerolog.Nop(),
	}
}

func startOne(t *testing.T, d *Deployer, store *memDeployments) *deployment.Deployment {
	t.Helper()
	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)
	hosts, _ := d.Hosts.OnlineHosts(context.Background(), platform.ID)
	rec, err := d.Start(context.Background(), Input{
		Actor:      Actor{UserID: uuid.New(), Username: "admin"},
		PlatformID: platform.ID,
		AppID:      "adguard",
		Node:       hosts[0].Name,
		Spec:       deployment.Spec{Cores: 2, MemoryMB: 1024, Hostname: "adguard-1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return rec
}

// --- tests -----------------------------------------------------------------

// The happy path end to end: the script is written and launched, the portal
// waits, and the outcome and transcript are recorded.
func TestADeploymentRunsAndRecordsWhatHappened(t *testing.T) {
	at := time.Now()
	store, runner, audit, queue, sync := newMemDeployments(), &fakeRunner{}, &nopAudit{}, &nopQueue{}, &nopSync{}
	runner.polls = []string{
		"running\nctid \n" + pollMarker + "\ninstalling…\n",
		"exit 0\nctid 142\n" + pollMarker + "\ninstalling…\ndone\n",
	}
	d := newDeployer(t, store, runner, audit, queue, sync, &at)
	rec := startOne(t, d, store)

	// Launch, then one poll that is not finished, then one that is.
	if err := d.Step(context.Background(), rec.ID); !errors.Is(err, ErrStillRunning) {
		t.Fatalf("launch = %v, want ErrStillRunning", err)
	}
	if err := d.Step(context.Background(), rec.ID); !errors.Is(err, ErrStillRunning) {
		t.Fatalf("first poll = %v, want ErrStillRunning", err)
	}
	if err := d.Step(context.Background(), rec.ID); err != nil {
		t.Fatalf("second poll: %v", err)
	}

	done, _ := store.Get(context.Background(), rec.ID)
	if done.State != deployment.StateReady {
		t.Fatalf("state = %s (error %q), want ready", done.State, done.Error)
	}
	if done.ExitCode == nil || *done.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", done.ExitCode)
	}
	if done.CTID != "142" {
		t.Errorf("ctid = %q, want the container the script made", done.CTID)
	}
	if !strings.Contains(done.Log, "done") {
		t.Errorf("the transcript was not kept: %q", done.Log)
	}
	if sync.calls != 1 {
		t.Errorf("inventory syncs = %d, want 1 — the container exists and nothing else will notice", sync.calls)
	}

	e := audit.all()
	if len(e) != 1 || e[0].Action != "container.deploy" || e[0].Outcome != ports.OutcomeSuccess {
		t.Fatalf("audit = %+v, want one successful container.deploy", e)
	}
	if e[0].Details["scripts_ref"] != ScriptsRef {
		t.Errorf("the audit entry does not record which upstream commit ran: %+v", e[0].Details)
	}
}

// A script that exits non-zero is a failed deployment, not a failed portal —
// and the transcript is the only thing that can explain it.
func TestANonZeroExitKeepsTheTranscript(t *testing.T) {
	at := time.Now()
	store, runner, audit := newMemDeployments(), &fakeRunner{}, &nopAudit{}
	runner.polls = []string{"exit 1\nctid \n" + pollMarker + "\nstep one\nE: no space left on device\n"}
	d := newDeployer(t, store, runner, audit, &nopQueue{}, &nopSync{}, &at)
	rec := startOne(t, d, store)

	_ = d.Step(context.Background(), rec.ID)
	if err := d.Step(context.Background(), rec.ID); err != nil {
		t.Fatalf("poll: %v", err)
	}

	done, _ := store.Get(context.Background(), rec.ID)
	if done.State != deployment.StateFailed {
		t.Fatalf("state = %s, want failed", done.State)
	}
	if !strings.Contains(done.Log, "no space left on device") {
		t.Errorf("the reason was not kept: %q", done.Log)
	}
	if e := audit.all(); len(e) != 1 || e[0].Outcome != ports.OutcomeFailure {
		t.Errorf("audit = %+v, want one failure", e)
	}
}

// The whole control, asserted where it is enforced: an application the binary
// does not know never becomes a path, a URL or a command.
func TestAnUnknownApplicationIsRefused(t *testing.T) {
	at := time.Now()
	store, runner := newMemDeployments(), &fakeRunner{}
	d := newDeployer(t, store, runner, &nopAudit{}, &nopQueue{}, &nopSync{}, &at)
	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)
	hosts, _ := d.Hosts.OnlineHosts(context.Background(), platform.ID)

	for _, id := range []string{"", "nginx", "../../etc/passwd", "adguard; reboot", "adguard/../x"} {
		_, err := d.Start(context.Background(), Input{
			PlatformID: platform.ID, AppID: id, Node: hosts[0].Name,
		})
		if !errors.Is(err, ErrUnknownApp) {
			t.Errorf("Start(%q) = %v, want ErrUnknownApp", id, err)
		}
	}
	if len(runner.ran()) != 0 {
		t.Fatalf("something was dialled: %v", runner.ran())
	}
}

// Settings that are not obviously numbers or identifiers are refused before
// anything is dialled — they end up in the environment of a root command.
func TestBadSettingsAreRefusedBeforeDialling(t *testing.T) {
	at := time.Now()
	store, runner := newMemDeployments(), &fakeRunner{}
	d := newDeployer(t, store, runner, &nopAudit{}, &nopQueue{}, &nopSync{}, &at)
	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)
	hosts, _ := d.Hosts.OnlineHosts(context.Background(), platform.ID)

	for _, spec := range []deployment.Spec{
		{Hostname: "a; reboot"},
		{Hostname: "$(id)"},
		{Storage: "local-lvm'; rm -rf /"},
		{Bridge: "vmbr0 && curl evil"},
		{Cores: 9999},
		{MemoryMB: 1},
		{DiskGB: -4},
	} {
		if _, err := d.Start(context.Background(), Input{
			PlatformID: platform.ID, AppID: "adguard", Node: hosts[0].Name, Spec: spec,
		}); err == nil {
			t.Errorf("Start accepted %+v", spec)
		}
	}
	if len(runner.ran()) != 0 {
		t.Fatalf("something was dialled: %v", runner.ran())
	}
}

// Installing runs a large third-party program as root; a node the portal is
// meeting for the first time is the wrong one to do that on.
func TestAnUnpinnedNodeIsRefused(t *testing.T) {
	at := time.Now()
	store, runner := newMemDeployments(), &fakeRunner{}
	d := newDeployer(t, store, runner, &nopAudit{}, &nopQueue{}, &nopSync{}, &at)
	d.SSH = fakeSSH{known: map[uuid.UUID]ports.NodeSSH{}}
	rec := startOne(t, d, store)

	if err := d.Step(context.Background(), rec.ID); err != nil {
		t.Fatalf("step: %v", err)
	}
	done, _ := store.Get(context.Background(), rec.ID)
	if done.State != deployment.StateFailed || !strings.Contains(done.Error, "pinned") {
		t.Fatalf("state = %s error = %q, want a failure naming the pin", done.State, done.Error)
	}
	if len(runner.ran()) != 0 {
		t.Fatalf("something was run on an unpinned node: %v", runner.ran())
	}
}

// A deploy that never finishes is closed out with what it managed to print,
// rather than being polled forever.
func TestAnInstallThatNeverFinishesIsClosedOut(t *testing.T) {
	at := time.Now()
	store, runner := newMemDeployments(), &fakeRunner{}
	runner.polls = []string{"running\nctid \n" + pollMarker + "\nstill going\n"}
	d := newDeployer(t, store, runner, &nopAudit{}, &nopQueue{}, &nopSync{}, &at)
	rec := startOne(t, d, store)

	_ = d.Step(context.Background(), rec.ID)
	if err := d.Step(context.Background(), rec.ID); !errors.Is(err, ErrStillRunning) {
		t.Fatalf("inside the window = %v, want ErrStillRunning", err)
	}

	at = at.Add(deployWindow + time.Minute)
	if err := d.Step(context.Background(), rec.ID); err != nil {
		t.Fatalf("past the window: %v", err)
	}
	done, _ := store.Get(context.Background(), rec.ID)
	if done.State != deployment.StateFailed {
		t.Fatalf("state = %s, want failed", done.State)
	}
	if !strings.Contains(done.Log, "still going") {
		t.Errorf("what it printed was not kept: %q", done.Log)
	}
}

// These scripts exit 0 when somebody closes their menu. A deploy that stopped
// at a question nobody was there to answer would otherwise be recorded as ready
// with nothing installed — found on a live node, where it stopped at "which
// storage pool for container?" and reported success.
func TestAScriptThatMadeNothingIsNotASuccess(t *testing.T) {
	at := time.Now()
	store, runner := newMemDeployments(), &fakeRunner{}
	runner.polls = []string{
		"exit 0\nctid \n" + pollMarker + "\nWhich storage pool for container?\n✖️  User exited script\n",
	}
	d := newDeployer(t, store, runner, &nopAudit{}, &nopQueue{}, &nopSync{}, &at)
	rec := startOne(t, d, store)

	_ = d.Step(context.Background(), rec.ID)
	if err := d.Step(context.Background(), rec.ID); err != nil {
		t.Fatalf("poll: %v", err)
	}

	done, _ := store.Get(context.Background(), rec.ID)
	if done.State != deployment.StateFailed {
		t.Fatalf("state = %s, want failed — it exited 0 having created nothing", done.State)
	}
	if !strings.Contains(done.Error, "stopped at a question") {
		t.Errorf("the note does not explain what happened: %q", done.Error)
	}
	// And the question it stopped at is in the transcript, which is the point
	// of keeping one.
	if !strings.Contains(done.Log, "storage pool") {
		t.Errorf("the question was not kept: %q", done.Log)
	}
}
