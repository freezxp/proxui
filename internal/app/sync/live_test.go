package sync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// Reading a platform on every page load is only defensible if it cannot
// stampede that platform and cannot make the portal slower than it. These
// tests are about those two properties; the third — that a failure degrades to
// synced data — is the one every case here shares.

type liveClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *liveClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *liveClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type livePlatforms struct{ p *inventory.Platform }

func (l livePlatforms) Get(context.Context, uuid.UUID) (*inventory.Platform, error) {
	return l.p, nil
}
func (livePlatforms) Create(context.Context, *inventory.Platform, ports.SealedCredential) error {
	return nil
}
func (livePlatforms) List(context.Context, bool) ([]*inventory.Platform, error) { return nil, nil }
func (livePlatforms) Update(context.Context, *inventory.Platform) error         { return nil }
func (livePlatforms) UpdateHealth(context.Context, *inventory.Platform) error   { return nil }
func (livePlatforms) SoftDelete(context.Context, uuid.UUID, time.Time) error    { return nil }
func (livePlatforms) Credential(context.Context, uuid.UUID, *crypto.Vault) (ports.PlainCredential, error) {
	return ports.PlainCredential{}, nil
}
func (livePlatforms) ReplaceCredential(context.Context, uuid.UUID, ports.SealedCredential) error {
	return nil
}

// memLiveCache is the Redis store in a map.
type memLiveCache struct {
	mu    sync.Mutex
	snaps map[uuid.UUID]ports.LiveSnapshot
}

func newMemLiveCache() *memLiveCache {
	return &memLiveCache{snaps: map[uuid.UUID]ports.LiveSnapshot{}}
}

func (m *memLiveCache) Put(_ context.Context, snap ports.LiveSnapshot, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps[snap.PlatformID] = snap
	return nil
}

func (m *memLiveCache) Forget(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.snaps, id)
	return nil
}

func (m *memLiveCache) Get(_ context.Context, id uuid.UUID) (ports.LiveSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, ok := m.snaps[id]
	if !ok {
		return ports.LiveSnapshot{}, ports.ErrNotFound
	}
	return snap, nil
}

// countingConnector answers ListVMs and counts how often it was asked.
type countingConnector struct {
	mu      sync.Mutex
	calls   int
	state   string
	err     error
	delay   time.Duration
	release chan struct{}
}

func (c *countingConnector) ListVMs(ctx context.Context) ([]connector.VMRecord, error) {
	c.mu.Lock()
	c.calls++
	release, delay, err, state := c.release, c.delay, c.err, c.state
	c.mu.Unlock()

	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return []connector.VMRecord{{ExternalID: "101", Name: "web-01", State: state, UptimeS: 42}}, nil
}

func (c *countingConnector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *countingConnector) Info() connector.Info                  { return connector.Info{} }
func (c *countingConnector) ValidateConfig(connector.Config) error { return nil }
func (c *countingConnector) Capabilities() []connector.Capability  { return nil }
func (c *countingConnector) Close() error                          { return nil }
func (c *countingConnector) TestConnection(context.Context) (connector.TestReport, error) {
	return connector.TestReport{}, nil
}

func newLiveFixture(t *testing.T, conn *countingConnector) (*Live, *liveClock, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	clock := &liveClock{t: time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)}
	platform := &inventory.Platform{ID: id, Name: "pve", IsEnabled: true}

	live := NewLive(livePlatforms{p: platform}, newMemLiveCache(),
		func(context.Context, *inventory.Platform) (connector.Connector, error) { return conn, nil },
		clock, zerolog.Nop())
	return live, clock, id
}

func TestLiveReadReturnsCurrentState(t *testing.T) {
	conn := &countingConnector{state: "stopped"}
	live, _, id := newLiveFixture(t, conn)

	snaps := live.Snapshot(context.Background(), []uuid.UUID{id})
	state, ok := snaps[id].States["101"]
	if !ok {
		t.Fatalf("no state for the guest: %+v", snaps)
	}
	if state.State != "stopped" {
		t.Errorf("state = %q, want stopped", state.State)
	}
}

func TestARecentReadIsReusedRatherThanRepeated(t *testing.T) {
	// Six requests during one page load must be one API call, or "live" turns
	// into a denial of service against your own cluster.
	conn := &countingConnector{state: "running"}
	live, clock, id := newLiveFixture(t, conn)

	for i := 0; i < 6; i++ {
		live.Snapshot(context.Background(), []uuid.UUID{id})
	}
	if got := conn.count(); got != 1 {
		t.Fatalf("%d platform calls for six reads, want 1", got)
	}

	// Past the reuse window, the platform is asked again.
	clock.advance(live.MinInterval + time.Second)
	live.Snapshot(context.Background(), []uuid.UUID{id})
	if got := conn.count(); got != 2 {
		t.Fatalf("%d calls after the interval elapsed, want 2", got)
	}
}

func TestConcurrentReadsCoalesceOntoOneCall(t *testing.T) {
	// Ten browsers refreshing at the same moment are one call: the late
	// arrivals wait on the in-flight read instead of starting their own.
	conn := &countingConnector{state: "running", release: make(chan struct{})}
	live, _, id := newLiveFixture(t, conn)

	var wg sync.WaitGroup
	results := make([]int, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snaps := live.Snapshot(context.Background(), []uuid.UUID{id})
			results[i] = len(snaps)
		}(i)
	}

	// Let them all pile up before the read completes.
	time.Sleep(50 * time.Millisecond)
	close(conn.release)
	wg.Wait()

	if got := conn.count(); got != 1 {
		t.Fatalf("%d platform calls for ten concurrent readers, want 1", got)
	}
	for i, n := range results {
		if n != 1 {
			t.Errorf("reader %d got %d snapshots, want 1", i, n)
		}
	}
}

func TestASlowPlatformIsBoundedAndFallsBack(t *testing.T) {
	// The failure mode that would make this feature a mistake: a page load
	// that waits on a wedged cluster.
	conn := &countingConnector{state: "running", delay: time.Second}
	live, _, id := newLiveFixture(t, conn)
	live.Timeout = 50 * time.Millisecond

	start := time.Now()
	snaps := live.Snapshot(context.Background(), []uuid.UUID{id})
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("a slow platform held the read for %v", elapsed)
	}
	// Nothing cached and the read timed out: no overlay, and the caller serves
	// the synced row.
	if len(snaps) != 0 {
		t.Errorf("a timed-out read produced an overlay: %+v", snaps)
	}
}

func TestAFailedReadFallsBackToTheLastGoodOne(t *testing.T) {
	conn := &countingConnector{state: "running"}
	live, clock, id := newLiveFixture(t, conn)

	live.Snapshot(context.Background(), []uuid.UUID{id})

	conn.mu.Lock()
	conn.err = errors.New("cluster unreachable")
	conn.mu.Unlock()
	clock.advance(live.MinInterval + time.Second)

	snaps := live.Snapshot(context.Background(), []uuid.UUID{id})
	if state, ok := snaps[id].States["101"]; !ok || state.State != "running" {
		t.Fatalf("a failed read discarded the last good answer: %+v", snaps)
	}
}

func TestAFailingPlatformIsNotRetriedOnEveryRequest(t *testing.T) {
	// A cluster that is down must cost one attempt per interval, not one per
	// page load, or an outage turns into a flood of doomed connections.
	conn := &countingConnector{err: errors.New("unreachable")}
	live, _, id := newLiveFixture(t, conn)

	for i := 0; i < 5; i++ {
		live.Snapshot(context.Background(), []uuid.UUID{id})
	}
	if got := conn.count(); got != 1 {
		t.Fatalf("%d attempts against a dead platform, want 1", got)
	}
}

func TestAnOpenBreakerIsNotDialledAtAll(t *testing.T) {
	conn := &countingConnector{state: "running"}
	id := uuid.New()
	clock := &liveClock{t: time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)}
	platform := &inventory.Platform{
		ID: id, Name: "pve", IsEnabled: true,
		BreakerOpenUntil: clock.Now().Add(5 * time.Minute),
	}
	live := NewLive(livePlatforms{p: platform}, newMemLiveCache(),
		func(context.Context, *inventory.Platform) (connector.Connector, error) { return conn, nil },
		clock, zerolog.Nop())

	live.Snapshot(context.Background(), []uuid.UUID{id})
	if got := conn.count(); got != 0 {
		t.Fatalf("a platform with an open breaker was dialled %d times", got)
	}
}

func TestADisabledPlatformIsNotRead(t *testing.T) {
	conn := &countingConnector{state: "running"}
	id := uuid.New()
	clock := &liveClock{t: time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)}
	platform := &inventory.Platform{ID: id, Name: "pve", IsEnabled: false}
	live := NewLive(livePlatforms{p: platform}, newMemLiveCache(),
		func(context.Context, *inventory.Platform) (connector.Connector, error) { return conn, nil },
		clock, zerolog.Nop())

	live.Snapshot(context.Background(), []uuid.UUID{id})
	if got := conn.count(); got != 0 {
		t.Fatalf("a disabled platform was read %d times", got)
	}
}

func TestNoPlatformsMeansNoWork(t *testing.T) {
	conn := &countingConnector{state: "running"}
	live, _, _ := newLiveFixture(t, conn)

	if got := live.Snapshot(context.Background(), nil); len(got) != 0 {
		t.Fatalf("got %d snapshots for no platforms", len(got))
	}
	if conn.count() != 0 {
		t.Error("a platform was read for an empty request")
	}
}

// A power action must not be followed by the snapshot taken just before it.
func TestForgetMakesTheNextReadReal(t *testing.T) {
	conn := &countingConnector{state: "running"}
	live, _, id := newLiveFixture(t, conn)

	live.Snapshot(context.Background(), []uuid.UUID{id})
	// Within the reuse window a second read would normally be free.
	live.Snapshot(context.Background(), []uuid.UUID{id})
	if got := conn.count(); got != 1 {
		t.Fatalf("%d calls before forgetting, want 1", got)
	}

	live.Forget(context.Background(), id)

	conn.mu.Lock()
	conn.state = "stopped"
	conn.mu.Unlock()

	snaps := live.Snapshot(context.Background(), []uuid.UUID{id})
	if got := conn.count(); got != 2 {
		t.Fatalf("%d calls after forgetting, want a fresh one", got)
	}
	if state := snaps[id].States["101"].State; state != "stopped" {
		t.Errorf("state = %q, want the state after the action", state)
	}
}
