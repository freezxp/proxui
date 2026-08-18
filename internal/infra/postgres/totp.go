package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// TOTPRepository stores second-factor enrolment on the users row (AUTH-04).
//
// The seed is sealed with the same envelope encryption as a platform
// credential, and it is deliberately not part of the user aggregate: nothing
// that renders a user, lists users or serializes one can carry it, because the
// only call that returns it is Secret.
type TOTPRepository struct{ db *Pool }

// NewTOTPRepository builds the repository.
func NewTOTPRepository(db *Pool) *TOTPRepository { return &TOTPRepository{db: db} }

var _ ports.TOTPStore = (*TOTPRepository)(nil)

// Enroll stores a sealed seed without enabling the factor.
//
// The flag stays false until a code proves the device actually holds the seed.
// Enabling on enrolment would lock an account out of its own portal the moment
// somebody scanned a QR badly.
func (r *TOTPRepository) Enroll(ctx context.Context, userID uuid.UUID,
	sealed crypto.SealedSecret, at time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE users SET
			totp_ciphertext=$2, totp_nonce=$3, totp_dek_wrapped=$4, totp_dek_nonce=$5,
			totp_key_version=$6, totp_enabled=false, totp_last_step=NULL, updated_at=$7
		WHERE id=$1`,
		userID, sealed.Ciphertext, sealed.Nonce, sealed.DEKWrapped, sealed.DEKNonce,
		sealed.KeyVersion, at)
	if err != nil {
		return fmt.Errorf("enroll totp: %w", err)
	}
	return nil
}

// Secret opens the sealed seed.
func (r *TOTPRepository) Secret(ctx context.Context, userID uuid.UUID, vault *crypto.Vault) (string, error) {
	var sealed crypto.SealedSecret
	err := r.db.QueryRow(ctx, `
		SELECT totp_ciphertext, totp_nonce, totp_dek_wrapped, totp_dek_nonce, totp_key_version
		  FROM users WHERE id=$1`, userID).
		Scan(&sealed.Ciphertext, &sealed.Nonce, &sealed.DEKWrapped, &sealed.DEKNonce,
			&sealed.KeyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ports.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read totp secret: %w", err)
	}
	if len(sealed.Ciphertext) == 0 {
		return "", ports.ErrNotFound
	}
	return vault.Open(sealed)
}

// Enable turns a confirmed enrolment on and spends the confirming step.
func (r *TOTPRepository) Enable(ctx context.Context, userID uuid.UUID, step int64, at time.Time) error {
	// The guard matters: without it, a confirm racing a disable could switch
	// the flag on for a row whose seed had just been cleared, leaving an
	// account demanding codes that nothing can produce.
	tag, err := r.db.Exec(ctx, `
		UPDATE users SET totp_enabled=true, totp_last_step=$2, updated_at=$3
		WHERE id=$1 AND totp_ciphertext IS NOT NULL`, userID, step, at)
	if err != nil {
		return fmt.Errorf("enable totp: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// Disable clears the seed and the flag in one statement.
func (r *TOTPRepository) Disable(ctx context.Context, userID uuid.UUID, at time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE users SET totp_enabled=false, totp_ciphertext=NULL, totp_nonce=NULL,
			totp_dek_wrapped=NULL, totp_dek_nonce=NULL, totp_last_step=NULL, updated_at=$2
		WHERE id=$1`, userID, at)
	if err != nil {
		return fmt.Errorf("disable totp: %w", err)
	}
	return nil
}

// LastStep reads the highest time step already accepted.
func (r *TOTPRepository) LastStep(ctx context.Context, userID uuid.UUID) (*int64, error) {
	var step *int64
	err := r.db.QueryRow(ctx, `SELECT totp_last_step FROM users WHERE id=$1`, userID).Scan(&step)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ports.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read totp step: %w", err)
	}
	return step, nil
}

// RecordStep marks a step as spent.
//
// The `>` guard makes the write itself the race winner: two requests arriving
// with the same code cannot both record it, and the second sees a last step
// that already covers its own.
func (r *TOTPRepository) RecordStep(ctx context.Context, userID uuid.UUID, step int64, at time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE users SET totp_last_step=$2, updated_at=$3
		WHERE id=$1 AND (totp_last_step IS NULL OR totp_last_step < $2)`, userID, step, at)
	if err != nil {
		return fmt.Errorf("record totp step: %w", err)
	}
	return nil
}
