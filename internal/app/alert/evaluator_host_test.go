package alert

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/alert"
	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// stubRepo records what the evaluator decided, per subject.
type stubRepo struct {
	rules      []alert.Rule
	hostStates map[uuid.UUID]alert.Status
	savedHosts map[uuid.UUID]alert.State
	savedVMs   map[uuid.UUID]alert.State
	prunedHost []uuid.UUID
}

func (s *stubRepo) EnabledRules(context.Context) ([]alert.Rule, error) { return s.rules, nil }

func (s *stubRepo) RuleStates(context.Context, uuid.UUID) (map[uuid.UUID]alert.Status, error) {
	return map[uuid.UUID]alert.Status{}, nil
}

func (s *stubRepo) SaveState(_ context.Context, _, vmID uuid.UUID, state alert.State,
	_ time.Time, _ float64, _ *time.Time) error {
	s.savedVMs[vmID] = state
	return nil
}
func (s *stubRepo) PruneStates(context.Context, uuid.UUID, []uuid.UUID) error { return nil }

func (s *stubRepo) RuleHostStates(context.Context, uuid.UUID) (map[uuid.UUID]alert.Status, error) {
	return s.hostStates, nil
}

func (s *stubRepo) SaveHostState(_ context.Context, _, hostID uuid.UUID, state alert.State,
	_ time.Time, _ float64, _ *time.Time) error {
	s.savedHosts[hostID] = state
	return nil
}

func (s *stubRepo) PruneHostStates(_ context.Context, _ uuid.UUID, keep []uuid.UUID) error {
	s.prunedHost = keep
	return nil
}

type stubMetrics struct{}

func (stubMetrics) LatestVMMetrics(context.Context, time.Time) (map[uuid.UUID]ports.MetricPoint, error) {
	return map[uuid.UUID]ports.MetricPoint{}, nil
}

type stubVMs struct{}

func (stubVMs) AllVMNames(context.Context) (map[uuid.UUID]string, error) {
	return map[uuid.UUID]string{}, nil
}

type stubGroups struct{}

func (stubGroups) VMGroupMemberIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }

type stubSensors struct {
	readings map[uuid.UUID]telemetry.Reading
}

func (s stubSensors) HottestNow(context.Context, time.Time) (map[uuid.UUID]telemetry.Reading, error) {
	return s.readings, nil
}

type stubHosts struct{ names map[uuid.UUID]string }

func (s stubHosts) AllHostNames(context.Context) (map[uuid.UUID]string, error) { return s.names, nil }

type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

func crit(f float64) *float64 { return &f }

func newEvaluator(repo *stubRepo, readings map[uuid.UUID]telemetry.Reading, names map[uuid.UUID]string) *Evaluator {
	return &Evaluator{
		Repo: repo, Metrics: stubMetrics{}, VMs: stubVMs{}, Groups: stubGroups{},
		Sensors: stubSensors{readings: readings}, Hosts: stubHosts{names: names},
		Clock: fixedClock{t: time.Date(2026, 8, 19, 2, 0, 0, 0, time.UTC)},
		Log:   zerolog.New(io.Discard),
	}
}

func hostRule(metric alert.Metric, threshold float64) alert.Rule {
	return alert.Rule{
		ID: uuid.New(), Name: "node heat", Subject: alert.SubjectHost,
		Metric: metric, Op: alert.OpGreater, Threshold: threshold,
		Severity: "warning", IsEnabled: true,
	}
}

func newRepo(rules ...alert.Rule) *stubRepo {
	return &stubRepo{
		rules: rules, hostStates: map[uuid.UUID]alert.Status{},
		savedHosts: map[uuid.UUID]alert.State{}, savedVMs: map[uuid.UUID]alert.State{},
	}
}

// A rule with no sustained duration fires the moment the threshold is crossed.
func TestHostRuleFiresOnTemperature(t *testing.T) {
	hot, cool := uuid.New(), uuid.New()
	repo := newRepo(hostRule(alert.MetricTempC, 80))
	e := newEvaluator(repo, map[uuid.UUID]telemetry.Reading{
		hot:  {Chip: "coretemp-isa-0000", Label: "Package id 0", Kind: telemetry.SensorTemp, Value: 84, Crit: crit(100)},
		cool: {Chip: "coretemp-isa-0000", Label: "Package id 0", Kind: telemetry.SensorTemp, Value: 45, Crit: crit(100)},
	}, map[uuid.UUID]string{hot: "pve1", cool: "pve2"})

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if repo.savedHosts[hot] != alert.StateFiring {
		t.Errorf("hot node is %q, want firing", repo.savedHosts[hot])
	}
	if repo.savedHosts[cool] != alert.StateOK {
		t.Errorf("cool node is %q, want ok", repo.savedHosts[cool])
	}
	// Both nodes reported, so both stay in scope; pruning either would drop
	// state the next pass needs.
	if len(repo.prunedHost) != 2 {
		t.Errorf("kept %d nodes in scope, want both", len(repo.prunedHost))
	}
}

// Headroom is the portable rule, and it can only be judged on a chip that
// declares its own limit. Inventing one would be worse than not firing.
func TestHeadroomRuleSkipsChipsWithNoLimit(t *testing.T) {
	rated, unrated := uuid.New(), uuid.New()
	// "Less than 15% headroom left" — the operator writes it as a floor.
	rule := hostRule(alert.MetricTempHeadroomPct, 15)
	rule.Op = alert.OpLess
	repo := newRepo(rule)

	e := newEvaluator(repo, map[uuid.UUID]telemetry.Reading{
		rated:   {Chip: "coretemp", Label: "Package id 0", Kind: telemetry.SensorTemp, Value: 90, Crit: crit(100)},
		unrated: {Chip: "acpitz", Label: "temp1", Kind: telemetry.SensorTemp, Value: 95},
	}, map[uuid.UUID]string{rated: "pve1", unrated: "pve2"})

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if repo.savedHosts[rated] != alert.StateFiring {
		t.Errorf("a node at 10%% headroom is %q, want firing", repo.savedHosts[rated])
	}
	if _, judged := repo.savedHosts[unrated]; judged {
		t.Error("a chip that declares no critical point was judged against a headroom rule")
	}
	if len(repo.prunedHost) != 1 {
		t.Errorf("scope = %d nodes, want only the one that could be judged", len(repo.prunedHost))
	}
}

// A portal that collects no sensors must not have its host rules error every
// pass; they simply find nothing to judge.
func TestHostRulesAreQuietWithoutSensors(t *testing.T) {
	repo := newRepo(hostRule(alert.MetricTempC, 80))
	e := newEvaluator(repo, nil, nil)
	e.Sensors, e.Hosts = nil, nil

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(repo.savedHosts) != 0 {
		t.Errorf("judged %d nodes with no readings", len(repo.savedHosts))
	}
}

// A rule with no subject is a rule written before nodes had sensors, and it
// still means what it meant: a VM rule.
func TestRulesWithoutASubjectAreStillAboutVMs(t *testing.T) {
	repo := newRepo(alert.Rule{
		ID: uuid.New(), Name: "cpu", Metric: alert.MetricCPUPct,
		Op: alert.OpGreater, Threshold: 90, IsEnabled: true,
	})
	host := uuid.New()
	e := newEvaluator(repo, map[uuid.UUID]telemetry.Reading{
		host: {Chip: "coretemp", Label: "Package id 0", Kind: telemetry.SensorTemp, Value: 95, Crit: crit(100)},
	}, map[uuid.UUID]string{host: "pve1"})

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(repo.savedHosts) != 0 {
		t.Errorf("a VM rule judged %d nodes", len(repo.savedHosts))
	}
}
