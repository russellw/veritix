package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/profile"
)

// oneOfFromCurrent is the rule a model would propose: the shape of the
// expectation, with no cell values in it.
func oneOfFromCurrent(id, table, column string, ignoreCase bool) *File {
	return &File{
		Version: 1,
		Rules: []Rule{{
			ID:         id,
			Table:      table,
			Column:     column,
			Expect:     ExpectOneOf,
			ValuesFrom: ValuesFromCurrent,
			IgnoreCase: ignoreCase,
		}},
	}
}

func TestValuesFromCurrentIsFilledInFromTheData(t *testing.T) {
	e, prof := fixture(t)

	f := oneOfFromCurrent("status_domain", "customers.csv", "status", false)
	if err := f.Validate(); err != nil {
		t.Fatalf("a rule that reads its values from the data must validate: %v", err)
	}
	if err := Materialize(t.Context(), e, prof, f); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	got := f.Rules[0].Values
	if len(got) == 0 {
		t.Fatal("no values were filled in")
	}
	// The fixture's status column is the point of the exercise: it holds a
	// typo and three spellings of the same word, and the person reviewing
	// the proposal is the one who strikes them out.
	for _, want := range []string{"Active", "active", "Actve", "Inactive"} {
		if !contains(got, want) {
			t.Errorf("values %q do not include %q, which the column holds", got, want)
		}
	}

	// A resolved rule is an ordinary one_of rule, so a second pass reads
	// nothing and changes nothing.
	if f.Rules[0].ValuesFrom != "" {
		t.Errorf("values_from survived materialization as %q", f.Rules[0].ValuesFrom)
	}
	before := strings.Join(f.Rules[0].Values, "|")
	if err := Materialize(t.Context(), e, prof, f); err != nil {
		t.Fatalf("Materialize again: %v", err)
	}
	if after := strings.Join(f.Rules[0].Values, "|"); after != before {
		t.Errorf("materializing twice changed the values: %q then %q", before, after)
	}
}

// The defining property: a rule whose values came from the data holds against
// that data. record_finding refuses a claim that reproduces zero rows; a
// proposed rule with zero violations is the best kind, and this is why the two
// must not share a test for "does it fire".
func TestAMaterializedRuleHoldsAgainstTheDataItCameFrom(t *testing.T) {
	for _, ignoreCase := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore_case=%v", ignoreCase), func(t *testing.T) {
			e, prof := fixture(t)

			f := oneOfFromCurrent("status_domain", "customers.csv", "status", ignoreCase)
			if err := Materialize(t.Context(), e, prof, f); err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			found, err := Evaluate(t.Context(), e, prof, f, nil)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			for _, fd := range found {
				t.Errorf("a rule filled in from the data fired against it: %s — %s", fd.Rule, fd.Title)
			}
		})
	}
}

// ignore_case is reduced by the engine, not by Go, so the list carries one
// spelling of a value the rule would treat as one value.
func TestIgnoreCaseCollapsesTheVocabulary(t *testing.T) {
	e, prof := fixture(t)

	f := oneOfFromCurrent("status_domain", "customers.csv", "status", true)
	if err := Materialize(t.Context(), e, prof, f); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	seen := map[string]string{}
	for _, v := range f.Rules[0].Values {
		key := strings.ToLower(strings.TrimSpace(v))
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q are the same value to an ignore_case rule", prev, v)
		}
		seen[key] = v
	}
	if len(seen) >= 6 {
		t.Errorf("ignore_case collapsed nothing: %q", f.Rules[0].Values)
	}
}

func TestValuesFromCurrentRefusesWhatNobodyCouldReview(t *testing.T) {
	e, prof := fixture(t)

	// A column with more distinct values than a person will read through.
	// The fixture has none, because the fixture is small on purpose.
	ctx := t.Context()
	if err := e.Exec(ctx, "CREATE TABLE wide AS SELECT CAST(i AS VARCHAR) AS v FROM range(500) t(i)"); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	prof.Tables = append(prof.Tables, &profile.Table{
		Name: "wide", Display: "wide.csv", RowCount: 500,
		Columns: []*profile.Column{{Name: "v", Original: "v"}},
	})

	f := oneOfFromCurrent("free_text", "wide.csv", "v", false)
	err := Materialize(ctx, e, prof, f)
	if err == nil {
		t.Fatal("a 500-value vocabulary was accepted")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the refusal does not say how many values there are: %v", err)
	}
	if f.Rules[0].Values != nil {
		t.Error("a refused rule was filled in anyway")
	}
}

func TestValuesFromCurrentRefusesFreeText(t *testing.T) {
	e, prof := fixture(t)

	ctx := t.Context()
	long := strings.Repeat("x", maxMaterializedValueLen+1)
	if err := e.Exec(ctx, fmt.Sprintf("CREATE TABLE notes AS SELECT %s AS v", engine.Literal(long))); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	prof.Tables = append(prof.Tables, &profile.Table{
		Name: "notes", Display: "notes.csv", RowCount: 1,
		Columns: []*profile.Column{{Name: "v", Original: "v"}},
	})

	f := oneOfFromCurrent("notes_domain", "notes.csv", "v", false)
	if err := Materialize(ctx, e, prof, f); err == nil {
		t.Fatal("a value of free text was accepted into a vocabulary")
	}
}

func TestValuesFromCurrentRefusesATargetThatIsNotThere(t *testing.T) {
	e, prof := fixture(t)

	f := oneOfFromCurrent("ghost", "customers.csv", "account_code", false)
	err := Materialize(t.Context(), e, prof, f)
	if err == nil {
		t.Fatal("a rule was filled in from a column that does not exist")
	}
	if !strings.Contains(err.Error(), "account_code") {
		t.Errorf("the refusal does not name the target: %v", err)
	}
}

// The values are cell values, so a rules file on disk carries them written
// out. A file still asking for them is a rule that cannot fire, and saying so
// at load time is cheaper than a customer trusting it for a year.
func TestAFileOnDiskMayNotDeferItsValues(t *testing.T) {
	body := `version: 1
rules:
  - id: status_domain
    table: customers.csv
    column: status
    expect: one_of
    values_from: current
`
	path := writeTemp(t, body)
	if _, err := Load(path); err == nil {
		t.Fatal("a rules file deferring its values loaded")
	} else if !strings.Contains(err.Error(), "accepted") {
		t.Errorf("the refusal does not say when values_from is resolved: %v", err)
	}
}

func TestValuesFromIsRefusedWhereItMeansNothing(t *testing.T) {
	cases := map[string]Rule{
		"listed and read at once": {
			ID: "a", Table: "t", Column: "c", Expect: ExpectOneOf,
			Values: []string{"x"}, ValuesFrom: ValuesFromCurrent,
		},
		"neither listed nor read": {
			ID: "b", Table: "t", Column: "c", Expect: ExpectOneOf,
		},
		"an expectation with no value list": {
			ID: "c", Table: "t", Column: "c", Expect: ExpectNotNull,
			ValuesFrom: ValuesFromCurrent,
		},
		"a source that does not exist": {
			ID: "d", Table: "t", Column: "c", Expect: ExpectOneOf,
			ValuesFrom: ValuesSource("everything_ever_seen"),
		},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			f := &File{Version: 1, Rules: []Rule{r}}
			if err := f.Validate(); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
