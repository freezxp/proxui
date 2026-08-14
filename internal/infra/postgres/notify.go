package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/notify"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// NotifyRepository stores channels, routing rules and the delivery log.
type NotifyRepository struct{ db *Pool }

// NewNotifyRepository builds the repository.
func NewNotifyRepository(db *Pool) *NotifyRepository { return &NotifyRepository{db: db} }

// --- channels ------------------------------------------------------------

// CreateChannel stores a channel, sealing its secret if one was supplied.
func (r *NotifyRepository) CreateChannel(ctx context.Context, ch *notify.Channel, sealed *crypto.SealedSecret) error {
	config, err := json.Marshal(ch.Config)
	if err != nil {
		return fmt.Errorf("encode channel config: %w", err)
	}
	var (
		ciphertext, nonce, dekWrapped, dekNonce []byte
		keyVersion                              = 1
	)
	if sealed != nil {
		ciphertext, nonce = sealed.Ciphertext, sealed.Nonce
		dekWrapped, dekNonce, keyVersion = sealed.DEKWrapped, sealed.DEKNonce, sealed.KeyVersion
	}

	_, err = r.db.Exec(ctx,
		`INSERT INTO notification_channels
		   (id, name, kind, config, ciphertext, nonce, dek_wrapped, dek_nonce, key_version, is_enabled, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		ch.ID, ch.Name, string(ch.Kind), config,
		ciphertext, nonce, dekWrapped, dekNonce, keyVersion, ch.IsEnabled, ch.CreatedAt)
	return wrapConflict(err, "create notification channel")
}

// UpdateChannel replaces a channel's configuration. A nil sealed secret keeps
// the stored one: an administrator editing a recipient list must not have to
// retype an SMTP password they cannot read back.
func (r *NotifyRepository) UpdateChannel(ctx context.Context, ch *notify.Channel, sealed *crypto.SealedSecret) error {
	config, err := json.Marshal(ch.Config)
	if err != nil {
		return fmt.Errorf("encode channel config: %w", err)
	}

	if sealed == nil {
		tag, err := r.db.Exec(ctx,
			`UPDATE notification_channels SET name=$2, config=$3, is_enabled=$4
			  WHERE id=$1 AND deleted_at IS NULL`,
			ch.ID, ch.Name, config, ch.IsEnabled)
		if err != nil {
			return wrapConflict(err, "update notification channel")
		}
		if tag.RowsAffected() == 0 {
			return ports.ErrNotFound
		}
		return nil
	}

	tag, err := r.db.Exec(ctx,
		`UPDATE notification_channels
		    SET name=$2, config=$3, is_enabled=$4,
		        ciphertext=$5, nonce=$6, dek_wrapped=$7, dek_nonce=$8, key_version=$9
		  WHERE id=$1 AND deleted_at IS NULL`,
		ch.ID, ch.Name, config, ch.IsEnabled,
		sealed.Ciphertext, sealed.Nonce, sealed.DEKWrapped, sealed.DEKNonce, sealed.KeyVersion)
	if err != nil {
		return wrapConflict(err, "update notification channel")
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

const channelColumns = `id, name, kind, config, is_enabled, created_at,
	(ciphertext IS NOT NULL) AS has_secret`

// ListChannels returns every live channel.
func (r *NotifyRepository) ListChannels(ctx context.Context) ([]notify.Channel, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+channelColumns+` FROM notification_channels
		  WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list notification channels: %w", err)
	}
	defer rows.Close()

	out := []notify.Channel{}
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// GetChannel reads one channel.
func (r *NotifyRepository) GetChannel(ctx context.Context, id uuid.UUID) (notify.Channel, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+channelColumns+` FROM notification_channels
		  WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return notify.Channel{}, fmt.Errorf("get notification channel: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return notify.Channel{}, ports.ErrNotFound
	}
	return scanChannel(rows)
}

func scanChannel(rows pgx.Rows) (notify.Channel, error) {
	var (
		ch     notify.Channel
		kind   string
		config []byte
	)
	if err := rows.Scan(&ch.ID, &ch.Name, &kind, &config, &ch.IsEnabled, &ch.CreatedAt, &ch.HasSecret); err != nil {
		return notify.Channel{}, fmt.Errorf("scan notification channel: %w", err)
	}
	ch.Kind = notify.Kind(kind)
	if len(config) > 0 {
		if err := json.Unmarshal(config, &ch.Config); err != nil {
			return notify.Channel{}, fmt.Errorf("decode channel config: %w", err)
		}
	}
	if ch.Config == nil {
		ch.Config = map[string]any{}
	}
	return ch, nil
}

// ChannelSecret opens a channel's stored secret. It is read per delivery and
// never cached, matching how platform credentials are handled.
func (r *NotifyRepository) ChannelSecret(ctx context.Context, id uuid.UUID, vault *crypto.Vault) (string, error) {
	var sealed crypto.SealedSecret
	err := r.db.QueryRow(ctx,
		`SELECT ciphertext, nonce, dek_wrapped, dek_nonce, key_version
		   FROM notification_channels WHERE id=$1 AND deleted_at IS NULL`, id).
		Scan(&sealed.Ciphertext, &sealed.Nonce, &sealed.DEKWrapped, &sealed.DEKNonce, &sealed.KeyVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ports.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read channel secret: %w", err)
	}
	if len(sealed.Ciphertext) == 0 {
		return "", nil // a channel may legitimately have no secret
	}
	return vault.Open(sealed)
}

// DeleteChannel soft-deletes a channel; its rules cascade on the hard delete
// path only, so they are removed explicitly here.
func (r *NotifyRepository) DeleteChannel(ctx context.Context, id uuid.UUID, at time.Time) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete notification channel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM notification_rules WHERE channel_id=$1`, id); err != nil {
		return fmt.Errorf("delete channel rules: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE notification_channels SET deleted_at=$2 WHERE id=$1 AND deleted_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("delete notification channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return tx.Commit(ctx)
}

// --- rules ---------------------------------------------------------------

// CreateRule stores a routing rule.
func (r *NotifyRepository) CreateRule(ctx context.Context, rule *notify.Rule) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO notification_rules
		   (id, category, min_severity, platform_id, vm_group_id, channel_id, is_enabled, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		rule.ID, rule.Category, string(rule.MinSeverity), rule.PlatformID, rule.VMGroupID,
		rule.ChannelID, rule.IsEnabled, rule.CreatedAt)
	return wrapConflict(err, "create notification rule")
}

// ListRules returns every rule with its channel's name attached, so the UI and
// the dispatcher both avoid a second lookup.
func (r *NotifyRepository) ListRules(ctx context.Context) ([]notify.Rule, error) {
	rows, err := r.db.Query(ctx,
		`SELECT r.id, r.category, r.min_severity, r.platform_id, r.vm_group_id,
		        r.channel_id, c.name, r.is_enabled, r.created_at
		   FROM notification_rules r
		   JOIN notification_channels c ON c.id = r.channel_id
		  WHERE c.deleted_at IS NULL
		  ORDER BY r.category, c.name`)
	if err != nil {
		return nil, fmt.Errorf("list notification rules: %w", err)
	}
	defer rows.Close()

	out := []notify.Rule{}
	for rows.Next() {
		var (
			rule     notify.Rule
			severity string
		)
		if err := rows.Scan(&rule.ID, &rule.Category, &severity, &rule.PlatformID, &rule.VMGroupID,
			&rule.ChannelID, &rule.ChannelName, &rule.IsEnabled, &rule.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan notification rule: %w", err)
		}
		rule.MinSeverity = notify.Severity(severity)
		out = append(out, rule)
	}
	return out, rows.Err()
}

// DeleteRule removes a routing rule.
func (r *NotifyRepository) DeleteRule(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM notification_rules WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete notification rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// --- deliveries ----------------------------------------------------------

// ClaimDelivery records an intent to deliver, and reports whether this caller
// is the one that should send it.
//
// The unique index on (outbox_id, channel_id) is what makes this safe: if the
// relay republishes an event after a crash, the second insert conflicts and
// this returns false, so the same message is never sent twice.
func (r *NotifyRepository) ClaimDelivery(ctx context.Context, outboxID int64, channelID uuid.UUID, subject string, now time.Time) (int64, bool, error) {
	var id int64
	err := r.db.QueryRow(ctx,
		`INSERT INTO notification_deliveries (outbox_id, channel_id, subject, status, created_at)
		 VALUES ($1,$2,$3,'pending',$4)
		 ON CONFLICT (outbox_id, channel_id) WHERE outbox_id IS NOT NULL DO NOTHING
		 RETURNING id`, outboxID, channelID, subject, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil // already claimed by an earlier pass
	}
	if err != nil {
		return 0, false, fmt.Errorf("claim delivery: %w", err)
	}
	return id, true, nil
}

// RecordDelivery stores an unclaimed delivery, for test sends which have no
// originating event.
func (r *NotifyRepository) RecordDelivery(ctx context.Context, channelID uuid.UUID, subject string, now time.Time) (int64, error) {
	var id int64
	err := r.db.QueryRow(ctx,
		`INSERT INTO notification_deliveries (channel_id, subject, status, created_at)
		 VALUES ($1,$2,'pending',$3) RETURNING id`, channelID, subject, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record delivery: %w", err)
	}
	return id, nil
}

// FinishDelivery marks a delivery sent or failed and counts the attempt.
func (r *NotifyRepository) FinishDelivery(ctx context.Context, id int64, sendErr error, now time.Time) error {
	status, message := "sent", ""
	if sendErr != nil {
		status, message = "failed", sendErr.Error()
	}
	var sentAt any
	if sendErr == nil {
		sentAt = now
	}
	_, err := r.db.Exec(ctx,
		`UPDATE notification_deliveries
		    SET status=$2, attempts=attempts+1, last_error=$3, sent_at=$4::timestamptz
		  WHERE id=$1`, id, status, message, sentAt)
	if err != nil {
		return fmt.Errorf("finish delivery: %w", err)
	}
	return nil
}

// ListDeliveries returns the delivery log, newest first.
func (r *NotifyRepository) ListDeliveries(ctx context.Context, limit int) ([]ports.DeliveryRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.Query(ctx,
		`SELECT d.id, d.channel_id, c.name, d.subject, d.status, d.attempts,
		        d.last_error, d.created_at, d.sent_at
		   FROM notification_deliveries d
		   JOIN notification_channels c ON c.id = d.channel_id
		  ORDER BY d.created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()

	out := []ports.DeliveryRecord{}
	for rows.Next() {
		var rec ports.DeliveryRecord
		if err := rows.Scan(&rec.ID, &rec.ChannelID, &rec.ChannelName, &rec.Subject,
			&rec.Status, &rec.Attempts, &rec.LastError, &rec.CreatedAt, &rec.SentAt); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
