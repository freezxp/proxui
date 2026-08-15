package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/edge"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// fakeEdgeProvider stands in for Cloudflare.
type fakeEdgeProvider struct {
	health    edge.Health
	verifyErr error
	tunnels   []edge.Tunnel
	closed    bool
}

func (f *fakeEdgeProvider) Verify(context.Context) (edge.Health, error) {
	return f.health, f.verifyErr
}
func (f *fakeEdgeProvider) Tunnels(context.Context) ([]edge.Tunnel, error) {
	return f.tunnels, nil
}
func (f *fakeEdgeProvider) Close() error { f.closed = true; return nil }

// fakeEdgeRepo records what would have been stored.
type fakeEdgeRepo struct {
	providers map[uuid.UUID]*publish.Provider
	creds     map[uuid.UUID]string
	conflict  bool
	health    []publish.Health
	failures  []int
}

func newFakeEdgeRepo() *fakeEdgeRepo {
	return &fakeEdgeRepo{
		providers: map[uuid.UUID]*publish.Provider{},
		creds:     map[uuid.UUID]string{},
	}
}

func (f *fakeEdgeRepo) Create(_ context.Context, p *publish.Provider, cred ports.SealedCredential) error {
	if f.conflict {
		return ports.ErrConflict
	}
	f.providers[p.ID] = p
	f.creds[p.ID] = string(cred.Sealed.Ciphertext)
	return nil
}
func (f *fakeEdgeRepo) Get(_ context.Context, id uuid.UUID) (*publish.Provider, error) {
	p, ok := f.providers[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return p, nil
}
func (f *fakeEdgeRepo) List(context.Context) ([]*publish.Provider, error) { return nil, nil }
func (f *fakeEdgeRepo) Update(context.Context, *publish.Provider) error   { return nil }
func (f *fakeEdgeRepo) RecordHealth(_ context.Context, _ uuid.UUID, h publish.Health,
	_ string, failures int, _ time.Time) error {
	f.health = append(f.health, h)
	f.failures = append(f.failures, failures)
	return nil
}
func (f *fakeEdgeRepo) Delete(context.Context, uuid.UUID) error { return nil }
func (f *fakeEdgeRepo) Credential(_ context.Context, id uuid.UUID, _ *crypto.Vault) (ports.PlainCredential, error) {
	if _, ok := f.creds[id]; !ok {
		return ports.PlainCredential{}, ports.ErrNotFound
	}
	return ports.PlainCredential{Secret: "token-value"}, nil
}
func (f *fakeEdgeRepo) ReplaceCredential(context.Context, uuid.UUID, ports.SealedCredential) error {
	return nil
}
func (f *fakeEdgeRepo) SaveSnapshot(context.Context, uuid.UUID, string, int, []byte, *uuid.UUID) error {
	return nil
}
func (f *fakeEdgeRepo) LatestSnapshot(context.Context, uuid.UUID, string) (ports.EdgeSnapshot, error) {
	return ports.EdgeSnapshot{}, ports.ErrNotFound
}

func testVault(t *testing.T) *crypto.Vault {
	t.Helper()
	key := make([]byte, crypto.MasterKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	v, err := crypto.NewVault(key, 1)
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}
	return v
}

func edgeDeps(t *testing.T, repo *fakeEdgeRepo, provider *fakeEdgeProvider) EdgeDeps {
	t.Helper()
	return EdgeDeps{
		Providers: repo,
		Vault:     testVault(t),
		Factory: func(edge.Credentials) (EdgeProvider, error) {
			return provider, nil
		},
		Audit: &fakeAudit{},
		Clock: &fakeClock{testNow},
	}
}

func manageableTunnel() edge.Tunnel {
	return edge.Tunnel{ID: "t1", Name: "home", RemotelyManaged: true, Connections: 2}
}

func TestRegisterStoresTheProviderAndSealsTheToken(t *testing.T) {
	repo := newFakeEdgeRepo()
	fake := &fakeEdgeProvider{health: edge.Health{
		Reachable: true, Authenticated: true, Tunnels: []edge.Tunnel{manageableTunnel()},
	}}
	h := &RegisterEdgeProvider{edgeDeps(t, repo, fake)}

	p, err := h.Handle(context.Background(), RegisterEdgeProviderInput{
		Name: "home", AccountID: "acct", Token: "cf-token",
		TunnelID: "t1", TunnelName: "home",
		AllowedZoneIDs: []string{"zone-1"},
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if repo.providers[p.ID] == nil {
		t.Fatal("the provider was not stored")
	}
	// The stored credential must not be the token in the clear.
	if stored := repo.creds[p.ID]; stored == "cf-token" || stored == "" {
		t.Errorf("the token was stored unsealed or not at all (%q)", stored)
	}
}

// Storing a credential that does not work produces something that looks
// configured and fails later, with the cause several steps away.
func TestRegisterRefusesACredentialThatDoesNotVerify(t *testing.T) {
	repo := newFakeEdgeRepo()
	fake := &fakeEdgeProvider{verifyErr: edge.Errorf(edge.ErrAuth, "verify_token", "token rejected")}
	h := &RegisterEdgeProvider{edgeDeps(t, repo, fake)}

	_, err := h.Handle(context.Background(), RegisterEdgeProviderInput{
		Name: "home", AccountID: "acct", Token: "bad",
	})
	if !errors.Is(err, edge.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	if len(repo.providers) != 0 {
		t.Error("a provider was stored despite the credential failing")
	}
}

// The single most consequential check: a locally-managed tunnel accepts every
// write and changes nothing, so selecting one must be refused at the point of
// selection rather than discovered at the first publish.
func TestRegisterRefusesALocallyManagedTunnel(t *testing.T) {
	repo := newFakeEdgeRepo()
	local := edge.Tunnel{ID: "t2", Name: "file-managed", RemotelyManaged: false, Connections: 1}
	fake := &fakeEdgeProvider{health: edge.Health{
		Reachable: true, Authenticated: true, Tunnels: []edge.Tunnel{local},
	}}
	h := &RegisterEdgeProvider{edgeDeps(t, repo, fake)}

	_, err := h.Handle(context.Background(), RegisterEdgeProviderInput{
		Name: "home", AccountID: "acct", Token: "cf-token", TunnelID: "t2",
	})
	if !errors.Is(err, edge.ErrNotManageable) {
		t.Fatalf("got %v, want ErrNotManageable", err)
	}
	// And the message has to explain why, since the fix is on Cloudflare's
	// side and is not obvious.
	if err != nil && !strings.Contains(err.Error(), "locally managed") {
		t.Errorf("error = %q, want it to explain the cause", err)
	}
	if len(repo.providers) != 0 {
		t.Error("a provider was stored with an unmanageable tunnel")
	}
}

func TestRegisterRefusesATunnelTheCredentialCannotSee(t *testing.T) {
	repo := newFakeEdgeRepo()
	fake := &fakeEdgeProvider{health: edge.Health{
		Reachable: true, Authenticated: true, Tunnels: []edge.Tunnel{manageableTunnel()},
	}}
	h := &RegisterEdgeProvider{edgeDeps(t, repo, fake)}

	_, err := h.Handle(context.Background(), RegisterEdgeProviderInput{
		Name: "home", AccountID: "acct", Token: "cf-token", TunnelID: "no-such-tunnel",
	})
	if !errors.Is(err, edge.ErrInvalidConfig) {
		t.Fatalf("got %v, want ErrInvalidConfig", err)
	}
}

// Registering without choosing a tunnel is legitimate: the credential has to
// work before its tunnels can be listed to pick from.
func TestRegisterWithoutATunnelIsAllowed(t *testing.T) {
	repo := newFakeEdgeRepo()
	fake := &fakeEdgeProvider{health: edge.Health{Reachable: true, Authenticated: true}}
	h := &RegisterEdgeProvider{edgeDeps(t, repo, fake)}

	p, err := h.Handle(context.Background(), RegisterEdgeProviderInput{
		Name: "home", AccountID: "acct", Token: "cf-token",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if p.Ready() {
		t.Error("a provider with no tunnel reported ready")
	}
}

func TestRegisterEdgeProviderValidatesItsInput(t *testing.T) {
	cases := map[string]RegisterEdgeProviderInput{
		"no name":    {AccountID: "acct", Token: "t"},
		"no account": {Name: "home", Token: "t"},
		"no token":   {Name: "home", AccountID: "acct"},
	}
	for name, in := range cases {
		repo := newFakeEdgeRepo()
		fake := &fakeEdgeProvider{health: edge.Health{Reachable: true, Authenticated: true}}
		h := &RegisterEdgeProvider{edgeDeps(t, repo, fake)}
		if _, err := h.Handle(context.Background(), in); err == nil {
			t.Errorf("%s: accepted invalid input", name)
		}
	}
}

// Testing a credential must store nothing — that is the whole point of having
// it as a separate command.
func TestTestingACredentialStoresNothing(t *testing.T) {
	repo := newFakeEdgeRepo()
	fake := &fakeEdgeProvider{health: edge.Health{
		Reachable: true, Authenticated: true, Tunnels: []edge.Tunnel{manageableTunnel()},
	}}
	audit := &fakeAudit{}
	deps := edgeDeps(t, repo, fake)
	deps.Audit = audit
	h := &TestEdgeCredential{deps}

	health, err := h.Handle(context.Background(), TestEdgeCredentialInput{
		AccountID: "acct", Token: "cf-token",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if !health.Authenticated || len(health.Tunnels) != 1 {
		t.Errorf("health = %+v", health)
	}
	if len(repo.providers) != 0 || len(repo.creds) != 0 {
		t.Error("a credential test stored something")
	}
	if !audit.has("edge_credential_tested") {
		t.Errorf("audit actions = %v, want edge_credential_tested", audit.actions())
	}
	if !fake.closed {
		t.Error("the provider was not closed")
	}
}

// Reachable but short a permission is degraded, not unreachable. Reporting it
// as unreachable sends someone to debug a network when the answer is a scope.
func TestVerifyRecordsDegradedForAMissingScope(t *testing.T) {
	repo := newFakeEdgeRepo()
	id := uuid.New()
	repo.providers[id] = &publish.Provider{ID: id, Name: "home", AccountID: "acct", IsEnabled: true}
	repo.creds[id] = "sealed"

	fake := &fakeEdgeProvider{health: edge.Health{
		Reachable: true, Authenticated: true,
		MissingScopes: []edge.ScopeGap{{Scope: "DNS: Edit (zone)", Blocks: "creating records"}},
	}}
	h := &VerifyEdgeProvider{edgeDeps(t, repo, fake)}

	if _, err := h.Handle(context.Background(), id); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(repo.health) != 1 || repo.health[0] != publish.HealthDegraded {
		t.Errorf("recorded health = %v, want degraded", repo.health)
	}
	if repo.failures[0] != 0 {
		t.Errorf("failures = %d; a missing scope is not a failed call", repo.failures[0])
	}
}

func TestVerifyRecordsUnreachableAndCountsFailures(t *testing.T) {
	repo := newFakeEdgeRepo()
	id := uuid.New()
	repo.providers[id] = &publish.Provider{
		ID: id, Name: "home", AccountID: "acct", IsEnabled: true, ConsecutiveFailures: 2,
	}
	repo.creds[id] = "sealed"

	fake := &fakeEdgeProvider{verifyErr: edge.Errorf(edge.ErrUnreachable, "verify_token", "dial tcp: timeout")}
	h := &VerifyEdgeProvider{edgeDeps(t, repo, fake)}

	if _, err := h.Handle(context.Background(), id); !errors.Is(err, edge.ErrUnreachable) {
		t.Fatalf("got %v, want ErrUnreachable", err)
	}
	if repo.health[0] != publish.HealthUnreachable {
		t.Errorf("recorded health = %v, want unreachable", repo.health[0])
	}
	if repo.failures[0] != 3 {
		t.Errorf("failures = %d, want the previous count plus one", repo.failures[0])
	}
}

// A breaker that is open must stop calls before they are made, or the
// backoff it exists to provide does not happen.
func TestAnOpenBreakerSuppressesTheCall(t *testing.T) {
	repo := newFakeEdgeRepo()
	id := uuid.New()
	repo.providers[id] = &publish.Provider{
		ID: id, Name: "home", AccountID: "acct", IsEnabled: true,
		BreakerOpenUntil: testNow.Add(time.Minute),
	}
	repo.creds[id] = "sealed"

	fake := &fakeEdgeProvider{health: edge.Health{Reachable: true, Authenticated: true}}
	h := &ListEdgeTunnels{edgeDeps(t, repo, fake)}

	if _, err := h.Handle(context.Background(), id); !errors.Is(err, edge.ErrUnreachable) {
		t.Fatalf("got %v, want the call to be suppressed", err)
	}
}

func TestADisabledProviderIsNotCalled(t *testing.T) {
	repo := newFakeEdgeRepo()
	id := uuid.New()
	repo.providers[id] = &publish.Provider{ID: id, Name: "home", AccountID: "acct", IsEnabled: false}
	repo.creds[id] = "sealed"

	h := &ListEdgeTunnels{edgeDeps(t, repo, &fakeEdgeProvider{})}
	if _, err := h.Handle(context.Background(), id); !errors.Is(err, publish.ErrInvalidProvider) {
		t.Fatalf("got %v, want ErrInvalidProvider", err)
	}
}

func TestRegisterSurfacesADuplicateName(t *testing.T) {
	repo := newFakeEdgeRepo()
	repo.conflict = true
	fake := &fakeEdgeProvider{health: edge.Health{Reachable: true, Authenticated: true}}
	h := &RegisterEdgeProvider{edgeDeps(t, repo, fake)}

	_, err := h.Handle(context.Background(), RegisterEdgeProviderInput{
		Name: "home", AccountID: "acct", Token: "cf-token",
	})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
}
