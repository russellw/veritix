package profile

import (
	"testing"

	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/ingest"
	"github.com/russellwallace/veritix/internal/source"
)

const fixtureDir = "../../testdata/dirty-retail"

func profileFixture(t *testing.T) *Dataset {
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
		t.Fatalf("Load: %v", err)
	}
	prof, err := Run(ctx, e, loaded, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return prof
}

func table(t *testing.T, ds *Dataset, display string) *Table {
	t.Helper()
	for _, tb := range ds.Tables {
		if tb.Display == display {
			return tb
		}
	}
	t.Fatalf("no profiled table %q", display)
	return nil
}

func column(t *testing.T, tb *Table, name string) *Column {
	t.Helper()
	if c := tb.Column(name); c != nil {
		return c
	}
	var names []string
	for _, c := range tb.Columns {
		names = append(names, c.Name)
	}
	t.Fatalf("no column %q in %s; have %v", name, tb.Display, names)
	return nil
}

func TestEveryColumnIsProfiled(t *testing.T) {
	ds := profileFixture(t)
	if len(ds.Tables) == 0 {
		t.Fatal("no tables profiled")
	}
	for _, tb := range ds.Tables {
		if len(tb.Columns) == 0 {
			t.Errorf("%s has no columns", tb.Display)
		}
		for _, c := range tb.Columns {
			if c.Total != tb.RowCount {
				t.Errorf("%s.%s Total = %d, want the table's %d rows",
					tb.Display, c.Name, c.Total, tb.RowCount)
			}
		}
	}
}

func TestTypeInference(t *testing.T) {
	ds := profileFixture(t)
	orders := table(t, ds, "orders.csv")

	// order_id is entirely integers.
	if got := column(t, orders, "order_id").Inferred.Kind; got != KindInteger {
		t.Errorf("order_id inferred as %s, want integer", got)
	}

	// order_date is entirely ISO dates.
	if got := column(t, orders, "order_date").Inferred.Kind; got != KindDate {
		t.Errorf("order_date inferred as %s, want date", got)
	}

	// customer_id is a formatted code, not a number.
	if got := column(t, orders, "customer_id").Inferred.Kind; got != KindText {
		t.Errorf("customer_id inferred as %s, want text", got)
	}
}

// A placeholder such as "N/A" is a missing value, not a type violation. A
// numeric column containing one is still a numeric column with a gap; calling
// it text would throw away every numeric check that column deserves.
func TestSentinelDoesNotDemoteColumnType(t *testing.T) {
	ds := profileFixture(t)
	amount := column(t, table(t, ds, "orders.csv"), "amount")

	if amount.Inferred.Kind != KindDecimal {
		t.Errorf("amount inferred as %s, want decimal despite the N/A", amount.Inferred.Kind)
	}

	var sawNA bool
	for _, s := range amount.Sentinels {
		if s.Value == "n/a" {
			sawNA = true
		}
	}
	if !sawNA {
		t.Errorf(`expected "N/A" among the sentinels, got %v`, amount.Sentinels)
	}
	if amount.Missing() <= amount.Nulls {
		t.Error("Missing() must count sentinels as well as nulls")
	}
	if amount.Populated() >= amount.Total {
		t.Error("Populated() must exclude the sentinel")
	}
}

// A value that is neither valid nor a recognised way of writing "missing" is a
// genuine type violation and must be reported as one.
func TestGenuineTypeViolationIsReported(t *testing.T) {
	ds := profileFixture(t)
	signup := column(t, table(t, ds, "customers.csv"), "signup_date")

	// signup_date holds "not a date", which is not a sentinel.
	if signup.Inferred.Nonconforming == 0 {
		t.Errorf(`"not a date" should be reported as nonconforming; inference = %+v`,
			signup.Inferred)
	}
	if signup.Inferred.Conformance >= 1.0 {
		t.Errorf("conformance = %v, want less than 1", signup.Inferred.Conformance)
	}
	// The other candidate types must still be recorded, so a report can show
	// how close the column came to each.
	if len(signup.Inferred.Candidates) == 0 {
		t.Error("candidate type counts should be retained for the report")
	}
}

func TestNumericStats(t *testing.T) {
	ds := profileFixture(t)
	amount := column(t, table(t, ds, "orders.csv"), "amount")

	if amount.Numeric == nil {
		t.Fatal("amount should have numeric statistics")
	}
	n := amount.Numeric
	if n.Negative == 0 {
		t.Error("order 1004 has a negative amount, which should be counted")
	}
	if n.Max < 99_999_999 {
		t.Errorf("Max = %v, want the 99999999.99 outlier to be seen", n.Max)
	}
	if n.Min >= 0 {
		t.Errorf("Min = %v, want a negative minimum", n.Min)
	}
}

// A date column that mixes formats is the classic silent corruption: whichever
// reading a downstream tool picks, some dates come out wrong and nothing errors.
func TestMixedAndAmbiguousDates(t *testing.T) {
	ds := profileFixture(t)
	signup := column(t, table(t, ds, "customers.csv"), "signup_date")

	if signup.Temporal == nil {
		t.Fatal("signup_date should have temporal statistics")
	}
	if len(signup.Temporal.Formats) < 2 {
		t.Errorf("expected more than one date format, got %v", signup.Temporal.Formats)
	}

	orders := column(t, table(t, ds, "orders.csv"), "order_date")
	if orders.Temporal == nil {
		t.Fatal("order_date should have temporal statistics")
	}
	if orders.Temporal.Implausible == 0 {
		t.Error("the 1900-01-01 placeholder date should be reported as implausible")
	}
}

// Values that differ only by case or surrounding whitespace are the same value
// to a human and different values to a GROUP BY.
func TestCaseAndWhitespaceVariantsAreVisible(t *testing.T) {
	ds := profileFixture(t)
	customers := table(t, ds, "customers.csv")

	status := column(t, customers, "status")
	if status.Distinct <= status.DistinctNormalised {
		t.Errorf("status has Active/active/ACTIVE and should have more raw distinct values "+
			"(%d) than normalised ones (%d)", status.Distinct, status.DistinctNormalised)
	}

	name := column(t, customers, "name")
	if name.LeadingWhitespace == 0 && name.TrailingWhitespace == 0 {
		t.Error(`"  Bob Jones  " should register as having surrounding whitespace`)
	}
}

// Shapes are what an agent is allowed to see instead of the values themselves,
// so they have to be genuinely descriptive.
func TestShapesDescribeFormatWithoutRevealingValues(t *testing.T) {
	ds := profileFixture(t)
	id := column(t, table(t, ds, "customers.csv"), "customer_id")

	if len(id.Shapes) == 0 {
		t.Fatal("customer_id should have value shapes")
	}
	top := id.Shapes[0]
	if top.Value != "XXX-999999" {
		t.Errorf("top shape = %q, want %q", top.Value, "XXX-999999")
	}
	if top.Share < 0.9 {
		t.Errorf("the dominant shape should cover most values, got %v", top.Share)
	}
	// The shape must not contain any character from the underlying value.
	if containsAny(top.Value, "CUS0123456789"[:3]) {
		t.Errorf("shape %q leaks characters from the source values", top.Value)
	}
}

func TestUniquenessDetection(t *testing.T) {
	ds := profileFixture(t)

	// customers.csv repeats CUS-000005, so customer_id is not unique.
	id := column(t, table(t, ds, "customers.csv"), "customer_id")
	if id.Unique() {
		t.Error("customer_id repeats a value and must not be reported as unique")
	}

	// orders.csv repeats order_id 1001 as well.
	orderID := column(t, table(t, ds, "orders.csv"), "order_id")
	if orderID.Unique() {
		t.Error("order_id repeats 1001 and must not be reported as unique")
	}

	// regions.csv has a genuinely unique column to prove the check can pass.
	city := column(t, table(t, ds, "regions.csv"), "city")
	if !city.Unique() {
		t.Errorf("city should be unique: distinct=%d total=%d nulls=%d",
			city.Distinct, city.Total, city.Nulls)
	}
}

func TestEmptyAndDeclaredTypes(t *testing.T) {
	ds := profileFixture(t)
	customers := table(t, ds, "customers.csv")

	email := column(t, customers, "email")
	if email.Nulls+email.Blanks == 0 {
		t.Error("one customer has no email; it should be counted as null or blank")
	}

	// Every column should carry the type a conventional import would have
	// guessed, so the report can contrast it with reality.
	for _, c := range customers.Columns {
		if c.DeclaredType == "" {
			t.Errorf("%s has no declared type recorded", c.Name)
		}
	}
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		for _, x := range s {
			if x == c {
				return true
			}
		}
	}
	return false
}
