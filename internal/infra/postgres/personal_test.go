package postgres_test

// Favourites and folders are one person's opinion about the fleet, so the
// property everything rests on is that two people can hold different ones and
// neither can see the other's. That can only be checked against a real database.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/postgres"
)

func newFolder(t *testing.T, repo *postgres.PersonalRepository, userID uuid.UUID, name string) ports.VMFolder {
	t.Helper()
	f := ports.VMFolder{ID: uuid.New(), Name: name, CreatedAt: time.Now().UTC()}
	if err := repo.CreateFolder(context.Background(), &f, userID); err != nil {
		t.Fatalf("CreateFolder(%s): %v", name, err)
	}
	return f
}

// The whole design rests on this: arranging your view cannot disturb anyone
// else's, and cannot be seen by them.
func TestTwoUsersArrangeTheSameVMsIndependently(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 4})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewPersonalRepository(f.pool)
	query := postgres.NewInventoryQuery(f.pool)
	alice := newTestUser(t, f.pool, identity.RoleAdmin)
	bob := newTestUser(t, f.pool, identity.RoleAdmin)

	page, err := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, UserID: alice.ID})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(page.Items) < 3 {
		t.Fatalf("only %d VMs to work with", len(page.Items))
	}
	shared := page.Items[0].ID

	// Both star the same machine and file it somewhere of their own.
	aliceFolder := newFolder(t, repo, alice.ID, "Alice's servers")
	bobFolder := newFolder(t, repo, bob.ID, "Bob's servers")
	now := time.Now().UTC()
	if err := repo.SetFavourite(ctx, alice.ID, shared, true, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.FileVMs(ctx, alice.ID, []uuid.UUID{shared}, &aliceFolder.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.FileVMs(ctx, bob.ID, []uuid.UUID{shared}, &bobFolder.ID, now); err != nil {
		t.Fatal(err)
	}

	alicePage, err := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, UserID: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	bobPage, err := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, UserID: bob.ID})
	if err != nil {
		t.Fatal(err)
	}

	aliceRow := findVM(t, alicePage.Items, shared)
	bobRow := findVM(t, bobPage.Items, shared)

	if !aliceRow.IsFavourite {
		t.Error("Alice's own favourite did not come back")
	}
	if bobRow.IsFavourite {
		t.Error("Bob can see a favourite that is not his")
	}
	if aliceRow.FolderName != "Alice's servers" {
		t.Errorf("Alice sees folder %q", aliceRow.FolderName)
	}
	if bobRow.FolderName != "Bob's servers" {
		t.Errorf("Bob sees folder %q", bobRow.FolderName)
	}

	// And neither can see the other's folders at all.
	folders, err := repo.ListFolders(ctx, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range folders {
		if folder.Name == "Alice's servers" {
			t.Error("Bob's folder list contains Alice's folder")
		}
	}
}

// This is the thing sorting in the browser cannot do: a favourite on the last
// page has to arrive on the first.
func TestAFavouriteFromTheLastPageArrivesOnTheFirst(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 12})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewPersonalRepository(f.pool)
	query := postgres.NewInventoryQuery(f.pool)
	user := newTestUser(t, f.pool, identity.RoleAdmin)

	all, err := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, UserID: user.ID, Sort: "name"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) < 6 {
		t.Fatalf("only %d VMs; need enough to page", len(all.Items))
	}
	last := all.Items[len(all.Items)-1]

	if err := repo.SetFavourite(ctx, user.ID, last.ID, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	first, err := query.ListVMs(ctx, ports.VMFilter{
		Role: identity.RoleAdmin, UserID: user.ID, Sort: "name", Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) == 0 || first.Items[0].ID != last.ID {
		t.Fatalf("first row is %v, want the favourite that sorted last by name", first.Items[0].Name)
	}
	if !first.Items[0].IsFavourite {
		t.Error("the pinned row is not marked as a favourite")
	}
	// Unstarring puts it back where its name says it belongs.
	if err := repo.SetFavourite(ctx, user.ID, last.ID, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	back, err := query.ListVMs(ctx, ports.VMFilter{
		Role: identity.RoleAdmin, UserID: user.ID, Sort: "name", Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if back.Items[0].ID == last.ID {
		t.Error("an unstarred VM is still pinned to the top")
	}
}

// One folder per VM is a constraint, not a convention: "where is this VM" has
// exactly one answer, and re-filing moves rather than duplicating.
func TestFilingAVMAgainMovesItRatherThanDuplicating(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewPersonalRepository(f.pool)
	query := postgres.NewInventoryQuery(f.pool)
	user := newTestUser(t, f.pool, identity.RoleAdmin)

	page, _ := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, UserID: user.ID})
	vm := page.Items[0].ID

	first := newFolder(t, repo, user.ID, "First")
	second := newFolder(t, repo, user.ID, "Second")
	now := time.Now().UTC()

	if err := repo.FileVMs(ctx, user.ID, []uuid.UUID{vm}, &first.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.FileVMs(ctx, user.ID, []uuid.UUID{vm}, &second.ID, now); err != nil {
		t.Fatalf("re-filing: %v", err)
	}

	folders, err := repo.ListFolders(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range folders {
		want := 0
		if folder.ID == second.ID {
			want = 1
		}
		if folder.VMCount != want {
			t.Errorf("%s holds %d VMs, want %d", folder.Name, folder.VMCount, want)
		}
	}

	// Unfiling takes it out of everything.
	if err := repo.FileVMs(ctx, user.ID, []uuid.UUID{vm}, nil, now); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.FolderOf(ctx, user.ID, vm); err != nil || got != nil {
		t.Errorf("FolderOf = %v, %v; want no folder", got, err)
	}
}

// Deleting a folder frees its VMs. A folder is a way of looking at machines,
// not a container that owns them.
func TestDeletingAFolderLeavesItsVMsAlone(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 3})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewPersonalRepository(f.pool)
	query := postgres.NewInventoryQuery(f.pool)
	user := newTestUser(t, f.pool, identity.RoleAdmin)

	page, _ := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, UserID: user.ID})
	vm := page.Items[0].ID
	folder := newFolder(t, repo, user.ID, "Doomed")
	if err := repo.FileVMs(ctx, user.ID, []uuid.UUID{vm}, &folder.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteFolder(ctx, user.ID, folder.ID); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}

	after, err := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, UserID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	row := findVM(t, after.Items, vm)
	if row.FolderID != nil || row.FolderName != "" {
		t.Errorf("the VM is still filed under %q", row.FolderName)
	}
	if len(after.Items) != len(page.Items) {
		t.Error("deleting a folder removed VMs from the inventory")
	}
}

// Filing into a folder that is not yours must be impossible, not merely absent
// from the UI.
func TestFilingIntoSomebodyElsesFolderIsRefused(t *testing.T) {
	f := newSyncFixture(t, map[string]any{"vm_count": 2})
	f.reconcile(t)
	ctx := context.Background()

	repo := postgres.NewPersonalRepository(f.pool)
	query := postgres.NewInventoryQuery(f.pool)
	alice := newTestUser(t, f.pool, identity.RoleAdmin)
	bob := newTestUser(t, f.pool, identity.RoleAdmin)

	page, _ := query.ListVMs(ctx, ports.VMFilter{Role: identity.RoleAdmin, UserID: bob.ID})
	aliceFolder := newFolder(t, repo, alice.ID, "Alice only")

	err := repo.FileVMs(ctx, bob.ID, []uuid.UUID{page.Items[0].ID}, &aliceFolder.ID, time.Now().UTC())
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}

	// Renaming and deleting are equally out of reach.
	if err := repo.UpdateFolder(ctx, bob.ID, aliceFolder.ID, "Bob's now", 0); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("rename = %v, want not found", err)
	}
	if err := repo.DeleteFolder(ctx, bob.ID, aliceFolder.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("delete = %v, want not found", err)
	}
}

func TestTwoFoldersCannotShareAName(t *testing.T) {
	f := newSyncFixture(t, nil)
	ctx := context.Background()
	repo := postgres.NewPersonalRepository(f.pool)
	user := newTestUser(t, f.pool, identity.RoleAdmin)

	newFolder(t, repo, user.ID, "Production")
	dup := ports.VMFolder{ID: uuid.New(), Name: "Production", CreatedAt: time.Now().UTC()}
	if err := repo.CreateFolder(ctx, &dup, user.ID); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("err = %v, want a conflict — two folders with one name are "+
			"indistinguishable in the picker they exist for", err)
	}

	// But another user may use the same name: these are private lists.
	other := newTestUser(t, f.pool, identity.RoleAdmin)
	newFolder(t, repo, other.ID, "Production")
}

func findVM(t *testing.T, items []ports.VMListItem, id uuid.UUID) ports.VMListItem {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("VM %s not in the listing", id)
	return ports.VMListItem{}
}
