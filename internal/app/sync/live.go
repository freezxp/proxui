package sync

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

// Live reads current guest state from a platform, on demand.
//
// This exists because the periodic sync is a minute wide and a power button is
// not: an operator who clicks Shut down and then watches a row that still says
// running for another fifty seconds concludes the portal is broken, and clicks
// again.
//
// Three properties make reading on demand safe to put in front of a page load,
// and all three are the point of this type rather than incidental to it:
//
//   - **It never writes to `vms`.** The reconciler decides what changed by
//     diffing that table; a live read that updated it first would leave nothing
//     to diff, and the vm.state_changed events that history and notifications
//     are built on would quietly stop. What this produces is an overlay with a
//     short life.
//   - **It cannot stampede a platform.** Concurrent callers coalesce onto one
//     in-flight read, and a completed read is reused for MinInterval. A list
//     page, a detail page and six browsers refreshing at once are one API call.
//   - **It cannot make the portal slower than the platform.** Every read is
//     bounded by Timeout, a platform whose breaker is open is skipped without
//     dialling, and any failure yields no overlay at all — the caller serves
//     the synced row, which is exactly what it would have served before this
//     type existed.
type Live struct {
	Platforms ports.PlatformRepository
	Cache     ports.LiveStateStore
	// Connect opens a connector for a platform. Injected rather than taken as a
	// *Service so this can be tested without a sync engine.
	Connect func(ctx context.Context, p *inventory.Platform) (connector.Connector, error)
	Clock   ports.Clock
	Log     zerolog.Logger

	// Timeout bounds one platform read. Short: this runs inside a page load,
	// and a stale answer now beats a fresh one after the user has given up.
	Timeout time.Duration
	// MinInterval is how long a completed read is reused before another is
	// made. It is the whole difference between "live" and "a denial of service
	// against your own cluster".
	MinInterval time.Duration
	// TTL is how long an overlay stays usable if no read succeeds after it.
	// Beyond this the portal would rather show the synced value than a live
	// one that has quietly stopped being live.
	TTL time.Duration

	mu       sync.Mutex
	inFlight map[uuid.UUID]*readCall
	recent   map[uuid.UUID]readResult
}

// readCall is one in-flight platform read that late arrivals wait on instead of
// starting their own.
type readCall struct {
	done chan struct{}
	snap ports.LiveSnapshot
	ok   bool
}

// readResult is the outcome of the last completed read, kept so the reuse
// window is served from memory.
//
// The snapshot is held rather than re-fetched because this process just built
// it: going back to Redis inside MinInterval costs a round-trip and a decode of
// the whole cluster's guest map to recover a value that is still on the heap.
// Redis stays the cross-replica share and the TTL fallback, which is what it is
// good at.
//
// It is recorded on failure too (with ok false): a platform that just timed out
// must not be dialled once per page load while it is down.
type readResult struct {
	at   time.Time
	snap ports.LiveSnapshot
	ok   bool
}

// Defaults chosen for a page load: fast enough that nobody notices, long
// enough that a busy cluster still answers.
const (
	DefaultLiveTimeout     = 2500 * time.Millisecond
	DefaultLiveMinInterval = 3 * time.Second
	DefaultLiveTTL         = 60 * time.Second
)

// NewLive builds the reader with the shipped bounds.
func NewLive(platforms ports.PlatformRepository, cache ports.LiveStateStore,
	connect func(context.Context, *inventory.Platform) (connector.Connector, error),
	clock ports.Clock, log zerolog.Logger) *Live {
	return &Live{
		Platforms: platforms, Cache: cache, Connect: connect, Clock: clock, Log: log,
		Timeout: DefaultLiveTimeout, MinInterval: DefaultLiveMinInterval, TTL: DefaultLiveTTL,
		inFlight: map[uuid.UUID]*readCall{},
		recent:   map[uuid.UUID]readResult{},
	}
}

var _ ports.LiveStateReader = (*Live)(nil)

// Snapshot returns what is currently known about the given platforms, reading
// from them if the cached answer is older than MinInterval.
//
// It never returns an error. Every failure mode - unreachable, slow, breaker
// open, no such platform - is the same answer: no overlay for that platform,
// and the caller uses the synced row.
func (l *Live) Snapshot(ctx context.Context, platformIDs []uuid.UUID) map[uuid.UUID]ports.LiveSnapshot {
	out := map[uuid.UUID]ports.LiveSnapshot{}
	if len(platformIDs) == 0 {
		return out
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range uniqueIDs(platformIDs) {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			snap, ok := l.forPlatform(ctx, id)
			if !ok {
				return
			}
			mu.Lock()
			out[id] = snap
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}

// forPlatform returns one platform's overlay, reading it if it is due.
func (l *Live) forPlatform(ctx context.Context, id uuid.UUID) (ports.LiveSnapshot, bool) {
	now := l.Clock.Now()

	// One critical section decides all three outcomes, because only a check
	// that still holds the lock when it registers the call is race-free.
	l.mu.Lock()
	// A recent read is reused without touching Redis or the platform: this is
	// the common case on a page that fires several requests at once. A recent
	// *failure* is reused just as deliberately — asking again now would put one
	// doomed connection on every page load for as long as the platform is down.
	if last, seen := l.recent[id]; seen && now.Sub(last.at) < l.MinInterval {
		l.mu.Unlock()
		return last.snap, last.ok
	}
	// Someone else is already asking this platform. Wait for their answer
	// rather than adding a second call to a cluster that is evidently busy.
	if existing := l.inFlight[id]; existing != nil {
		l.mu.Unlock()
		select {
		case <-existing.done:
			return existing.snap, existing.ok
		case <-ctx.Done():
			return ports.LiveSnapshot{}, false
		}
	}
	call := &readCall{done: make(chan struct{})}
	l.inFlight[id] = call
	l.mu.Unlock()

	snap, ok := l.read(ctx, id)

	l.mu.Lock()
	call.snap, call.ok = snap, ok
	close(call.done)
	delete(l.inFlight, id)
	l.recent[id] = readResult{at: l.Clock.Now(), snap: snap, ok: ok}
	l.mu.Unlock()

	return snap, ok
}

// read asks the platform, and falls back to whatever is still cached.
func (l *Live) read(ctx context.Context, id uuid.UUID) (ports.LiveSnapshot, bool) {
	// Fetched only where it is used. On the success path — the common one — the
	// snapshot is about to be replaced, so reading it first would be a Redis
	// round-trip and a full-cluster decode thrown away every time.
	fallback := func() (ports.LiveSnapshot, bool) {
		cached, err := l.Cache.Get(ctx, id)
		return cached, err == nil
	}

	platform, err := l.Platforms.Get(ctx, id)
	if err != nil || !platform.IsEnabled {
		return fallback()
	}
	// A platform the breaker has given up on is not dialled: the breaker
	// exists precisely so that a dead cluster costs one failure per window
	// rather than one per request.
	if platform.BreakerOpen(l.Clock.Now()) {
		return fallback()
	}

	// Detached from the request: a page load that is cancelled halfway must not
	// leave a half-read connection, and the answer is worth caching for the
	// next caller even if this one has gone.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.Timeout)
	defer cancel()

	conn, err := l.Connect(readCtx, platform)
	if err != nil {
		l.Log.Debug().Err(err).Str("platform", platform.Name).Msg("live read: connect failed")
		return fallback()
	}
	defer conn.Close()

	lister, ok := conn.(connector.VirtualMachineCollector)
	if !ok {
		return fallback()
	}

	// One call. On Proxmox this is /cluster/resources, which answers for every
	// guest in the cluster at once - which is what makes reading on demand
	// affordable at all.
	records, err := lister.ListVMs(readCtx)
	if err != nil {
		l.Log.Debug().Err(err).Str("platform", platform.Name).Msg("live read: listing failed")
		return fallback()
	}

	snap := ports.LiveSnapshot{
		PlatformID: id, ReadAt: l.Clock.Now(),
		States: make(map[string]ports.LiveVMState, len(records)),
	}
	for _, rec := range records {
		snap.States[rec.ExternalID] = ports.LiveVMState{
			ExternalID: rec.ExternalID,
			State:      rec.State,
			UptimeS:    rec.UptimeS,
		}
	}

	if err := l.Cache.Put(context.WithoutCancel(ctx), snap, l.TTL); err != nil {
		// The read still stands; only the sharing of it failed.
		l.Log.Debug().Err(err).Msg("live read: caching failed")
	}
	return snap, true
}

// Forget drops what is known about a platform so the next read is a real one.
//
// The portal calls this when it has just changed something itself. Without it,
// an operator who clicks Shut down and lands back on the list would be shown
// the snapshot taken moments before their own action - the one case where a
// few seconds of reuse is not a saving but a lie.
func (l *Live) Forget(ctx context.Context, platformID uuid.UUID) {
	l.mu.Lock()
	delete(l.recent, platformID)
	l.mu.Unlock()
	if err := l.Cache.Forget(ctx, platformID); err != nil {
		l.Log.Debug().Err(err).Msg("live read: could not drop the cached snapshot")
	}
}

func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
