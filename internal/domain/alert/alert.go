// Package alert is the Alerting bounded context: threshold rules over metrics,
// and the state machine that decides when one fires and when it recovers
// (NOTIF-04, NOTIF-05).
package alert

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Errors the transport maps onto status codes.
var (
	ErrInvalidMetric    = errors.New("alert: unknown metric")
	ErrInvalidOperator  = errors.New("alert: unknown comparison")
	ErrInvalidThreshold = errors.New("alert: threshold is out of range")
	ErrInvalidName      = errors.New("alert: a rule needs a name")
	ErrInvalidSubject   = errors.New("alert: a rule watches a VM or a node")
)

// Metric names a series a rule can watch. Only gauges are offered: a rule over
// a rate ("disk reads above 5000/s") is meaningful, but a rule over a
// cumulative counter is not, and the two are easy to confuse.
type Metric string

const (
	MetricCPUPct       Metric = "cpu_pct"
	MetricMemPct       Metric = "mem_pct"
	MetricDiskReadBps  Metric = "disk_read_bps"
	MetricDiskWriteBps Metric = "disk_write_bps"
	MetricNetRxBps     Metric = "net_rx_bps"
	MetricNetTxBps     Metric = "net_tx_bps"

	// MetricTempC is the hottest reading on a node, in degrees (SENSOR-04).
	MetricTempC Metric = "temp_c"
	// MetricTempHeadroomPct is how much of the chip's own critical point is
	// left, as a percentage. It is the portable one: a rule at 15% headroom
	// holds across an estate whose CPUs disagree about what hot means, where
	// a rule at 80°C holds only on the machine it was written on.
	MetricTempHeadroomPct Metric = "temp_headroom_pct"
)

// Subject is what a rule is about. Every rule was over a VM until nodes grew
// sensors, and a rule about a node's hardware has no VM to name (ADR 0007).
type Subject string

const (
	SubjectVM   Subject = "vm"
	SubjectHost Subject = "host"
)

// Valid reports whether the subject is one the evaluator knows.
func (s Subject) Valid() bool { return s == SubjectVM || s == SubjectHost }

// Metrics lists what can be watched for this subject. A VM has no
// temperature and a node is not in a VM group, so offering either one's
// metrics for the other would only produce rules that never fire.
func (s Subject) Metrics() []Metric {
	if s == SubjectHost {
		return []Metric{MetricTempC, MetricTempHeadroomPct}
	}
	return []Metric{MetricCPUPct, MetricMemPct, MetricDiskReadBps,
		MetricDiskWriteBps, MetricNetRxBps, MetricNetTxBps}
}

// Valid reports whether the metric is one the evaluator can read.
func (m Metric) Valid() bool {
	switch m {
	case MetricCPUPct, MetricMemPct, MetricDiskReadBps, MetricDiskWriteBps,
		MetricNetRxBps, MetricNetTxBps, MetricTempC, MetricTempHeadroomPct:
		return true
	}
	return false
}

// Unit is how a value of this metric should be written in a message.
func (m Metric) Unit() string {
	switch m {
	case MetricCPUPct, MetricMemPct, MetricTempHeadroomPct:
		return "%"
	case MetricTempC:
		return "°C"
	default:
		return "B/s"
	}
}

// Label is the metric's name in prose.
func (m Metric) Label() string {
	switch m {
	case MetricCPUPct:
		return "CPU"
	case MetricMemPct:
		return "memory"
	case MetricDiskReadBps:
		return "disk read"
	case MetricDiskWriteBps:
		return "disk write"
	case MetricNetRxBps:
		return "network receive"
	case MetricNetTxBps:
		return "network transmit"
	case MetricTempC:
		return "temperature"
	case MetricTempHeadroomPct:
		return "thermal headroom"
	}
	return string(m)
}

// Operator compares a sample against a threshold.
type Operator string

const (
	OpGreater Operator = ">"
	OpLess    Operator = "<"
)

// Valid reports whether the operator is supported.
func (o Operator) Valid() bool { return o == OpGreater || o == OpLess }

// Breaches reports whether value violates the threshold under this operator.
func (o Operator) Breaches(value, threshold float64) bool {
	if o == OpLess {
		return value < threshold
	}
	return value > threshold
}

// State is where a rule stands for one VM.
type State string

const (
	// StateOK means the metric is within the threshold.
	StateOK State = "ok"
	// StatePending means the threshold is breached but not yet for long
	// enough. This is a real state rather than an implementation detail: it is
	// what stops a momentary spike becoming an alert.
	StatePending State = "pending"
	// StateFiring means the breach has been sustained.
	StateFiring State = "firing"
)

// Rule is a threshold over a metric, scoped to some VMs.
type Rule struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// Subject defaults to a VM, so every rule written before nodes had
	// sensors keeps meaning what it meant.
	Subject   Subject    `json:"subject"`
	Metric    Metric     `json:"metric"`
	Op        Operator   `json:"op"`
	Threshold float64    `json:"threshold"`
	DurationS int        `json:"duration_s"`
	VMGroupID *uuid.UUID `json:"vm_group_id,omitempty"`
	Severity  string     `json:"severity"`
	CooldownS int        `json:"cooldown_s"`
	IsEnabled bool       `json:"is_enabled"`
	CreatedAt time.Time  `json:"created_at"`
	// FiringCount is filled by the query that lists rules, so the UI can show
	// which rules are currently unhappy without a second round trip.
	FiringCount int `json:"firing_count"`
}

// Validate checks a rule before it is stored. Rejecting nonsense here beats
// discovering at evaluation time that a rule can never fire.
func (r Rule) Validate() error {
	if r.Name == "" {
		return ErrInvalidName
	}
	subject := r.SubjectOrDefault()
	if !subject.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidSubject, r.Subject)
	}
	if !r.Metric.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidMetric, r.Metric)
	}
	// A rule watching a metric its subject does not have can never fire, and
	// the administrator wants to hear that now rather than never.
	if !metricAllowed(subject, r.Metric) {
		return fmt.Errorf("%w: %s has no %s", ErrInvalidMetric, subject, r.Metric)
	}
	// Nodes are not in VM groups. Scoping a host rule to one would silently
	// mean something other than what it says.
	if subject == SubjectHost && r.VMGroupID != nil {
		return fmt.Errorf("%w: a node rule cannot be scoped to a VM group", ErrInvalidSubject)
	}
	if !r.Op.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidOperator, r.Op)
	}
	// A percentage rule above 100 or below 0 can never fire, which is a
	// misconfiguration the administrator wants to hear about now.
	if r.Metric.Unit() == "%" && (r.Threshold < 0 || r.Threshold > 100) {
		return fmt.Errorf("%w: a percentage threshold must be between 0 and 100", ErrInvalidThreshold)
	}
	if r.Threshold < 0 {
		return fmt.Errorf("%w: a threshold cannot be negative", ErrInvalidThreshold)
	}
	return nil
}

// SubjectOrDefault is the rule's subject, defaulting to a VM for every rule
// written before nodes had any sensors to watch.
func (r Rule) SubjectOrDefault() Subject {
	if r.Subject == "" {
		return SubjectVM
	}
	return r.Subject
}

func metricAllowed(s Subject, m Metric) bool {
	for _, allowed := range s.Metrics() {
		if allowed == m {
			return true
		}
	}
	return false
}

// Status is a rule's current state for one VM.
type Status struct {
	RuleID   uuid.UUID `json:"rule_id"`
	RuleName string    `json:"rule_name"`
	// Subject says which of the two identifiers below is filled in.
	Subject        Subject    `json:"subject"`
	VMID           uuid.UUID  `json:"vm_id,omitempty"`
	VMName         string     `json:"vm_name,omitempty"`
	HostID         uuid.UUID  `json:"host_id,omitempty"`
	HostName       string     `json:"host_name,omitempty"`
	Metric         Metric     `json:"metric"`
	Severity       string     `json:"severity"`
	State          State      `json:"state"`
	Since          time.Time  `json:"since"`
	LastValue      float64    `json:"last_value"`
	LastNotifiedAt *time.Time `json:"last_notified_at,omitempty"`
}

// Decision is what the evaluator concluded for one rule and VM.
type Decision struct {
	State     State
	Since     time.Time
	Fired     bool // crossed into firing on this evaluation
	Recovered bool // returned to ok on this evaluation
	Notify    bool // a message should be sent now
}

// Evaluate advances the state machine for one sample.
//
// The transitions are: ok → pending when the threshold is first breached,
// pending → firing once the breach has lasted the rule's duration, and
// anything → ok the moment it stops. Notification happens on the transition
// into firing and on recovery, plus a repeat once per cooldown while it stays
// firing, so a long incident does not go silent but does not shout either
// (NOTIF-05).
func Evaluate(rule Rule, previous Status, value float64, now time.Time) Decision {
	breached := rule.Op.Breaches(value, rule.Threshold)

	if !breached {
		if previous.State == StateFiring {
			// Recovery is worth a message precisely because the firing one
			// was: an alert that never says "resolved" trains people to
			// ignore alerts.
			return Decision{State: StateOK, Since: now, Recovered: true, Notify: true}
		}
		if previous.State == StatePending {
			return Decision{State: StateOK, Since: now}
		}
		return Decision{State: StateOK, Since: orNow(previous.Since, now)}
	}

	switch previous.State {
	case StateFiring:
		// Still firing. Repeat only when the cooldown has elapsed.
		notify := false
		if rule.CooldownS > 0 && previous.LastNotifiedAt != nil {
			notify = now.Sub(*previous.LastNotifiedAt) >= time.Duration(rule.CooldownS)*time.Second
		}
		return Decision{State: StateFiring, Since: previous.Since, Notify: notify}

	case StatePending:
		since := orNow(previous.Since, now)
		if now.Sub(since) >= time.Duration(rule.DurationS)*time.Second {
			return Decision{State: StateFiring, Since: since, Fired: true, Notify: true}
		}
		return Decision{State: StatePending, Since: since}

	default: // ok, or no previous state at all
		// A rule with no sustained duration fires immediately; otherwise the
		// clock starts now.
		if rule.DurationS <= 0 {
			return Decision{State: StateFiring, Since: now, Fired: true, Notify: true}
		}
		return Decision{State: StatePending, Since: now}
	}
}

func orNow(t, now time.Time) time.Time {
	if t.IsZero() {
		return now
	}
	return t
}

// Describe renders the message a firing or recovering alert carries.
func Describe(rule Rule, vmName string, value float64, recovered bool) (subject, body string) {
	unit := rule.Metric.Unit()
	now := formatValue(value, unit)
	limit := formatValue(rule.Threshold, unit)

	if recovered {
		subject = fmt.Sprintf("Resolved: %s on %s", rule.Name, vmName)
		body = fmt.Sprintf("%s on %s is no longer %s %s. It is currently %s.",
			rule.Metric.Label(), vmName, operatorProse(rule.Op), limit, now)
		return subject, body
	}

	subject = fmt.Sprintf("%s on %s: %s %s", rule.Name, vmName, rule.Metric.Label(), now)
	// A rule with no sustained duration has no "for at least 0 seconds" to
	// report, and saying so would read as a bug rather than a setting.
	if rule.DurationS <= 0 {
		body = fmt.Sprintf("%s on %s is %s %s. It is currently %s.",
			rule.Metric.Label(), vmName, operatorProse(rule.Op), limit, now)
		return subject, body
	}
	body = fmt.Sprintf("%s on %s has been %s %s for at least %s. It is currently %s.",
		rule.Metric.Label(), vmName, operatorProse(rule.Op), limit,
		humanDuration(time.Duration(rule.DurationS)*time.Second), now)
	return subject, body
}

// formatValue writes a measurement the way a person would read it. Raw float
// precision ("4.634626865386963%") is noise that makes a message harder to
// scan, and byte rates are more legible in units than in digits.
func formatValue(value float64, unit string) string {
	if unit == "%" {
		return fmt.Sprintf("%.1f%%", value)
	}
	const step = 1024.0
	units := []string{"B/s", "KiB/s", "MiB/s", "GiB/s", "TiB/s"}
	scaled, index := value, 0
	for scaled >= step && index < len(units)-1 {
		scaled /= step
		index++
	}
	if index == 0 {
		return fmt.Sprintf("%.0f %s", scaled, units[index])
	}
	return fmt.Sprintf("%.1f %s", scaled, units[index])
}

func operatorProse(op Operator) string {
	if op == OpLess {
		return "below"
	}
	return "above"
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%g hours", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%g minutes", d.Minutes())
	default:
		return fmt.Sprintf("%g seconds", d.Seconds())
	}
}
