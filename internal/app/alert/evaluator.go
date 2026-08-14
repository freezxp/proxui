// Package alert evaluates threshold rules against the metric pipeline and
// emits events when one fires or recovers (NOTIF-04).
package alert

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	appsync "github.com/freezxp/proxui/internal/app/sync"
	"github.com/freezxp/proxui/internal/domain/alert"
	"github.com/freezxp/proxui/internal/infra/metrics"
)

// Repository is the alert store the evaluator drives.
type Repository interface {
	EnabledRules(ctx context.Context) ([]alert.Rule, error)
	RuleStates(ctx context.Context, ruleID uuid.UUID) (map[uuid.UUID]alert.Status, error)
	SaveState(ctx context.Context, ruleID, vmID uuid.UUID, state alert.State, since time.Time, value float64, notifiedAt *time.Time) error
	PruneStates(ctx context.Context, ruleID uuid.UUID, keep []uuid.UUID) error
}

// MetricReader supplies the latest sample per VM.
type MetricReader interface {
	LatestVMMetrics(ctx context.Context, since time.Time) (map[uuid.UUID]ports.MetricPoint, error)
}

// VMReader names VMs and lists them, so an alert can say which machine.
type VMReader interface {
	AllVMNames(ctx context.Context) (map[uuid.UUID]string, error)
}

// GroupReader resolves a rule's VM-group scope.
type GroupReader interface {
	VMGroupMemberIDs(ctx context.Context, groupID uuid.UUID) ([]uuid.UUID, error)
}

// EventPublisher queues an event for the outbox, which is what carries an
// alert to the notification channels.
//
// Alerts are derived from metrics rather than produced by a state change, so
// there is no transaction to enlist in: the outbox write is the state change.
type EventPublisher interface {
	Begin(ctx context.Context) (appsync.Tx, error)
	PublishEvent(ctx context.Context, tx ports.Querier, event ports.DomainEvent) error
}

// Evaluator applies every enabled rule to the newest metrics.
type Evaluator struct {
	Repo    Repository
	Metrics MetricReader
	VMs     VMReader
	Groups  GroupReader
	Events  EventPublisher
	Clock   ports.Clock
	Log     zerolog.Logger

	// Staleness bounds how old a sample may be and still count. A VM that
	// stopped reporting must not keep an alert firing on its last known value,
	// nor recover one on missing data — it simply drops out of evaluation.
	Staleness time.Duration
}

// StalenessDefault is three metric intervals: long enough to survive one
// missed collection, short enough that a dead VM stops driving alerts.
const StalenessDefault = 3 * time.Minute

// Evaluate runs one pass over every enabled rule.
func (e *Evaluator) Evaluate(ctx context.Context) error {
	rules, err := e.Repo.EnabledRules(ctx)
	if err != nil {
		return fmt.Errorf("evaluate alerts: %w", err)
	}
	if len(rules) == 0 {
		return nil
	}

	staleness := e.Staleness
	if staleness <= 0 {
		staleness = StalenessDefault
	}
	now := e.Clock.Now()

	samples, err := e.Metrics.LatestVMMetrics(ctx, now.Add(-staleness))
	if err != nil {
		return fmt.Errorf("evaluate alerts: %w", err)
	}
	if len(samples) == 0 {
		return nil
	}

	names, err := e.VMs.AllVMNames(ctx)
	if err != nil {
		return fmt.Errorf("evaluate alerts: %w", err)
	}

	firing := 0
	for _, rule := range rules {
		firing += rule.FiringCount
		if err := e.evaluateRule(ctx, rule, samples, names, now); err != nil {
			// One bad rule must not stop the others: an alert that never runs
			// is worse than one that logs a failure.
			e.Log.Error().Err(err).Str("rule", rule.Name).Msg("could not evaluate alert rule")
		}
	}
	// Counted from the state the pass started with, which is the figure the
	// firing list showed a moment ago rather than a half-updated one.
	metrics.AlertsFiring.Set(float64(firing))
	return nil
}

func (e *Evaluator) evaluateRule(ctx context.Context, rule alert.Rule, samples map[uuid.UUID]ports.MetricPoint, names map[uuid.UUID]string, now time.Time) error {
	scope, err := e.scope(ctx, rule, samples)
	if err != nil {
		return err
	}
	if len(scope) == 0 {
		return nil
	}

	previous, err := e.Repo.RuleStates(ctx, rule.ID)
	if err != nil {
		return err
	}

	for _, vmID := range scope {
		sample := samples[vmID]
		value, ok := metricValue(rule.Metric, sample)
		if !ok {
			continue
		}

		decision := alert.Evaluate(rule, previous[vmID], value, now)

		var notifiedAt *time.Time
		if decision.Notify {
			notifiedAt = &now
			e.publish(ctx, rule, vmID, names[vmID], value, decision, now)
		}
		if err := e.Repo.SaveState(ctx, rule.ID, vmID, decision.State, decision.Since, value, notifiedAt); err != nil {
			return err
		}
	}

	// State for VMs no longer in scope would otherwise sit at "firing"
	// forever, showing an alert nobody can resolve.
	return e.Repo.PruneStates(ctx, rule.ID, scope)
}

// scope resolves which VMs a rule applies to, restricted to those that
// actually reported recently.
func (e *Evaluator) scope(ctx context.Context, rule alert.Rule, samples map[uuid.UUID]ports.MetricPoint) ([]uuid.UUID, error) {
	if rule.VMGroupID == nil {
		out := make([]uuid.UUID, 0, len(samples))
		for id := range samples {
			out = append(out, id)
		}
		return out, nil
	}

	members, err := e.Groups.VMGroupMemberIDs(ctx, *rule.VMGroupID)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(members))
	for _, id := range members {
		if _, reported := samples[id]; reported {
			out = append(out, id)
		}
	}
	return out, nil
}

func (e *Evaluator) publish(ctx context.Context, rule alert.Rule, vmID uuid.UUID, vmName string, value float64, decision alert.Decision, now time.Time) {
	if e.Events == nil {
		return
	}
	if vmName == "" {
		vmName = vmID.String()
	}
	subject, body := alert.Describe(rule, vmName, value, decision.Recovered)

	severity := rule.Severity
	if decision.Recovered {
		// A recovery is good news whatever the rule's severity: sending it at
		// "critical" would page someone to tell them everything is fine.
		severity = ports.SeverityInfo
	}

	eventType := ports.EventAlertFiring
	if decision.Recovered {
		eventType = ports.EventAlertResolved
	}

	event := ports.DomainEvent{
		OccurredAt: now,
		Category:   ports.EventCategoryPerformanceAlert,
		Type:       eventType,
		Severity:   severity,
		Payload: map[string]any{
			"vm_id": vmID.String(), "vm_name": vmName,
			"rule_id": rule.ID.String(), "rule_name": rule.Name,
			"metric": string(rule.Metric), "threshold": rule.Threshold,
			"value": value, "subject": subject, "body": body,
			"state": string(decision.State),
		},
	}

	detached := context.WithoutCancel(ctx)
	tx, err := e.Events.Begin(detached)
	if err != nil {
		e.Log.Error().Err(err).Str("rule", rule.Name).Msg("could not queue alert event")
		return
	}
	defer func() { _ = tx.Rollback(detached) }()

	if err := e.Events.PublishEvent(detached, tx, event); err != nil {
		e.Log.Error().Err(err).Str("rule", rule.Name).Msg("could not queue alert event")
		return
	}
	if err := tx.Commit(detached); err != nil {
		e.Log.Error().Err(err).Str("rule", rule.Name).Msg("could not queue alert event")
	}
}

// metricValue pulls the rule's series out of a sample. Memory is expressed as
// a percentage because a threshold in bytes would mean something different on
// every VM.
func metricValue(metric alert.Metric, point ports.MetricPoint) (float64, bool) {
	switch metric {
	case alert.MetricCPUPct:
		return point.CPUPct, true
	case alert.MetricMemPct:
		if point.MemTotalBytes <= 0 {
			return 0, false
		}
		return float64(point.MemUsedBytes) / float64(point.MemTotalBytes) * 100, true
	case alert.MetricDiskReadBps:
		return float64(point.DiskReadBps), true
	case alert.MetricDiskWriteBps:
		return float64(point.DiskWriteBps), true
	case alert.MetricNetRxBps:
		return float64(point.NetRxBps), true
	case alert.MetricNetTxBps:
		return float64(point.NetTxBps), true
	}
	return 0, false
}
