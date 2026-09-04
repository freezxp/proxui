package postgres_test

// The storage half of ADR 0009. What matters here is not that rows round-trip
// but that the list behaves like a statement about the cluster as it is now:
// members that leave the cluster leave the list, and a platform that is deleted
// takes its addresses with it.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/infra/postgres"
)

func seedPlatform(t *testing.T, pool *postgres.Pool) (*postgres.PlatformRepository, *inventory.Platform) {
	t.Helper()
	repo := postgres.NewPlatformRepository(pool)
	sealed, err := testVault(t).Seal("not-a-real-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	p := &inventory.Platform{
		ID: uuid.New(), Name: "endpoints-" + uuid.NewString()[:8], Type: "proxmox",
		EndpointURL: "https://10.0.30.111:8006", Datacenter: "home", IsEnabled: true,
		TLSMode: "fingerprint", SyncIntervals: inventory.DefaultSyncIntervals(),
		CreatedAt: time.Now().UTC(),
	}
	if err := repo.Create(context.Background(), p,
		ports.SealedCredential{Kind: "api_token", TokenID: "root@pam!t", Sealed: sealed}); err != nil {
		t.Fatalf("create platform: %v", err)
	}
	return repo, p
}

func TestPlatformEndpointsReplaceDropsMembersThatLeft(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo, p := seedPlatform(t, pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	first := []ports.PlatformEndpoint{
		{Address: "10.0.30.111", Fingerprint: "aa", Source: "configured", RefreshedAt: now},
		{Address: "10.0.29.111", Fingerprint: "bb", Source: "discovered", RefreshedAt: now},
		{Address: "10.0.29.11", Fingerprint: "cc", Source: "discovered", RefreshedAt: now},
	}
	if err := repo.ReplaceEndpoints(ctx, p.ID, first, now); err != nil {
		t.Fatalf("ReplaceEndpoints: %v", err)
	}

	got, err := repo.Endpoints(ctx, p.ID)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("stored %d endpoints, want 3", len(got))
	}
	// The configured address sorts first, so the client can put it at the head
	// of the preference order without re-deriving which one it is.
	if got[0].Source != "configured" || got[0].Address != "10.0.30.111" {
		t.Errorf("first row = %+v, want the configured endpoint", got[0])
	}

	// A node removed from the cluster must not linger: every future failover
	// would spend a timeout on it.
	later := now.Add(time.Hour)
	if err := repo.ReplaceEndpoints(ctx, p.ID, []ports.PlatformEndpoint{
		{Address: "10.0.30.111", Fingerprint: "aa", Source: "configured", RefreshedAt: later},
		{Address: "10.0.29.111", Fingerprint: "b2", Source: "discovered", RefreshedAt: later},
	}, later); err != nil {
		t.Fatalf("ReplaceEndpoints (second): %v", err)
	}

	got, err = repo.Endpoints(ctx, p.ID)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stored %d endpoints after a member left, want 2", len(got))
	}
	for _, ep := range got {
		if ep.Address == "10.0.29.11" {
			t.Error("a member that left the cluster is still a failover candidate")
		}
		if ep.Address == "10.0.29.111" && ep.Fingerprint != "b2" {
			t.Errorf("fingerprint = %q, want the rediscovered pin", ep.Fingerprint)
		}
		if !ep.RefreshedAt.Equal(later) {
			t.Errorf("%s refreshed at %s, want %s", ep.Address, ep.RefreshedAt, later)
		}
	}
}

// An empty discovery is not evidence that the cluster has no members; it is
// evidence that the question could not be answered.
func TestPlatformEndpointsReplaceIgnoresAnEmptyList(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo, p := seedPlatform(t, pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.ReplaceEndpoints(ctx, p.ID, []ports.PlatformEndpoint{
		{Address: "10.0.29.111", Source: "discovered", RefreshedAt: now},
	}, now); err != nil {
		t.Fatalf("ReplaceEndpoints: %v", err)
	}
	if err := repo.ReplaceEndpoints(ctx, p.ID, nil, now.Add(time.Hour)); err != nil {
		t.Fatalf("ReplaceEndpoints (empty): %v", err)
	}

	got, err := repo.Endpoints(ctx, p.ID)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("stored %d endpoints, want the previous list kept", len(got))
	}
}

// A platform that goes takes its addresses with it: they are meaningless
// without the credential and the TLS policy they were discovered under.
func TestPlatformEndpointsCascadeOnPlatformDelete(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo, p := seedPlatform(t, pool)

	now := time.Now().UTC()
	if err := repo.ReplaceEndpoints(ctx, p.ID, []ports.PlatformEndpoint{
		{Address: "10.0.29.111", Source: "discovered", RefreshedAt: now},
	}, now); err != nil {
		t.Fatalf("ReplaceEndpoints: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM platforms WHERE id = $1`, p.ID); err != nil {
		t.Fatalf("delete platform: %v", err)
	}

	got, err := repo.Endpoints(ctx, p.ID)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("stored = %v, want the rows removed with the platform", got)
	}
}
