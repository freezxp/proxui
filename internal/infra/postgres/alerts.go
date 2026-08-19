package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/alert"
)

// AlertRepository stores alert rules and their per-VM state.
type AlertRepository struct{ db *Pool }

// NewAlertRepository builds the repository.
func NewAlertRepository(db *Pool) *AlertRepository { return &AlertRepository{db: db} }

// CreateRule stores a rule.
func (r *AlertRepository) CreateRule(ctx context.Context, rule *alert.Rule) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO alert_rules
		   (id, name, subject, metric, op, threshold, duration_s, vm_group_id, severity, cooldown_s, is_enabled, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		rule.ID, rule.Name, string(rule.SubjectOrDefault()), string(rule.Metric),
		string(rule.Op), rule.Threshold,
		rule.DurationS, rule.VMGroupID, rule.Severity, rule.CooldownS, rule.IsEnabled, rule.CreatedAt)
	return wrapConflict(err, "create alert rule")
}

// ListRules returns live rules, each carrying how many VMs it is firing for.
func (r *AlertRepository) ListRules(ctx context.Context) ([]alert.Rule, error) {
	rows, err := r.db.Query(ctx,
		`SELECT r.id, r.name, r.subject, r.metric, r.op, r.threshold, r.duration_s, r.vm_group_id,
		        r.severity, r.cooldown_s, r.is_enabled, r.created_at,
		        (SELECT count(*) FROM alert_states s
		          WHERE s.alert_rule_id = r.id AND s.state = 'firing')
		   FROM alert_rules r
		  WHERE r.deleted_at IS NULL
		  ORDER BY r.name`)
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer rows.Close()

	out := []alert.Rule{}
	for rows.Next() {
		var (
			rule                alert.Rule
			subject, metric, op string
		)
		if err := rows.Scan(&rule.ID, &rule.Name, &subject, &metric, &op, &rule.Threshold, &rule.DurationS,
			&rule.VMGroupID, &rule.Severity, &rule.CooldownS, &rule.IsEnabled, &rule.CreatedAt,
			&rule.FiringCount); err != nil {
			return nil, fmt.Errorf("scan alert rule: %w", err)
		}
		rule.Subject = alert.Subject(subject)
		rule.Metric, rule.Op = alert.Metric(metric), alert.Operator(op)
		out = append(out, rule)
	}
	return out, rows.Err()
}

// EnabledRules returns the rules the evaluator should apply.
func (r *AlertRepository) EnabledRules(ctx context.Context) ([]alert.Rule, error) {
	all, err := r.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	enabled := make([]alert.Rule, 0, len(all))
	for _, rule := range all {
		if rule.IsEnabled {
			enabled = append(enabled, rule)
		}
	}
	return enabled, nil
}

// DeleteRule soft-deletes a rule and drops its state, so a rule that is
// recreated later does not inherit a stale firing history.
func (r *AlertRepository) DeleteRule(ctx context.Context, id uuid.UUID, at time.Time) error {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete alert rule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM alert_states WHERE alert_rule_id=$1`, id); err != nil {
		return fmt.Errorf("clear alert state: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE alert_rules SET deleted_at=$2 WHERE id=$1 AND deleted_at IS NULL`, id, at)
	if err != nil {
		return fmt.Errorf("delete alert rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return tx.Commit(ctx)
}

// SetRuleEnabled turns a rule on or off without losing it.
func (r *AlertRepository) SetRuleEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE alert_rules SET is_enabled=$2 WHERE id=$1 AND deleted_at IS NULL`, id, enabled)
	if err != nil {
		return fmt.Errorf("update alert rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ports.ErrNotFound
	}
	return nil
}

// RuleStates reads the stored state for one rule, keyed by VM.
func (r *AlertRepository) RuleStates(ctx context.Context, ruleID uuid.UUID) (map[uuid.UUID]alert.Status, error) {
	rows, err := r.db.Query(ctx,
		`SELECT vm_id, state, since, coalesce(last_value, 0), last_notified_at
		   FROM alert_states WHERE alert_rule_id=$1 AND vm_id IS NOT NULL`, ruleID)
	if err != nil {
		return nil, fmt.Errorf("read alert state: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]alert.Status{}
	for rows.Next() {
		var (
			status alert.Status
			state  string
		)
		if err := rows.Scan(&status.VMID, &state, &status.Since, &status.LastValue, &status.LastNotifiedAt); err != nil {
			return nil, fmt.Errorf("scan alert state: %w", err)
		}
		status.RuleID, status.State = ruleID, alert.State(state)
		out[status.VMID] = status
	}
	return out, rows.Err()
}

// SaveState writes one rule-and-VM state. notifiedAt is nil when this
// evaluation sent nothing, which leaves the stored value alone so the cooldown
// keeps measuring from the last message actually sent.
func (r *AlertRepository) SaveState(ctx context.Context, ruleID, vmID uuid.UUID, state alert.State, since time.Time, value float64, notifiedAt *time.Time) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO alert_states (alert_rule_id, vm_id, state, since, last_value, last_notified_at)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (alert_rule_id, vm_id) DO UPDATE
		   SET state = EXCLUDED.state,
		       since = EXCLUDED.since,
		       last_value = EXCLUDED.last_value,
		       last_notified_at = COALESCE(EXCLUDED.last_notified_at, alert_states.last_notified_at)`,
		ruleID, vmID, string(state), since, value, notifiedAt)
	if err != nil {
		return fmt.Errorf("save alert state: %w", err)
	}
	return nil
}

// FiringStatuses lists everything currently firing, newest breach first, with
// the names an operator needs to act on it.
func (r *AlertRepository) FiringStatuses(ctx context.Context) ([]alert.Status, error) {
	// One list, two subjects. A LEFT JOIN to each side rather than a UNION:
	// the ordering is over the whole list, and a UNION would sort each half.
	rows, err := r.db.Query(ctx,
		`SELECT s.alert_rule_id, r.name, s.vm_id, v.name, s.host_id, h.name,
		        r.metric, r.severity, s.state, s.since,
		        coalesce(s.last_value, 0), s.last_notified_at
		   FROM alert_states s
		   JOIN alert_rules r ON r.id = s.alert_rule_id
		   LEFT JOIN vms v   ON v.id = s.vm_id
		   LEFT JOIN hosts h ON h.id = s.host_id
		  WHERE s.state = 'firing' AND r.deleted_at IS NULL
		    AND (v.id IS NOT NULL OR h.id IS NOT NULL)
		  ORDER BY s.since DESC`)
	if err != nil {
		return nil, fmt.Errorf("list firing alerts: %w", err)
	}
	defer rows.Close()

	out := []alert.Status{}
	for rows.Next() {
		var (
			status           alert.Status
			metric, state    string
			vmID, hostID     *uuid.UUID
			vmName, hostName *string
		)
		if err := rows.Scan(&status.RuleID, &status.RuleName, &vmID, &vmName,
			&hostID, &hostName, &metric, &status.Severity, &state, &status.Since,
			&status.LastValue, &status.LastNotifiedAt); err != nil {
			return nil, fmt.Errorf("scan firing alert: %w", err)
		}
		status.Subject = alert.SubjectVM
		if vmID != nil {
			status.VMID = *vmID
		}
		if vmName != nil {
			status.VMName = *vmName
		}
		if hostID != nil {
			status.Subject, status.HostID = alert.SubjectHost, *hostID
		}
		if hostName != nil {
			status.HostName = *hostName
		}
		status.Metric, status.State = alert.Metric(metric), alert.State(state)
		out = append(out, status)
	}
	return out, rows.Err()
}

// PruneStates removes state for VMs that no longer exist. Deleting a VM
// cascades, so this exists only for rules whose scope changed.
func (r *AlertRepository) PruneStates(ctx context.Context, ruleID uuid.UUID, keep []uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM alert_states
		  WHERE alert_rule_id=$1 AND vm_id IS NOT NULL AND NOT (vm_id = ANY($2))`,
		ruleID, keep)
	if err != nil {
		return fmt.Errorf("prune alert state: %w", err)
	}
	return nil
}

// RuleHostStates reads a node rule's state, one row per node.
func (r *AlertRepository) RuleHostStates(ctx context.Context, ruleID uuid.UUID) (map[uuid.UUID]alert.Status, error) {
	rows, err := r.db.Query(ctx,
		`SELECT host_id, state, since, coalesce(last_value, 0), last_notified_at
		   FROM alert_states WHERE alert_rule_id=$1 AND host_id IS NOT NULL`, ruleID)
	if err != nil {
		return nil, fmt.Errorf("read node alert state: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]alert.Status{}
	for rows.Next() {
		var (
			status alert.Status
			state  string
		)
		if err := rows.Scan(&status.HostID, &state, &status.Since, &status.LastValue, &status.LastNotifiedAt); err != nil {
			return nil, fmt.Errorf("scan node alert state: %w", err)
		}
		status.RuleID, status.State = ruleID, alert.State(state)
		status.Subject = alert.SubjectHost
		out[status.HostID] = status
	}
	return out, rows.Err()
}

// SaveHostState records where a node rule stands for one node.
func (r *AlertRepository) SaveHostState(ctx context.Context, ruleID, hostID uuid.UUID,
	state alert.State, since time.Time, value float64, notifiedAt *time.Time) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO alert_states (alert_rule_id, host_id, state, since, last_value, last_notified_at)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (alert_rule_id, host_id) WHERE host_id IS NOT NULL DO UPDATE
		   SET state = EXCLUDED.state,
		       since = EXCLUDED.since,
		       last_value = EXCLUDED.last_value,
		       last_notified_at = COALESCE(EXCLUDED.last_notified_at, alert_states.last_notified_at)`,
		ruleID, hostID, string(state), since, value, notifiedAt)
	if err != nil {
		return fmt.Errorf("save node alert state: %w", err)
	}
	return nil
}

// PruneHostStates removes state for nodes that have stopped reporting, which
// would otherwise sit at "firing" forever showing an alert nobody can resolve.
func (r *AlertRepository) PruneHostStates(ctx context.Context, ruleID uuid.UUID, keep []uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM alert_states
		  WHERE alert_rule_id=$1 AND host_id IS NOT NULL AND NOT (host_id = ANY($2))`,
		ruleID, keep)
	if err != nil {
		return fmt.Errorf("prune node alert state: %w", err)
	}
	return nil
}
