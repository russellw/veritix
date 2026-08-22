package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A proposal is only worth anything if what comes out the other end is a rule
// Veritix can load and run. Rendering to YAML and reading it back is the whole
// accept step in miniature, so it is tested as a round trip rather than as a
// string.
func TestProposalsRenderAsRulesThatLoadAndRun(t *testing.T) {
	e, prof := fixture(t)
	ctx := t.Context()

	vocabulary := oneOfFromCurrent("status_domain", "customers.csv", "status", true)
	if err := Materialize(ctx, e, prof, vocabulary); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	proposals := []Proposal{
		{
			Rule:      vocabulary.Rules[0],
			Display:   "customers.csv",
			Rationale: "status drives billing, so a new spelling of it is a billing defect",
		},
		{
			Rule: Rule{
				ID: "amount_within_reason", Description: "an order above a million needs sign-off",
				Table: "orders_csv", Expect: ExpectSQL,
				Where: "TRY_CAST(amount AS DOUBLE) > 1000000",
			},
			Display:       "orders.csv",
			ViolationsNow: 1,
		},
	}

	var out strings.Builder
	header := ProposalHeader("testdata/dirty-retail", time.Now())
	if err := RenderProposals(&out, proposals, header); err != nil {
		t.Fatalf("RenderProposals: %v", err)
	}
	t.Logf("\n%s", out.String())

	path := filepath.Join(t.TempDir(), "proposed.yaml")
	if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("what was proposed does not load as a rules file: %v", err)
	}
	if len(f.Rules) != 2 {
		t.Fatalf("loaded %d rules, want 2", len(f.Rules))
	}

	found, err := Evaluate(ctx, e, prof, f, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, fd := range found {
		switch fd.Rule {
		case "rule.never_applied", "rule.invalid":
			t.Errorf("a rendered proposal does not run: %s — %s", fd.Rule, fd.Detail)
		case "rule.status_domain":
			t.Errorf("the vocabulary rule fired against the data it came from: %s", fd.Title)
		case "rule.amount_within_reason":
			if fd.Count != 1 {
				t.Errorf("the sql rule measured %d rows, want the 1 it was proposed with", fd.Count)
			}
		}
	}
}

// The file has to say what it is. A list of rules that looks authoritative and
// is not would be worse than no list: the review step only works if the person
// reading knows the decision is still theirs.
func TestARenderedProposalSaysItIsNotInForce(t *testing.T) {
	e, prof := fixture(t)
	vocabulary := oneOfFromCurrent("status_domain", "customers.csv", "status", true)
	if err := Materialize(t.Context(), e, prof, vocabulary); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	var out strings.Builder
	if err := RenderProposals(&out, []Proposal{{
		Rule: vocabulary.Rules[0], Display: "customers.csv",
	}}, ProposalHeader("testdata/dirty-retail", time.Now())); err != nil {
		t.Fatalf("RenderProposals: %v", err)
	}
	body := out.String()

	if !strings.Contains(body, "in force") {
		t.Error("the header does not say the rules are not in force")
	}
	// The values were materialized from a column holding a typo. Somebody has
	// to be told that before they accept the list.
	if !strings.Contains(body, "Strike out anything") {
		t.Errorf("the permitted values carry no warning to read them:\n%s", body)
	}
	if !strings.Contains(body, "Actve") {
		t.Error("the permitted values were not written out, so there is nothing to review")
	}
}
