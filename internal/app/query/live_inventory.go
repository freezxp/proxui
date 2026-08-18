package query

import (
	"context"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// LiveInventory answers inventory reads with the freshest state a platform
// will admit to, falling back to the synced row.
//
// It wraps the read model rather than replacing it, and that shape is the
// whole design. The synced table stays the store of record — history,
// notifications, deleted-detection and every permission check keep working off
// it untouched — and what this adds is a thin, expiring overlay of the two or
// three fields that go stale in seconds.
//
// Every failure is the same failure: no overlay, and the caller gets exactly
// what it would have got before. A platform that is down makes the portal no
// slower and no less useful than the sync-only version was.
type LiveInventory struct {
	Reader ports.InventoryReader
	Live   ports.LiveStateReader

	// Enabled turns the live read off without a redeploy. A cluster that is
	// slow enough to hurt a page load is exactly when somebody needs to be
	// able to switch this off, and needing a restart to do it is a bad hour.
	//
	// Consulted per request, like the registration switch, so flipping it in
	// Settings takes effect on the next page rather than the next deploy.
	Enabled func(ctx context.Context) bool
}

var _ ports.InventoryReader = (*LiveInventory)(nil)

func (l *LiveInventory) live(ctx context.Context) bool {
	return l.Live != nil && (l.Enabled == nil || l.Enabled(ctx))
}

// ListVMs overlays the page with live state.
//
// The platforms to read are taken from the page itself, so a filtered list of
// four VMs on one platform reads one platform — not the estate.
func (l *LiveInventory) ListVMs(ctx context.Context, f ports.VMFilter) (ports.VMPage, error) {
	page, err := l.Reader.ListVMs(ctx, f)
	if err != nil || !l.live(ctx) || len(page.Items) == 0 {
		return page, err
	}

	ids := make([]uuid.UUID, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.PlatformID)
	}
	snaps := l.Live.Snapshot(ctx, ids)
	if len(snaps) == 0 {
		return page, nil
	}
	for i := range page.Items {
		applyLive(&page.Items[i], snaps)
	}
	return page, nil
}

// GetVM overlays one VM.
func (l *LiveInventory) GetVM(ctx context.Context, id uuid.UUID, role identity.Role,
	userID uuid.UUID) (ports.VMDetail, error) {
	detail, err := l.Reader.GetVM(ctx, id, role, userID)
	if err != nil || !l.live(ctx) {
		return detail, err
	}
	applyLive(&detail.VMListItem, l.Live.Snapshot(ctx, []uuid.UUID{detail.PlatformID}))
	return detail, nil
}

// Dashboard is served from the synced table alone.
//
// Deliberately: its counts are aggregates over the whole estate, so overlaying
// them would mean reading every platform on every dashboard load, and the
// numbers move on a timescale where a minute does not mislead anybody. The
// pages where a stale state costs something — the list and the detail, the two
// places with power buttons — are the ones that read live.
func (l *LiveInventory) Dashboard(ctx context.Context, role identity.Role,
	userID uuid.UUID) (ports.DashboardSummary, error) {
	return l.Reader.Dashboard(ctx, role, userID)
}

// The rest is pass-through: permissions, history and the portal-owned fields
// have no live equivalent and must keep coming from the store of record.

func (l *LiveInventory) CanAccessVM(ctx context.Context, id uuid.UUID, role identity.Role,
	userID uuid.UUID) (bool, error) {
	return l.Reader.CanAccessVM(ctx, id, role, userID)
}

func (l *LiveInventory) VMHistory(ctx context.Context, id uuid.UUID, limit int) ([]ports.HistoryEntry, error) {
	return l.Reader.VMHistory(ctx, id, limit)
}

func (l *LiveInventory) SetPortalTags(ctx context.Context, id uuid.UUID, tags []string) error {
	return l.Reader.SetPortalTags(ctx, id, tags)
}

func (l *LiveInventory) SetNotes(ctx context.Context, id uuid.UUID, notes string) error {
	return l.Reader.SetNotes(ctx, id, notes)
}

// applyLive replaces the fields a live read is authoritative about.
//
// Only those fields. A live read carries no name, no address, no tags, and
// overwriting them from a partial view is how an overlay turns into a second,
// worse copy of the inventory. `sync_state` is likewise left alone: whether a
// VM has gone missing is a judgement the reconciler makes over several runs,
// not something one read can decide.
func applyLive(item *ports.VMListItem, snaps map[uuid.UUID]ports.LiveSnapshot) {
	snap, ok := snaps[item.PlatformID]
	if !ok {
		return
	}
	state, ok := snap.States[item.ExternalID]
	if !ok {
		// The platform did not mention this guest. That is not evidence it is
		// gone — a filtered read, a node in maintenance, a race with a
		// migration all look the same from here — so the synced row stands.
		return
	}

	item.State = state.State
	item.UptimeS = state.UptimeS
	item.LiveAt = snap.ReadAt

	// A stopped guest reports no usage. Leaving the last synced percentages on
	// screen beside the word "stopped" is the kind of small contradiction that
	// makes people distrust the whole page.
	if state.State != "running" {
		item.CPUPct = 0
		item.MemPct = 0
		item.UptimeS = 0
	}
}
