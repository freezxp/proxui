package postgres_test

// What matters here is durability: a provisioning request exists so that work
// which outlives an HTTP call, and a portal restart, is still there afterwards.
// These run against a real database because that is the only place the claim
// can be checked.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/provision"
	"github.com/freezxp/proxui/internal/infra/postgres"
)

func newProvisionRequest(platformID uuid.UUID) *provision.Request {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &provision.Request{
		ID: uuid.New(), PlatformID: platformID, Kind: provision.KindProvision,
		State: provision.StatePending, TemplateExternalID: "9000",
		GuestName: "web-02", TargetNode: "pve", RequestedByName: "an administrator",
		Spec: provision.Spec{
			TemplateNode: "pve", TemplateType: "qemu", FullClone: true,
			CIUser: "ubuntu", SSHKeys: []string{"ssh-ed25519 AAAA portal@proxui"},
			Cores: 4, MemoryMB: 4096, DiskName: "scsi0", DiskGrowBytes: 20 << 30,
			StartAfterCreate: true,
		},
		Created: now, Updated: now,
	}
}

func TestProvisionRequestRoundTripsWithItsSpec(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, platform := seedPlatform(t, pool)
	repo := postgres.NewProvisionRepository(pool)

	req := newProvisionRequest(platform.ID)
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}

	got, err := repo.GetRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if got.State != provision.StatePending || got.Kind != provision.KindProvision {
		t.Errorf("kind/state = %s/%s", got.Kind, got.State)
	}
	if got.GuestName != "web-02" || got.TemplateExternalID != "9000" {
		t.Errorf("identity = %+v", got)
	}
	// The spec is what the driver replays from after a restart, so every field
	// of it has to survive the round trip through jsonb.
	if got.Spec.Cores != 4 || got.Spec.MemoryMB != 4096 {
		t.Errorf("sizing = %d cores / %d MB", got.Spec.Cores, got.Spec.MemoryMB)
	}
	if got.Spec.DiskGrowBytes != 20<<30 || got.Spec.DiskName != "scsi0" {
		t.Errorf("disk = %s +%d", got.Spec.DiskName, got.Spec.DiskGrowBytes)
	}
	if len(got.Spec.SSHKeys) != 1 {
		t.Errorf("ssh keys = %v", got.Spec.SSHKeys)
	}
	if !got.Spec.StartAfterCreate || !got.Spec.FullClone {
		t.Error("booleans in the spec did not survive")
	}
}

// The point of the table: a request that was mid-clone when the process
// stopped is still there, and still findable, when it comes back.
func TestOpenRequestsSurviveAndFinishedOnesDropOut(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, platform := seedPlatform(t, pool)
	repo := postgres.NewProvisionRepository(pool)

	open := newProvisionRequest(platform.ID)
	open.State = provision.StateCloning
	open.TaskID = "UPID:pve:0000:clone::root@pam:"
	open.VMID = "135"
	if err := repo.CreateRequest(ctx, open); err != nil {
		t.Fatal(err)
	}

	finished := newProvisionRequest(platform.ID)
	finished.State = provision.StateReady
	if err := repo.CreateRequest(ctx, finished); err != nil {
		t.Fatal(err)
	}

	resumable, err := repo.ListOpenRequests(ctx)
	if err != nil {
		t.Fatalf("ListOpenRequests: %v", err)
	}
	var sawOpen, sawFinished bool
	for _, r := range resumable {
		switch r.ID {
		case open.ID:
			sawOpen = true
			if r.TaskID != open.TaskID || r.VMID != "135" {
				t.Errorf("the resumed request lost what it was waiting on: %+v", r)
			}
		case finished.ID:
			sawFinished = true
		}
	}
	if !sawOpen {
		t.Error("a request that was mid-clone would never be picked up again")
	}
	if sawFinished {
		t.Error("a finished request was offered for resumption")
	}
}

func TestSaveRequestPersistsAnAdvance(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, platform := seedPlatform(t, pool)
	repo := postgres.NewProvisionRepository(pool)

	req := newProvisionRequest(platform.ID)
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}

	if err := req.Advance(time.Now().UTC()); err != nil { // → cloning
		t.Fatal(err)
	}
	req.VMID = "135"
	req.TaskID = "UPID:pve:0000:clone::root@pam:"
	if err := repo.SaveRequest(ctx, req); err != nil {
		t.Fatalf("SaveRequest: %v", err)
	}

	got, err := repo.GetRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != provision.StateCloning || got.VMID != "135" || got.TaskID == "" {
		t.Errorf("stored = %s / %s / %q", got.State, got.VMID, got.TaskID)
	}

	// A failure keeps the identifier of the guest that was made, which is how
	// an administrator finds what to clean up.
	req.Fail(time.Now().UTC(), context.DeadlineExceeded)
	if err := repo.SaveRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	got, err = repo.GetRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != provision.StateFailed || got.Step != string(provision.StateCloning) {
		t.Errorf("state/step = %s/%s", got.State, got.Step)
	}
	if got.VMID != "135" || got.Error == "" {
		t.Errorf("failed request lost its guest or its cause: %+v", got)
	}
}

// The driver asks this to find the guest it just made, and "not yet" is the
// expected answer for a machine created seconds ago.
func TestFindVMByExternalIDReportsAbsenceRatherThanFailing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, platform := seedPlatform(t, pool)
	repo := postgres.NewProvisionRepository(pool)

	id, err := repo.FindVMByExternalID(ctx, platform.ID, "135")
	if err != nil {
		t.Fatalf("FindVMByExternalID: %v", err)
	}
	if id != uuid.Nil {
		t.Errorf("id = %s, want the nil UUID for a guest not synced yet", id)
	}

	if id, err = repo.FindVMByExternalID(ctx, platform.ID, ""); err != nil || id != uuid.Nil {
		t.Errorf("empty identifier = %s, %v", id, err)
	}
}

func TestRequestsGoWithTheirPlatform(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, platform := seedPlatform(t, pool)
	repo := postgres.NewProvisionRepository(pool)

	req := newProvisionRequest(platform.ID)
	if err := repo.CreateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM platforms WHERE id = $1`, platform.ID); err != nil {
		t.Fatalf("delete platform: %v", err)
	}

	_, err := repo.GetRequest(ctx, req.ID)
	if err == nil {
		t.Fatal("the request outlived the platform it belonged to")
	}
	if !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("err = %v, want not found", err)
	}
}
