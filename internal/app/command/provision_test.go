package command

// These test the controls that replaced a guarantee.
//
// Until ADR 0010 the portal could not destroy a guest because its credential
// could not: the limit was arithmetic, and it held whatever the code did. That
// is gone, and what stands in its place is the three checks below. They are
// worth testing precisely because nothing behind them will catch a mistake any
// more.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/domain/provision"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// fakeInventory answers with one guest.
type fakeInventory struct {
	vm  ports.VMDetail
	err error
}

func (f *fakeInventory) GetVM(context.Context, uuid.UUID, identity.Role, uuid.UUID) (ports.VMDetail, error) {
	return f.vm, f.err
}
func (f *fakeInventory) ListVMs(context.Context, ports.VMFilter) (ports.VMPage, error) {
	return ports.VMPage{}, nil
}
func (f *fakeInventory) CanAccessVM(context.Context, uuid.UUID, identity.Role, uuid.UUID) (bool, error) {
	return true, nil
}
func (f *fakeInventory) VMHistory(context.Context, uuid.UUID, int) ([]ports.HistoryEntry, error) {
	return nil, nil
}
func (f *fakeInventory) SetPortalTags(context.Context, uuid.UUID, []string) error { return nil }
func (f *fakeInventory) SetNotes(context.Context, uuid.UUID, string) error        { return nil }
func (f *fakeInventory) Dashboard(context.Context, identity.Role, uuid.UUID) (ports.DashboardSummary, error) {
	return ports.DashboardSummary{}, nil
}

// recordingRequests notices if a request is ever stored.
type recordingRequests struct{ created []*provision.Request }

func (r *recordingRequests) CreateRequest(_ context.Context, req *provision.Request) error {
	r.created = append(r.created, req)
	return nil
}
func (r *recordingRequests) GetRequest(context.Context, uuid.UUID) (*provision.Request, error) {
	return nil, ports.ErrNotFound
}
func (r *recordingRequests) SaveRequest(context.Context, *provision.Request) error { return nil }
func (r *recordingRequests) ListRequests(context.Context, uuid.UUID, int) ([]*provision.Request, error) {
	return nil, nil
}
func (r *recordingRequests) ListOpenRequests(context.Context) ([]*provision.Request, error) {
	return nil, nil
}
func (r *recordingRequests) FindVMByExternalID(context.Context, uuid.UUID, string) (uuid.UUID, error) {
	return uuid.Nil, nil
}

func guest(name string, attrs map[string]any) ports.VMDetail {
	return ports.VMDetail{
		VMListItem: ports.VMListItem{
			ID: uuid.New(), ExternalID: "135", Name: name, VMType: "qemu",
			PlatformID: uuid.New(), HostName: "pve",
		},
		Attrs: attrs,
	}
}

// stubPlatforms stops the two tests that get past the guards from reaching a
// platform there is no fake for. What they assert is which check fired, and
// both of those checks happen before any platform is touched.
type stubPlatforms struct{}

func (stubPlatforms) Get(context.Context, uuid.UUID) (*inventory.Platform, error) {
	return nil, ports.ErrNotFound
}
func (stubPlatforms) Create(context.Context, *inventory.Platform, ports.SealedCredential) error {
	return nil
}
func (stubPlatforms) List(context.Context, bool) ([]*inventory.Platform, error) { return nil, nil }
func (stubPlatforms) Update(context.Context, *inventory.Platform) error         { return nil }
func (stubPlatforms) UpdateHealth(context.Context, *inventory.Platform) error   { return nil }
func (stubPlatforms) SoftDelete(context.Context, uuid.UUID, time.Time) error    { return nil }
func (stubPlatforms) Credential(context.Context, uuid.UUID, *crypto.Vault) (ports.PlainCredential, error) {
	return ports.PlainCredential{}, nil
}
func (stubPlatforms) ReplaceCredential(context.Context, uuid.UUID, ports.SealedCredential) error {
	return nil
}
func (stubPlatforms) Endpoints(context.Context, uuid.UUID) ([]ports.PlatformEndpoint, error) {
	return nil, nil
}
func (stubPlatforms) ReplaceEndpoints(context.Context, uuid.UUID, []ports.PlatformEndpoint, time.Time) error {
	return nil
}

func testActor() Actor {
	return Actor{UserID: uuid.New(), Username: "someone", IP: "10.0.0.5"}
}

// Every role but admin is refused, and refused before the platform is touched —
// which is why this can be tested with no platform or connector at all.
func TestProvisioningIsRefusedToEveryRoleButAdmin(t *testing.T) {
	for _, role := range []identity.Role{identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor} {
		t.Run(string(role), func(t *testing.T) {
			audit := &fakeAudit{}
			requests := &recordingRequests{}
			h := &Provision{Requests: requests, Audit: audit, Clock: &fakeClock{t: time.Now()}}

			_, err := h.Handle(context.Background(), ProvisionInput{
				Actor: testActor(), Role: role, PlatformID: uuid.New(),
				TemplateID: "9000", Name: "web-02",
			})
			if !errors.Is(err, ErrProvisionNotPermitted) {
				t.Fatalf("err = %v, want ErrProvisionNotPermitted", err)
			}
			if len(requests.created) != 0 {
				t.Error("a refused request was still recorded")
			}
			if len(audit.entries) != 1 || audit.entries[0].Outcome != ports.OutcomeDenied {
				t.Errorf("audit = %+v, want one denied entry", audit.entries)
			}
		})
	}
}

func TestDestroyIsRefusedToEveryRoleButAdmin(t *testing.T) {
	audit := &fakeAudit{}
	requests := &recordingRequests{}
	h := &Destroy{
		Requests: requests, Inventory: &fakeInventory{vm: guest("web-01", nil)},
		Audit: audit, Clock: &fakeClock{t: time.Now()},
	}

	_, err := h.Handle(context.Background(), DestroyInput{
		Actor: testActor(), Role: identity.RoleOperator,
		VMID: uuid.New(), ConfirmName: "web-01",
	})
	if !errors.Is(err, ErrProvisionNotPermitted) {
		t.Fatalf("err = %v, want ErrProvisionNotPermitted", err)
	}
	if len(requests.created) != 0 {
		t.Error("a refused destruction was still recorded")
	}
}

// Typing the name is the last thing between an administrator and an
// irreversible action, so it is enforced here rather than in the browser: a
// client cannot be relied on to have asked.
func TestDestroyRequiresTheGuestsExactName(t *testing.T) {
	for _, confirm := range []string{"", "web-0", "WEB-01", "web-02", "  "} {
		t.Run("confirm="+confirm, func(t *testing.T) {
			audit := &fakeAudit{}
			requests := &recordingRequests{}
			h := &Destroy{
				Requests: requests, Inventory: &fakeInventory{vm: guest("web-01", nil)},
				Audit: audit, Clock: &fakeClock{t: time.Now()},
			}

			_, err := h.Handle(context.Background(), DestroyInput{
				Actor: testActor(), Role: identity.RoleAdmin,
				VMID: uuid.New(), ConfirmName: confirm,
			})
			if !errors.Is(err, ErrNameMismatch) {
				t.Fatalf("err = %v, want ErrNameMismatch", err)
			}
			if len(requests.created) != 0 {
				t.Error("a mismatched confirmation still recorded a destruction")
			}
			if len(audit.entries) != 1 || audit.entries[0].Outcome != ports.OutcomeDenied {
				t.Errorf("audit = %+v, want one denied entry", audit.entries)
			}
		})
	}
}

// Surrounding whitespace is a paste artefact, not a different answer.
func TestDestroyAcceptsAPastedNameWithWhitespace(t *testing.T) {
	h := &Destroy{
		Requests:  &recordingRequests{},
		Inventory: &fakeInventory{vm: guest("web-01", nil)},
		Platforms: stubPlatforms{},
		Audit:     &fakeAudit{}, Clock: &fakeClock{t: time.Now()},
	}

	// It gets past the name check and then fails for want of a platform, which
	// is the next thing it reaches — the point is that it is not ErrNameMismatch.
	_, err := h.Handle(context.Background(), DestroyInput{
		Actor: testActor(), Role: identity.RoleAdmin,
		VMID: uuid.New(), ConfirmName: "  web-01\n",
	})
	if errors.Is(err, ErrNameMismatch) {
		t.Error("a correctly typed name with stray whitespace was rejected")
	}
}

// Templates are what every other guest is cloned from. Losing one costs far
// more than the guest that asked to go, and a platform configured to show
// templates in its inventory is exactly where one could be reached by accident.
func TestDestroyRefusesTemplates(t *testing.T) {
	for _, attrs := range []map[string]any{
		{"template": true},
		{"template": float64(1)},
		{"template": "1"},
	} {
		audit := &fakeAudit{}
		requests := &recordingRequests{}
		h := &Destroy{
			Requests: requests, Inventory: &fakeInventory{vm: guest("golden-debian", attrs)},
			Audit: audit, Clock: &fakeClock{t: time.Now()},
		}

		_, err := h.Handle(context.Background(), DestroyInput{
			Actor: testActor(), Role: identity.RoleAdmin,
			VMID: uuid.New(), ConfirmName: "golden-debian",
		})
		if !errors.Is(err, ErrTemplateProtected) {
			t.Fatalf("attrs %v: err = %v, want ErrTemplateProtected", attrs, err)
		}
		if len(requests.created) != 0 {
			t.Error("a template destruction was recorded")
		}
		if len(audit.entries) != 1 || audit.entries[0].Outcome != ports.OutcomeDenied {
			t.Errorf("audit = %+v, want one denied entry", audit.entries)
		}
	}
}

// An ordinary guest carrying no template marker must not be caught by the
// protection meant for templates.
func TestDestroyDoesNotMistakeAnOrdinaryGuestForATemplate(t *testing.T) {
	for _, attrs := range []map[string]any{nil, {}, {"template": false}, {"template": float64(0)}, {"template": "0"}} {
		h := &Destroy{
			Requests:  &recordingRequests{},
			Inventory: &fakeInventory{vm: guest("web-01", attrs)},
			Platforms: stubPlatforms{},
			Audit:     &fakeAudit{}, Clock: &fakeClock{t: time.Now()},
		}
		_, err := h.Handle(context.Background(), DestroyInput{
			Actor: testActor(), Role: identity.RoleAdmin,
			VMID: uuid.New(), ConfirmName: "web-01",
		})
		if errors.Is(err, ErrTemplateProtected) {
			t.Errorf("attrs %v: an ordinary guest was treated as a template", attrs)
		}
	}
}

func buildInput(role identity.Role) BuildTemplateInput {
	return BuildTemplateInput{
		Actor: testActor(), Role: role, PlatformID: uuid.New(),
		Name: "debian-13-cloud", Node: "pve",
		ImageURL:     "https://cloud.example/debian-13-generic-amd64.qcow2",
		ImageFile:    "debian-13-generic-amd64.qcow2",
		ImageStorage: "local", DiskStorage: "local-lvm",
		Checksum: "abc123", ChecksumAlgo: "sha512",
	}
}

func TestBuildingATemplateIsAdminOnly(t *testing.T) {
	for _, role := range []identity.Role{identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor} {
		audit := &fakeAudit{}
		requests := &recordingRequests{}
		h := &BuildTemplate{Requests: requests, Audit: audit, Clock: &fakeClock{t: time.Now()}}

		if _, err := h.Handle(context.Background(), buildInput(role)); !errors.Is(err, ErrProvisionNotPermitted) {
			t.Fatalf("%s: err = %v, want ErrProvisionNotPermitted", role, err)
		}
		if len(requests.created) != 0 {
			t.Errorf("%s: a refused build was still recorded", role)
		}
		if len(audit.entries) != 1 || audit.entries[0].Outcome != ports.OutcomeDenied {
			t.Errorf("%s: audit = %+v, want one denied entry", role, audit.entries)
		}
	}
}

// This image becomes the ancestor of every guest cloned from it, so an
// unverified download must be asked for rather than arrived at by leaving a
// field blank.
func TestBuildingRefusesAMissingChecksumUnlessSkippingIsStated(t *testing.T) {
	for _, tc := range []struct {
		name         string
		digest, algo string
	}{
		{"nothing at all", "", ""},
		{"digest without algorithm", "abc123", ""},
		{"algorithm without digest", "", "sha512"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := &recordingRequests{}
			h := &BuildTemplate{Requests: requests, Audit: &fakeAudit{}, Clock: &fakeClock{t: time.Now()}}

			in := buildInput(identity.RoleAdmin)
			in.Checksum, in.ChecksumAlgo = tc.digest, tc.algo

			_, err := h.Handle(context.Background(), in)
			if err == nil {
				t.Fatal("accepted a build with nothing to verify against")
			}
			if !errors.Is(err, connector.ErrInvalidConfig) {
				t.Errorf("err = %v, want an invalid-config refusal", err)
			}
			if len(requests.created) != 0 {
				t.Error("an unverifiable build was recorded")
			}
		})
	}
}

// Skipping is allowed, deliberate, and written down. The audit entry is the
// whole point: the one thing worse than no checksum is no record that there
// was none.
func TestSkippingVerificationIsAudited(t *testing.T) {
	audit := &fakeAudit{}
	h := &BuildTemplate{
		Requests: &recordingRequests{}, Platforms: stubPlatforms{},
		Audit: audit, Clock: &fakeClock{t: time.Now()},
	}

	in := buildInput(identity.RoleAdmin)
	in.Checksum, in.ChecksumAlgo = "", ""
	in.SkipChecksum = true

	// It gets past the checksum rule and stops at the platform, which is the
	// next thing it reaches. What matters is which refusal it was not.
	_, err := h.Handle(context.Background(), in)
	if errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("a deliberate skip was refused: %v", err)
	}

	var skipped *ports.AuditEntry
	for i := range audit.entries {
		if audit.entries[i].Action == "template.build.unverified" {
			skipped = &audit.entries[i]
		}
	}
	if skipped == nil {
		t.Fatalf("skipping verification was not audited: %+v", audit.entries)
	}
	if skipped.Category != ports.AuditCategorySecurity {
		t.Errorf("category = %q, want the security category an auditor searches", skipped.Category)
	}
	if skipped.Details["image_url"] != in.ImageURL {
		t.Errorf("the audit entry does not name the image: %+v", skipped.Details)
	}
	if skipped.ActorName == "" {
		t.Error("the audit entry does not name who skipped verification")
	}
}
