package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
)

// AuditQuery searches the append-only audit log. There is no update or delete
// here by design: retention is enforced by dropping whole partitions, and the
// application's database role holds only INSERT and SELECT (AUD-03).
type AuditQuery struct{ db *Pool }

// NewAuditQuery builds the query side.
func NewAuditQuery(db *Pool) *AuditQuery { return &AuditQuery{db: db} }

const auditColumns = `id, ts, actor_user_id, actor_name, category, action,
	coalesce(target_type,''), coalesce(target_id,''), coalesce(target_name,''),
	coalesce(host(source_ip),''), outcome, coalesce(request_id,''), details`

func (q *AuditQuery) buildFilter(f ports.AuditFilter) (string, []any) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	// An unbounded audit search would scan every partition. Default to the
	// last 24 hours rather than the whole retention window.
	from := f.From
	if from.IsZero() {
		from = time.Now().Add(-24 * time.Hour)
	}
	add("ts >= $%d", from)

	if !f.To.IsZero() {
		add("ts <= $%d", f.To)
	}
	if f.ActorID != uuid.Nil {
		add("actor_user_id = $%d", f.ActorID)
	}
	if f.Category != "" {
		add("category = $%d", f.Category)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.Outcome != "" {
		add("outcome = $%d", f.Outcome)
	}
	if f.TargetType != "" {
		add("target_type = $%d", f.TargetType)
	}
	if f.TargetID != "" {
		add("target_id = $%d", f.TargetID)
	}
	if f.Query != "" {
		// Search the human-readable columns and the JSON detail together, so
		// an auditor can find "that thing about 10.0.30.111" without knowing
		// which field held it.
		args = append(args, f.Query)
		where = append(where, fmt.Sprintf(
			`(actor_name ILIKE '%%' || $%d || '%%' OR target_name ILIKE '%%' || $%d || '%%'
			  OR details::text ILIKE '%%' || $%d || '%%')`, len(args), len(args), len(args)))
	}
	return "WHERE " + strings.Join(where, " AND "), args
}

// Search returns a page of audit records, newest first.
func (q *AuditQuery) Search(ctx context.Context, f ports.AuditFilter) (ports.AuditPage, error) {
	whereSQL, args := q.buildFilter(f)

	var total int
	if err := q.db.QueryRow(ctx, "SELECT count(*) FROM audit_logs "+whereSQL, args...).Scan(&total); err != nil {
		return ports.AuditPage{}, fmt.Errorf("count audit records: %w", err)
	}

	limit, offset := f.Limit, f.Offset
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit, offset)

	sql := fmt.Sprintf("SELECT %s FROM audit_logs %s ORDER BY ts DESC, id DESC LIMIT $%d OFFSET $%d",
		auditColumns, whereSQL, len(args)-1, len(args))

	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return ports.AuditPage{}, fmt.Errorf("search audit log: %w", err)
	}
	defer rows.Close()

	page := ports.AuditPage{Total: total, Limit: limit, Offset: offset, Items: []ports.AuditRecord{}}
	for rows.Next() {
		record, err := scanAuditRecord(rows)
		if err != nil {
			return ports.AuditPage{}, err
		}
		page.Items = append(page.Items, record)
	}
	return page, rows.Err()
}

// Stream walks every matching record without buffering them, so a CSV export
// of a large window does not have to fit in memory.
func (q *AuditQuery) Stream(ctx context.Context, f ports.AuditFilter, fn func(ports.AuditRecord) error) error {
	whereSQL, args := q.buildFilter(f)

	limit := f.Limit
	if limit <= 0 {
		limit = 100000 // the documented export ceiling (AUD-04)
	}
	args = append(args, limit)

	sql := fmt.Sprintf("SELECT %s FROM audit_logs %s ORDER BY ts DESC, id DESC LIMIT $%d",
		auditColumns, whereSQL, len(args))

	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("stream audit log: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		record, err := scanAuditRecord(rows)
		if err != nil {
			return err
		}
		if err := fn(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Categories lists the categories and actions present, so the filter UI offers
// what actually exists rather than a hardcoded list that drifts.
func (q *AuditQuery) Categories(ctx context.Context) (map[string][]string, error) {
	rows, err := q.db.Query(ctx, `
		SELECT DISTINCT category, action FROM audit_logs
		WHERE ts >= now() - interval '90 days' ORDER BY category, action`)
	if err != nil {
		return nil, fmt.Errorf("list audit categories: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var category, action string
		if err := rows.Scan(&category, &action); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		out[category] = append(out[category], action)
	}
	return out, rows.Err()
}

func scanAuditRecord(s scanner) (ports.AuditRecord, error) {
	var (
		r       ports.AuditRecord
		actorID *uuid.UUID
		details []byte
	)
	err := s.Scan(&r.ID, &r.Time, &actorID, &r.ActorName, &r.Category, &r.Action,
		&r.TargetType, &r.TargetID, &r.TargetName, &r.SourceIP, &r.Outcome,
		&r.RequestID, &details)
	if err != nil {
		return ports.AuditRecord{}, fmt.Errorf("scan audit record: %w", err)
	}
	r.ActorID = actorID
	if len(details) > 0 {
		_ = json.Unmarshal(details, &r.Details)
	}
	return r, nil
}
