package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/freezxp/proxui/internal/app/ports"
)

// AuditRepository appends entries to the append-only audit log.
// There is deliberately no Update or Delete: retention is handled by dropping
// whole partitions, and the application DB role holds only INSERT and SELECT
// on this table (docs/03-frs.md AUD-03).
type AuditRepository struct{ db *Pool }

// NewAuditRepository builds the repository.
func NewAuditRepository(db *Pool) *AuditRepository { return &AuditRepository{db: db} }

// Write appends one audit entry.
func (r *AuditRepository) Write(ctx context.Context, e ports.AuditEntry) error {
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	payload, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode audit details: %w", err)
	}
	if e.Outcome == "" {
		e.Outcome = ports.OutcomeSuccess
	}

	_, err = r.db.Exec(ctx, `
		INSERT INTO audit_logs (ts, actor_user_id, actor_name, category, action,
			target_type, target_id, target_name, source_ip, user_agent,
			outcome, request_id, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		e.Time, e.ActorUserID, e.ActorName, e.Category, e.Action,
		nullString(e.TargetType), nullString(e.TargetID), nullString(e.TargetName),
		nullIP(e.SourceIP), nullString(e.UserAgent), e.Outcome, nullString(e.RequestID), payload)
	if err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}
	return nil
}
