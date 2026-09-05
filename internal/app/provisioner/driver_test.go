package provisioner

// The driver is exercised against the mock platform rather than a real one, so
// the whole path — the step ordering, the task polling, the filing into a VM
// group at the end — runs in CI with no hypervisor anywhere (docs/09 §9.5).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/connectors/mock"
	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/domain/provision"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

type memRequests struct {
	byID map[uuid.UUID]*provision.Request
	// found is what FindVMByExternalID answers with; the nil UUID stands for a
	// guest a sync has not brought in yet.
	found uuid.UUID
}

func newMemRequests() *memRequests {
	return &memRequests{byID: map[uuid.UUID]*provision.Request{}}
}

func (m *memRequests) CreateRequest(_ context.Context, r *provision.Request) error {
	copied := *r
	m.byID[r.ID] = &copied
	return nil
}

func (m *memRequests) GetRequest(_ context.Context, id uuid.UUID) (*provision.Request, error) {
	r, ok := m.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	copied := *r
	return &copied, nil
}

func (m *memRequests) SaveRequest(_ context.Context, r *provision.Request) error {
	if _, ok := m.byID[r.ID]; !ok {
		return ports.ErrNotFound
	}
	copied := *r
	m.byID[r.ID] = &copied
	return nil
}

func (m *memRequests) ListRequests(context.Context, uuid.UUID, int) ([]*provision.Request, error) {
	return nil, nil
}

func (m *memRequests) ListOpenRequests(context.Context) ([]*provision.Request, error) {
	var out []*provision.Request
	for _, r := range m.byID {
		if !r.State.Terminal() {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memRequests) FindVMByExternalID(context.Context, uuid.UUID, string) (uuid.UUID, error) {
	return m.found, nil
}

// memAccess records what a group's membership was set to.
type memAccess struct {
	members map[uuid.UUID][]uuid.UUID
	sets    int
}

func (m *memAccess) VMGroupMemberIDs(_ context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	return m.members[id], nil
}

func (m *memAccess) SetVMGroupMembers(_ context.Context, id uuid.UUID, ids []uuid.UUID) error {
	m.sets++
	m.members[id] = ids
	return nil
}

func (m *memAccess) CreateUserGroup(context.Context, *access.UserGroup) error   { return nil }
func (m *memAccess) ListUserGroups(context.Context) ([]access.UserGroup, error) { return nil, nil }
func (m *memAccess) DeleteUserGroup(context.Context, uuid.UUID) error           { return nil }
func (m *memAccess) SetUserGroups(context.Context, uuid.UUID, []uuid.UUID) error {
	return nil
}
func (m *memAccess) UserGroupNames(context.Context, uuid.UUID) ([]string, error) { return nil, nil }
func (m *memAccess) CreateVMGroup(context.Context, *access.VMGroup) error        { return nil }
func (m *memAccess) ListVMGroups(context.Context) ([]access.VMGroup, error)      { return nil, nil }
func (m *memAccess) DeleteVMGroup(context.Context, uuid.UUID) error              { return nil }
func (m *memAccess) CreateGrant(context.Context, *access.Grant) error            { return nil }
func (m *memAccess) ListGrants(context.Context) ([]access.Grant, error)          { return nil, nil }
func (m *memAccess) DeleteGrant(context.Context, uuid.UUID) error                { return nil }
func (m *memAccess) VisibleVMGroupIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// mockPlatforms hands out one platform backed by the mock connector.
type mockPlatforms struct{ p *inventory.Platform }

func (m mockPlatforms) Get(context.Context, uuid.UUID) (*inventory.Platform, error) { return m.p, nil }
func (m mockPlatforms) Credential(context.Context, uuid.UUID, *crypto.Vault) (ports.PlainCredential, error) {
	return ports.PlainCredential{Kind: "api_token", TokenID: "t", Secret: "s"}, nil
}
func (mockPlatforms) Create(context.Context, *inventory.Platform, ports.SealedCredential) error {
	return nil
}
func (mockPlatforms) List(context.Context, bool) ([]*inventory.Platform, error) { return nil, nil }
func (mockPlatforms) Update(context.Context, *inventory.Platform) error         { return nil }
func (mockPlatforms) UpdateHealth(context.Context, *inventory.Platform) error   { return nil }
func (mockPlatforms) SoftDelete(context.Context, uuid.UUID, time.Time) error    { return nil }
func (mockPlatforms) ReplaceCredential(context.Context, uuid.UUID, ports.SealedCredential) error {
	return nil
}
func (mockPlatforms) Endpoints(context.Context, uuid.UUID) ([]ports.PlatformEndpoint, error) {
	return nil, nil
}
func (mockPlatforms) ReplaceEndpoints(context.Context, uuid.UUID, []ports.PlatformEndpoint, time.Time) error {
	return nil
}

type nopQueue struct{ syncs int }

func (q *nopQueue) EnqueueProvisionStep(context.Context, uuid.UUID, time.Duration) error { return nil }
func (q *nopQueue) EnqueueInventorySync(context.Context, uuid.UUID, string) error {
	q.syncs++
	return nil
}

type nopAudit struct{ entries []ports.AuditEntry }

func (a *nopAudit) Write(_ context.Context, e ports.AuditEntry) error {
	a.entries = append(a.entries, e)
	return nil
}

type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

// persistentPlatform hands out the same fleet on every connection, because a
// real platform does not forget the guest it just made when the portal
// reconnects. The mock keeps its fleet per instance, so each step would
// otherwise see a fresh, empty cluster.
type persistentPlatform struct{ conn *mock.Connector }

func (p persistentPlatform) Connect(context.Context, *inventory.Platform) (connector.Connector, error) {
	return sharedFleet{p.conn}, nil
}

// sharedFleet is the mock with a Close that does not end it, and with the
// capability interfaces forwarded explicitly.
//
// Embedding the connector.Connector interface would not do: a type assertion
// sees the wrapper's method set, and the capabilities the driver looks for —
// Provisioner, Destroyer, PowerManager, TaskWatcher — are not part of the base
// interface, so every one of them would silently come back "not supported".
// Forwarding them by hand is what keeps the wrapper honest.
type sharedFleet struct{ c *mock.Connector }

func (s sharedFleet) Info() connector.Info                      { return s.c.Info() }
func (s sharedFleet) ValidateConfig(cfg connector.Config) error { return s.c.ValidateConfig(cfg) }
func (s sharedFleet) Capabilities() []connector.Capability      { return s.c.Capabilities() }
func (s sharedFleet) Close() error                              { return nil }
func (s sharedFleet) TestConnection(ctx context.Context) (connector.TestReport, error) {
	return s.c.TestConnection(ctx)
}
func (s sharedFleet) ListVMs(ctx context.Context) ([]connector.VMRecord, error) {
	return s.c.ListVMs(ctx)
}
func (s sharedFleet) ListTemplates(ctx context.Context) ([]connector.TemplateRecord, error) {
	return s.c.ListTemplates(ctx)
}
func (s sharedFleet) NextID(ctx context.Context) (string, error) { return s.c.NextID(ctx) }
func (s sharedFleet) Clone(ctx context.Context, spec connector.CloneSpec) (connector.TaskRef, error) {
	return s.c.Clone(ctx, spec)
}
func (s sharedFleet) Configure(ctx context.Context, vm connector.VMRef, spec connector.CloudInitSpec) error {
	return s.c.Configure(ctx, vm, spec)
}
func (s sharedFleet) ResizeDisk(ctx context.Context, vm connector.VMRef, disk string, grow int64) error {
	return s.c.ResizeDisk(ctx, vm, disk, grow)
}
func (s sharedFleet) Destroy(ctx context.Context, vm connector.VMRef, opts connector.DestroyOptions) (connector.TaskRef, error) {
	return s.c.Destroy(ctx, vm, opts)
}
func (s sharedFleet) Power(ctx context.Context, vm connector.VMRef, a connector.PowerAction) (connector.TaskRef, error) {
	return s.c.Power(ctx, vm, a)
}
func (s sharedFleet) TaskState(ctx context.Context, t connector.TaskRef) (bool, bool, string, error) {
	return s.c.TaskState(ctx, t)
}
func (s sharedFleet) ImageExists(ctx context.Context, node, storage, filename string) (bool, error) {
	return s.c.ImageExists(ctx, node, storage, filename)
}
func (s sharedFleet) DownloadImage(ctx context.Context, spec connector.ImageDownloadSpec) (connector.TaskRef, error) {
	return s.c.DownloadImage(ctx, spec)
}
func (s sharedFleet) CreateGuest(ctx context.Context, spec connector.GuestCreateSpec) (connector.TaskRef, error) {
	return s.c.CreateGuest(ctx, spec)
}
func (s sharedFleet) ImportDisk(ctx context.Context, vm connector.VMRef, spec connector.DiskImportSpec) (connector.TaskRef, error) {
	return s.c.ImportDisk(ctx, vm, spec)
}
func (s sharedFleet) ConvertToTemplate(ctx context.Context, vm connector.VMRef) (connector.TaskRef, error) {
	return s.c.ConvertToTemplate(ctx, vm)
}

func newDriver(t *testing.T, requests *memRequests, acc *memAccess, q *nopQueue, audit *nopAudit) *Driver {
	t.Helper()
	platform := &inventory.Platform{
		ID: uuid.New(), Name: "mock", Type: mock.Type, EndpointURL: "mock://local",
		IsEnabled: true, TLSMode: "verify",
	}
	built, err := mock.New(connector.Config{Endpoint: platform.EndpointURL},
		connector.Credentials{}, connector.Options{})
	if err != nil {
		t.Fatalf("mock.New: %v", err)
	}
	conn := built.(*mock.Connector)
	t.Cleanup(func() { _ = conn.Close() })

	return &Driver{
		Requests:  requests,
		Platforms: mockPlatforms{p: platform},
		Access:    acc,
		Platform:  persistentPlatform{conn: conn},
		Queue:     q,
		Audit:     audit,
		Clock:     fixedClock{time.Now()},
		Log:       zerolog.Nop(),
	}
}

// run turns the driver until the request settles, the way the job layer does.
func run(t *testing.T, d *Driver, id uuid.UUID) *provision.Request {
	t.Helper()
	for i := 0; i < 20; i++ {
		err := d.Step(context.Background(), id)
		if err != nil && !errors.Is(err, ErrStillRunning) {
			t.Fatalf("step: %v", err)
		}
		req, err := d.Requests.GetRequest(context.Background(), id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if req.State.Terminal() {
			return req
		}
	}
	t.Fatal("request never settled")
	return nil
}

func provisionRequest(platformID uuid.UUID, group *uuid.UUID) *provision.Request {
	now := time.Now()
	return &provision.Request{
		ID: uuid.New(), PlatformID: platformID, Kind: provision.KindProvision,
		State: provision.StatePending, TemplateExternalID: "9000",
		GuestName: "web-02", TargetNode: "node01", VMGroupID: group,
		RequestedByName: "someone",
		Spec: provision.Spec{
			TemplateNode: "node01", TemplateType: "qemu", FullClone: true,
			CIUser: "ubuntu", SSHKeys: []string{"ssh-ed25519 AAAA portal@proxui"},
			Cores: 4, MemoryMB: 4096, DiskName: "scsi0", DiskGrowBytes: 20 << 30,
			StartAfterCreate: true,
		},
		Created: now, Updated: now,
	}
}

func TestAProvisioningRunCreatesConfiguresAndStartsTheGuest(t *testing.T) {
	requests := newMemRequests()
	acc := &memAccess{members: map[uuid.UUID][]uuid.UUID{}}
	queue := &nopQueue{}
	d := newDriver(t, requests, acc, queue, &nopAudit{})

	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)
	req := provisionRequest(platform.ID, nil)
	if err := requests.CreateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	done := run(t, d, req.ID)
	if done.State != provision.StateReady {
		t.Fatalf("state = %s (step %s, error %q), want ready", done.State, done.Step, done.Error)
	}
	if done.VMID == "" {
		t.Fatal("the finished request does not name the guest it created")
	}

	// The guest is on the platform, with the sizing that was asked for.
	conn, err := d.Platform.Connect(context.Background(), platform)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	vms, err := conn.(connector.VirtualMachineCollector).ListVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found *connector.VMRecord
	for i := range vms {
		if vms[i].ExternalID == done.VMID {
			found = &vms[i]
		}
	}
	if found == nil {
		t.Fatalf("no guest %s on the platform", done.VMID)
	}
	if found.Name != "web-02" {
		t.Errorf("name = %q, want web-02", found.Name)
	}
	if found.CPUCores != 4 || found.MemoryBytes != 4096<<20 {
		t.Errorf("sizing = %d cores / %d bytes, want the configured 4 / 4 GiB",
			found.CPUCores, found.MemoryBytes)
	}
	// Configure runs before the resize, and both before the guest boots — a
	// disk grown after first boot is a disk the filesystem inside does not know
	// about.
	if found.DiskBytes <= 10<<30 {
		t.Errorf("disk = %d bytes, want the template's plus the growth", found.DiskBytes)
	}
	if queue.syncs == 0 {
		t.Error("no inventory sync was requested; the guest would stay invisible")
	}
}

// SetVMGroupMembers replaces a group's manual membership, so filing the new
// guest by passing only its id would empty the group of everything else.
func TestFilingTheGuestAppendsToTheGroupRatherThanReplacingIt(t *testing.T) {
	existing := []uuid.UUID{uuid.New(), uuid.New()}
	groupID := uuid.New()
	acc := &memAccess{members: map[uuid.UUID][]uuid.UUID{groupID: existing}}

	requests := newMemRequests()
	requests.found = uuid.New() // the sync has brought the new guest in
	d := newDriver(t, requests, acc, &nopQueue{}, &nopAudit{})

	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)
	req := provisionRequest(platform.ID, &groupID)
	if err := requests.CreateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if done := run(t, d, req.ID); done.State != provision.StateReady {
		t.Fatalf("state = %s, want ready", done.State)
	}

	members := acc.members[groupID]
	if len(members) != len(existing)+1 {
		t.Fatalf("group has %d members, want the %d it had plus the new guest",
			len(members), len(existing))
	}
	for _, want := range existing {
		var present bool
		for _, got := range members {
			if got == want {
				present = true
			}
		}
		if !present {
			t.Errorf("member %s was dropped from the group", want)
		}
	}
}

// A guest that has not appeared in inventory yet is waited for, not failed
// over: it arrives with the next sync.
func TestFilingWaitsForTheGuestToAppear(t *testing.T) {
	groupID := uuid.New()
	acc := &memAccess{members: map[uuid.UUID][]uuid.UUID{groupID: nil}}
	requests := newMemRequests()
	requests.found = uuid.Nil // not synced yet
	d := newDriver(t, requests, acc, &nopQueue{}, &nopAudit{})

	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)
	req := provisionRequest(platform.ID, &groupID)
	if err := requests.CreateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	var last error
	for i := 0; i < 10; i++ {
		last = d.Step(context.Background(), req.ID)
		if errors.Is(last, ErrStillRunning) {
			break
		}
	}
	if !errors.Is(last, ErrStillRunning) {
		t.Fatalf("err = %v, want the driver to wait for the guest", last)
	}
	stored, _ := requests.GetRequest(context.Background(), req.ID)
	if stored.State.Terminal() {
		t.Errorf("state = %s; the request finished without filing the guest", stored.State)
	}
	if acc.sets != 0 {
		t.Error("group membership was written before the guest existed")
	}
}

// Past the grace period the request completes anyway. A working guest that is
// merely unfiled beats a request that never finishes.
func TestFilingGivesUpAfterTheGracePeriod(t *testing.T) {
	groupID := uuid.New()
	acc := &memAccess{members: map[uuid.UUID][]uuid.UUID{groupID: nil}}
	requests := newMemRequests()
	requests.found = uuid.Nil
	d := newDriver(t, requests, acc, &nopQueue{}, &nopAudit{})

	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)
	req := provisionRequest(platform.ID, &groupID)
	req.Created = d.Clock.Now().Add(-groupAssignmentGrace - time.Minute)
	if err := requests.CreateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	done := run(t, d, req.ID)
	if done.State != provision.StateReady {
		t.Fatalf("state = %s, want ready", done.State)
	}
	if done.Error == "" {
		t.Error("the request finished silently; the unfiled guest should be reported")
	}
}

// A run that fails leaves the guest alone. The portal does not destroy a
// machine on the strength of an error it may have misread.
func TestAFailedRunKeepsThePartialGuest(t *testing.T) {
	requests := newMemRequests()
	d := newDriver(t, requests, &memAccess{members: map[uuid.UUID][]uuid.UUID{}}, &nopQueue{}, &nopAudit{})

	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)
	req := provisionRequest(platform.ID, nil)
	// A guest that does not exist: configure will be refused, which stands in
	// for any mid-run refusal from the platform.
	req.State = provision.StateConfiguring
	req.VMID = "999"
	if err := requests.CreateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if err := d.Step(context.Background(), req.ID); err != nil {
		t.Fatalf("step: %v", err)
	}
	stored, _ := requests.GetRequest(context.Background(), req.ID)
	if stored.State != provision.StateFailed {
		t.Fatalf("state = %s, want failed", stored.State)
	}
	if stored.Step != string(provision.StateConfiguring) {
		t.Errorf("step = %q, want the step that failed", stored.Step)
	}
	if stored.VMID != "999" {
		t.Error("the identifier of the partial guest was discarded")
	}
	if stored.Error == "" {
		t.Error("the cause was not recorded")
	}
}

func TestDestroyRemovesTheGuest(t *testing.T) {
	requests := newMemRequests()
	queue := &nopQueue{}
	d := newDriver(t, requests, &memAccess{members: map[uuid.UUID][]uuid.UUID{}}, queue, &nopAudit{})

	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)

	// Create one first, so there is something to remove.
	create := provisionRequest(platform.ID, nil)
	create.Spec.StartAfterCreate = false
	if err := requests.CreateRequest(context.Background(), create); err != nil {
		t.Fatal(err)
	}
	created := run(t, d, create.ID)

	now := time.Now()
	del := &provision.Request{
		ID: uuid.New(), PlatformID: platform.ID, Kind: provision.KindDestroy,
		State: provision.StatePending, VMID: created.VMID, TargetNode: "node01",
		GuestName: "web-02", Spec: provision.Spec{TemplateType: "qemu"},
		Created: now, Updated: now,
	}
	if err := requests.CreateRequest(context.Background(), del); err != nil {
		t.Fatal(err)
	}

	if done := run(t, d, del.ID); done.State != provision.StateDeleted {
		t.Fatalf("state = %s (error %q), want deleted", done.State, done.Error)
	}

	conn, err := d.Platform.Connect(context.Background(), platform)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	vms, err := conn.(connector.VirtualMachineCollector).ListVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range vms {
		if vm.ExternalID == created.VMID {
			t.Fatalf("guest %s is still on the platform", created.VMID)
		}
	}
	if queue.syncs == 0 {
		t.Error("no sync was requested; the portal would keep showing the guest")
	}
}

func templateBuildRequest(platformID uuid.UUID) *provision.Request {
	now := time.Now()
	return &provision.Request{
		ID: uuid.New(), PlatformID: platformID, Kind: provision.KindTemplate,
		State: provision.StatePending, GuestName: "debian-13-cloud",
		TargetNode: "node01", RequestedByName: "an administrator",
		Spec: provision.Spec{
			TemplateType: "qemu",
			ImageURL:     "https://cloud.example/debian-13-generic-amd64.qcow2",
			ImageFile:    "debian-13-generic-amd64.qcow2",
			ImageStorage: "local", Storage: "local-lvm",
			Checksum: "abc123", ChecksumAlgo: "sha512",
			Cores: 2, MemoryMB: 2048, Bridge: "vmbr0",
		},
		Created: now, Updated: now,
	}
}

// The run this feature exists for: no template, then one, without anybody
// opening a shell on a node.
func TestBuildingATemplateProducesOne(t *testing.T) {
	requests := newMemRequests()
	d := newDriver(t, requests, &memAccess{members: map[uuid.UUID][]uuid.UUID{}}, &nopQueue{}, &nopAudit{})
	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)

	conn, _ := d.Platform.Connect(context.Background(), platform)
	before, err := conn.(connector.Provisioner).ListTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	req := templateBuildRequest(platform.ID)
	if err := requests.CreateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	done := run(t, d, req.ID)
	if done.State != provision.StateReady {
		t.Fatalf("state = %s (step %s, error %q), want ready", done.State, done.Step, done.Error)
	}

	after, err := conn.(connector.Provisioner).ListTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("templates went %d → %d, want one more", len(before), len(after))
	}
	var found bool
	for _, tpl := range after {
		if tpl.Name == "debian-13-cloud" {
			found = true
			// A template without a cloud-init drive is one nobody can log into
			// a guest cloned from.
			if !tpl.HasCloudInit {
				t.Error("the built template has no cloud-init drive")
			}
		}
	}
	if !found {
		t.Error("the built template is not in the listing")
	}

	// It must not also be counted as a guest — that is what makes fleet totals
	// wrong, and why templates are excluded from the inventory.
	vms, err := conn.(connector.VirtualMachineCollector).ListVMs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range vms {
		if vm.ExternalID == done.VMID {
			t.Errorf("the finished template is still listed as a guest")
		}
	}
}

// The image is hundreds of megabytes. A second template from the same one must
// not fetch it again.
func TestASecondBuildReusesTheDownloadedImage(t *testing.T) {
	requests := newMemRequests()
	d := newDriver(t, requests, &memAccess{members: map[uuid.UUID][]uuid.UUID{}}, &nopQueue{}, &nopAudit{})
	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)

	first := templateBuildRequest(platform.ID)
	if err := requests.CreateRequest(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if done := run(t, d, first.ID); done.State != provision.StateReady {
		t.Fatalf("first build = %s (%s)", done.State, done.Error)
	}

	second := templateBuildRequest(platform.ID)
	second.GuestName = "debian-13-cloud-2"
	if err := requests.CreateRequest(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	done := run(t, d, second.ID)
	if done.State != provision.StateReady {
		t.Fatalf("second build = %s (%s)", done.State, done.Error)
	}
	// The decision is recorded on the request rather than re-derived, so it is
	// visible to anyone reading what happened.
	if !done.Spec.ImagePresent {
		t.Error("the second build did not notice the image was already there")
	}
	if done.Step == string(provision.StateDownloading) {
		t.Error("the second build downloaded the image again")
	}
}

// A build that fails leaves the half-made guest in place, the same as
// provisioning: the portal does not destroy on the strength of an error.
func TestAFailedBuildKeepsThePartialGuest(t *testing.T) {
	requests := newMemRequests()
	d := newDriver(t, requests, &memAccess{members: map[uuid.UUID][]uuid.UUID{}}, &nopQueue{}, &nopAudit{})
	platform, _ := d.Platforms.Get(context.Background(), uuid.Nil)

	req := templateBuildRequest(platform.ID)
	// Importing onto a guest that does not exist stands in for any mid-build
	// refusal from the platform.
	req.State = provision.StateImporting
	req.VMID = "9999"
	if err := requests.CreateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if err := d.Step(context.Background(), req.ID); err != nil {
		t.Fatalf("step: %v", err)
	}
	stored, _ := requests.GetRequest(context.Background(), req.ID)
	if stored.State != provision.StateFailed {
		t.Fatalf("state = %s, want failed", stored.State)
	}
	if stored.Step != string(provision.StateImporting) || stored.VMID != "9999" {
		t.Errorf("failed build lost its step or its guest: %+v", stored)
	}
}
