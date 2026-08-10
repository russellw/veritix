package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/ingest"
	"github.com/russellwallace/veritix/internal/profile"
	"github.com/russellwallace/veritix/internal/source"
)

const fixtureDir = "../../testdata/dirty-retail"

func fixture(t *testing.T) (*engine.Engine, *profile.Dataset) {
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
	return e, prof
}

func evaluateYAML(t *testing.T, body string) []finding.Finding {
	t.Helper()
	e, prof := fixture(t)

	path := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found, err := Evaluate(t.Context(), e, prof, f, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return found
}

func byRule(found []finding.Finding, rule string) *finding.Finding {
	for i := range found {
		if found[i].Rule == rule {
			return &found[i]
		}
	}
	return nil
}

func TestShippedExampleRulesFire(t *testing.T) {
	f, err := Load(filepath.Join(fixtureDir, "veritix-rules.yaml"))
	if err != nil {
		t.Fatalf("the shipped example rules must be valid: %v", err)
	}

	e, prof := fixture(t)
	found, err := Evaluate(t.Context(), e, prof, f, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	for _, want := range []string{
		"rule.order_amount_positive",
		"rule.customer_status_domain",
		"rule.orders_reference_customers",
		"rule.no_implausible_order_value",
		"rule.never_applied",
	} {
		if byRule(found, want) == nil {
			var got []string
			for _, f := range found {
				got = append(got, f.Rule)
			}
			t.Errorf("expected %s to fire; got %v", want, got)
		}
	}
}

// A rule the customer wrote is an expectation they hold, so breaking it is an
// error unless they say otherwise.
func TestOmittedSeverityDefaultsToError(t *testing.T) {
	found := evaluateYAML(t, `
rules:
  - id: implicit
    table: orders.csv
    column: amount
    expect: positive
  - id: explicit
    table: orders.csv
    column: amount
    expect: positive
    severity: info
`)
	implicit := byRule(found, "rule.implicit")
	if implicit == nil {
		t.Fatal("the implicit rule should have fired")
	}
	if implicit.Severity != finding.Error {
		t.Errorf("severity = %v, want error when omitted", implicit.Severity)
	}

	explicit := byRule(found, "rule.explicit")
	if explicit == nil {
		t.Fatal("the explicit rule should have fired")
	}
	if explicit.Severity != finding.Info {
		t.Errorf("severity = %v, want the explicit info to be honoured", explicit.Severity)
	}
}

// allow_missing means "where there is no value" — including a cell somebody
// typed N/A into, which is a missing value written differently.
func TestAllowMissingExemptsPlaceholders(t *testing.T) {
	strict := evaluateYAML(t, `
rules:
  - id: strict
    table: orders.csv
    column: amount
    expect: positive
`)
	lenient := evaluateYAML(t, `
rules:
  - id: lenient
    table: orders.csv
    column: amount
    expect: positive
    allow_missing: true
`)

	s, l := byRule(strict, "rule.strict"), byRule(lenient, "rule.lenient")
	if s == nil || l == nil {
		t.Fatal("both rules should have fired on the negative amount")
	}
	if l.Count >= s.Count {
		t.Errorf("allow_missing should exempt the N/A placeholder: strict=%d lenient=%d",
			s.Count, l.Count)
	}
	if l.Count != 1 {
		t.Errorf("only the genuinely negative amount should remain, got %d", l.Count)
	}
}

func TestIgnoreCase(t *testing.T) {
	sensitive := evaluateYAML(t, `
rules:
  - id: cased
    table: customers.csv
    column: status
    expect: one_of
    values: [Active, Inactive]
`)
	insensitive := evaluateYAML(t, `
rules:
  - id: uncased
    table: customers.csv
    column: status
    expect: one_of
    values: [Active, Inactive]
    ignore_case: true
`)

	c, u := byRule(sensitive, "rule.cased"), byRule(insensitive, "rule.uncased")
	if c == nil {
		t.Fatal("the case-sensitive rule should flag 'active' and 'ACTIVE'")
	}
	if u != nil && u.Count >= c.Count {
		t.Errorf("ignore_case should reduce violations: %d then %d", c.Count, u.Count)
	}
}

// A rule that matches nothing must say so. Silence from a rule the customer
// is relying on is the worst possible outcome.
func TestRuleThatMatchesNothingIsReported(t *testing.T) {
	found := evaluateYAML(t, `
rules:
  - id: typo
    table: customers.csv
    column: no_such_column
    expect: not_null
`)
	f := byRule(found, "rule.never_applied")
	if f == nil {
		t.Fatal("a rule matching nothing must be reported")
	}
	if !strings.Contains(f.Detail, "no_such_column") {
		t.Errorf("the report should name the missing target, got %q", f.Detail)
	}
	if f.Location.Display == "" {
		t.Error("the finding should say which target the rule was aimed at")
	}
}

func TestGlobMatchesSeveralTables(t *testing.T) {
	found := evaluateYAML(t, `
rules:
  - id: id_format
    table: "*.csv"
    column: customer_id
    expect: matches
    pattern: 'CUS-[0-9]{6}'
    allow_missing: true
`)
	// customers.csv and orders.csv both have a customer_id; only orders.csv
	// has one that breaks the pattern... but neither does, so the rule should
	// have applied without firing rather than reporting it never ran.
	if f := byRule(found, "rule.never_applied"); f != nil {
		t.Errorf("the glob should have matched at least one table, got %q", f.Title)
	}
}

func TestValidationRejectsUnusableRules(t *testing.T) {
	cases := map[string]string{
		"no id":            `rules: [{table: t, expect: not_null, column: c}]`,
		"no table":         `rules: [{id: a, expect: not_null, column: c}]`,
		"no expectation":   `rules: [{id: a, table: t, column: c}]`,
		"no column":        `rules: [{id: a, table: t, expect: not_null}]`,
		"one_of no values": `rules: [{id: a, table: t, column: c, expect: one_of}]`,
		"matches no regex": `rules: [{id: a, table: t, column: c, expect: matches}]`,
		"range unbounded":  `rules: [{id: a, table: t, column: c, expect: range}]`,
		"sql no where":     `rules: [{id: a, table: t, expect: sql}]`,
		"bad references":   `rules: [{id: a, table: t, column: c, expect: references, references: nodot}]`,
		"unknown expect":   `rules: [{id: a, table: t, column: c, expect: wishful}]`,
		"duplicate id": `rules:
  - {id: a, table: t, column: c, expect: not_null}
  - {id: a, table: t, column: d, expect: not_null}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "r.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Error("want a validation error, got nil")
			}
		})
	}
}

func TestUnknownVersionIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.yaml")
	body := "version: 99\nrules: []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("a future document version should be rejected rather than half-understood")
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"orders.csv", "orders.csv", true},
		{"ORDERS.CSV", "orders.csv", true},
		{"*.csv", "orders.csv", true},
		{"*.csv", "sales.xlsx", false},
		{"sales.xlsx#*", "sales.xlsx#Q1", true},
		{"*orders*", "2024/orders.csv", true},
		{"orders", "orders.csv", false},
		{"*", "anything", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// Rule-produced findings must carry evidence like any other, so the same
// verification applies to them.
func TestRuleFindingsCarryVerifiableEvidence(t *testing.T) {
	e, prof := fixture(t)

	f, err := Load(filepath.Join(fixtureDir, "veritix-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	found, err := Evaluate(t.Context(), e, prof, f, nil)
	if err != nil {
		t.Fatal(err)
	}

	set := finding.NewSet()
	set.AddAll(found)
	dropped, err := set.Verify(t.Context(), e)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, d := range dropped {
		t.Errorf("%s did not reproduce: %s", d.Rule, d.Evidence.CountQuery)
	}
}
