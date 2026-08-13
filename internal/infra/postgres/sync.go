package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	appsync "github.com/freezxp/proxui/internal/app/sync"
)

// SyncRepository records how synchronization went and queues the events it
// produced.
type SyncRepository struct{ db *Pool }

// NewSyncRepository builds the repository.
func NewSyncRepository(db *Pool) *SyncRepository { return &SyncRepository{db: db} }

// StartRun opens a sync run and returns its id.
func (r *SyncRepository) StartRun(ctx context.Context, platformID uuid.UUID, kind, trigger string, now time.Time) (int64, error) {
	var id int64
	err := r.db.QueryRow(ctx, `
		INSERT INTO sync_runs (platform_id, kind, status, trigger, started_at)
		VALUES ($1,$2,'running',$3,$4) RETURNING id`,
		platformID, kind, trigger, now).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("start sync run: %w", err)
	}
	return id, nil
}

// FinishRun closes a sync run with its outcome and counters.
func (r *SyncRepository) FinishRun(ctx context.Context, runID int64, status string, stats map[string]any, runErr string, now time.Time) error {
	payload, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("encode sync stats: %w", err)
	}
	_, err = r.db.Exec(ctx, `
		UPDATE sync_runs SET status=$2, stats=$3, error=$4, finished_at=$5 WHERE id=$1`,
		runID, status, payload, runErr, now)
	if err != nil {
		return fmt.Errorf("finish sync run: %w", err)
	}
	return nil
}

// RecordError attaches a scoped error to a run. One failing node should not
// fail a whole cluster sync, so these accumulate and the run ends "partial".
func (r *SyncRepository) RecordError(ctx context.Context, runID int64, scope, message string, detail map[string]any) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		payload = []byte(`{}`)
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO sync_errors (sync_run_id, scope, message, detail) VALUES ($1,$2,$3,$4)`,
		runID, scope, message, payload)
	if err != nil {
		return fmt.Errorf("record sync error: %w", err)
	}
	return nil
}

// ListRuns returns a platform's recent sync history.
func (r *SyncRepository) ListRuns(ctx context.Context, platformID uuid.UUID, limit int) ([]ports.SyncRunSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, kind::text, status::text, trigger, started_at, finished_at, stats, error
		FROM sync_runs WHERE platform_id=$1 ORDER BY started_at DESC LIMIT $2`,
		platformID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sync runs: %w", err)
	}
	defer rows.Close()

	var out []ports.SyncRunSummary
	for rows.Next() {
		var (
			s        ports.SyncRunSummary
			finished *time.Time
			stats    []byte
		)
		if err := rows.Scan(&s.ID, &s.Kind, &s.Status, &s.Trigger, &s.StartedAt, &finished, &stats, &s.Error); err != nil {
			return nil, fmt.Errorf("scan sync run: %w", err)
		}
		if finished != nil {
			s.FinishedAt = *finished
			s.DurationMS = s.FinishedAt.Sub(s.StartedAt).Milliseconds()
		}
		if len(stats) > 0 {
			_ = json.Unmarshal(stats, &s.Stats)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PublishEvent appends a domain event to the outbox. Called inside the same
// transaction as the state change it describes, so an event can never be lost
// to a crash between the write and the publish (docs/10-sync-engine.md §10.8).
func (r *SyncRepository) PublishEvent(ctx context.Context, tx ports.Querier, e ports.DomainEvent) error {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("encode event payload: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO events_outbox (occurred_at, category, event_type, severity, payload)
		VALUES ($1,$2,$3,$4,$5)`,
		e.OccurredAt, e.Category, e.Type, e.Severity, payload)
	if err != nil {
		return fmt.Errorf("queue event: %w", err)
	}
	return nil
}

// UnpublishedEvents returns queued events for the relay to publish.
func (r *SyncRepository) UnpublishedEvents(ctx context.Context, limit int) ([]ports.DomainEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, occurred_at, category, event_type, severity, payload
		FROM events_outbox WHERE published_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("load outbox: %w", err)
	}
	defer rows.Close()

	var out []ports.DomainEvent
	for rows.Next() {
		var (
			e       ports.DomainEvent
			payload []byte
		)
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.Category, &e.Type, &e.Severity, &payload); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkEventsPublished records that events reached the bus. Consumers are
// idempotent, so at-least-once delivery is safe.
func (r *SyncRepository) MarkEventsPublished(ctx context.Context, ids []int64, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.db.Exec(ctx, `UPDATE events_outbox SET published_at=$2 WHERE id = ANY($1)`, ids, now)
	if err != nil {
		return fmt.Errorf("mark events published: %w", err)
	}
	return nil
}

// Begin starts a transaction for the reconciler's batched writes. pgx.Tx
// already satisfies the application's Tx interface, so no adapter is needed.
func (r *SyncRepository) Begin(ctx context.Context) (appsync.Tx, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	return tx, nil
}
