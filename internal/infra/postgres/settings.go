package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/infra/crypto"
)

// SettingsRepository stores the values an administrator has changed. Defaults
// live in the catalogue, not the table: storing them would mean a later change
// to a default silently failed to apply.
type SettingsRepository struct{ db *Pool }

// NewSettingsRepository builds the repository.
func NewSettingsRepository(db *Pool) *SettingsRepository { return &SettingsRepository{db: db} }

// All returns every stored override, undecoded: a setting may hold a number
// or a string, and only the catalogue knows which.
//
// A secret's value is never returned. Its key appears with a null marker, so
// callers can tell one is stored without the ciphertext travelling anywhere it
// does not need to.
func (r *SettingsRepository) All(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := r.db.Query(ctx,
		`SELECT key, value, (ciphertext IS NOT NULL) FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	defer rows.Close()

	out := map[string]json.RawMessage{}
	for rows.Next() {
		var (
			key       string
			raw       []byte
			hasSecret bool
		)
		if err := rows.Scan(&key, &raw, &hasSecret); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		if hasSecret {
			// Present but unreadable from here, which is exactly what the
			// caller needs to know.
			out[key] = json.RawMessage(`null`)
			continue
		}
		out[key] = json.RawMessage(raw)
	}
	return out, rows.Err()
}

// Set stores one override. The value is marshalled by the caller, which is
// the layer that knows whether this setting holds a number or a string.
func (r *SettingsRepository) Set(ctx context.Context, key string, value any, by uuid.UUID, at time.Time) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx,
		`INSERT INTO settings (key, value, updated_at, updated_by) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,
		     updated_at=EXCLUDED.updated_at, updated_by=EXCLUDED.updated_by`,
		key, raw, at, by)
	if err != nil {
		return fmt.Errorf("store setting: %w", err)
	}
	return nil
}

// SetSecret stores an encrypted value, replacing whatever was there.
func (r *SettingsRepository) SetSecret(ctx context.Context, key, value string, vault *crypto.Vault, by uuid.UUID, at time.Time) error {
	sealed, err := vault.Seal(value)
	if err != nil {
		return fmt.Errorf("seal setting: %w", err)
	}
	_, err = r.db.Exec(ctx,
		`INSERT INTO settings (key, value, ciphertext, nonce, dek_wrapped, dek_nonce, key_version, updated_at, updated_by)
		 VALUES ($1, NULL, $2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (key) DO UPDATE SET value=NULL,
		     ciphertext=EXCLUDED.ciphertext, nonce=EXCLUDED.nonce,
		     dek_wrapped=EXCLUDED.dek_wrapped, dek_nonce=EXCLUDED.dek_nonce,
		     key_version=EXCLUDED.key_version,
		     updated_at=EXCLUDED.updated_at, updated_by=EXCLUDED.updated_by`,
		key, sealed.Ciphertext, sealed.Nonce, sealed.DEKWrapped, sealed.DEKNonce,
		sealed.KeyVersion, at, by)
	if err != nil {
		return fmt.Errorf("store secret setting: %w", err)
	}
	return nil
}

// Secret opens a stored secret. Read per use and never cached, matching how
// platform credentials are handled.
func (r *SettingsRepository) Secret(ctx context.Context, key string, vault *crypto.Vault) (string, error) {
	var sealed crypto.SealedSecret
	err := r.db.QueryRow(ctx,
		`SELECT ciphertext, nonce, dek_wrapped, dek_nonce, key_version
		   FROM settings WHERE key = $1`, key).
		Scan(&sealed.Ciphertext, &sealed.Nonce, &sealed.DEKWrapped, &sealed.DEKNonce, &sealed.KeyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // not set is not an error; the caller decides
	}
	if err != nil {
		return "", fmt.Errorf("read secret setting: %w", err)
	}
	if len(sealed.Ciphertext) == 0 {
		return "", nil
	}
	return vault.Open(sealed)
}

// Reset removes an override, returning the setting to its default.
func (r *SettingsRepository) Reset(ctx context.Context, key string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM settings WHERE key=$1`, key)
	if err != nil {
		return fmt.Errorf("reset setting: %w", err)
	}
	return nil
}
