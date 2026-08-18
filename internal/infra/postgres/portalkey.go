package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/shell"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// PortalKeyRepository stores the portal's SSH key pair (SSH-11, ADR 0006).
//
// The vault is held here rather than passed per call, unlike the platform
// credentials repository: there is one key, every read of it needs opening,
// and threading a vault through the connect path would put a decryption
// argument on a command that already carries a credential.
type PortalKeyRepository struct {
	db    *Pool
	vault *crypto.Vault
}

// NewPortalKeyRepository builds the repository.
func NewPortalKeyRepository(db *Pool, vault *crypto.Vault) *PortalKeyRepository {
	return &PortalKeyRepository{db: db, vault: vault}
}

var _ ports.PortalKeyStore = (*PortalKeyRepository)(nil)

// Get returns the public record of the key, and never the private half.
func (r *PortalKeyRepository) Get(ctx context.Context) (shell.PortalKey, error) {
	var key shell.PortalKey
	var createdBy *uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT algorithm, public_key, fingerprint, created_at, created_by
		   FROM ssh_portal_key WHERE id`).
		Scan(&key.Algorithm, &key.PublicKey, &key.Fingerprint, &key.CreatedAt, &createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return shell.PortalKey{}, ports.ErrNotFound
	}
	if err != nil {
		return shell.PortalKey{}, fmt.Errorf("read portal ssh key: %w", err)
	}
	if createdBy != nil {
		key.CreatedBy = *createdBy
	}
	return key, nil
}

// PrivateKey opens the sealed private half.
func (r *PortalKeyRepository) PrivateKey(ctx context.Context) (string, error) {
	var sealed crypto.SealedSecret
	err := r.db.QueryRow(ctx,
		`SELECT ciphertext, nonce, dek_wrapped, dek_nonce, key_version
		   FROM ssh_portal_key WHERE id`).
		Scan(&sealed.Ciphertext, &sealed.Nonce, &sealed.DEKWrapped, &sealed.DEKNonce,
			&sealed.KeyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ports.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read portal ssh key: %w", err)
	}
	return r.vault.Open(sealed)
}

// Replace writes a new pair over whatever was there.
//
// Installs are deliberately not touched. Their fingerprints now differ from
// the current key's, which is what marks them stale, and that list is the
// thing an administrator needs after a rotation: every guest still carrying a
// line for a key nobody holds.
func (r *PortalKeyRepository) Replace(ctx context.Context, key shell.PortalKey, privatePEM string) error {
	sealed, err := r.vault.Seal(privatePEM)
	if err != nil {
		return fmt.Errorf("seal portal ssh key: %w", err)
	}
	var createdBy *uuid.UUID
	if key.CreatedBy != uuid.Nil {
		createdBy = &key.CreatedBy
	}
	_, err = r.db.Exec(ctx,
		`INSERT INTO ssh_portal_key
		     (id, algorithm, public_key, fingerprint,
		      ciphertext, nonce, dek_wrapped, dek_nonce, key_version, created_at, created_by)
		 VALUES (true, $1,$2,$3, $4,$5,$6,$7,$8, $9,$10)
		 ON CONFLICT (id) DO UPDATE SET
		     algorithm=EXCLUDED.algorithm, public_key=EXCLUDED.public_key,
		     fingerprint=EXCLUDED.fingerprint, ciphertext=EXCLUDED.ciphertext,
		     nonce=EXCLUDED.nonce, dek_wrapped=EXCLUDED.dek_wrapped,
		     dek_nonce=EXCLUDED.dek_nonce, key_version=EXCLUDED.key_version,
		     created_at=EXCLUDED.created_at, created_by=EXCLUDED.created_by`,
		key.Algorithm, key.PublicKey, key.Fingerprint,
		sealed.Ciphertext, sealed.Nonce, sealed.DEKWrapped, sealed.DEKNonce,
		sealed.KeyVersion, key.CreatedAt, createdBy)
	if err != nil {
		return fmt.Errorf("store portal ssh key: %w", err)
	}
	return nil
}

// Delete removes the pair.
func (r *PortalKeyRepository) Delete(ctx context.Context) error {
	if _, err := r.db.Exec(ctx, `DELETE FROM ssh_portal_key WHERE id`); err != nil {
		return fmt.Errorf("delete portal ssh key: %w", err)
	}
	return nil
}

// Installs lists where the key is believed to be. Passing uuid.Nil lists the
// whole estate, which is the view an administrator needs before rotating.
func (r *PortalKeyRepository) Installs(ctx context.Context, vmID uuid.UUID) ([]shell.KeyInstall, error) {
	// The staleness comparison is done in SQL against the one key row so that
	// a caller cannot forget it and show a rotated-away install as usable.
	const q = `
		SELECT i.vm_id, v.name, i.ssh_user, i.fingerprint, i.installed_at,
		       COALESCE(i.installed_by, '00000000-0000-0000-0000-000000000000'::uuid),
		       (k.fingerprint IS NULL OR k.fingerprint <> i.fingerprint)
		  FROM ssh_key_installs i
		  JOIN vms v ON v.id = i.vm_id
		  LEFT JOIN ssh_portal_key k ON true
		 WHERE ($1::uuid IS NULL OR i.vm_id = $1)
		 ORDER BY i.installed_at DESC`

	var filter *uuid.UUID
	if vmID != uuid.Nil {
		filter = &vmID
	}
	rows, err := r.db.Query(ctx, q, filter)
	if err != nil {
		return nil, fmt.Errorf("list key installs: %w", err)
	}
	defer rows.Close()

	out := []shell.KeyInstall{}
	for rows.Next() {
		var in shell.KeyInstall
		if err := rows.Scan(&in.VMID, &in.VMName, &in.SSHUser, &in.Fingerprint,
			&in.InstalledAt, &in.InstalledBy, &in.Stale); err != nil {
			return nil, fmt.Errorf("scan key install: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// RecordInstall notes an install, replacing any earlier one for the same
// account on the same VM - which is what re-installing after a rotation is.
func (r *PortalKeyRepository) RecordInstall(ctx context.Context, in shell.KeyInstall) error {
	var by *uuid.UUID
	if in.InstalledBy != uuid.Nil {
		by = &in.InstalledBy
	}
	_, err := r.db.Exec(ctx,
		`INSERT INTO ssh_key_installs (vm_id, ssh_user, fingerprint, installed_at, installed_by)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (vm_id, ssh_user) DO UPDATE SET
		     fingerprint=EXCLUDED.fingerprint, installed_at=EXCLUDED.installed_at,
		     installed_by=EXCLUDED.installed_by`,
		in.VMID, in.SSHUser, in.Fingerprint, in.InstalledAt, by)
	if err != nil {
		return fmt.Errorf("record key install: %w", err)
	}
	return nil
}

// ForgetInstall drops one record.
func (r *PortalKeyRepository) ForgetInstall(ctx context.Context, vmID uuid.UUID, sshUser string) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM ssh_key_installs WHERE vm_id=$1 AND ssh_user=$2`, vmID, sshUser)
	if err != nil {
		return fmt.Errorf("forget key install: %w", err)
	}
	return nil
}
