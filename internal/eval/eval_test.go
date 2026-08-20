package eval

import (
	"testing"

	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/finding"
)

const fixtureDir = "../../testdata/dirty-retail"

// runFixture audits the fixture with no model configured: exactly the auditor
// a customer gets by default.
func runFixture(t *testing.T) *audit.Result {
	t.Helper()
	res, err := audit.Run(t.Context(), audit.Options{
		Paths:  []string{fixtureDir},
		Engine: config.Default().Engine,
	}, nil)
	if err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	return res
}

func loadFixtureManifest(t *testing.T) *Manifest {
	t.Helper()
	m, err := Load(fixtureDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

// A target whose count is wrong is a target nothing can ever match, and the
// eval would report every model missing it forever without saying why. The
// manifest's own claims are re-run for the same reason a finding's are.
func TestAgentTargetCountsAreMeasurable(t *testing.T) {
	m := loadFixtureManifest(t)
	res := runFixture(t)

	targets := m.AgentTargets()
	if len(targets) == 0 {
		t.Fatal("the fixture manifest lists no agent targets")
	}
	for _, d := range targets {
		var got int64
		if err := res.Engine().ScanOne(t.Context(), d.Agent.Query, []any{&got}); err != nil {
			t.Errorf("%s: its query did not run: %v\n    %s", d.ID, err, d.Agent.Query)
			continue
		}
		if got != d.Agent.Count {
			t.Errorf("%s claims %d affected rows, but its own query returns %d\n    %s",
				d.ID, d.Agent.Count, got, d.Agent.Query)
		}
	}
}

// The deterministic pass is the baseline every agent score is measured
// against. It has to clear the manifest's deterministic half completely and
// score zero on the agent half, or the two halves are not separable and the
// eval is measuring the checks.
func TestTheDeterministicRunIsTheBaseline(t *testing.T) {
	m := loadFixtureManifest(t)
	res := runFixture(t)

	score := ScoreRun(m, res.Findings.All())
	if !score.Checks.Complete() {
		t.Errorf("the deterministic pass did not clear the manifest: %d missed, %d false positives",
			len(score.Checks.Missed), len(score.Checks.FalsePositives))
	}
	if len(score.Detected) != 0 {
		t.Errorf("a run with no model scored %v; agent targets must need an agent", score.Detected)
	}
	if score.Recall() != 0 {
		t.Errorf("recall with no model = %v, want 0", score.Recall())
	}
	if len(score.Unclassified) != 0 {
		t.Errorf("a run with no model produced agent claims: %v", score.Unclassified)
	}
}

// Credit needs both halves: the right place and the engine's number. A model
// that reports something else about the same column has not found the defect,
// and neither has one that reports the right number somewhere else.
func TestCreditNeedsBothLocationAndCount(t *testing.T) {
	m := loadFixtureManifest(t)
	target := m.AgentTargets()[0]

	agentFinding := func(display, column string, count int64) finding.Finding {
		return finding.Finding{
			Rule:     "agent.orphaned_reference",
			Origin:   finding.OriginAgent,
			Title:    "some region codes do not resolve",
			Count:    count,
			Location: finding.Location{Table: "customers_csv", Display: display, Column: column},
		}
	}

	cases := []struct {
		name string
		f    finding.Finding
		want bool
	}{
		{"the defect, exactly", agentFinding("customers.csv", "region", 4), true},
		{"the table, without naming a column", agentFinding("customers.csv", "", 4), true},
		{"the right place, a different measurement", agentFinding("customers.csv", "region", 2), false},
		{"the right number, the wrong column", agentFinding("customers.csv", "status", 4), false},
		{"the right number, the wrong table", agentFinding("orders.csv", "", 4), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchesTarget(c.f, target); got != c.want {
				t.Errorf("MatchesTarget = %v, want %v", got, c.want)
			}
		})
	}
}

// A finding is credited to at most one target, so a model cannot be paid twice
// for one observation.
func TestOneFindingCreditsOneTarget(t *testing.T) {
	m := &Manifest{
		Version: 1,
		Dataset: "synthetic",
		Defects: []Defect{
			{ID: "a", Where: "t.csv.x", Why: "x", CaughtBy: "none",
				Agent: &AgentTarget{Count: 3, Query: "SELECT 3"}},
			{ID: "b", Where: "t.csv", Why: "y", CaughtBy: "none",
				Agent: &AgentTarget{Count: 3, Query: "SELECT 3"}},
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	one := finding.Finding{
		Rule: "agent.thing", Origin: finding.OriginAgent, Count: 3,
		Location: finding.Location{Display: "t.csv", Column: "x"},
	}
	score := ScoreRun(m, []finding.Finding{one})
	if len(score.Detected) != 1 || score.Detected[0] != "a" {
		t.Errorf("detected %v, want only [a]", score.Detected)
	}
	if len(score.Missed) != 1 || score.Missed[0] != "b" {
		t.Errorf("missed %v, want [b]", score.Missed)
	}
	if len(score.Unclassified) != 0 {
		t.Errorf("the credited finding was also counted as unclassified: %v", score.Unclassified)
	}
}

// Mean recall and coverage answer different questions, and a model that finds
// a different defect on every run is the case that separates them. It is not
// hypothetical: it is what gpt-oss-120b did on this fixture across three runs.
func TestRecallAndCoverageSeparate(t *testing.T) {
	m := loadFixtureManifest(t)
	targets := m.AgentTargets()
	if len(targets) < 2 {
		t.Skip("this needs a manifest with at least two agent targets")
	}

	runs := []RunScore{
		{Detected: []string{targets[0].ID}, Missed: []string{targets[1].ID}},
		{Detected: []string{targets[1].ID}, Missed: []string{targets[0].ID}},
	}
	s := Aggregate(m, runs)

	if got := s.MeanRecall(); got != 0.5 {
		t.Errorf("MeanRecall = %v, want 0.5", got)
	}
	if got := s.Coverage(); got != 1 {
		t.Errorf("Coverage = %v, want 1", got)
	}
	for _, target := range s.Targets {
		if target.Hits != 1 || target.Runs != 2 {
			t.Errorf("%s: %d/%d runs, want 1/2", target.Defect.ID, target.Hits, target.Runs)
		}
	}
}

// A manifest that cannot score anything is refused rather than reporting a
// perfect run, for the same reason rules refuses one that would never fire.
func TestValidateRefusesAManifestThatWouldScoreNothing(t *testing.T) {
	cases := []struct {
		name string
		m    Manifest
	}{
		{"no defects", Manifest{Version: 1}},
		{"a defect with no id", Manifest{Version: 1, Defects: []Defect{
			{Where: "t.csv", Why: "w", CaughtBy: "none"}}}},
		{"a defect listed twice", Manifest{Version: 1, Defects: []Defect{
			{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "none"},
			{ID: "a", Where: "u.csv", Why: "w", CaughtBy: "none"}}}},
		{"a defect that does not say what catches it", Manifest{Version: 1, Defects: []Defect{
			{ID: "a", Where: "t.csv", Why: "w"}}}},
		{"an agent target with no count", Manifest{Version: 1, Defects: []Defect{
			{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "none", Agent: &AgentTarget{Query: "SELECT 1"}}}}},
		{"an agent target with no query", Manifest{Version: 1, Defects: []Defect{
			{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "none", Agent: &AgentTarget{Count: 1}}}}},
		{"a clean entry with no reason", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "r"}},
			Clean:   []Clean{{Rule: "r", Where: "t.csv"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.m.Validate(); err == nil {
				t.Error("Validate accepted it")
			}
		})
	}
}
