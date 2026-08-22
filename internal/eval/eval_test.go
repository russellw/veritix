package eval

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/agent/llm/llmtest"
	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/rules"
)

const fixtureDir = "../../testdata/dirty-retail"

// scoredFixtures is every dataset in the repository that carries a manifest.
// Each one is scored by the tests below, so a new fixture is covered by adding
// it here and nothing else.
var scoredFixtures = []string{
	"../../testdata/dirty-retail",
	"../../testdata/dirty-logistics",
	"../../testdata/dirty-meters",
}

// runFixture audits a fixture with no model configured: exactly the auditor a
// customer gets by default.
func runFixture(t *testing.T, dir string) *audit.Result {
	t.Helper()
	res, err := audit.Run(t.Context(), audit.Options{
		Paths:  []string{dir},
		Engine: config.Default().Engine,
	}, nil)
	if err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	return res
}

func loadManifest(t *testing.T, dir string) *Manifest {
	t.Helper()
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m
}

func loadFixtureManifest(t *testing.T) *Manifest {
	t.Helper()
	return loadManifest(t, fixtureDir)
}

// eachFixture runs a body over every scored dataset.
func eachFixture(t *testing.T, body func(t *testing.T, m *Manifest, res *audit.Result)) {
	t.Helper()
	for _, dir := range scoredFixtures {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			body(t, loadManifest(t, dir), runFixture(t, dir))
		})
	}
}

// A target whose count is wrong is a target nothing can ever match, and the
// eval would report every model missing it forever without saying why. The
// manifest's own claims are re-run for the same reason a finding's are.
func TestAgentTargetCountsAreMeasurable(t *testing.T) {
	eachFixture(t, func(t *testing.T, m *Manifest, res *audit.Result) {
		targets := m.AgentTargets()
		if len(targets) == 0 {
			t.Fatal("this manifest lists no agent targets")
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
	})
}

// Credit is given on the engine's number, so a target whose number depends on
// how the question was phrased cannot be credited reliably. Counting affected
// rows and counting distinct offenders are the two ways a model asks, and a
// fixture is only scorable where they agree.
func TestAgentTargetCountsDoNotDependOnPhrasing(t *testing.T) {
	eachFixture(t, func(t *testing.T, m *Manifest, res *audit.Result) {
		for _, d := range m.AgentTargets() {
			column := d.Where[strings.LastIndex(d.Where, ".")+1:]
			distinct := strings.Replace(d.Agent.Query,
				"count(*)", "count(DISTINCT "+engine.Ident(column)+")", 1)
			if distinct == d.Agent.Query {
				continue // not phrased as a row count; nothing to compare
			}
			var got int64
			if err := res.Engine().ScanOne(t.Context(), distinct, []any{&got}); err != nil {
				// A join or an expression can make the rewrite meaningless.
				// That is not a failure of the fixture.
				t.Logf("%s: the distinct rewrite does not apply: %v", d.ID, err)
				continue
			}
			if !d.Agent.Measures(got) {
				t.Errorf("%s: %d affected rows but %d distinct values in %s.\n"+
					"    A model counting one way would be credited and a model counting\n"+
					"    the other way would not. Adjust the fixture so they agree, or\n"+
					"    list %d under equivalent: and say why both are true.",
					d.ID, d.Agent.Count, got, column, got)
			}
		}
	})
}

// The fixture carries a known set of planted defects and the manifest beside it
// is the list. This is the assertion that used to live in internal/checks as two
// Go slices; it moved when the list stopped being about checks alone, since half
// of it is defects source and ingest are responsible for.
//
// The deterministic pass is also the baseline every agent score is measured
// against. It has to clear the manifest's deterministic half completely and
// score zero on the agent half, or the two halves are not separable and the
// eval is measuring the checks.
func TestTheDeterministicRunIsTheBaseline(t *testing.T) {
	eachFixture(t, theDeterministicRunIsTheBaseline)
}

func theDeterministicRunIsTheBaseline(t *testing.T, m *Manifest, res *audit.Result) {
	score := ScoreRun(m, res.Findings.All())
	for _, d := range score.Checks.Missed {
		t.Errorf("missed %s at %s\n    (%s)", d.CaughtBy, d.Where, d.Why)
	}
	// The other half of a defect manifest: a check that fires on everything is
	// useless, and only the clean list catches one.
	for _, c := range score.Checks.FalsePositives {
		t.Errorf("false positive: %s fired at %s\n    (%s)", c.Rule, c.Where, c.Why)
	}
	if !score.Checks.Complete() {
		t.Logf("findings actually produced:\n%s", describe(res.Findings.All()))
	}
	if len(score.Checks.Found) == 0 {
		t.Fatal("the manifest scored nothing; it is probably not being read")
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

// The defects no check proposes are the agentic tier's whole reason for
// existing, so a deterministic run must miss them. If one starts being caught
// by a check that is good news and the manifest has to say so — otherwise the
// eval goes on crediting a model for restating what the checks already found.
func TestUncoveredDefectsAreNotCaughtByAnyCheck(t *testing.T) {
	eachFixture(t, uncoveredDefectsAreNotCaughtByAnyCheck)
}

func uncoveredDefectsAreNotCaughtByAnyCheck(t *testing.T, m *Manifest, res *audit.Result) {
	score := ScoreChecks(m, res.Findings.All())
	if len(score.Uncovered) == 0 {
		t.Fatal("the manifest lists nothing for the agent to find")
	}
	for _, d := range score.Uncovered {
		if d.Agent == nil {
			continue
		}
		for _, f := range res.Findings.All() {
			if f.Origin == finding.OriginCheck && MatchesTarget(f, d) {
				t.Errorf("%s is marked caught_by: none, but %s already measures it at %s\n"+
					"    (%s)\n"+
					"    Name that rule in caught_by and drop the agent block, or the eval "+
					"scores a model for work the checks now do.",
					d.ID, f.Rule, d.Where, d.Why)
			}
		}
	}
}

// describe lists what a run actually produced, for a failure that needs to show
// its work.
func describe(findings []finding.Finding) string {
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, fmt.Sprintf("  %-32s %s", f.Rule, locationOf(f)))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
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
		{"a noise entry with no count", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "r"}},
			Noise:   []Noise{{Where: "t.csv", Why: "the enum"}}}},
		{"a noise entry with no reason", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "r"}},
			Noise:   []Noise{{Where: "t.csv", Count: 4}}}},
		{"a noise entry describing a target", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "none",
				Agent: &AgentTarget{Count: 4, Query: "SELECT 4"}}},
			Noise: []Noise{{Where: "t.csv", Count: 4, Why: "nothing to see here"}}}},
		{"a context document with no id", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "r"}},
			Context: []Context{{File: "context/d.md", Why: "the dictionary"}}}},
		{"a context document listed twice", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "r"}},
			Context: []Context{
				{ID: "d", File: "context/d.md", Why: "the dictionary"},
				{ID: "d", File: "context/e.md", Why: "the other one"}}}},
		{"a context document with no reason", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "r"}},
			Context: []Context{{ID: "d", File: "context/d.md"}}}},
		{"a context document outside the dataset", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "r"}},
			Context: []Context{{ID: "d", File: "../../../etc/passwd", Why: "not this"}}}},
		{"a target needing a document nothing lists", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "none",
				NeedsContext: []string{"dictionary"},
				Agent:        &AgentTarget{Count: 1, Query: "SELECT 1"}}}}},
		{"a check-caught defect claiming to need context", Manifest{Version: 1,
			Defects: []Defect{{ID: "a", Where: "t.csv", Why: "w", CaughtBy: "column.empty",
				NeedsContext: []string{"d"}}},
			Context: []Context{{ID: "d", File: "context/d.md", Why: "the dictionary"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.m.Validate(); err == nil {
				t.Error("Validate accepted it")
			}
		})
	}
}

// A scripted model that finds one target on the first run and the other on the
// second is the case the whole harness exists to distinguish. It is also what
// gpt-oss-120b actually did on this fixture, three times, and the reason a
// single run was never evidence of anything.
func TestRepeatedRunsSeparateAModelThatAlternates(t *testing.T) {
	m := loadFixtureManifest(t)
	targets := m.AgentTargets()

	record := func(table, column string, count int64, query string) llmtest.Turn {
		return llmtest.Turn{Calls: []llmtest.Call{{
			ID:   "call-" + table,
			Name: "record_finding",
			Input: map[string]any{
				"rule":           "orphaned_reference",
				"severity":       "error",
				"table":          table,
				"column":         column,
				"title":          "region codes that resolve against nothing",
				"detail":         "joining on region silently drops these rows",
				"count_query":    query,
				"affected_count": count,
			},
		}}}
	}
	done := llmtest.Turn{Text: "Nothing further."}

	provider := llmtest.New(
		// Run 1 finds the customers orphans and stops.
		record("customers_csv", "region", targets[0].Agent.Count, targets[0].Agent.Query),
		done,
		// Run 2 finds the Q1 orphans instead.
		record("sales_xlsx_q1", "region", targets[1].Agent.Count, targets[1].Agent.Query),
		done,
	)

	score, err := Run(t.Context(), Options{
		Paths:    []string{fixtureDir},
		Manifest: m,
		Engine:   config.Default().Engine,
		Agent:    &agent.Options{Provider: provider, MaxSteps: 4},
		Runs:     2,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(score.Runs) != 2 {
		t.Fatalf("scored %d runs, want 2", len(score.Runs))
	}
	if got := score.MeanRecall(); got != 0.5 {
		t.Errorf("MeanRecall = %v, want 0.5: each run found one of two", got)
	}
	if got := score.Coverage(); got != 1 {
		t.Errorf("Coverage = %v, want 1: between them the runs found both", got)
	}
	for _, target := range score.Targets {
		if target.Hits != 1 {
			t.Errorf("%s was found by %d of %d runs, want 1",
				target.Defect.ID, target.Hits, target.Runs)
		}
	}
	// The deterministic half must be unaffected by the model, and identical
	// both times.
	if !score.Checks.Complete() || score.ChecksUnstable {
		t.Errorf("the deterministic pass did not hold steady under the agent: %+v", score.Checks)
	}
}

// A finding the model records that is true but not planted is reported without
// being scored. It is not a false positive — the engine measured it — and it is
// not a hit either.
func TestAnUnplannedFindingIsReportedNotScored(t *testing.T) {
	m := loadFixtureManifest(t)

	provider := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{{
			ID:   "call-1",
			Name: "record_finding",
			Input: map[string]any{
				"rule":           "lowercase_currency",
				"severity":       "warning",
				"table":          "orders_csv",
				"column":         "currency",
				"title":          "one currency code is lowercased",
				"detail":         "a case-sensitive join on currency will miss it",
				"count_query":    `SELECT count(*) FROM orders_csv WHERE currency = 'gbp'`,
				"affected_count": 1,
			},
		}}},
		llmtest.Turn{Text: "Nothing further."},
	)

	score, err := Run(t.Context(), Options{
		Paths:    []string{fixtureDir},
		Manifest: m,
		Engine:   config.Default().Engine,
		Agent:    &agent.Options{Provider: provider, MaxSteps: 4},
		Runs:     1,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if score.MeanRecall() != 0 {
		t.Errorf("MeanRecall = %v; an unplanned finding is not a target", score.MeanRecall())
	}
	claims := score.UnclassifiedClaims()
	if len(claims) != 1 || claims[0].Rule != "agent.lowercase_currency" {
		t.Fatalf("unclassified claims = %v, want the one recorded finding", claims)
	}
	if claims[0].Where != "orders.csv.currency" || claims[0].Count != 1 {
		t.Errorf("claim = %+v, want orders.csv.currency measuring 1", claims[0])
	}
}

// A claim somebody has already adjudicated is labeled with the answer instead
// of appearing, run after run, as something still to look into. It is keyed on
// the engine's count rather than on the rule name, because the rule name is the
// part the model wrote: gpt-oss-120b reported the same observation about
// dirty-logistics as inconsistent_status_length and then as mixed_status_format.
func TestAKnownNonDefectIsLabeledAndNotGraded(t *testing.T) {
	m := &Manifest{
		Version: 1,
		Dataset: "synthetic",
		Defects: []Defect{
			{ID: "a", Where: "t.csv.x", Why: "x", CaughtBy: "none",
				Agent: &AgentTarget{Count: 3, Query: "SELECT 3"}},
		},
		Noise: []Noise{{Where: "t.csv", Count: 4, Why: "the status enum, not a defect"}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	claim := func(rule string) finding.Finding {
		return finding.Finding{
			Rule: rule, Origin: finding.OriginAgent, Count: 4,
			Location: finding.Location{Display: "t.csv"},
		}
	}
	// The two wordings the model actually used, which must land the same way.
	for _, rule := range []string{"agent.inconsistent_status_length", "agent.mixed_status_format"} {
		t.Run(rule, func(t *testing.T) {
			score := ScoreRun(m, []finding.Finding{claim(rule)})
			if len(score.Unclassified) != 1 {
				t.Fatalf("unclassified = %v, want the one claim", score.Unclassified)
			}
			if got := score.Unclassified[0].Known; got != m.Noise[0].Why {
				t.Errorf("Known = %q, want the manifest's reason", got)
			}
			// Labeling is not grading: the model is not paid for it and not
			// charged for it either.
			if score.Recall() != 0 {
				t.Errorf("Recall = %v; a known non-defect is not a target", score.Recall())
			}
			if len(score.Missed) != 1 {
				t.Errorf("missed = %v; the real target is still missed", score.Missed)
			}
		})
	}

	// The same wording measuring something else is still an open question.
	other := claim("agent.mixed_status_format")
	other.Count = 7
	score := ScoreRun(m, []finding.Finding{other})
	if score.Unclassified[0].Known != "" {
		t.Errorf("a different measurement was absolved by the noise entry: %+v", score.Unclassified[0])
	}
}

// The scorecard renders without a model, which is the configuration CI runs.
func TestScorecardRendersForADeterministicRun(t *testing.T) {
	m := loadFixtureManifest(t)
	res := runFixture(t, fixtureDir)
	score := Aggregate(m, []RunScore{ScoreRun(m, res.Findings.All())})

	var text, jsonOut strings.Builder
	if err := WriteText(&text, score); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if err := WriteJSON(&jsonOut, score); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	for _, want := range []string{"dirty-retail", "Deterministic checks", "customers.region_orphans"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the scorecard does not mention %q:\n%s", want, text.String())
		}
	}

	var back doc
	if err := json.Unmarshal([]byte(jsonOut.String()), &back); err != nil {
		t.Fatalf("the JSON scorecard does not parse: %v", err)
	}
	if back.Checks.Found != back.Checks.Total || back.Checks.Total == 0 {
		t.Errorf("checks = %d/%d, want everything found", back.Checks.Found, back.Checks.Total)
	}
	if back.Agent.Coverage != 0 || len(back.Agent.Targets) == 0 {
		t.Errorf("agent = %+v, want targets listed and none found", back.Agent)
	}
}

// A run the provider abandoned says nothing about the model, and averaging it
// in as a zero measures the machine the model was running on. This is not
// hypothetical: the first measurement taken with this harness was a 4B on a
// four-core laptop, whose 24th step exceeded a 30-minute request timeout after
// the transcript had grown to 20k tokens.
func TestARunTheProviderAbandonedIsNotScoredAgainstTheModel(t *testing.T) {
	m := loadFixtureManifest(t)
	targets := m.AgentTargets()

	found := RunScore{
		Detected: []string{targets[0].ID, targets[1].ID},
		Trace:    &agent.Trace{Model: "scripted", Stopped: agent.StoppedModelFinished},
	}
	abandoned := RunScore{
		Missed: []string{targets[0].ID, targets[1].ID},
		Trace:  &agent.Trace{Model: "scripted", Stopped: agent.StoppedProviderError},
	}

	s := Aggregate(m, []RunScore{found, abandoned})
	if s.Scored() != 1 || s.Unscored() != 1 {
		t.Fatalf("scored %d and left out %d, want one of each", s.Scored(), s.Unscored())
	}
	if got := s.MeanRecall(); got != 1 {
		t.Errorf("MeanRecall = %v, want 1: the only run that says anything found both", got)
	}
	for _, target := range s.Targets {
		if target.Runs != 1 || target.Hits != 1 {
			t.Errorf("%s: %d/%d, want 1/1 — the abandoned run is not a run it missed",
				target.Defect.ID, target.Hits, target.Runs)
		}
	}

	// It is still printed. Excluding a failure from an average is defensible;
	// hiding it is not.
	var out strings.Builder
	if err := WriteText(&out, s); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(out.String(), "left out of the averages") {
		t.Errorf("the scorecard does not disclose the excluded run:\n%s", out.String())
	}
}

// A run stopped by its own step budget is entirely different: the model had its
// chance and spent it. That one counts.
func TestARunThatSpentItsStepBudgetIsScored(t *testing.T) {
	m := loadFixtureManifest(t)

	s := Aggregate(m, []RunScore{{
		Missed: []string{m.AgentTargets()[0].ID},
		Trace:  &agent.Trace{Stopped: agent.StoppedStepBudget},
	}})
	if s.Scored() != 1 {
		t.Errorf("a run that hit its step budget was not scored")
	}
	if s.MeanRecall() != 0 {
		t.Errorf("MeanRecall = %v, want 0", s.MeanRecall())
	}
}

// TestAnAcceptedRuleConvertsAnAgentTarget is the rule-proposal loop measured
// rather than asserted.
//
// dirty-logistics has four targets no check tool can measure, and gpt-oss-120b
// scored 42% mean recall against 75% coverage on them: the whole of that gap is
// defects found on one run of three. The claim propose_rule makes is that one
// accepted rule closes the gap for its own target permanently, and the eval is
// the instrument that has to be able to read that — otherwise the milestone is
// a story about a number nothing measures.
//
// So: audit with no model at all, with the rule a reviewer would have accepted
// for shipments.delivered_before_dispatch, and require the scorecard to say
// that target is now covered without one.
func TestAnAcceptedRuleConvertsAnAgentTarget(t *testing.T) {
	const dir = "../../testdata/dirty-logistics"
	const target = "shipments.delivered_before_dispatch"

	m := loadManifest(t, dir)

	// Exactly what propose_rule produces for a contradiction between two
	// columns: expect: sql, with the WHERE clause that selects the wrong rows.
	//
	// It names a column, and has to. A rule scoped to a whole table is not
	// evidence about any particular defect in it — shipments.csv has two agent
	// targets, and a table-level rule measuring two rows was credited for the
	// wrong one until convertedBy started asking for the exact location. The
	// column is the reviewer saying what this rule protects.
	accepted := &rules.File{Version: 1, Rules: []rules.Rule{{
		ID:          "delivered_after_dispatch",
		Description: "a shipment cannot be delivered before it was dispatched",
		Table:       "shipments_csv",
		Column:      "delivered_at",
		Expect:      rules.ExpectSQL,
		Where:       "TRY_CAST(delivered_at AS DATE) < TRY_CAST(dispatched_at AS DATE)",
	}}}

	// No agent. That is the point: the defect was the model's to find, and
	// from here on it is not.
	res, err := audit.Run(t.Context(), audit.Options{
		Paths:  []string{dir},
		Engine: config.Default().Engine,
		Rules:  accepted,
	}, nil)
	if err != nil {
		t.Fatalf("auditing with the accepted rule: %v", err)
	}
	defer res.Close() //nolint:errcheck // nothing is written back

	score := ScoreChecks(m, res.Findings.All())

	if !slices.Contains(score.Converted, target) {
		t.Errorf("%s is not reported as converted; converted = %v, uncovered = %v",
			target, score.Converted, ids(score.Uncovered))
	}
	for _, d := range score.Uncovered {
		if d.ID == target {
			t.Errorf("%s is still listed as the agent's to find after the rule was accepted", target)
		}
	}

	// The other three stay the agent's. A rule that converted targets it does
	// not measure would be the scorecard flattering itself.
	if len(score.Uncovered) != len(m.AgentTargets())-1 {
		t.Errorf("accepting one rule converted %d targets, want 1",
			len(m.AgentTargets())-len(score.Uncovered))
	}

	// And the run without that rule must not report it converted, or the test
	// above passes for a reason that has nothing to do with the rule.
	bare := ScoreChecks(m, runFixture(t, dir).Findings.All())
	if len(bare.Converted) != 0 {
		t.Errorf("with no rules loaded, %v is reported as converted", bare.Converted)
	}
}

func ids(ds []Defect) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

// A context document has to be about the column its target names.
//
// The failure this catches is the fixture drifting apart: a target says it is
// invisible without the dictionary, somebody rewrites the dictionary, and the
// sentence that made the defect visible is gone. Nothing else would notice.
// Every run would simply score zero on that target, which is indistinguishable
// from a model that did not look — and this whole fixture exists to tell those
// two apart.
func TestAnAidedTargetsDocumentsMentionItsColumn(t *testing.T) {
	for _, dir := range scoredFixtures {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			m := loadManifest(t, dir)
			for _, d := range m.AgentTargets() {
				if !d.Aided() {
					continue
				}
				column := d.Where[strings.LastIndex(d.Where, ".")+1:]
				var found bool
				for _, id := range d.NeedsContext {
					text, err := m.ReadContext(id)
					if err != nil {
						t.Errorf("%s: %v", d.ID, err)
						continue
					}
					if strings.TrimSpace(text) == "" {
						t.Errorf("%s: context document %q is empty", d.ID, id)
					}
					if strings.Contains(text, column) {
						found = true
					}
				}
				if !found {
					t.Errorf("%s says it needs %v, and no one of those mentions %s.\n"+
						"    A document that does not talk about the column cannot be what\n"+
						"    makes the defect visible, so the target would score zero for a\n"+
						"    reason that has nothing to do with the model.",
						d.ID, d.NeedsContext, column)
				}
			}
		})
	}
}

// A fixture that carries context has to carry a control as well.
//
// Recall over the aided targets alone can only go up: with the documents
// loaded a model may find them, without the documents it cannot. That measures
// whether fetching the context worked and says nothing about what it cost, and
// the cost is the real risk — a transcript filling with documents is how a
// model stops doing the work it was already doing. The unaided targets are the
// control, and they are only a control if they are on the same fixture and the
// same runs.
func TestAFixtureWithContextAlsoCarriesAControl(t *testing.T) {
	for _, dir := range scoredFixtures {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			m := loadManifest(t, dir)
			if len(m.Context) == 0 {
				t.Skip("this fixture has no context documents")
			}
			var aided, unaided int
			for _, d := range m.AgentTargets() {
				if d.Aided() {
					aided++
				} else {
					unaided++
				}
			}
			if aided == 0 {
				t.Error("context documents are listed and no target needs one")
			}
			if unaided == 0 {
				t.Error("every agent target needs a document, so nothing measures what " +
					"loading them costs")
			}
		})
	}
}

// The split has to be computed from the same runs the overall figure is, and
// has to come apart from it. A model that answers everything the documents
// unlock and nothing else scores 100% aided, 0% unaided, and 50% overall --
// and the middle number on its own would look like a model doing half the job
// evenly.
func TestTheContextSplitSeparatesWhatADocumentBought(t *testing.T) {
	m := loadManifest(t, "../../testdata/dirty-meters")

	var aided, unaided []string
	for _, d := range m.AgentTargets() {
		if d.Aided() {
			aided = append(aided, d.ID)
		} else {
			unaided = append(unaided, d.ID)
		}
	}
	if len(aided) == 0 || len(unaided) == 0 {
		t.Fatalf("this test needs both halves; got %d aided and %d unaided",
			len(aided), len(unaided))
	}

	s := Aggregate(m, []RunScore{{Detected: aided, Missed: unaided}})

	if got := s.MeanRecallOf(s.Aided()); got != 1 {
		t.Errorf("aided recall = %v, want 1", got)
	}
	if got := s.MeanRecallOf(s.Unaided()); got != 0 {
		t.Errorf("unaided recall = %v, want 0", got)
	}
	if got, want := len(s.Aided()), len(aided); got != want {
		t.Errorf("Aided() returned %d targets, want %d", got, want)
	}
	if got, want := len(s.Unaided()), len(unaided); got != want {
		t.Errorf("Unaided() returned %d targets, want %d", got, want)
	}
	// The overall figure is the blend, and is exactly what would hide the two.
	want := float64(len(aided)) / float64(len(aided)+len(unaided))
	if got := s.MeanRecall(); got != want {
		t.Errorf("MeanRecall = %v, want %v", got, want)
	}

	var buf strings.Builder
	s.Model, s.Provider = "scripted", "test"
	if err := WriteText(&buf, s); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	for _, want := range []string{"with context", "unaided"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the scorecard does not mention %q:\n%s", want, buf.String())
		}
	}
}

// The documents are not data. A fixture whose context leaked into the export
// would be scoring a model on a defect it can see in a column, which is the
// one thing these targets are supposed not to be.
func TestContextDocumentsAreNotIngested(t *testing.T) {
	dir := "../../testdata/dirty-meters"
	m := loadManifest(t, dir)
	if len(m.Context) == 0 {
		t.Fatal("this fixture is supposed to carry context documents")
	}
	res := runFixture(t, dir)
	for _, table := range res.Profile.Tables {
		for _, c := range m.Context {
			if strings.Contains(table.Display, filepath.Base(c.File)) {
				t.Errorf("context document %s was ingested as table %s", c.File, table.Display)
			}
		}
	}
}
