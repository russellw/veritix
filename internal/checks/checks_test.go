package checks

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/eval"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/ingest"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/source"
)

const fixtureDir = "../../testdata/dirty-retail"

func runChecks(t *testing.T) (*engine.Engine, *finding.Set) {
	t.Helper()
	ctx := t.Context()

	e, err := engine.Open(ctx, "", config.Default().Engine, nil)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	ds, err := source.Discover([]string{fixtureDir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	loaded, err := ingest.Load(ctx, e, ds, ingest.Options{}, nil)
	if err != nil {
		t.Fatalf("ingest.Load: %v", err)
	}
	prof, err := profile.Run(ctx, e, loaded, profile.Options{}, nil)
	if err != nil {
		t.Fatalf("profile.Run: %v", err)
	}
	set, err := Run(ctx, e, prof, nil)
	if err != nil {
		t.Fatalf("checks.Run: %v", err)
	}
	return e, set
}

func describe(set *finding.Set) string {
	var lines []string
	for _, f := range set.All() {
		loc := f.Location.Display
		if f.Location.Column != "" {
			loc += "." + f.Location.Column
		}
		lines = append(lines, fmt.Sprintf("  %-32s %s", f.Rule, loc))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// The fixtures carry a known set of planted defects, and the manifest beside
// them is the list. It is read rather than repeated here: `veritix eval` scores
// a model against the same file, and two copies of a defect list would
// eventually disagree — at which point a passing test would mean nothing.
func TestPlantedDefectsAreAllFound(t *testing.T) {
	_, set := runChecks(t)

	m, err := eval.Load(fixtureDir)
	if err != nil {
		t.Fatalf("loading the manifest: %v", err)
	}
	score := eval.ScoreChecks(m, set.All())

	for _, d := range score.Missed {
		t.Errorf("missed %s at %s\n    (%s)", d.CaughtBy, d.Where, d.Why)
	}

	// The other half of a defect manifest: a check that fires on everything is
	// useless, and only the clean list catches one.
	for _, c := range score.FalsePositives {
		t.Errorf("false positive: %s fired at %s\n    (%s)", c.Rule, c.Where, c.Why)
	}

	if !score.Complete() {
		t.Logf("findings actually produced:\n%s", describe(set))
	}
	if len(score.Found) == 0 {
		t.Fatal("the manifest scored nothing; it is probably not being read")
	}
}

// The defects no check proposes are the agentic tier's whole reason for
// existing, so a deterministic run must miss them. If one starts being caught
// by a check, that is good news and the manifest has to say so — otherwise
// `veritix eval` goes on crediting a model for restating what the checks
// already found.
func TestUncoveredDefectsAreNotCaughtByAnyCheck(t *testing.T) {
	_, set := runChecks(t)

	m, err := eval.Load(fixtureDir)
	if err != nil {
		t.Fatalf("loading the manifest: %v", err)
	}
	score := eval.ScoreChecks(m, set.All())
	if len(score.Uncovered) == 0 {
		t.Fatal("the manifest lists nothing for the agent to find")
	}

	for _, d := range score.Uncovered {
		if d.Agent == nil {
			continue
		}
		for _, f := range set.All() {
			if f.Origin == finding.OriginCheck && eval.MatchesTarget(f, d) {
				t.Errorf("%s is marked caught_by: none, but %s already measures it at %s\n"+
					"    (%s)\n"+
					"    Name that rule in caught_by and drop the agent block, or the eval "+
					"scores a model for work the checks now do.",
					d.ID, f.Rule, d.Where, d.Why)
			}
		}
	}
}

// Every finding must be reproducible from its own evidence. This is the
// property that will let agent-proposed findings into the same report as
// deterministic ones.
func TestEveryFindingReproducesFromItsEvidence(t *testing.T) {
	e, set := runChecks(t)

	before := set.Len()
	dropped, err := set.Verify(t.Context(), e)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(dropped) > 0 {
		for _, d := range dropped {
			t.Errorf("%s at %s did not reproduce: %s",
				d.Rule, d.Location.String(), d.Evidence.CountQuery)
		}
	}
	if set.Len() != before {
		t.Errorf("verification changed the finding count from %d to %d", before, set.Len())
	}

	for _, f := range set.All() {
		if f.Evidence.CountQuery != "" && !f.Verified {
			t.Errorf("%s at %s has evidence but was not marked verified",
				f.Rule, f.Location.String())
		}
		if f.Title == "" {
			t.Errorf("%s has no title", f.Rule)
		}
		if f.Severity == finding.Error && f.Detail == "" {
			t.Errorf("%s is an error with no explanation", f.Rule)
		}
	}
}

// A finding whose count no longer matches must be corrected or dropped, never
// reported as originally claimed.
func TestVerifyDropsFindingsThatNoLongerHold(t *testing.T) {
	e, _ := runChecks(t)

	set := finding.NewSet()
	set.Add(finding.Finding{
		Rule:     "test.fabricated",
		Severity: finding.Error,
		Origin:   finding.OriginAgent,
		Title:    "a claim with no basis",
		Count:    999,
		Evidence: finding.Evidence{
			// True of the fixture, but nothing like 999 rows.
			CountQuery: "SELECT count(*) FROM customers_csv WHERE customer_id IS NULL",
		},
	})
	set.Add(finding.Finding{
		Rule:     "test.real",
		Severity: finding.Error,
		Origin:   finding.OriginAgent,
		Title:    "a claim that holds",
		Count:    8,
		Evidence: finding.Evidence{
			CountQuery: "SELECT count(*) FROM customers_csv",
		},
	})

	dropped, err := set.Verify(t.Context(), e)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(dropped) != 1 || dropped[0].Rule != "test.fabricated" {
		t.Errorf("the fabricated finding should have been dropped; dropped %v", dropped)
	}
	if set.Len() != 1 {
		t.Errorf("one finding should survive, got %d", set.Len())
	}
}

func TestSeverityOrdering(t *testing.T) {
	_, set := runChecks(t)

	all := set.All()
	if len(all) == 0 {
		t.Fatal("the fixtures should produce findings")
	}
	for i := 1; i < len(all); i++ {
		if all[i].Severity > all[i-1].Severity {
			t.Fatalf("findings are not ordered by severity: %s came after %s",
				all[i].Severity, all[i-1].Severity)
		}
	}

	counts := set.Counts()
	if counts[finding.Error] == 0 {
		t.Error("the fixtures contain unambiguous errors and should report some")
	}
	if worst, any := set.Max(); !any || worst != finding.Error {
		t.Errorf("Max() = %v, %v; want error", worst, any)
	}
}

func TestNamesOwnTable(t *testing.T) {
	cases := []struct {
		table, column string
		want          bool
	}{
		{"orders_csv", "order_id", true},
		{"orders", "order_id", true},
		{"customers_csv", "customer_id", true},
		{"orders_csv", "customer_id", false},
		{"sales_xlsx_q1", "order_id", false},
		{"anything", "id", true},
		{"orders_csv", "amount", false},
	}
	for _, c := range cases {
		if got := namesOwnTable(c.table, c.column); got != c.want {
			t.Errorf("namesOwnTable(%q, %q) = %v, want %v", c.table, c.column, got, c.want)
		}
	}
}
