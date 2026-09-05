package command

// The access check is what this layer exists for. Storage cannot make it: the
// tables are keyed by user and VM, and nothing in them knows which VMs a user
// was granted.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// scopedInventory answers CanAccessVM from a fixed allow-list.
type scopedInventory struct {
	fakeInventory
	visible map[uuid.UUID]bool
	asked   int
}

func (s *scopedInventory) CanAccessVM(_ context.Context, id uuid.UUID, _ identity.Role, _ uuid.UUID) (bool, error) {
	s.asked++
	return s.visible[id], nil
}

// memPersonal records what reached storage.
type memPersonal struct {
	favourites map[uuid.UUID]bool
	filed      []uuid.UUID
	folders    []ports.VMFolder
	created    []ports.VMFolder
}

func newMemPersonal() *memPersonal {
	return &memPersonal{favourites: map[uuid.UUID]bool{}}
}

func (m *memPersonal) SetFavourite(_ context.Context, _, vmID uuid.UUID, on bool, _ time.Time) error {
	m.favourites[vmID] = on
	return nil
}
func (m *memPersonal) ListFolders(context.Context, uuid.UUID) ([]ports.VMFolder, error) {
	return m.folders, nil
}
func (m *memPersonal) CreateFolder(_ context.Context, f *ports.VMFolder, _ uuid.UUID) error {
	m.created = append(m.created, *f)
	m.folders = append(m.folders, *f)
	return nil
}
func (m *memPersonal) UpdateFolder(context.Context, uuid.UUID, uuid.UUID, string, int) error {
	return nil
}
func (m *memPersonal) DeleteFolder(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (m *memPersonal) FileVMs(_ context.Context, _ uuid.UUID, vmIDs []uuid.UUID, _ *uuid.UUID, _ time.Time) error {
	m.filed = append(m.filed, vmIDs...)
	return nil
}
func (m *memPersonal) FolderOf(context.Context, uuid.UUID, uuid.UUID) (*uuid.UUID, error) {
	return nil, nil
}

func newPersonal(t *testing.T, visible ...uuid.UUID) (*Personal, *memPersonal, *scopedInventory) {
	t.Helper()
	allowed := map[uuid.UUID]bool{}
	for _, id := range visible {
		allowed[id] = true
	}
	store := newMemPersonal()
	inv := &scopedInventory{visible: allowed}
	return &Personal{Store: store, Inventory: inv, Clock: &fakeClock{t: time.Now()}}, store, inv
}

// Without this check the favourites table is a way to probe for VM ids: star
// one, see whether it was accepted, and learn by trial which machines exist
// behind a grant you do not have.
func TestFavouritingAVMYouCannotSeeIsRefused(t *testing.T) {
	mine, theirs := uuid.New(), uuid.New()
	h, store, _ := newPersonal(t, mine)

	if err := h.SetFavourite(context.Background(), uuid.New(), theirs, identity.RoleOperator, true); !errors.Is(err, ErrVMNotVisible) {
		t.Fatalf("err = %v, want ErrVMNotVisible", err)
	}
	if _, present := store.favourites[theirs]; present {
		t.Error("a VM outside the caller's grants was starred anyway")
	}

	if err := h.SetFavourite(context.Background(), uuid.New(), mine, identity.RoleOperator, true); err != nil {
		t.Fatalf("starring a visible VM: %v", err)
	}
	if !store.favourites[mine] {
		t.Error("a visible VM was not starred")
	}
}

// A request naming one machine the caller cannot see files none of them. A
// partial success would be both a disclosure and a mess to undo.
func TestFilingIsRefusedEntirelyIfAnyVMIsNotVisible(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	hidden := uuid.New()
	h, store, _ := newPersonal(t, a, b)
	folder := uuid.New()

	err := h.FileVMs(context.Background(), uuid.New(), identity.RoleOperator,
		[]uuid.UUID{a, hidden, b}, &folder)
	if !errors.Is(err, ErrVMNotVisible) {
		t.Fatalf("err = %v, want ErrVMNotVisible", err)
	}
	if len(store.filed) != 0 {
		t.Errorf("filed %v; a request with one invisible VM must file none", store.filed)
	}

	// The three-VMs-into-one-folder case this feature exists for.
	if err := h.FileVMs(context.Background(), uuid.New(), identity.RoleOperator,
		[]uuid.UUID{a, b}, &folder); err != nil {
		t.Fatalf("filing visible VMs: %v", err)
	}
	if len(store.filed) != 2 {
		t.Errorf("filed %v, want both", store.filed)
	}
}

func TestFilingNothingTouchesNothing(t *testing.T) {
	h, store, inv := newPersonal(t)
	if err := h.FileVMs(context.Background(), uuid.New(), identity.RoleAdmin, nil, nil); err != nil {
		t.Fatalf("FileVMs: %v", err)
	}
	if len(store.filed) != 0 || inv.asked != 0 {
		t.Error("an empty request reached storage or the access check")
	}
}

// "Production " and "Production" would be two folders that look identical in
// the picker they exist for.
func TestFolderNamesAreTidiedAndRequired(t *testing.T) {
	h, store, _ := newPersonal(t)
	ctx := context.Background()

	folder, err := h.CreateFolder(ctx, uuid.New(), "  Production   Web  ")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if folder.Name != "Production Web" {
		t.Errorf("name = %q, want the whitespace collapsed", folder.Name)
	}
	// A new folder goes to the end of the user's own ordering, which is where
	// somebody who just made one looks for it.
	if folder.Position != 0 {
		t.Errorf("position = %d, want 0 for the first folder", folder.Position)
	}
	second, err := h.CreateFolder(ctx, uuid.New(), "Staging")
	if err != nil {
		t.Fatal(err)
	}
	if second.Position != 1 {
		t.Errorf("position = %d, want it after the first", second.Position)
	}

	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, err := h.CreateFolder(ctx, uuid.New(), bad); !errors.Is(err, ErrFolderNameRequired) {
			t.Errorf("CreateFolder(%q) = %v, want ErrFolderNameRequired", bad, err)
		}
	}
	if _, err := h.CreateFolder(ctx, uuid.New(), strings.Repeat("x", maxFolderName+1)); err == nil {
		t.Error("a name too long for any picker was accepted")
	}
	if len(store.created) != 2 {
		t.Errorf("stored %d folders, want only the two valid ones", len(store.created))
	}
}

// Renaming goes through the same tidying, and a negative position is a client
// bug rather than a reason to fail.
func TestRenamingTidiesAndClampsPosition(t *testing.T) {
	h, _, _ := newPersonal(t)
	if err := h.RenameFolder(context.Background(), uuid.New(), uuid.New(), "  Kept  ", -5); err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}
	if err := h.RenameFolder(context.Background(), uuid.New(), uuid.New(), "  ", 0); !errors.Is(err, ErrFolderNameRequired) {
		t.Errorf("an empty rename was accepted")
	}
}
