package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/postgres"
)

// These are the tests that matter most in the whole suite: they prove a user
// cannot see a VM nobody granted them. Everything else is a feature; this is
// the security boundary (RBAC-05).

// Fixtures for the external test package. They mirror the helpers used by the
// in-package tests but build everything through the exported API.
func newTestUser(t *testing.T, pool *postgres.Pool, role identity.Role) *identity.User {
	t.Helper()
	id := uuid.New()
	u := &identity.User{
		ID: id, Username: "user-" + id.String()[:8],
		Email: id.String()[:8] + "@example.test", DisplayName: "Test User",
		PasswordHash: "$argon2id$stub", Role: role, IsActive: true,
		CreatedAt: time.Now().UTC(),
	}
	if err := postgres.NewUserRepository(pool).Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func newUserGroup(t *testing.T, repo *postgres.AccessRepository) *access.UserGroup {
	t.Helper()
	g := &access.UserGroup{ID: uuid.New(), Name: "ug-" + uuid.NewString()[:8], CreatedAt: time.Now().UTC()}
	if err := repo.CreateUserGroup(context.Background(), g); err != nil {
		t.Fatalf("create user group: %v", err)
	}
	return g
}

func newVMGroup(t *testing.T, repo *postgres.AccessRepository) *access.VMGroup {
	t.Helper()
	g := &access.VMGroup{ID: uuid.New(), Name: "vg-" + uuid.NewString()[:8], CreatedAt: time.Now().UTC()}
	if err := repo.CreateVMGroup(context.Background(), g); err != nil {
		t.Fatalf("create vm group: %v", err)
	}
	return g
}

func TestScopedInventoryHidesUngrantedVMs(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 6})
	f.reconcile(t)
	ctx := context.Background()

	query := postgres.NewInventoryQuery(f.pool)
	accessRepo := postgres.NewAccessRepository(f.pool)

	operator := newTestUser(t, f.pool, identity.RoleOperator)
	userGroup := newUserGroup(t, accessRepo)
	vmGroup := newVMGroup(t, accessRepo)

	if err := accessRepo.SetUserGroups(ctx, operator.ID, []uuid.UUID{userGroup.ID}); err != nil {
		t.Fatalf("SetUserGroups: %v", err)
	}
	grant := &access.Grant{ID: uuid.New(), UserGroupID: userGroup.ID, VMGroupID: vmGroup.ID, CreatedAt: time.Now().UTC()}
	if err := accessRepo.CreateGrant(ctx, grant); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	// Before any VM joins the granted group, the operator sees nothing.
	page, err := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleOperator, UserID: operator.ID})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("operator sees %d VMs with an empty group, want 0", page.Total)
	}

	// Admins are not scoped: they manage the estate.
	adminPage, err := query.ListVMs(ctx, ports.VMFilter{
		Role: identity.RoleAdmin, UserID: uuid.New(), PlatformID: f.platform.ID,
	})
	if err != nil {
		t.Fatalf("ListVMs (admin): %v", err)
	}
	if adminPage.Total != 6 {
		t.Fatalf("admin sees %d VMs on this platform, want the 6 just synced", adminPage.Total)
	}

	// Put two VMs in the granted group.
	var granted []uuid.UUID
	for _, item := range adminPage.Items {
		granted = append(granted, item.ID)
		if len(granted) == 2 {
			break
		}
	}
	for _, id := range granted {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO vm_group_members (vm_group_id, vm_id) VALUES ($1,$2)`, vmGroup.ID, id); err != nil {
			t.Fatalf("add vm to group: %v", err)
		}
	}

	page, err = query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleOperator, UserID: operator.ID})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("operator sees %d VMs, want exactly the 2 granted", page.Total)
	}
	visible := map[uuid.UUID]bool{}
	for _, item := range page.Items {
		visible[item.ID] = true
	}
	for _, id := range granted {
		if !visible[id] {
			t.Errorf("granted VM %s is not visible", id)
		}
	}
}

// Fetching an ungranted VM directly must be indistinguishable from it not
// existing: a 403 would confirm the id is real.
func TestGetVMHidesUngrantedAsNotFound(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	f.reconcile(t)
	ctx := context.Background()

	query := postgres.NewInventoryQuery(f.pool)
	operator := newTestUser(t, f.pool, identity.RoleOperator)

	adminPage, err := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, PlatformID: f.platform.ID})
	if err != nil {
		t.Fatalf("ListVMs (admin): %v", err)
	}
	if len(adminPage.Items) == 0 {
		t.Fatal("fixture produced no VMs")
	}
	target := adminPage.Items[0].ID

	if _, err := query.GetVM(ctx, target, identity.RoleOperator, operator.ID); err == nil {
		t.Fatal("an ungranted VM was returned")
	}
	if allowed, _ := query.CanAccessVM(ctx, target, identity.RoleOperator, operator.ID); allowed {
		t.Error("CanAccessVM allowed an ungranted VM")
	}

	// The same VM is visible to an admin, proving the id itself is valid.
	if _, err := query.GetVM(ctx, target, identity.RoleAdmin, uuid.New()); err != nil {
		t.Errorf("admin could not read the VM: %v", err)
	}
}

// Auditors read everything by design: an audit that can only see part of the
// estate is not an audit.
func TestAuditorSeesEverything(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 4})
	f.reconcile(t)
	ctx := context.Background()

	query := postgres.NewInventoryQuery(f.pool)
	auditor := newTestUser(t, f.pool, identity.RoleAuditor)

	page, err := query.ListVMs(ctx, ports.VMFilter{
		Role: identity.RoleAuditor, UserID: auditor.ID, PlatformID: f.platform.ID,
	})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if page.Total < 4 {
		t.Errorf("auditor sees %d VMs, want the whole estate", page.Total)
	}
}

func TestInventoryFilters(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 9})
	f.reconcile(t)
	ctx := context.Background()
	query := postgres.NewInventoryQuery(f.pool)

	base := ports.VMFilter{Role: identity.RoleAdmin, PlatformID: f.platform.ID}

	all, err := query.ListVMs(ctx, base)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if all.Total != 9 {
		t.Fatalf("platform filter returned %d VMs, want 9", all.Total)
	}

	running := base
	running.State = "running"
	runningPage, err := query.ListVMs(ctx, running)
	if err != nil {
		t.Fatalf("ListVMs (state): %v", err)
	}
	if runningPage.Total == 0 || runningPage.Total == all.Total {
		t.Errorf("state filter returned %d of %d; expected a strict subset", runningPage.Total, all.Total)
	}
	for _, item := range runningPage.Items {
		if item.State != "running" {
			t.Errorf("state filter returned a %q VM", item.State)
		}
	}

	named := base
	named.Query = all.Items[0].Name
	namedPage, err := query.ListVMs(ctx, named)
	if err != nil {
		t.Fatalf("ListVMs (name): %v", err)
	}
	if namedPage.Total == 0 {
		t.Error("name search found nothing for an existing VM")
	}

	// An unknown sort key must not error or inject: it falls back to name.
	weird := base
	weird.Sort = "'; DROP TABLE vms; --"
	if _, err := query.ListVMs(ctx, weird); err != nil {
		t.Errorf("unknown sort key errored instead of falling back: %v", err)
	}
}

func TestInventoryPagination(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 10})
	f.reconcile(t)
	ctx := context.Background()
	query := postgres.NewInventoryQuery(f.pool)

	first, err := query.ListVMs(ctx, ports.VMFilter{
		Role: identity.RoleAdmin, PlatformID: f.platform.ID, Limit: 4, Offset: 0, Sort: "name",
	})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(first.Items) != 4 || first.Total != 10 {
		t.Fatalf("page = %d items of %d total, want 4 of 10", len(first.Items), first.Total)
	}

	second, err := query.ListVMs(ctx, ports.VMFilter{
		Role: identity.RoleAdmin, PlatformID: f.platform.ID, Limit: 4, Offset: 4, Sort: "name",
	})
	if err != nil {
		t.Fatalf("ListVMs (page 2): %v", err)
	}
	for _, a := range first.Items {
		for _, b := range second.Items {
			if a.ID == b.ID {
				t.Errorf("VM %s appears on both pages", a.ID)
			}
		}
	}
}

func TestPortalAnnotationsRoundTrip(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 2})
	f.reconcile(t)
	ctx := context.Background()
	query := postgres.NewInventoryQuery(f.pool)

	page, _ := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, PlatformID: f.platform.ID})
	target := page.Items[0].ID

	if err := query.SetPortalTags(ctx, target, []string{"tier:frontend", "owner:ops"}); err != nil {
		t.Fatalf("SetPortalTags: %v", err)
	}
	if err := query.SetNotes(ctx, target, "scheduled for decommission"); err != nil {
		t.Fatalf("SetNotes: %v", err)
	}

	detail, err := query.GetVM(ctx, target, identity.RoleAdmin, uuid.New())
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if len(detail.PortalTags) != 2 || detail.Notes != "scheduled for decommission" {
		t.Errorf("annotations = %v / %q", detail.PortalTags, detail.Notes)
	}

	// Clearing tags must store an empty array, not null.
	if err := query.SetPortalTags(ctx, target, nil); err != nil {
		t.Fatalf("SetPortalTags (clear): %v", err)
	}
	detail, _ = query.GetVM(ctx, target, identity.RoleAdmin, uuid.New())
	if len(detail.PortalTags) != 0 {
		t.Errorf("tags = %v after clearing", detail.PortalTags)
	}
}

func TestDashboardIsScoped(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 5})
	f.reconcile(t)
	ctx := context.Background()
	query := postgres.NewInventoryQuery(f.pool)

	admin, err := query.Dashboard(ctx, identity.RoleAdmin, uuid.New())
	if err != nil {
		t.Fatalf("Dashboard (admin): %v", err)
	}
	if admin.TotalVMs < 5 {
		t.Errorf("admin dashboard shows %d VMs, want at least 5", admin.TotalVMs)
	}
	if admin.RunningVMs+admin.StoppedVMs+admin.OtherVMs != admin.TotalVMs {
		t.Errorf("counts do not add up: %d + %d + %d != %d",
			admin.RunningVMs, admin.StoppedVMs, admin.OtherVMs, admin.TotalVMs)
	}
	if len(admin.Platforms) == 0 {
		t.Error("dashboard lists no platforms")
	}

	// An operator with no grants sees an empty fleet, not everyone else's.
	operator := newTestUser(t, f.pool, identity.RoleOperator)
	scoped, err := query.Dashboard(ctx, identity.RoleOperator, operator.ID)
	if err != nil {
		t.Fatalf("Dashboard (operator): %v", err)
	}
	if scoped.TotalVMs != 0 {
		t.Errorf("ungranted operator sees %d VMs on the dashboard", scoped.TotalVMs)
	}
}

func TestVMHistoryIsReturned(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	f.reconcile(t)
	ctx := context.Background()
	query := postgres.NewInventoryQuery(f.pool)

	page, _ := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, PlatformID: f.platform.ID})
	entries, err := query.VMHistory(ctx, page.Items[0].ID, 50)
	if err != nil {
		t.Fatalf("VMHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no history for a freshly discovered VM; creation should be recorded")
	}
	if entries[0].Field == "" {
		t.Error("history entry has no field name")
	}
}

// Deleting a platform must take its inventory with it. Leaving the assets
// behind showed VMs, hosts, storage and networks attributed to a platform the
// administrator could no longer see — which is how this was found.
func TestDeletingAPlatformHidesItsInventory(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 4})
	f.reconcile(t)
	ctx := context.Background()

	query := postgres.NewInventoryQuery(f.pool)
	// Scoped to this fixture's platform: the integration database accumulates
	// rows across runs, so a global listing would not reliably contain them.
	filter := ports.VMFilter{Role: identity.RoleAdmin, PlatformID: f.platform.ID, Limit: 200}

	before, err := query.ListVMs(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if countFromPlatform(before, f.platform.Name) == 0 {
		t.Fatal("the fixture's VMs were not visible before deleting the platform")
	}
	hostsBefore, err := query.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if countHostsFrom(hostsBefore, f.platform.Name) == 0 {
		t.Fatal("the fixture's hosts were not visible before deleting the platform")
	}

	if err := postgres.NewPlatformRepository(f.pool).
		SoftDelete(ctx, f.platform.ID, time.Now().UTC()); err != nil {
		t.Fatalf("delete platform: %v", err)
	}

	after, err := query.ListVMs(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if n := countFromPlatform(after, f.platform.Name); n != 0 {
		t.Errorf("%d VMs outlived the platform they were synced from", n)
	}

	hostsAfter, err := query.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := countHostsFrom(hostsAfter, f.platform.Name); n != 0 {
		t.Errorf("%d hosts outlived the platform they belonged to", n)
	}

	storage, err := query.ListStoragePools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range storage {
		if pool.PlatformName == f.platform.Name {
			t.Error("a storage pool outlived its platform")
		}
	}

	networks, err := query.ListNetworks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, net := range networks {
		if net.PlatformName == f.platform.Name {
			t.Error("a network outlived its platform")
		}
	}

	// The alert evaluator reads names separately, and would otherwise keep
	// firing rules against machines that are gone.
	names, err := query.AllVMNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range before.Items {
		if vm.PlatformName != f.platform.Name {
			continue
		}
		if _, present := names[vm.ID]; present {
			t.Error("the alert evaluator can still see a deleted platform's VM")
			break
		}
	}
}

func countFromPlatform(page ports.VMPage, platform string) int {
	n := 0
	for _, vm := range page.Items {
		if vm.PlatformName == platform {
			n++
		}
	}
	return n
}

func countHostsFrom(hosts []ports.HostRow, platform string) int {
	n := 0
	for _, host := range hosts {
		if host.PlatformName == platform {
			n++
		}
	}
	return n
}
