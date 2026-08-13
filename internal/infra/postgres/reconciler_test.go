package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	appsync "github.com/freezxp/proxui/internal/app/sync"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/connectors/mock"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/infra/crypto"
	"github.com/freezxp/proxui/internal/infra/postgres"
)

// These exercise the reconciler against a real database and the mock platform:
// the combination the design calls the proof that the core is platform
// independent (docs/09 §9.5). No hypervisor is involved.

func testPool(t *testing.T) *postgres.Pool {
	t.Helper()
	dsn := os.Getenv("PROXUI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PROXUI_TEST_DATABASE_URL not set; skipping database integration tests")
	}
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, dsn, zerolog.Nop()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func testVault(t *testing.T) *crypto.Vault {
	t.Helper()
	key := make([]byte, crypto.MasterKeySize)
	for i := range key {
		key[i] = byte(i)
	}
	v, err := crypto.NewVault(key, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	return v
}

type syncFixture struct {
	pool      *postgres.Pool
	platform  *inventory.Platform
	conn      *mock.Connector
	reconcile func(t *testing.T) appsync.Result
}

func newSyncFixture(t *testing.T, extra map[string]any) *syncFixture {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	platforms := postgres.NewPlatformRepository(pool)
	assets := postgres.NewAssetRepository(pool)
	runs := postgres.NewSyncRepository(pool)
	vault := testVault(t)

	p := &inventory.Platform{
		ID: uuid.New(), Name: "mock-" + uuid.NewString()[:8], Type: mock.Type,
		EndpointURL: "mock://local", Datacenter: "test", IsEnabled: true,
		TLSMode: "verify", SyncIntervals: inventory.DefaultSyncIntervals(),
		CreatedAt: time.Now().UTC(),
	}
	sealed, err := vault.Seal("not-a-real-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := platforms.Create(ctx, p, ports.SealedCredential{Kind: "api_token", TokenID: "t", Sealed: sealed}); err != nil {
		t.Fatalf("create platform: %v", err)
	}

	c, err := mock.New(connector.Config{Endpoint: "mock://local", Extra: extra}, connector.Credentials{}, connector.Options{})
	if err != nil {
		t.Fatalf("mock connector: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	rec := &appsync.Reconciler{
		Platforms: platforms, Assets: assets, Runs: runs,
		Clock: ports.SystemClock{}, Log: zerolog.Nop(),
	}

	return &syncFixture{
		pool: pool, platform: p, conn: c.(*mock.Connector),
		reconcile: func(t *testing.T) appsync.Result {
			t.Helper()
			result, err := rec.Reconcile(context.Background(), p, c, "test")
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			return result
		},
	}
}

func (f *syncFixture) countVMs(t *testing.T, state string) int {
	t.Helper()
	var n int
	err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM vms WHERE platform_id=$1 AND sync_state=$2::sync_state`,
		f.platform.ID, state).Scan(&n)
	if err != nil {
		t.Fatalf("count vms: %v", err)
	}
	return n
}

func TestReconcilePersistsInventory(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 8, "host_count": 3})

	result := f.reconcile(t)
	if result.Status != "success" {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if result.Stats.VMs != 8 || result.Stats.Hosts != 3 {
		t.Errorf("stats = %+v, want 8 VMs and 3 hosts", result.Stats)
	}
	if result.Stats.Added != 11 {
		t.Errorf("added = %d, want 11 (8 VMs + 3 hosts) on a first run", result.Stats.Added)
	}
	if got := f.countVMs(t, "active"); got != 8 {
		t.Errorf("%d active VMs persisted, want 8", got)
	}

	// VMs must be linked to the host they run on, or the UI cannot group them.
	var linked int
	err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM vms WHERE platform_id=$1 AND host_id IS NOT NULL`, f.platform.ID).Scan(&linked)
	if err != nil {
		t.Fatalf("count linked: %v", err)
	}
	if linked != 8 {
		t.Errorf("%d VMs linked to a host, want 8", linked)
	}
}

// A second run over an unchanged fleet must be a no-op: if it recorded changes,
// every sync would fill the history table with noise forever.
func TestSecondRunOverUnchangedFleetIsQuiet(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 6})

	f.reconcile(t)
	second := f.reconcile(t)

	if second.Stats.Added != 0 || second.Stats.Changed != 0 {
		t.Errorf("second run reported added=%d changed=%d, want zero for an unchanged fleet",
			second.Stats.Added, second.Stats.Changed)
	}

	var historyRows int
	err := f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM asset_state_history h
		JOIN vms v ON v.id = h.asset_id
		WHERE v.platform_id=$1 AND h.field NOT IN ('_created')`, f.platform.ID).Scan(&historyRows)
	if err != nil {
		t.Fatalf("count history: %v", err)
	}
	if historyRows != 0 {
		t.Errorf("%d change-history rows after an unchanged run, want 0", historyRows)
	}
}

func TestStateChangeIsRecordedAndPublished(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 5})
	ctx := context.Background()
	f.reconcile(t)

	// Flip one VM's power state, exactly as a platform would.
	vms, err := f.conn.ListVMs(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	target := vms[0]
	action := connector.PowerStop
	if target.State != "running" {
		action = connector.PowerStart
	}
	if _, err := f.conn.Power(ctx, connector.VMRef{ExternalID: target.ExternalID}, action); err != nil {
		t.Fatalf("power: %v", err)
	}

	second := f.reconcile(t)
	if second.Stats.Changed != 1 {
		t.Fatalf("changed = %d, want 1", second.Stats.Changed)
	}

	var field, oldVal, newVal string
	err = f.pool.QueryRow(ctx, `
		SELECT h.field, coalesce(h.old_value,''), coalesce(h.new_value,'')
		FROM asset_state_history h JOIN vms v ON v.id=h.asset_id
		WHERE v.platform_id=$1 AND v.external_id=$2 AND h.field='state'`,
		f.platform.ID, target.ExternalID).Scan(&field, &oldVal, &newVal)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if oldVal == newVal {
		t.Errorf("history recorded %q -> %q, expected a real transition", oldVal, newVal)
	}

	// The change must also be queued for notification, in the same transaction.
	var events int
	err = f.pool.QueryRow(ctx, `
		SELECT count(*) FROM events_outbox
		WHERE event_type='vm.state_changed'
		  AND payload->>'external_id'=$1 AND payload->>'platform_id'=$2`,
		target.ExternalID, f.platform.ID.String()).Scan(&events)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("%d vm.state_changed events queued, want 1", events)
	}
}

// The three-strike rule is what stops an API hiccup deleting real inventory.
func TestDeletedVMNeedsThreeMisses(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 4})
	ctx := context.Background()
	f.reconcile(t)

	vms, _ := f.conn.ListVMs(ctx)
	gone := vms[0]
	f.conn.RemoveVM(gone.ExternalID)

	for i := 1; i < inventory.MissingThreshold; i++ {
		result := f.reconcile(t)
		if result.Stats.Deleted != 0 {
			t.Fatalf("VM deleted after %d misses, want %d", i, inventory.MissingThreshold)
		}
		if result.Stats.Missing != 1 {
			t.Errorf("missing = %d after miss %d, want 1", result.Stats.Missing, i)
		}
		if got := f.countVMs(t, "missing"); got != 1 {
			t.Errorf("%d VMs marked missing, want 1", got)
		}
	}

	final := f.reconcile(t)
	if final.Stats.Deleted != 1 {
		t.Fatalf("deleted = %d on miss %d, want 1", final.Stats.Deleted, inventory.MissingThreshold)
	}
	if got := f.countVMs(t, "deleted"); got != 1 {
		t.Errorf("%d VMs marked deleted, want 1", got)
	}

	var events int
	err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM events_outbox
		WHERE event_type='vm.deleted'
		  AND payload->>'external_id'=$1 AND payload->>'platform_id'=$2`,
		gone.ExternalID, f.platform.ID.String()).Scan(&events)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("%d vm.deleted events, want 1", events)
	}
}

// A VM that reappears must come back rather than staying missing forever.
func TestMissingVMRecovers(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	ctx := context.Background()
	f.reconcile(t)

	vms, _ := f.conn.ListVMs(ctx)
	f.conn.RemoveVM(vms[0].ExternalID)
	f.reconcile(t)
	if got := f.countVMs(t, "missing"); got != 1 {
		t.Fatalf("%d missing VMs, want 1", got)
	}

	f.conn.RestoreVMs()
	f.reconcile(t)
	if got := f.countVMs(t, "missing"); got != 0 {
		t.Errorf("%d VMs still missing after the platform reported them again", got)
	}
	if got := f.countVMs(t, "active"); got != 3 {
		t.Errorf("%d active VMs after recovery, want 3", got)
	}
}

// The whole point of the fatal-listing rule: a failing platform must not look
// like an empty one, or three failures would delete the entire inventory.
func TestListingFailureDoesNotSweepInventory(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 5})
	f.reconcile(t)

	f.conn.SetFault(mock.FaultUnreachable)
	result, err := (&appsync.Reconciler{
		Platforms: postgres.NewPlatformRepository(f.pool),
		Assets:    postgres.NewAssetRepository(f.pool),
		Runs:      postgres.NewSyncRepository(f.pool),
		Clock:     ports.SystemClock{}, Log: zerolog.Nop(),
	}).Reconcile(context.Background(), f.platform, f.conn, "test")

	if err == nil {
		t.Fatal("an unreachable platform produced no error")
	}
	if result.Status != "failed" {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if got := f.countVMs(t, "active"); got != 5 {
		t.Errorf("%d VMs still active after a failed run, want all 5 untouched", got)
	}
	if got := f.countVMs(t, "missing"); got != 0 {
		t.Errorf("%d VMs marked missing by a failed listing; a network blip would erase inventory", got)
	}
}

// Portal-owned fields are the one thing sync must never touch.
func TestSyncPreservesPortalOwnedFields(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	ctx := context.Background()
	f.reconcile(t)

	_, err := f.pool.Exec(ctx, `
		UPDATE vms SET portal_tags=ARRAY['tier:frontend'], notes='handle with care'
		WHERE platform_id=$1`, f.platform.ID)
	if err != nil {
		t.Fatalf("set portal fields: %v", err)
	}

	// Force a change so the update path runs, not just the cheap touch.
	vms, _ := f.conn.ListVMs(ctx)
	if _, err := f.conn.Power(ctx, connector.VMRef{ExternalID: vms[0].ExternalID}, connector.PowerStop); err != nil {
		t.Fatalf("power: %v", err)
	}
	f.reconcile(t)

	var tags []string
	var notes string
	err = f.pool.QueryRow(ctx,
		`SELECT portal_tags, notes FROM vms WHERE platform_id=$1 AND external_id=$2`,
		f.platform.ID, vms[0].ExternalID).Scan(&tags, &notes)
	if err != nil {
		t.Fatalf("read portal fields: %v", err)
	}
	if len(tags) != 1 || tags[0] != "tier:frontend" || notes != "handle with care" {
		t.Errorf("sync overwrote portal-owned fields: tags=%v notes=%q", tags, notes)
	}
}

func TestSyncRunsAreRecorded(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 2})
	result := f.reconcile(t)

	runs, err := postgres.NewSyncRepository(f.pool).ListRuns(context.Background(), f.platform.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs recorded, want 1", len(runs))
	}
	run := runs[0]
	if run.ID != result.RunID || run.Status != "success" || run.Kind != "inventory" {
		t.Errorf("run = %+v, want the successful inventory run %d", run, result.RunID)
	}
	if run.FinishedAt.IsZero() {
		t.Error("run was never finalized; it would sit in 'running' forever")
	}
	if run.Stats["vms"] == nil {
		t.Error("run stats did not record VM counts")
	}
}

func TestCredentialsRoundTripThroughVault(t *testing.T) {
	pool := testPool(t)
	repo := postgres.NewPlatformRepository(pool)
	vault := testVault(t)
	ctx := context.Background()

	secret := "pve-token-secret-value"
	sealed, err := vault.Seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	p := &inventory.Platform{
		ID: uuid.New(), Name: "cred-" + uuid.NewString()[:8], Type: "proxmox",
		EndpointURL: "https://10.0.30.111:8006", IsEnabled: true, TLSMode: "fingerprint",
		SyncIntervals: inventory.DefaultSyncIntervals(), CreatedAt: time.Now().UTC(),
	}
	if err := repo.Create(ctx, p, ports.SealedCredential{Kind: "api_token", TokenID: "proxui@pve!portal", Sealed: sealed}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Credential(ctx, p.ID, vault)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if got.Secret != secret || got.TokenID != "proxui@pve!portal" {
		t.Errorf("credential = %+v, want the original token", got)
	}

	// The stored ciphertext must not contain the plaintext.
	var ciphertext []byte
	if err := pool.QueryRow(ctx, `SELECT ciphertext FROM platform_credentials WHERE platform_id=$1`, p.ID).Scan(&ciphertext); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if string(ciphertext) == secret {
		t.Fatal("the credential was stored in plaintext")
	}
}
