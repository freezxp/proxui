package command

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/edge"
)

// fakeIngressReader returns a fixed routing table and records that it was
// asked, so a test can prove a read happened rather than a cache being used.
type fakeIngressReader struct {
	cfg    edge.Config
	err    error
	reads  int
	closed bool
}

func (f *fakeIngressReader) Ingress(context.Context, string) (edge.Config, error) {
	f.reads++
	return f.cfg, f.err
}
func (f *fakeIngressReader) Close() error { f.closed = true; return nil }

// snapshotRepo records snapshots alongside the usual provider behaviour.
type snapshotRepo struct {
	*fakeEdgeRepo
	saved []savedSnapshot
}

type savedSnapshot struct {
	tunnelID string
	version  int
	ingress  []byte
}

func (s *snapshotRepo) SaveSnapshot(_ context.Context, _ uuid.UUID, tunnelID string,
	version int, ingress []byte, _ *uuid.UUID) error {
	s.saved = append(s.saved, savedSnapshot{tunnelID, version, ingress})
	return nil
}

const portalHost = "vm.example.com"

func liveConfig() edge.Config {
	return edge.Config{
		Version: 34,
		Rules: []edge.Rule{
			{Hostname: portalHost, Service: "http://10.0.0.5:8080"},
			{Hostname: "app.example.com", Service: "http://10.0.0.9:80",
				Origin: map[string]any{"noTLSVerify": true}},
			{Service: "http_status:404"},
		},
	}
}

func safetyDeps(t *testing.T, reader *fakeIngressReader) (EdgeSafetyDeps, *snapshotRepo) {
	t.Helper()
	base := newFakeEdgeRepo()
	id := uuid.New()
	base.providers[id] = &publish.Provider{
		ID: id, Name: "home", AccountID: "acct", TunnelID: "t1", IsEnabled: true,
	}
	base.creds[id] = "sealed"
	repo := &snapshotRepo{fakeEdgeRepo: base}

	return EdgeSafetyDeps{
		Providers: repo,
		Factory: func(context.Context, uuid.UUID) (EdgeIngressReader, error) {
			return reader, nil
		},
		Audit:        &fakeAudit{},
		Clock:        &fakeClock{testNow},
		SelfHostname: portalHost,
	}, repo
}

func onlyProviderID(repo *snapshotRepo) uuid.UUID {
	for id := range repo.providers {
		return id
	}
	return uuid.Nil
}

func TestSnapshotStoresTheTableVerbatim(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()}
	deps, repo := safetyDeps(t, reader)
	h := &SnapshotEdgeIngress{deps}

	got, err := h.Handle(context.Background(), onlyProviderID(repo), Actor{})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Version != 34 || got.Rules != 3 {
		t.Errorf("result = %+v, want version 34 and 3 rules", got)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("%d snapshots saved, want 1", len(repo.saved))
	}

	// Settings the portal does not understand must survive, because a restore
	// that dropped them would put back a table that never existed.
	var restored []edge.Rule
	if err := json.Unmarshal(repo.saved[0].ingress, &restored); err != nil {
		t.Fatalf("the snapshot is not decodable: %v", err)
	}
	if len(restored) != 3 {
		t.Fatalf("%d rules in the snapshot, want 3", len(restored))
	}
	if restored[1].Origin["noTLSVerify"] != true {
		t.Errorf("per-rule origin settings were lost: %+v", restored[1].Origin)
	}
	if !reader.closed {
		t.Error("the reader was not closed")
	}
}

func TestPreviewWritesNothing(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()}
	deps, repo := safetyDeps(t, reader)
	h := &PreviewEdgeIngress{deps}

	_, err := h.Handle(context.Background(), onlyProviderID(repo), PreviewEdgeIngressInput{
		Desired: []publish.Rule{
			{Hostname: portalHost, Service: "http://10.0.0.5:8080"},
			{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
			{Hostname: "new.example.com", Service: "http://10.0.0.20:3000"},
		},
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(repo.saved) != 0 {
		t.Error("a preview saved a snapshot")
	}
}

func TestPreviewDescribesAnAddition(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()}
	deps, repo := safetyDeps(t, reader)
	h := &PreviewEdgeIngress{deps}

	got, err := h.Handle(context.Background(), onlyProviderID(repo), PreviewEdgeIngressInput{
		Desired: []publish.Rule{
			{Hostname: portalHost, Service: "http://10.0.0.5:8080"},
			{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
			{Hostname: "new.example.com", Service: "http://10.0.0.20:3000"},
		},
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if !got.Plan.Safe() {
		t.Fatalf("a safe addition was refused: %v", got.Plan.Refusal)
	}
	if got.Plan.Added != 1 || got.Plan.Removed != 0 {
		t.Errorf("added=%d removed=%d, want 1/0", got.Plan.Added, got.Plan.Removed)
	}
	if got.CurrentVersion != 34 {
		t.Errorf("current version = %d, want 34", got.CurrentVersion)
	}
}

// The reason this whole sprint comes before the write path: a proposed table
// that drops the portal's own rule has to be refused, with the diff still
// visible so the person can see what they nearly did.
func TestPreviewRefusesRemovingThePortalsOwnRule(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()}
	deps, repo := safetyDeps(t, reader)
	h := &PreviewEdgeIngress{deps}

	got, err := h.Handle(context.Background(), onlyProviderID(repo), PreviewEdgeIngressInput{
		Desired: []publish.Rule{{Hostname: "app.example.com", Service: "http://10.0.0.9:80"}},
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Plan.Safe() {
		t.Fatal("a plan removing the portal's rule was allowed")
	}
	if !errors.Is(got.Plan.Refusal, publish.ErrSelfRemoved) {
		t.Errorf("refusal = %v, want ErrSelfRemoved", got.Plan.Refusal)
	}
	if got.Plan.Removed != 1 {
		t.Error("the diff should still be computed for a refused plan")
	}
}

// With no configured hostname the guard must stay off rather than protecting
// an arbitrary rule — a portal reached another way has nothing to protect here.
func TestPreviewWithNoConfiguredHostnameDoesNotGuard(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()}
	deps, repo := safetyDeps(t, reader)
	deps.SelfHostname = ""
	h := &PreviewEdgeIngress{deps}

	got, err := h.Handle(context.Background(), onlyProviderID(repo), PreviewEdgeIngressInput{
		Desired: []publish.Rule{{Hostname: "app.example.com", Service: "http://10.0.0.9:80"}},
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if !got.Plan.Safe() {
		t.Errorf("the guard fired with no hostname configured: %v", got.Plan.Refusal)
	}
}

// PUB-31. Silently winning this race deletes somebody's app.
func TestPreviewReportsAStaleRead(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()} // live is version 34
	deps, repo := safetyDeps(t, reader)
	h := &PreviewEdgeIngress{deps}

	got, err := h.Handle(context.Background(), onlyProviderID(repo), PreviewEdgeIngressInput{
		ReadVersion: 33, // what the caller last saw
		Desired: []publish.Rule{
			{Hostname: portalHost, Service: "http://10.0.0.5:8080"},
			{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		},
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if !errors.Is(got.Stale, publish.ErrStaleRead) {
		t.Fatalf("stale = %v, want ErrStaleRead", got.Stale)
	}
	// The plan still comes back: the caller needs to see both what it wanted
	// and that the ground moved.
	if len(got.Plan.Entries) == 0 {
		t.Error("no plan was returned alongside the staleness report")
	}
}

func TestPreviewAcceptsAMatchingVersion(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()}
	deps, repo := safetyDeps(t, reader)
	h := &PreviewEdgeIngress{deps}

	got, err := h.Handle(context.Background(), onlyProviderID(repo), PreviewEdgeIngressInput{
		ReadVersion: 34,
		Desired:     []publish.Rule{{Hostname: portalHost, Service: "http://10.0.0.5:8080"}},
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if got.Stale != nil {
		t.Errorf("stale = %v, want nil for a matching version", got.Stale)
	}
}

// A cached routing table is the exact ingredient of a write that deletes
// someone else's rule, so every call must read fresh (PUB-30).
func TestEveryPreviewReadsLive(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()}
	deps, repo := safetyDeps(t, reader)
	h := &PreviewEdgeIngress{deps}
	id := onlyProviderID(repo)

	for i := 0; i < 3; i++ {
		if _, err := h.Handle(context.Background(), id, PreviewEdgeIngressInput{
			Desired: []publish.Rule{{Hostname: portalHost, Service: "http://10.0.0.5:8080"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if reader.reads != 3 {
		t.Errorf("%d reads for 3 previews; the table must never come from a cache", reader.reads)
	}
}

func TestSafetyCommandsRefuseAProviderWithNoTunnel(t *testing.T) {
	reader := &fakeIngressReader{cfg: liveConfig()}
	deps, repo := safetyDeps(t, reader)
	id := onlyProviderID(repo)
	repo.providers[id].TunnelID = ""

	if _, err := (&PreviewEdgeIngress{deps}).Handle(context.Background(), id,
		PreviewEdgeIngressInput{}); !errors.Is(err, publish.ErrNoTunnel) {
		t.Errorf("preview: got %v, want ErrNoTunnel", err)
	}
	if _, err := (&SnapshotEdgeIngress{deps}).Handle(context.Background(), id,
		Actor{}); !errors.Is(err, publish.ErrNoTunnel) {
		t.Errorf("snapshot: got %v, want ErrNoTunnel", err)
	}
}

func TestSafetyCommandsSurfaceAReadFailure(t *testing.T) {
	reader := &fakeIngressReader{err: edge.Errorf(edge.ErrUnreachable, "get_ingress", "timeout")}
	deps, repo := safetyDeps(t, reader)

	_, err := (&PreviewEdgeIngress{deps}).Handle(context.Background(), onlyProviderID(repo),
		PreviewEdgeIngressInput{})
	if !errors.Is(err, edge.ErrUnreachable) {
		t.Errorf("got %v, want ErrUnreachable", err)
	}
}

var _ ports.EdgeProviderRepository = (*snapshotRepo)(nil)
