package checks

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/engine"
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

// at reports whether a finding with this rule exists at this location.
// Location is "source" or "source.column".
func at(set *finding.Set, rule, where string) bool {
	for _, f := range set.All() {
		if f.Rule != rule {
			continue
		}
		loc := f.Location.Display
		if f.Location.Column != "" {
			loc += "." + f.Location.Column
		}
		if loc == where {
			return true
		}
	}
	return false
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

// The fixtures carry a known set of planted defects. This test is the
// manifest: each entry is a defect deliberately placed in a file, and the
// check that must catch it.
func TestPlantedDefectsAreAllFound(t *testing.T) {
	_, set := runChecks(t)

	manifest := []struct {
		rule  string
		where string
		why   string
	}{
		{"column.duplicate_header", "customers.csv.region_1",
			"customers.csv repeats the header name 'region'"},
		{"column.mixed_date_formats", "customers.csv.signup_date",
			"signup_date mixes ISO and DD/MM/YYYY"},
		{"column.type_violation", "customers.csv.signup_date",
			"signup_date contains the literal 'not a date'"},
		{"column.case_variants", "customers.csv.status",
			"status holds Active, active, and ACTIVE"},
		{"column.whitespace_padding", "customers.csv.name",
			"'  Bob Jones  ' is padded with spaces"},
		{"column.missing_values", "customers.csv.region",
			"region uses '-' and 'N/A' as placeholders"},
		{"table.duplicate_rows", "customers.csv",
			"CUS-000005 appears as two identical rows"},
		{"key.duplicate_values", "customers.csv.customer_id",
			"customer_id repeats CUS-000005"},

		{"table.unreadable_rows", "orders.csv",
			"two rows have the wrong number of fields"},
		{"reference.orphan_values", "orders.csv.customer_id",
			"CUS-999999 is not in customers.csv"},
		{"column.implausible_dates", "orders.csv.order_date",
			"1900-01-01 is a placeholder date"},
		{"column.unexpected_negative", "orders.csv.amount",
			"an amount cannot sensibly be negative"},
		{"csv.width_disagreement", "orders.csv",
			"a stray comma makes one row wider than the header"},

		{"csv.encoding_not_utf8", "regions.csv",
			"regions.csv is Latin-1 encoded"},
		{"csv.no_header", "products.tsv",
			"products.tsv has no header row"},

		{"excel.hidden_rows", "sales.xlsx#Q1",
			"one data row is hidden from a reader of the workbook"},
		{"excel.formula_errors", "sales.xlsx#Q1",
			"#REF! and #DIV/0! are left in cells"},
		{"excel.merged_cells", "sales.xlsx#Q1",
			"the title row is merged across four columns"},
		{"excel.stacked_tables", "sales.xlsx#Q1",
			"a TOTAL row sits below a blank separator"},
		{"excel.header_offset", "sales.xlsx#Q1",
			"the header is on row 3, under two title rows"},
		{"excel.hidden_sheet", "sales.xlsx#Archive",
			"the Archive worksheet is hidden"},
	}

	var missed int
	for _, m := range manifest {
		if !at(set, m.rule, m.where) {
			missed++
			t.Errorf("missed %s at %s\n    (%s)", m.rule, m.where, m.why)
		}
	}
	if missed > 0 {
		t.Logf("findings actually produced:\n%s", describe(set))
	}
}

// The other half of a defect manifest: a check that fires on everything is
// useless. These are places the fixtures are deliberately clean.
func TestCleanDataProducesNoFindings(t *testing.T) {
	_, set := runChecks(t)

	notExpected := []struct {
		rule  string
		where string
		why   string
	}{
		{"column.type_violation", "orders.csv.order_id",
			"every order_id is a whole number"},
		{"column.case_variants", "customers.csv.customer_id",
			"the ids differ by more than case"},
		{"column.whitespace_padding", "customers.csv.email",
			"the emails are not padded"},
		{"table.duplicate_rows", "orders.csv",
			"no two order rows are identical in every column"},
		{"table.empty", "customers.csv",
			"customers.csv has rows"},
		{"column.empty", "customers.csv.name",
			"every customer has a name"},
		{"reference.orphan_values", "sales.xlsx#Reference.region_code",
			"the reference sheet is the authority, not a referrer"},
	}

	for _, n := range notExpected {
		if at(set, n.rule, n.where) {
			t.Errorf("false positive: %s fired at %s\n    (%s)", n.rule, n.where, n.why)
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
