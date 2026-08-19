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
	"github.com/freezxp/proxui/internal/domain/telemetry"
	"github.com/freezxp/proxui/internal/infra/metrics"
)

// Repository is the alert store the evaluator drives.
type Repository interface {
	EnabledRules(ctx context.Context) ([]alert.Rule, error)
	RuleStates(ctx context.Context, ruleID uuid.UUID) (map[uuid.UUID]alert.Status, error)
	SaveState(ctx context.Context, ruleID, vmID uuid.UUID, state alert.State, since time.Time, value float64, notifiedAt *time.Time) error
	PruneStates(ctx context.Context, ruleID uuid.UUID, keep []uuid.UUID) error

	// The same four for rules about a node rather than a VM. Separate methods
	// rather than a nullable subject argument: the two write different
	// columns, and a caller that mixes them up should not compile.
	RuleHostStates(ctx context.Context, ruleID uuid.UUID) (map[uuid.UUID]alert.Status, error)
	SaveHostState(ctx context.Context, ruleID, hostID uuid.UUID, state alert.State, since time.Time, value float64, notifiedAt *time.Time) error
	PruneHostStates(ctx context.Context, ruleID uuid.UUID, keep []uuid.UUID) error
}

// SensorReader supplies the hottest current reading per node.
type SensorReader interface {
	HottestNow(ctx context.Context, since time.Time) (map[uuid.UUID]telemetry.Reading, error)
}

// HostReader names nodes, so an alert can say which machine.
type HostReader interface {
	AllHostNames(ctx context.Context) (map[uuid.UUID]string, error)
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
	// Sensors and Hosts serve rules about node hardware. Both nil is a portal
	// that collects no sensors, where a host rule simply never fires.
	Sensors SensorReader
	Hosts   HostReader
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
	names, err := e.VMs.AllVMNames(ctx)
	if err != nil {
		return fmt.Errorf("evaluate alerts: %w", err)
	}

	// Node readings arrive on a slower cadence than metrics, so they get a
	// staleness of their own; the metric window would call every node silent.
	hottest, hostNames := e.nodeState(ctx, now)

	firing := 0
	for _, rule := range rules {
		firing += rule.FiringCount

		var err error
		if rule.SubjectOrDefault() == alert.SubjectHost {
			err = e.evaluateHostRule(ctx, rule, hottest, hostNames, now)
		} else {
			err = e.evaluateRule(ctx, rule, samples, names, now)
		}
		if err != nil {
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

// SensorStaleness bounds how old a node reading may be and still count.
// Three collection intervals, like the metric one, but the interval is five
// minutes rather than one.
const SensorStaleness = 16 * time.Minute

// nodeState reads what host rules need. A portal that collects no sensors
// returns nothing, and every host rule then finds nothing in scope — which is
// the correct behaviour, not an error to log every pass.
func (e *Evaluator) nodeState(ctx context.Context, now time.Time) (map[uuid.UUID]telemetry.Reading, map[uuid.UUID]string) {
	if e.Sensors == nil || e.Hosts == nil {
		return nil, nil
	}
	hottest, err := e.Sensors.HottestNow(ctx, now.Add(-SensorStaleness))
	if err != nil {
		e.Log.Error().Err(err).Msg("could not read node sensors for alerting")
		return nil, nil
	}
	names, err := e.Hosts.AllHostNames(ctx)
	if err != nil {
		e.Log.Error().Err(err).Msg("could not read node names for alerting")
		return nil, nil
	}
	return hottest, names
}

// evaluateHostRule applies one rule to every node that reported recently.
//
// A node rule is scoped to the whole estate: nodes are not in VM groups, and
// a portal with two of them does not need a grouping vocabulary to say so.
func (e *Evaluator) evaluateHostRule(ctx context.Context, rule alert.Rule,
	hottest map[uuid.UUID]telemetry.Reading, names map[uuid.UUID]string, now time.Time) error {
	if len(hottest) == 0 {
		return nil
	}

	previous, err := e.Repo.RuleHostStates(ctx, rule.ID)
	if err != nil {
		return err
	}

	scope := make([]uuid.UUID, 0, len(hottest))
	for hostID, reading := range hottest {
		value, ok := sensorValue(rule.Metric, reading)
		if !ok {
			// A headroom rule cannot judge a chip that declares no limit.
			// Skipping is right: the alternative is inventing one.
			continue
		}
		scope = append(scope, hostID)

		decision := alert.Evaluate(rule, previous[hostID], value, now)

		var notifiedAt *time.Time
		if decision.Notify {
			notifiedAt = &now
			// The sensor travels in the name, because "the node is hot" and
			// "its NVMe is hot" call for different afternoons.
			e.publishHost(ctx, rule, hostID, nodeLabel(names[hostID], reading), value, decision, now)
		}
		if err := e.Repo.SaveHostState(ctx, rule.ID, hostID, decision.State, decision.Since, value, notifiedAt); err != nil {
			return err
		}
	}

	return e.Repo.PruneHostStates(ctx, rule.ID, scope)
}

// nodeLabel names the node and the sensor that spoke for it.
func nodeLabel(name string, reading telemetry.Reading) string {
	if name == "" {
		name = "a node"
	}
	if reading.Label == "" {
		return name
	}
	return name + " (" + telemetry.ShortChip(reading.Chip) + " " + reading.Label + ")"
}

// sensorValue projects a reading onto what a rule watches.
func sensorValue(metric alert.Metric, reading telemetry.Reading) (float64, bool) {
	switch metric {
	case alert.MetricTempC:
		return reading.Value, true
	case alert.MetricTempHeadroomPct:
		headroom, ok := reading.Headroom()
		return headroom * 100, ok
	}
	return 0, false
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

	e.emit(ctx, rule, ports.DomainEvent{
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
	})
}

// publishHost is publish for a rule about a node. The payload names a host
// rather than a VM, because a notification rule filtering on vm_id must not
// match an alert that has no VM in it.
func (e *Evaluator) publishHost(ctx context.Context, rule alert.Rule, hostID uuid.UUID,
	hostName string, value float64, decision alert.Decision, now time.Time) {
	if e.Events == nil {
		return
	}
	if hostName == "" {
		hostName = hostID.String()
	}
	subject, body := alert.Describe(rule, hostName, value, decision.Recovered)

	severity := rule.Severity
	if decision.Recovered {
		severity = ports.SeverityInfo
	}
	eventType := ports.EventAlertFiring
	if decision.Recovered {
		eventType = ports.EventAlertResolved
	}

	e.emit(ctx, rule, ports.DomainEvent{
		OccurredAt: now,
		Category:   ports.EventCategoryPerformanceAlert,
		Type:       eventType,
		Severity:   severity,
		Payload: map[string]any{
			"host_id": hostID.String(), "host_name": hostName,
			"rule_id": rule.ID.String(), "rule_name": rule.Name,
			"metric": string(rule.Metric), "threshold": rule.Threshold,
			"value": value, "subject": subject, "body": body,
			"state": string(decision.State),
		},
	})
}

// emit writes one event to the outbox, which is what carries it to the
// notification channels.
func (e *Evaluator) emit(ctx context.Context, rule alert.Rule, event ports.DomainEvent) {
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
