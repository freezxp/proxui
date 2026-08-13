package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/identity"
)

func newUserGroup(t *testing.T, repo *AccessRepository) *access.UserGroup {
	t.Helper()
	g := &access.UserGroup{ID: uuid.New(), Name: "ug-" + uuid.NewString()[:8], CreatedAt: time.Now().UTC()}
	if err := repo.CreateUserGroup(context.Background(), g); err != nil {
		t.Fatalf("create user group: %v", err)
	}
	return g
}

func newVMGroup(t *testing.T, repo *AccessRepository) *access.VMGroup {
	t.Helper()
	g := &access.VMGroup{ID: uuid.New(), Name: "vg-" + uuid.NewString()[:8], CreatedAt: time.Now().UTC()}
	if err := repo.CreateVMGroup(context.Background(), g); err != nil {
		t.Fatalf("create vm group: %v", err)
	}
	return g
}

func TestVisibilityFollowsGrantChain(t *testing.T) {
	pool := testPool(t)
	repo := NewAccessRepository(pool)
	ctx := context.Background()

	user := newTestUser(t, pool, identity.RoleOperator)
	userGroup := newUserGroup(t, repo)
	granted := newVMGroup(t, repo)
	ungranted := newVMGroup(t, repo)

	// No membership yet: the user sees nothing.
	visible, err := repo.VisibleVMGroupIDs(ctx, user.ID)
	if err != nil {
		t.Fatalf("VisibleVMGroupIDs: %v", err)
	}
	if len(visible) != 0 {
		t.Fatalf("visible groups = %v, want none before any grant", visible)
	}

	if err := repo.SetUserGroups(ctx, user.ID, []uuid.UUID{userGroup.ID}); err != nil {
		t.Fatalf("SetUserGroups: %v", err)
	}
	// Membership without a grant still yields nothing.
	if visible, _ = repo.VisibleVMGroupIDs(ctx, user.ID); len(visible) != 0 {
		t.Fatalf("visible groups = %v, want none until a grant exists", visible)
	}

	grantedBy := user.ID
	grant := &access.Grant{
		ID: uuid.New(), UserGroupID: userGroup.ID, VMGroupID: granted.ID,
		GrantedBy: &grantedBy, CreatedAt: time.Now().UTC(),
	}
	if err := repo.CreateGrant(ctx, grant); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	visible, err = repo.VisibleVMGroupIDs(ctx, user.ID)
	if err != nil {
		t.Fatalf("VisibleVMGroupIDs: %v", err)
	}
	if len(visible) != 1 || visible[0] != granted.ID {
		t.Fatalf("visible groups = %v, want exactly the granted group %s", visible, granted.ID)
	}
	for _, id := range visible {
		if id == ungranted.ID {
			t.Error("ungranted VM group leaked into the visible set")
		}
	}

	// Revoking the grant closes access again.
	if err := repo.DeleteGrant(ctx, grant.ID); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	if visible, _ = repo.VisibleVMGroupIDs(ctx, user.ID); len(visible) != 0 {
		t.Errorf("visible groups = %v after revocation, want none", visible)
	}
}

func TestDeletingGroupCascadesGrants(t *testing.T) {
	pool := testPool(t)
	repo := NewAccessRepository(pool)
	ctx := context.Background()

	user := newTestUser(t, pool, identity.RoleOperator)
	userGroup, vmGroup := newUserGroup(t, repo), newVMGroup(t, repo)
	if err := repo.SetUserGroups(ctx, user.ID, []uuid.UUID{userGroup.ID}); err != nil {
		t.Fatalf("SetUserGroups: %v", err)
	}
	grant := &access.Grant{ID: uuid.New(), UserGroupID: userGroup.ID, VMGroupID: vmGroup.ID, CreatedAt: time.Now().UTC()}
	if err := repo.CreateGrant(ctx, grant); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	if err := repo.DeleteVMGroup(ctx, vmGroup.ID); err != nil {
		t.Fatalf("DeleteVMGroup: %v", err)
	}
	if visible, _ := repo.VisibleVMGroupIDs(ctx, user.ID); len(visible) != 0 {
		t.Errorf("visible groups = %v after deleting the VM group, want none", visible)
	}
}

func TestSetUserGroupsReplacesMembership(t *testing.T) {
	pool := testPool(t)
	repo := NewAccessRepository(pool)
	ctx := context.Background()

	user := newTestUser(t, pool, identity.RoleReadOnly)
	first, second := newUserGroup(t, repo), newUserGroup(t, repo)

	if err := repo.SetUserGroups(ctx, user.ID, []uuid.UUID{first.ID}); err != nil {
		t.Fatalf("SetUserGroups: %v", err)
	}
	if err := repo.SetUserGroups(ctx, user.ID, []uuid.UUID{second.ID}); err != nil {
		t.Fatalf("SetUserGroups (replace): %v", err)
	}

	names, err := repo.UserGroupNames(ctx, user.ID)
	if err != nil {
		t.Fatalf("UserGroupNames: %v", err)
	}
	if len(names) != 1 || names[0] != second.Name {
		t.Errorf("groups = %v, want only %s", names, second.Name)
	}
}

func TestGroupNamesAreUniqueAndErrorsMap(t *testing.T) {
	pool := testPool(t)
	repo := NewAccessRepository(pool)
	ctx := context.Background()

	g := newUserGroup(t, repo)
	dup := &access.UserGroup{ID: uuid.New(), Name: g.Name, CreatedAt: time.Now().UTC()}
	if err := repo.CreateUserGroup(ctx, dup); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("duplicate group error = %v, want ErrConflict", err)
	}
	if err := repo.DeleteUserGroup(ctx, uuid.New()); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("deleting a missing group = %v, want ErrNotFound", err)
	}
	if err := repo.DeleteGrant(ctx, uuid.New()); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("deleting a missing grant = %v, want ErrNotFound", err)
	}
}

func TestGrantRequiresExistingGroups(t *testing.T) {
	pool := testPool(t)
	repo := NewAccessRepository(pool)

	grant := &access.Grant{ID: uuid.New(), UserGroupID: uuid.New(), VMGroupID: uuid.New(), CreatedAt: time.Now().UTC()}
	if err := repo.CreateGrant(context.Background(), grant); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("grant to nonexistent groups = %v, want ErrNotFound", err)
	}
}
