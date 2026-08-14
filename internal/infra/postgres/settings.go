package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SettingsRepository stores the values an administrator has changed. Defaults
// live in the catalogue, not the table: storing them would mean a later change
// to a default silently failed to apply.
type SettingsRepository struct{ db *Pool }

// NewSettingsRepository builds the repository.
func NewSettingsRepository(db *Pool) *SettingsRepository { return &SettingsRepository{db: db} }

// All returns every stored override.
func (r *SettingsRepository) All(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var (
			key string
			raw []byte
		)
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		var value int
		if err := json.Unmarshal(raw, &value); err != nil {
			// A value that is not a number is a value nothing can use; skip it
			// rather than failing the whole page.
			continue
		}
		out[key] = value
	}
	return out, rows.Err()
}

// Set stores one override.
func (r *SettingsRepository) Set(ctx context.Context, key string, value int, by uuid.UUID, at time.Time) error {
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

// Reset removes an override, returning the setting to its default.
func (r *SettingsRepository) Reset(ctx context.Context, key string) error {
	_, err := r.db.Exec(ctx, `DELETE FROM settings WHERE key=$1`, key)
	if err != nil {
		return fmt.Errorf("reset setting: %w", err)
	}
	return nil
}
