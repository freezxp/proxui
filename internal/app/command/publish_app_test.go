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
)

// fakeWriter models the provider closely enough to test the ordering that
// matters: what was written, in what order, and what was put back.
type fakeWriter struct {
	cfg edge.Config

	writes    []edge.Config // every ReplaceIngress, in order
	writeErr  error         // fails the first ingress write
	dnsErr    error         // fails CreateTunnelDNS
	findErr   error
	deleteErr error
	rollbackE error // fails the restoring write specifically

	existing   *edge.DNSRecord
	created    []string
	deleted    []string
	zones      []edge.Zone
	zonesErr   error
	closed     bool
	writeCalls int
}

func (f *fakeWriter) Ingress(context.Context, string) (edge.Config, error) { return f.cfg, nil }
func (f *fakeWriter) Close() error                                         { f.closed = true; return nil }

func (f *fakeWriter) ReplaceIngress(_ context.Context, _ string, cfg edge.Config) error {
	f.writeCalls++
	if f.writeCalls == 1 && f.writeErr != nil {
		return f.writeErr
	}
	if f.writeCalls > 1 && f.rollbackE != nil {
		return f.rollbackE
	}
	f.writes = append(f.writes, cfg)
	return nil
}

func (f *fakeWriter) Zones(context.Context) ([]edge.Zone, error) {
	if f.zonesErr != nil {
		return nil, f.zonesErr
	}
	if f.zones == nil {
		return []edge.Zone{{ID: "z1", Name: "example.com"}}, nil
	}
	return f.zones, nil
}

func (f *fakeWriter) FindDNSRecord(context.Context, string, string) (edge.DNSRecord, bool, error) {
	if f.findErr != nil {
		return edge.DNSRecord{}, false, f.findErr
	}
	if f.existing != nil {
		return *f.existing, true, nil
	}
	return edge.DNSRecord{}, false, nil
}

func (f *fakeWriter) CreateTunnelDNS(_ context.Context, _, hostname, _ string) (edge.DNSRecord, error) {
	if f.dnsErr != nil {
		return edge.DNSRecord{}, f.dnsErr
	}
	f.created = append(f.created, hostname)
	return edge.DNSRecord{ID: "rec-" + hostname, Name: hostname}, nil
}

func (f *fakeWriter) DeleteDNSRecord(_ context.Context, _, recordID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, recordID)
	return nil
}

// fakeApps records published apps.
type fakeApps struct {
	apps      map[uuid.UUID]*publish.App
	createErr error
}

func newFakeApps() *fakeApps { return &fakeApps{apps: map[uuid.UUID]*publish.App{}} }

func (f *fakeApps) Create(_ context.Context, a *publish.App) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.apps[a.ID] = a
	return nil
}
func (f *fakeApps) Get(_ context.Context, id uuid.UUID) (*publish.App, error) {
	a, ok := f.apps[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return a, nil
}
func (f *fakeApps) ListByProvider(context.Context, uuid.UUID) ([]*publish.App, error) {
	return nil, nil
}
func (f *fakeApps) Update(context.Context, *publish.App) error { return nil }
func (f *fakeApps) Delete(_ context.Context, id uuid.UUID, _ time.Time) error {
	delete(f.apps, id)
	return nil
}

func publishSetup(t *testing.T, w *fakeWriter) (PublishDeps, *snapshotRepo, *fakeApps, uuid.UUID) {
	t.Helper()
	base := newFakeEdgeRepo()
	id := uuid.New()
	base.providers[id] = &publish.Provider{
		ID: id, Name: "home", AccountID: "acct", TunnelID: "t1", IsEnabled: true,
		AllowedZoneIDs: []string{"z1"},
	}
	base.creds[id] = "sealed"
	repo := &snapshotRepo{fakeEdgeRepo: base}
	apps := newFakeApps()

	return PublishDeps{
		Providers: repo, Apps: apps,
		Factory: func(context.Context, uuid.UUID) (EdgeWriter, error) { return w, nil },
		Audit:   &fakeAudit{}, Clock: &fakeClock{testNow},
		SelfHostname: "vm.example.com",
	}, repo, apps, id
}

func liveTable() edge.Config {
	return edge.Config{
		Version: 34,
		Rules: []edge.Rule{
			{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
			{Hostname: "keep.example.com", Service: "http://10.0.0.7:80",
				Origin: map[string]any{"noTLSVerify": true}},
			{Service: "http_status:404"},
		},
	}
}

func publishInput(providerID uuid.UUID) PublishAppInput {
	return PublishAppInput{
		ProviderID: providerID, Hostname: "new.example.com",
		ServiceURL: "http://10.0.0.20:3000", AcknowledgeExposure: true,
	}
}

func TestPublishWritesIngressThenDNS(t *testing.T) {
	w := &fakeWriter{cfg: liveTable()}
	deps, repo, apps, id := publishSetup(t, w)

	app, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if len(w.writes) != 1 {
		t.Fatalf("%d ingress writes, want 1", len(w.writes))
	}
	written := w.writes[0]
	if len(written.Rules) != 4 {
		t.Fatalf("%d rules written, want 4", len(written.Rules))
	}
	// Everything that was there is still there, in order, with its settings.
	if written.Rules[0].Hostname != "vm.example.com" || written.Rules[1].Hostname != "keep.example.com" {
		t.Errorf("existing rules were disturbed: %+v", written.Rules)
	}
	if written.Rules[1].Origin["noTLSVerify"] != true {
		t.Error("a rule's origin settings were dropped on rewrite")
	}
	// The new rule sits just before the catch-all.
	if written.Rules[2].Hostname != "new.example.com" || !written.Rules[3].IsCatchAll() {
		t.Errorf("new rule is misplaced: %+v", written.Rules)
	}

	if len(w.created) != 1 || w.created[0] != "new.example.com" {
		t.Errorf("DNS created = %v", w.created)
	}
	if app.DNSRecordID == "" {
		t.Error("the created record id was not kept; unpublish could not remove it")
	}
	// A snapshot must exist and must be of the table *before* the change.
	if len(repo.saved) != 1 || repo.saved[0].version != 34 {
		t.Errorf("snapshots = %+v, want one at version 34", repo.saved)
	}
	if len(apps.apps) != 1 {
		t.Error("the app was not recorded")
	}
}

// The rollback is the reason ingress goes first. If DNS fails afterwards, the
// routing table must go back to exactly what it was.
func TestPublishRollsBackTheIngressWhenDNSFails(t *testing.T) {
	w := &fakeWriter{cfg: liveTable(), dnsErr: edge.Errorf(edge.ErrPermission, "dns_record", "no DNS scope")}
	deps, _, apps, id := publishSetup(t, w)

	_, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if !errors.Is(err, edge.ErrPermission) {
		t.Fatalf("got %v, want the DNS error surfaced", err)
	}
	if w.writeCalls != 2 {
		t.Fatalf("%d ingress writes, want 2 — the change and the undo", w.writeCalls)
	}

	restored := w.writes[len(w.writes)-1]
	if len(restored.Rules) != 3 {
		t.Fatalf("restored %d rules, want the original 3", len(restored.Rules))
	}
	for _, r := range restored.Rules {
		if r.Hostname == "new.example.com" {
			t.Error("the rolled-back table still contains the new rule")
		}
	}
	if len(apps.apps) != 0 {
		t.Error("an app was recorded despite the publish failing")
	}
}

// Both the change and the undo failing is the case the snapshot and runbook
// exist for, and it must be unmistakable rather than reported as a plain error.
func TestPublishSaysSoWhenTheRollbackAlsoFails(t *testing.T) {
	w := &fakeWriter{
		cfg:       liveTable(),
		dnsErr:    edge.Errorf(edge.ErrPermission, "dns_record", "no DNS scope"),
		rollbackE: edge.Errorf(edge.ErrUnreachable, "put_ingress", "connection reset"),
	}
	deps, _, _, id := publishSetup(t, w)

	_, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if err == nil {
		t.Fatal("a failed rollback was reported as success")
	}
	if !strings.Contains(err.Error(), "could not be rolled back") {
		t.Errorf("error = %q, want it to say the table was left changed", err)
	}
}

// Overwriting somebody else's DNS record is not this feature's business.
func TestPublishRefusesToStealAnExistingDNSRecord(t *testing.T) {
	w := &fakeWriter{
		cfg:      liveTable(),
		existing: &edge.DNSRecord{ID: "rec9", Name: "new.example.com", Content: "10.1.2.3"},
	}
	deps, _, _, id := publishSetup(t, w)

	_, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	// And the ingress change must be undone, since the publish did not happen.
	if w.writeCalls != 2 {
		t.Errorf("%d ingress writes, want the change and its undo", w.writeCalls)
	}
	if len(w.created) != 0 {
		t.Error("a DNS record was created over an existing one")
	}
}

// PUB-43. The most consequential thing this feature does.
func TestPublishRequiresTheExposureAcknowledgement(t *testing.T) {
	w := &fakeWriter{cfg: liveTable()}
	deps, _, _, id := publishSetup(t, w)

	in := publishInput(id)
	in.AcknowledgeExposure = false

	_, err := (&PublishApp{deps}).Handle(context.Background(), in)
	if !errors.Is(err, publish.ErrExposureNotAcknowledged) {
		t.Fatalf("got %v, want ErrExposureNotAcknowledged", err)
	}
	if w.writeCalls != 0 {
		t.Error("something was written without the acknowledgement")
	}
}

// PUB-04. DNS:Edit reaches a whole zone, so the allowed list is the boundary
// and it is checked before anything is sent.
func TestPublishRefusesAZoneOutsideTheAllowedList(t *testing.T) {
	w := &fakeWriter{cfg: liveTable(), zones: []edge.Zone{{ID: "z-other", Name: "example.com"}}}
	deps, _, _, id := publishSetup(t, w)

	_, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if !errors.Is(err, publish.ErrZoneNotAllowed) {
		t.Fatalf("got %v, want ErrZoneNotAllowed", err)
	}
	if w.writeCalls != 0 {
		t.Error("the zone check happened after a write")
	}
}

func TestPublishRefusesAHostnameNoZoneCovers(t *testing.T) {
	w := &fakeWriter{cfg: liveTable()}
	deps, _, _, id := publishSetup(t, w)

	in := publishInput(id)
	in.Hostname = "app.somewhere-else.test"

	if _, err := (&PublishApp{deps}).Handle(context.Background(), in); !errors.Is(err, publish.ErrZoneNotAllowed) {
		t.Fatalf("got %v, want ErrZoneNotAllowed", err)
	}
}

// PUB-31. Silently winning this race deletes someone's app.
func TestPublishRefusesAStaleWrite(t *testing.T) {
	w := &fakeWriter{cfg: liveTable()} // live is 34
	deps, _, _, id := publishSetup(t, w)

	in := publishInput(id)
	in.ReadVersion = 33

	if _, err := (&PublishApp{deps}).Handle(context.Background(), in); !errors.Is(err, publish.ErrStaleRead) {
		t.Fatalf("got %v, want ErrStaleRead", err)
	}
	if w.writeCalls != 0 {
		t.Error("a stale write reached the provider")
	}
}

// PUB-33, at the point where it matters most.
func TestPublishCannotDisplaceThePortalsOwnRoute(t *testing.T) {
	w := &fakeWriter{cfg: liveTable()}
	deps, _, _, id := publishSetup(t, w)

	// A rule for the portal's hostname with a path sorts before the pathless
	// one and is legitimate; publishing the portal's own bare hostname at a
	// different target replaces it, which the plan reports as a modification.
	// The dangerous shape — removing it — cannot be expressed by publishing,
	// so this checks the guard is wired by removing the portal from the table.
	w.cfg = edge.Config{Version: 34, Rules: []edge.Rule{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Service: "http_status:404"},
	}}
	deps.SelfHostname = "somewhere.else.example.com" // not in the table at all

	_, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if !errors.Is(err, publish.ErrSelfRemoved) {
		t.Fatalf("got %v, want the guard to refuse a table without the portal", err)
	}
	if w.writeCalls != 0 {
		t.Error("a table missing the portal's rule was written")
	}
}

func TestPublishRefusesADuplicateRoute(t *testing.T) {
	w := &fakeWriter{cfg: liveTable()}
	deps, _, _, id := publishSetup(t, w)

	in := publishInput(id)
	in.Hostname = "keep.example.com"
	in.ServiceURL = "http://10.0.0.7:80" // identical to what is already there

	if _, err := (&PublishApp{deps}).Handle(context.Background(), in); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict for a no-op publish", err)
	}
	if w.writeCalls != 0 {
		t.Error("a write that changes nothing was still sent")
	}
}

// --- unpublish -----------------------------------------------------------

func TestUnpublishRemovesTheRuleAndOnlyItsOwnDNSRecord(t *testing.T) {
	w := &fakeWriter{cfg: liveTable()}
	deps, _, apps, id := publishSetup(t, w)

	app, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if err != nil {
		t.Fatal(err)
	}
	// The provider now serves the new table.
	w.cfg = w.writes[0]
	w.writes = nil
	w.writeCalls = 0

	if err := (&UnpublishApp{deps}).Handle(context.Background(), app.ID, Actor{}, 0); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if len(w.writes) != 1 {
		t.Fatalf("%d ingress writes, want 1", len(w.writes))
	}
	for _, r := range w.writes[0].Rules {
		if r.Hostname == "new.example.com" {
			t.Error("the rule survived unpublishing")
		}
	}
	if len(w.deleted) != 1 || w.deleted[0] != "rec-new.example.com" {
		t.Errorf("DNS deleted = %v, want exactly the record we created", w.deleted)
	}
	if len(apps.apps) != 0 {
		t.Error("the app row survived")
	}
}

// PUB-23. A CNAME somebody else made on the same name is not ours to delete,
// and an adopted record has no id recorded precisely so this cannot happen.
func TestUnpublishLeavesADNSRecordItDidNotCreate(t *testing.T) {
	w := &fakeWriter{
		cfg:      liveTable(),
		existing: &edge.DNSRecord{ID: "rec9", Name: "new.example.com", Content: "t1.cfargotunnel.com"},
	}
	deps, _, _, id := publishSetup(t, w)

	app, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if err != nil {
		t.Fatalf("adopting an existing tunnel record should succeed: %v", err)
	}
	if app.DNSRecordID != "" {
		t.Fatal("an adopted record was recorded as ours")
	}

	w.cfg = w.writes[0]
	if err := (&UnpublishApp{deps}).Handle(context.Background(), app.ID, Actor{}, 0); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if len(w.deleted) != 0 {
		t.Errorf("deleted %v — a record the portal did not create", w.deleted)
	}
}

func TestUnpublishRefusesAStaleWrite(t *testing.T) {
	w := &fakeWriter{cfg: liveTable()}
	deps, _, apps, id := publishSetup(t, w)

	app, err := (&PublishApp{deps}).Handle(context.Background(), publishInput(id))
	if err != nil {
		t.Fatal(err)
	}
	w.cfg = w.writes[0]
	w.cfg.Version = 40
	w.writeCalls = 0

	if err := (&UnpublishApp{deps}).Handle(context.Background(), app.ID, Actor{}, 34); !errors.Is(err, publish.ErrStaleRead) {
		t.Fatalf("got %v, want ErrStaleRead", err)
	}
	if w.writeCalls != 0 {
		t.Error("a stale unpublish reached the provider")
	}
	if len(apps.apps) != 1 {
		t.Error("the app row was removed despite the refusal")
	}
}

var _ ports.PublishedAppRepository = (*fakeApps)(nil)
