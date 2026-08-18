package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// The overlay's contract, which is mostly about what it must NOT do: never
// invent a row, never touch a field a live read has no opinion about, and
// never turn a platform's silence into an answer.

type stubReader struct {
	page   ports.VMPage
	detail ports.VMDetail
	err    error
	calls  int
}

func (s *stubReader) ListVMs(context.Context, ports.VMFilter) (ports.VMPage, error) {
	s.calls++
	return s.page, s.err
}

func (s *stubReader) GetVM(context.Context, uuid.UUID, identity.Role, uuid.UUID) (ports.VMDetail, error) {
	s.calls++
	return s.detail, s.err
}

func (s *stubReader) CanAccessVM(context.Context, uuid.UUID, identity.Role, uuid.UUID) (bool, error) {
	return true, nil
}
func (s *stubReader) VMHistory(context.Context, uuid.UUID, int) ([]ports.HistoryEntry, error) {
	return nil, nil
}
func (s *stubReader) SetPortalTags(context.Context, uuid.UUID, []string) error { return nil }
func (s *stubReader) SetNotes(context.Context, uuid.UUID, string) error        { return nil }
func (s *stubReader) Dashboard(context.Context, identity.Role, uuid.UUID) (ports.DashboardSummary, error) {
	return ports.DashboardSummary{}, nil
}

type stubLive struct {
	snaps map[uuid.UUID]ports.LiveSnapshot
	asked [][]uuid.UUID
}

func (s *stubLive) Forget(context.Context, uuid.UUID) {}

func (s *stubLive) Snapshot(_ context.Context, ids []uuid.UUID) map[uuid.UUID]ports.LiveSnapshot {
	s.asked = append(s.asked, ids)
	if s.snaps == nil {
		return map[uuid.UUID]ports.LiveSnapshot{}
	}
	return s.snaps
}

var (
	platformA = uuid.New()
	readAt    = time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
)

func syncedPage(items ...ports.VMListItem) ports.VMPage {
	return ports.VMPage{Items: items, Total: len(items)}
}

func vm(external, state string) ports.VMListItem {
	return ports.VMListItem{
		ID: uuid.New(), ExternalID: external, Name: "vm-" + external,
		State: state, PlatformID: platformA, CPUPct: 42, MemPct: 50, UptimeS: 1000,
		SyncState: "active",
	}
}

func snapshot(states ...ports.LiveVMState) map[uuid.UUID]ports.LiveSnapshot {
	byID := map[string]ports.LiveVMState{}
	for _, s := range states {
		byID[s.ExternalID] = s
	}
	return map[uuid.UUID]ports.LiveSnapshot{
		platformA: {PlatformID: platformA, ReadAt: readAt, States: byID},
	}
}

func TestLiveStateOverridesTheSyncedRow(t *testing.T) {
	// The reason the feature exists: somebody shut a VM down twenty seconds
	// ago and the page must not still call it running.
	reader := &stubReader{page: syncedPage(vm("101", "running"))}
	live := &stubLive{snaps: snapshot(ports.LiveVMState{ExternalID: "101", State: "stopped"})}
	inv := &LiveInventory{Reader: reader, Live: live}

	page, err := inv.ListVMs(context.Background(), ports.VMFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := page.Items[0]
	if got.State != "stopped" {
		t.Fatalf("state = %q, want stopped", got.State)
	}
	if got.LiveAt != readAt {
		t.Errorf("live_at = %v, want the moment the platform answered", got.LiveAt)
	}
	// A stopped guest showing 42% CPU is the contradiction that makes people
	// stop believing the page.
	if got.CPUPct != 0 || got.MemPct != 0 || got.UptimeS != 0 {
		t.Errorf("stopped VM still shows usage: %+v", got)
	}
}

func TestASilentPlatformLeavesTheSyncedRowAlone(t *testing.T) {
	// A guest the live read did not mention is not evidence of anything: a
	// node in maintenance, a migration in flight and a filtered read all look
	// identical from here.
	reader := &stubReader{page: syncedPage(vm("101", "running"), vm("102", "running"))}
	live := &stubLive{snaps: snapshot(ports.LiveVMState{ExternalID: "101", State: "stopped"})}
	inv := &LiveInventory{Reader: reader, Live: live}

	page, _ := inv.ListVMs(context.Background(), ports.VMFilter{})
	if page.Items[0].State != "stopped" {
		t.Errorf("the mentioned VM was not updated")
	}
	if page.Items[1].State != "running" {
		t.Errorf("an unmentioned VM was changed to %q", page.Items[1].State)
	}
	if !page.Items[1].LiveAt.IsZero() {
		t.Error("an unmentioned VM was marked as live-confirmed")
	}
}

func TestAnUnreachablePlatformDegradesToSyncedData(t *testing.T) {
	// The whole risk of reading on page load: this must be a non-event.
	reader := &stubReader{page: syncedPage(vm("101", "running"))}
	inv := &LiveInventory{Reader: reader, Live: &stubLive{snaps: nil}}

	page, err := inv.ListVMs(context.Background(), ports.VMFilter{})
	if err != nil {
		t.Fatalf("a failed live read must not fail the page: %v", err)
	}
	if page.Items[0].State != "running" || page.Items[0].CPUPct != 42 {
		t.Errorf("the synced row was not served intact: %+v", page.Items[0])
	}
	if !page.Items[0].LiveAt.IsZero() {
		t.Error("a row with no live read claims to be live")
	}
}

func TestTheOverlayTouchesNothingElse(t *testing.T) {
	// A live read carries no name, no tags and no opinion about whether a VM
	// has gone missing. Overwriting those would make the overlay a second,
	// worse copy of the inventory.
	item := vm("101", "running")
	item.Name = "web-01"
	item.PortalTags = []string{"prod"}
	item.SyncState = "missing"
	reader := &stubReader{page: syncedPage(item)}
	live := &stubLive{snaps: snapshot(ports.LiveVMState{ExternalID: "101", State: "running"})}
	inv := &LiveInventory{Reader: reader, Live: live}

	page, _ := inv.ListVMs(context.Background(), ports.VMFilter{})
	got := page.Items[0]
	if got.Name != "web-01" || len(got.PortalTags) != 1 || got.SyncState != "missing" {
		t.Fatalf("the overlay overwrote fields it has no opinion about: %+v", got)
	}
}

func TestTheSwitchTurnsItOff(t *testing.T) {
	reader := &stubReader{page: syncedPage(vm("101", "running"))}
	live := &stubLive{snaps: snapshot(ports.LiveVMState{ExternalID: "101", State: "stopped"})}
	inv := &LiveInventory{
		Reader: reader, Live: live,
		Enabled: func(context.Context) bool { return false },
	}

	page, _ := inv.ListVMs(context.Background(), ports.VMFilter{})
	if page.Items[0].State != "running" {
		t.Error("the live read ran with the setting off")
	}
	if len(live.asked) != 0 {
		t.Error("the platform was asked with the setting off")
	}
}

func TestOnlyThePlatformsOnThePageAreRead(t *testing.T) {
	// A filtered list of four VMs on one platform must not wake the estate.
	reader := &stubReader{page: syncedPage(vm("101", "running"), vm("102", "running"))}
	live := &stubLive{}
	inv := &LiveInventory{Reader: reader, Live: live}

	_, _ = inv.ListVMs(context.Background(), ports.VMFilter{})
	if len(live.asked) != 1 {
		t.Fatalf("snapshot called %d times, want 1", len(live.asked))
	}
	for _, id := range live.asked[0] {
		if id != platformA {
			t.Errorf("read platform %s, which is not on the page", id)
		}
	}
}

func TestAnEmptyPageAsksNothing(t *testing.T) {
	reader := &stubReader{page: syncedPage()}
	live := &stubLive{}
	inv := &LiveInventory{Reader: reader, Live: live}

	if _, err := inv.ListVMs(context.Background(), ports.VMFilter{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(live.asked) != 0 {
		t.Error("an empty page still read a platform")
	}
}

func TestAReadErrorIsPassedThroughUntouched(t *testing.T) {
	want := errors.New("database is on fire")
	reader := &stubReader{err: want}
	live := &stubLive{}
	inv := &LiveInventory{Reader: reader, Live: live}

	if _, err := inv.ListVMs(context.Background(), ports.VMFilter{}); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the underlying one", err)
	}
	if len(live.asked) != 0 {
		t.Error("a failed read still asked the platform")
	}
}

func TestGetVMOverlaysTheDetailPage(t *testing.T) {
	detail := ports.VMDetail{VMListItem: vm("101", "running"), Notes: "keep"}
	reader := &stubReader{detail: detail}
	live := &stubLive{snaps: snapshot(ports.LiveVMState{ExternalID: "101", State: "stopped"})}
	inv := &LiveInventory{Reader: reader, Live: live}

	got, err := inv.GetVM(context.Background(), detail.ID, identity.RoleOperator, uuid.New())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "stopped" {
		t.Errorf("state = %q, want stopped", got.State)
	}
	if got.Notes != "keep" {
		t.Errorf("the overlay disturbed a portal-owned field: %q", got.Notes)
	}
}
