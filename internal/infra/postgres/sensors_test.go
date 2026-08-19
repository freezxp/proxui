package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/telemetry"
	"github.com/freezxp/proxui/internal/infra/postgres"
)

func crit(f float64) *float64 { return &f }

// hostID reconciles the mock platform and returns one of its hosts.
func hostID(t *testing.T, f *syncFixture) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM hosts WHERE platform_id=$1 ORDER BY name LIMIT 1`, f.platform.ID).Scan(&id)
	if err != nil {
		t.Fatalf("no host to test against: %v", err)
	}
	return id
}

func TestSensorReadingsRoundTrip(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 2})
	f.reconcile(t)
	ctx := context.Background()
	repo := postgres.NewSensorRepository(f.pool)
	host := hostID(t, f)

	now := time.Now().UTC().Truncate(time.Second)
	write := func(at time.Time, pkg, nvme float64) {
		t.Helper()
		n, err := repo.Write(ctx, ports.SensorReadings{HostID: host, At: at, Readings: []telemetry.Reading{
			{Chip: "coretemp-isa-0000", Label: "Package id 0", Kind: telemetry.SensorTemp, Value: pkg, Crit: crit(100)},
			{Chip: "coretemp-isa-0000", Label: "Core 0", Kind: telemetry.SensorTemp, Value: pkg - 2, Crit: crit(100)},
			{Chip: "nvme-pci-0100", Label: "Composite", Kind: telemetry.SensorTemp, Value: nvme, Crit: crit(85)},
		}})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != 3 {
			t.Fatalf("wrote %d readings, want 3", n)
		}
	}
	write(now.Add(-2*time.Minute), 40, 30)
	write(now, 47, 38)

	// Latest is one row per sensor, the newest of each.
	latest, err := repo.Latest(ctx, host)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(latest.Readings) != 3 {
		t.Fatalf("got %d sensors, want 3: %+v", len(latest.Readings), latest.Readings)
	}
	for _, r := range latest.Readings {
		if r.Label == "Package id 0" && r.Value != 47 {
			t.Errorf("package = %v, want the newest reading 47", r.Value)
		}
		if r.Crit == nil {
			t.Errorf("%s lost its critical point on the way through the database", r.Name())
		}
	}

	// The summary is what a list row shows: the reading nearest its own limit.
	summaries, err := repo.Summaries(ctx, []uuid.UUID{host})
	if err != nil {
		t.Fatalf("Summaries: %v", err)
	}
	got := summaries[host]
	if got.Count != 3 || len(got.Chips) != 2 {
		t.Errorf("summary = %+v, want 3 readings across 2 chips", got)
	}
	if got.Hottest.Label != "Package id 0" {
		t.Errorf("hottest = %s (%v°C), want the package", got.Hottest.Label, got.Hottest.Value)
	}

	// And the evaluator's view, one reading per host.
	hottest, err := repo.HottestNow(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("HottestNow: %v", err)
	}
	if hottest[host].Value != 47 {
		t.Errorf("hottest now = %+v, want the 47°C package", hottest[host])
	}

	series, err := repo.Series(ctx, host, "coretemp-isa-0000", "Package id 0",
		now.Add(-time.Hour), now.Add(time.Minute), telemetry.ResolutionRaw)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(series) != 2 {
		t.Errorf("series has %d points, want both readings", len(series))
	}
}

// A reading older than the staleness window is not "the current temperature".
// A node that stopped answering an hour ago must not keep showing 47°C.
func TestStaleReadingsAreNotCurrent(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 1})
	f.reconcile(t)
	ctx := context.Background()
	repo := postgres.NewSensorRepository(f.pool)
	host := hostID(t, f)

	old := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := repo.Write(ctx, ports.SensorReadings{HostID: host, At: old, Readings: []telemetry.Reading{
		{Chip: "coretemp-isa-0000", Label: "Package id 0", Kind: telemetry.SensorTemp, Value: 47},
	}}); err != nil {
		t.Fatal(err)
	}

	latest, err := repo.Latest(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Readings) != 0 {
		t.Errorf("a two-hour-old reading is still being reported as current: %+v", latest.Readings)
	}
}

func TestNodeKeyIsPinnedOnceAndForgettable(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 1})
	f.reconcile(t)
	ctx := context.Background()
	repo := postgres.NewSensorRepository(f.pool)
	host := hostID(t, f)

	if _, err := repo.Get(ctx, host); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound before anything was pinned", err)
	}

	first := ports.NodeSSH{
		HostID: host, Address: "10.0.30.111", SSHUser: "root",
		Algorithm: "ssh-ed25519", Fingerprint: "SHA256:first",
		PublicKey: []byte("first"), FirstSeenAt: time.Now().UTC(),
	}
	if err := repo.Pin(ctx, first); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	// A second pin must not overwrite the first: an upsert here would turn
	// every reconnection into a re-pin and defeat the mismatch check entirely.
	second := first
	second.Fingerprint, second.PublicKey = "SHA256:second", []byte("second")
	if err := repo.Pin(ctx, second); err != nil {
		t.Fatalf("Pin again: %v", err)
	}
	got, err := repo.Get(ctx, host)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Fingerprint != "SHA256:first" {
		t.Errorf("fingerprint = %q; a second pin overwrote the first", got.Fingerprint)
	}

	if err := repo.Forget(ctx, host); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := repo.Get(ctx, host); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("got %v after Forget, want ErrNotFound", err)
	}
}

// A node with no key installed has no pin, and the reason it failed is the one
// thing the host page has to show. So an attempt has to record without one.
func TestAttemptsRecordWithoutAPin(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 1})
	f.reconcile(t)
	ctx := context.Background()
	repo := postgres.NewSensorRepository(f.pool)
	host := hostID(t, f)

	at := time.Now().UTC().Truncate(time.Second)
	if err := repo.RecordAttempt(ctx, host, at, "the node refused the portal's key"); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	got, err := repo.Get(ctx, host)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastError == "" || !got.LastOKAt.IsZero() {
		t.Errorf("record = %+v, want the failure and no success", got)
	}

	// And a success clears it, or a node that was fixed keeps showing why it
	// used to be broken.
	if err := repo.RecordAttempt(ctx, host, at.Add(time.Minute), ""); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	got, err = repo.Get(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError != "" || got.LastOKAt.IsZero() {
		t.Errorf("record = %+v, want the failure cleared and a success time", got)
	}
}

func TestOnlineHostsAreListedForPolling(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 2})
	f.reconcile(t)
	repo := postgres.NewSensorRepository(f.pool)

	hosts, err := repo.OnlineHosts(context.Background(), f.platform.ID)
	if err != nil {
		t.Fatalf("OnlineHosts: %v", err)
	}
	if len(hosts) == 0 {
		t.Fatal("no hosts to poll after a reconcile")
	}
	for _, h := range hosts {
		if h.ExternalID == "" || h.Name == "" {
			t.Errorf("host %+v is missing what the collector needs to dial it", h)
		}
	}
}
