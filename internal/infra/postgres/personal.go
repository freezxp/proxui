package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/freezxp/proxui/internal/app/ports"
)

// PersonalRepository stores one user's own view of the inventory: which VMs
// they have starred and how they have filed them (INV-16…INV-19).
//
// Every query here is keyed by user id, and nothing it stores is readable by
// anyone else. That is the whole contract — these are opinions about the fleet,
// not facts about it, and two people are allowed to disagree.
type PersonalRepository struct{ db *Pool }

// NewPersonalRepository builds the repository.
func NewPersonalRepository(db *Pool) *PersonalRepository { return &PersonalRepository{db: db} }

// SetFavourite stars or unstars a VM. Idempotent in both directions: starring
// something already starred is what a double-click on a slow connection looks
// like, and it is not an error.
func (r *PersonalRepository) SetFavourite(ctx context.Context, userID, vmID uuid.UUID, on bool, at time.Time) error {
	if !on {
		if _, err := r.db.Exec(ctx,
			`DELETE FROM vm_favourites WHERE user_id=$1 AND vm_id=$2`, userID, vmID); err != nil {
			return fmt.Errorf("unfavourite vm: %w", err)
		}
		return nil
	}
	if _, err := r.db.Exec(ctx, `
		INSERT INTO vm_favourites (user_id, vm_id, created_at) VALUES ($1,$2,$3)
		ON CONFLICT (user_id, vm_id) DO NOTHING`, userID, vmID, at); err != nil {
		return fmt.Errorf("favourite vm: %w", err)
	}
	return nil
}

// ListFolders returns a user's folders in their own order, with how many VMs
// are in each.
//
// The count comes from the same query rather than a second round trip because
// a folder picker showing "Production" without saying it holds four machines is
// most of a folder picker.
func (r *PersonalRepository) ListFolders(ctx context.Context, userID uuid.UUID) ([]ports.VMFolder, error) {
	rows, err := r.db.Query(ctx, `
		SELECT f.id, f.name, f.position, f.created_at, count(m.vm_id)
		FROM vm_folders f
		LEFT JOIN vm_folder_members m ON m.folder_id = f.id
		WHERE f.user_id = $1
		GROUP BY f.id
		ORDER BY f.position, f.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer rows.Close()

	out := []ports.VMFolder{}
	for rows.Next() {
		var f ports.VMFolder
		if err := rows.Scan(&f.ID, &f.Name, &f.Position, &f.CreatedAt, &f.VMCount); err != nil {
			return nil, fmt.Errorf("scan folder: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CreateFolder adds a folder. A name already in use by this user is a conflict
// rather than a second folder: two folders with the same name are
// indistinguishable in the picker they exist for.
func (r *PersonalRepository) CreateFolder(ctx context.Context, f *ports.VMFolder, userID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO vm_folders (id, user_id, name, position, created_at)
		VALUES ($1,$2,$3,$4,$5)`, f.ID, userID, f.Name, f.Position, f.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: a folder named %q already exists", ports.ErrConflict, f.Name)
		}
		return fmt.Errorf("create folder: %w", err)
	}
	return nil
}

// UpdateFolder renames or reorders a folder. The user id is part of the WHERE
// rather than checked beforehand, so another user's folder is not found rather
// than refused — the same reasoning GetVM uses for VMs it will not show.
func (r *PersonalRepository) UpdateFolder(ctx context.Context, userID, folderID uuid.UUID, name string, position int) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE vm_folders SET name=$3, position=$4 WHERE id=$2 AND user_id=$1`,
		userID, folderID, name, position)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: a folder named %q already exists", ports.ErrConflict, name)
		}
		return fmt.Errorf("update folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// DeleteFolder removes a folder. The VMs in it are freed, not removed: they go
// back to being unfiled, which is where they started.
func (r *PersonalRepository) DeleteFolder(ctx context.Context, userID, folderID uuid.UUID) error {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM vm_folders WHERE id=$2 AND user_id=$1`, userID, folderID)
	if err != nil {
		return fmt.Errorf("delete folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// FileVMs puts VMs into a folder, or takes them out of whatever folder they are
// in when folderID is nil.
//
// One statement per VM inside one transaction: filing three machines at once is
// the case this exists for, and half of it happening is not an outcome anybody
// asked for. The upsert on the primary key is what enforces one folder per VM —
// re-filing moves rather than duplicating.
func (r *PersonalRepository) FileVMs(ctx context.Context, userID uuid.UUID, vmIDs []uuid.UUID,
	folderID *uuid.UUID, at time.Time) error {

	if len(vmIDs) == 0 {
		return nil
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin file vms: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if folderID == nil {
		if _, err := tx.Exec(ctx,
			`DELETE FROM vm_folder_members WHERE user_id=$1 AND vm_id = ANY($2)`,
			userID, vmIDs); err != nil {
			return fmt.Errorf("unfile vms: %w", err)
		}
		return tx.Commit(ctx)
	}

	// Belongs to this user? Checked here rather than trusted from the request,
	// so filing into somebody else's folder is impossible rather than merely
	// not offered.
	var owned bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM vm_folders WHERE id=$1 AND user_id=$2)`,
		*folderID, userID).Scan(&owned); err != nil {
		return fmt.Errorf("check folder owner: %w", err)
	}
	if !owned {
		return ports.ErrNotFound
	}

	for _, vmID := range vmIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO vm_folder_members (user_id, vm_id, folder_id, filed_at)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (user_id, vm_id) DO UPDATE SET folder_id=EXCLUDED.folder_id, filed_at=EXCLUDED.filed_at`,
			userID, vmID, *folderID, at); err != nil {
			return fmt.Errorf("file vm: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// FolderOf reports which folder a user has filed a VM into, if any.
func (r *PersonalRepository) FolderOf(ctx context.Context, userID, vmID uuid.UUID) (*uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT folder_id FROM vm_folder_members WHERE user_id=$1 AND vm_id=$2`, userID, vmID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("folder of vm: %w", err)
	}
	return &id, nil
}
