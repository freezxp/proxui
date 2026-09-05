package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// A user's own view of the inventory: favourites and folders (INV-16…INV-19).
//
// None of it is audited. This is one person's opinion about their own list, not
// a change to the estate, and a stream of "starred a VM" entries would bury the
// security events an auditor is actually searching for.

// ErrVMNotVisible means the caller may not act on a VM they cannot see.
var ErrVMNotVisible = errors.New("command: the VM is not visible to this user")

// ErrFolderNameRequired means a folder was submitted without a usable name.
var ErrFolderNameRequired = errors.New("command: a folder needs a name")

// maxFolderName bounds what will fit in a picker without becoming the whole UI.
const maxFolderName = 60

// Personal handles favourites and folders.
type Personal struct {
	Store     ports.PersonalRepository
	Inventory ports.InventoryReader
	Clock     ports.Clock
}

// SetFavourite stars or unstars a VM for one user.
//
// The access check is the point of this layer. Without it the favourites table
// becomes a way to probe for VM ids: star an id, see whether it was accepted,
// and learn by trial which machines exist behind a grant you do not have. It is
// the same reason GetVM refuses to distinguish "does not exist" from "not
// yours" (RBAC-05).
func (h *Personal) SetFavourite(ctx context.Context, userID, vmID uuid.UUID, role identity.Role, on bool) error {
	if err := h.mayTouch(ctx, userID, vmID, role); err != nil {
		return err
	}
	return h.Store.SetFavourite(ctx, userID, vmID, on, h.Clock.Now())
}

// ListFolders returns the caller's own folders.
func (h *Personal) ListFolders(ctx context.Context, userID uuid.UUID) ([]ports.VMFolder, error) {
	return h.Store.ListFolders(ctx, userID)
}

// CreateFolder adds a folder for the caller.
func (h *Personal) CreateFolder(ctx context.Context, userID uuid.UUID, name string) (ports.VMFolder, error) {
	clean, err := folderName(name)
	if err != nil {
		return ports.VMFolder{}, err
	}
	// New folders go to the end of the user's ordering, which is where someone
	// who just made one expects to find it.
	existing, err := h.Store.ListFolders(ctx, userID)
	if err != nil {
		return ports.VMFolder{}, err
	}
	folder := ports.VMFolder{
		ID: uuid.New(), Name: clean, Position: len(existing), CreatedAt: h.Clock.Now(),
	}
	if err := h.Store.CreateFolder(ctx, &folder, userID); err != nil {
		return ports.VMFolder{}, err
	}
	return folder, nil
}

// RenameFolder renames and repositions a folder the caller owns.
func (h *Personal) RenameFolder(ctx context.Context, userID, folderID uuid.UUID, name string, position int) error {
	clean, err := folderName(name)
	if err != nil {
		return err
	}
	if position < 0 {
		position = 0
	}
	return h.Store.UpdateFolder(ctx, userID, folderID, clean, position)
}

// DeleteFolder removes a folder. Its VMs become unfiled rather than being
// removed from the inventory — a folder is a way of looking at machines, not a
// container that owns them.
func (h *Personal) DeleteFolder(ctx context.Context, userID, folderID uuid.UUID) error {
	return h.Store.DeleteFolder(ctx, userID, folderID)
}

// FileVMs moves VMs into a folder, or out of any folder when folderID is nil.
//
// Every VM is access-checked before any of them moves, so a request naming one
// machine the caller cannot see files none of them. A partial success here
// would be both a disclosure and a mess to undo.
func (h *Personal) FileVMs(ctx context.Context, userID uuid.UUID, role identity.Role,
	vmIDs []uuid.UUID, folderID *uuid.UUID) error {

	if len(vmIDs) == 0 {
		return nil
	}
	for _, vmID := range vmIDs {
		if err := h.mayTouch(ctx, userID, vmID, role); err != nil {
			return err
		}
	}
	return h.Store.FileVMs(ctx, userID, vmIDs, folderID, h.Clock.Now())
}

// mayTouch reports whether the caller can see the VM they are trying to
// organise.
func (h *Personal) mayTouch(ctx context.Context, userID, vmID uuid.UUID, role identity.Role) error {
	allowed, err := h.Inventory.CanAccessVM(ctx, vmID, role, userID)
	if err != nil {
		return fmt.Errorf("personal: %w", err)
	}
	if !allowed {
		return ErrVMNotVisible
	}
	return nil
}

// folderName trims a name and collapses inner whitespace, so "Production " and
// "Production" cannot become two folders that look identical in a list.
func folderName(name string) (string, error) {
	clean := strings.Join(strings.Fields(name), " ")
	if clean == "" {
		return "", ErrFolderNameRequired
	}
	if len(clean) > maxFolderName {
		return "", fmt.Errorf("%w: at most %d characters", ErrFolderNameRequired, maxFolderName)
	}
	return clean, nil
}
