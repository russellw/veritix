package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/agent/llm/llmtest"
	"github.com/russellw/veritix/internal/rules"
)

// statusValues are the contents of the fixture's status column. A rule's
// permitted set is materialized from exactly these, which is what makes
// propose_rule the most likely place for a cell value to escape.
var statusValues = []string{"ACTIVE", "Actve", "Inactive"}

func customerRules(t *testing.T) *rules.File {
	t.Helper()
	f, err := rules.Load(filepath.Join(fixtureDir, "veritix-rules.yaml"))
	if err != nil {
		t.Fatalf("loading the fixture's rules: %v", err)
	}
	return f
}

// proposalFor returns the proposal with that slug, or fails.
func proposalFor(t *testing.T, res *Result, slug string) rules.Proposal {
	t.Helper()
	for _, p := range res.Proposals {
		if p.Rule.ID == slug {
			return p
		}
	}
	t.Fatalf("no proposal called %q among %d", slug, len(res.Proposals))
	return rules.Proposal{}
}

// refusals gathers every tool result the model was told was an error.
func refusals(res *Result) []string {
	var out []string
	for _, s := range res.Trace.Steps {
		for _, c := range s.Calls {
			if c.IsError {
				out = append(out, c.Result)
			}
		}
	}
	return out
}

// The engine decides the number here exactly as it does for a finding — with
// one inversion. A count query returning zero refuses a finding, because a
// problem that does not reproduce is not a problem. A rule that nothing
// violates today is the best kind there is, and is accepted as it stands.
func TestAProposalIsMeasuredBeforeItIsAccepted(t *testing.T) {
	in := fixture(t)
	customers := tableNamed(t, in, "customer")
	orders := tableNamed(t, in, "order")

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{
			// A real expectation with an invented violation count.
			{Name: "propose_rule", Input: map[string]any{
				"rule":           "currency_uppercase",
				"description":    "currency codes are recorded in upper case",
				"table":          orders,
				"column":         "currency",
				"expect":         "matches",
				"pattern":        "[A-Z]{3}",
				"allow_missing":  true,
				"violations_now": 400,
			}},
			// An invariant that already holds, stated as holding.
			{Name: "propose_rule", Input: map[string]any{
				"rule":           "customer_id_format",
				"description":    "customer identifiers are CUS- and six digits",
				"table":          customers,
				"column":         "customer_id",
				"expect":         "matches",
				"pattern":        "CUS-[0-9]{6}",
				"allow_missing":  true,
				"violations_now": 0,
			}},
		}},
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "propose_rule", Input: map[string]any{
				"rule":           "currency_uppercase",
				"description":    "currency codes are recorded in upper case",
				"table":          orders,
				"column":         "currency",
				"expect":         "matches",
				"pattern":        "[A-Z]{3}",
				"allow_missing":  true,
				"violations_now": 1,
			}},
		}},
		llmtest.Turn{Text: "done"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 6}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Proposals) != 2 {
		t.Fatalf("proposed %d rules, want 2: %+v", len(res.Proposals), res.Proposals)
	}

	// The one that already holds is a proposal like any other, and its zero
	// is recorded rather than treated as a failure to reproduce.
	held := proposalFor(t, res, "customer_id_format")
	if held.ViolationsNow != 0 {
		t.Errorf("violations_now = %d, want 0", held.ViolationsNow)
	}

	// The inflated claim was refused, told the real figure, and accepted on
	// the retry at the engine's number.
	corrected := proposalFor(t, res, "currency_uppercase")
	if corrected.ViolationsNow != 1 {
		t.Errorf("violations_now = %d, want 1", corrected.ViolationsNow)
	}
	if res.Trace.Proposals != 2 {
		t.Errorf("the trace records %d proposals, want 2", res.Trace.Proposals)
	}

	told := strings.Join(refusals(res), "\n")
	if !strings.Contains(told, "400") || !strings.Contains(told, "breaks on 1") {
		t.Errorf("the refusal must hand back both figures; it said: %s", told)
	}
}

// The permitted set of a one_of rule is materialized from the column, so it is
// cell values by construction. The model asks for the rule and is told how
// many values it got; the person reviewing the proposal is the one who reads
// them.
func TestAProposalsValuesNeverReachTheModel(t *testing.T) {
	in := fixture(t)
	customers := tableNamed(t, in, "customer")

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "propose_rule", Input: map[string]any{
				"rule":           "status_domain",
				"description":    "status is drawn from a fixed vocabulary",
				"rationale":      "the column has few distinct values and drives downstream billing",
				"table":          customers,
				"column":         "status",
				"expect":         "one_of",
				"ignore_case":    true,
				"allow_missing":  true,
				"violations_now": 0,
			}},
		}},
		llmtest.Turn{Text: "done"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 4}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	p := proposalFor(t, res, "status_domain")
	if len(p.Rule.Values) < 3 {
		t.Fatalf("the permitted set was not filled in from the data: %q", p.Rule.Values)
	}
	if p.Rule.ValuesFrom != "" {
		t.Errorf("the proposal is still deferring its values: %q", p.Rule.ValuesFrom)
	}

	sent := llmtest.Outbound(script.Requests())
	for _, v := range append(statusValues, rawValuesInFixture...) {
		if strings.Contains(sent, v) {
			t.Errorf("the value %q was sent to the model", v)
		}
	}
	// The count is not a value, and the model does need it.
	if !strings.Contains(sent, `"permitted_values":`) {
		t.Error("the model was not told how many values were filled in")
	}
}

// Protection the customer already has is not worth a step of the budget, and
// the rules they wrote are not a channel for their data either: a one_of rule
// in their file lists cell values verbatim, so the brief carries ids and
// targets and nothing else.
func TestRulesAlreadyInForceAreNotProposedAgain(t *testing.T) {
	in := fixture(t)
	in.Rules = customerRules(t)
	customers := tableNamed(t, in, "customer")

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "propose_rule", Input: map[string]any{
				"rule":           "status_domain",
				"description":    "status is drawn from a fixed vocabulary",
				"table":          customers,
				"column":         "status",
				"expect":         "one_of",
				"violations_now": 0,
			}},
		}},
		llmtest.Turn{Text: "done"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 4}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Proposals) != 0 {
		t.Fatalf("a rule already in force was proposed again: %+v", res.Proposals)
	}
	if told := strings.Join(refusals(res), "\n"); !strings.Contains(told, "customer_status_domain") {
		t.Errorf("the refusal must name the rule that already covers it; it said: %s", told)
	}

	// "Suspended" and "Closed" are in the customer's rules file and in none of
	// the data files, so finding either in what was sent means the brief
	// rendered a rule's body rather than its target.
	sent := llmtest.Outbound(script.Requests())
	for _, v := range []string{"Suspended", "Closed", "1000000"} {
		if strings.Contains(sent, v) {
			t.Errorf("the brief sent the body of a customer's rule: %q", v)
		}
	}
	if !strings.Contains(sent, "customer_status_domain") {
		t.Error("the brief did not list the rules already in force")
	}
}

// A rule is stored and re-run on every future audit, so an identifier the
// model invented must not reach SQL — not once, and not a year from now.
func TestAProposalCannotNameWhatIsNotThere(t *testing.T) {
	in := fixture(t)
	customers := tableNamed(t, in, "customer")

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "propose_rule", Input: map[string]any{
				"rule": "ghost_column", "description": "a column that is not there",
				"table": customers, "column": "account_code",
				"expect": "not_null", "violations_now": 0,
			}},
			{Name: "propose_rule", Input: map[string]any{
				"rule": "ghost_table", "description": "a table that is not there",
				"table": "invoices", "column": "total",
				"expect": "positive", "violations_now": 0,
			}},
			{Name: "propose_rule", Input: map[string]any{
				"rule": "ghost_reference", "description": "a reference to nothing",
				"table": customers, "column": "region",
				"expect": "references", "references": "warehouses.code",
				"violations_now": 0,
			}},
			{Name: "propose_rule", Input: map[string]any{
				"rule": "bad_sql", "description": "a clause the engine will not take",
				"table": customers, "expect": "sql",
				"where": "DROP TABLE customers_csv", "violations_now": 0,
			}},
			{Name: "propose_rule", Input: map[string]any{
				"rule": "unknown_expectation", "description": "an expectation that does not exist",
				"table": customers, "column": "status",
				"expect": "looks_sensible", "violations_now": 0,
			}},
		}},
		llmtest.Turn{Text: "done"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 4}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Proposals) != 0 {
		t.Fatalf("an unresolvable rule was proposed: %+v", res.Proposals)
	}
	if got := len(refusals(res)); got != 5 {
		t.Errorf("%d of 5 bad proposals were refused", got)
	}
}

// The same expectation proposed twice is one proposal to review. The identity
// is what the rule asserts, not the words around it: the slug and the
// description are the model's wording, and two runs word one expectation two
// ways.
func TestAProposalIsIdentifiedByWhatItAsserts(t *testing.T) {
	in := fixture(t)
	orders := tableNamed(t, in, "order")

	base := map[string]any{
		"rule": "amount_positive", "description": "an order line is a charge",
		"table": orders, "column": "amount", "expect": "positive",
		"allow_missing": true, "violations_now": 1,
	}
	reworded := map[string]any{}
	for k, v := range base {
		reworded[k] = v
	}
	reworded["rule"] = "order_amount_above_zero"
	reworded["description"] = "the same expectation, worded differently"

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{{Name: "propose_rule", Input: base}}},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "propose_rule", Input: reworded}}},
		llmtest.Turn{Text: "done"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 6}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Proposals) != 1 {
		t.Fatalf("one expectation produced %d proposals: %+v", len(res.Proposals), res.Proposals)
	}
	if told := strings.Join(refusals(res), "\n"); !strings.Contains(told, "already proposed") {
		t.Errorf("the second proposal must say it is a repeat; it said: %s", told)
	}
}

// A where clause is model-authored SQL that a future audit will run without
// anybody watching, so it goes through the same parse as every other statement
// the model writes.
func TestAProposedWhereClauseIsRunAsTheRuleWouldRunIt(t *testing.T) {
	in := fixture(t)
	orders := tableNamed(t, in, "order")

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "propose_rule", Input: map[string]any{
				"rule":        "amount_within_reason",
				"description": "an order above a million needs sign-off before it is exported",
				"table":       orders, "expect": "sql",
				"where":          "TRY_CAST(amount AS DOUBLE) > 1000000",
				"severity":       "warning",
				"violations_now": 1,
			}},
		}},
		llmtest.Turn{Text: "done"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 4}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	p := proposalFor(t, res, "amount_within_reason")
	if p.Rule.Severity == nil || p.Rule.Severity.String() != "warning" {
		t.Errorf("the model's severity was not carried: %v", p.Rule.Severity)
	}

	// What was proposed has to be a rule Veritix can load and run, or the
	// accept step has nothing to accept.
	file := &rules.File{Version: 1, Rules: []rules.Rule{p.Rule}}
	if err := file.Validate(); err != nil {
		t.Errorf("the proposal is not a valid rule: %v", err)
	}
	found, err := rules.Evaluate(t.Context(), in.Engine, in.Profile, file, nil)
	if err != nil {
		t.Fatalf("re-evaluating the proposal: %v", err)
	}
	for _, f := range found {
		if f.Rule == "rule.never_applied" || f.Rule == "rule.invalid" {
			t.Errorf("the proposed rule does not run: %s — %s", f.Rule, f.Detail)
		}
	}
}

// propose_rule is an output, not a way to ask questions with. It has to be in
// the surface the model is offered, and its schema must not carry the egress
// policy any more than any other tool's does.
func TestProposeRuleIsOfferedAndCarriesNoPolicy(t *testing.T) {
	in := fixture(t)
	script := llmtest.New(llmtest.Turn{Text: "nothing to do"})
	if _, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 2}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := script.Requests()
	var tool *llm.Tool
	for i, def := range reqs[0].Tools {
		if def.Name == "propose_rule" {
			tool = &reqs[0].Tools[i]
		}
	}
	if tool == nil {
		t.Fatal("propose_rule is not offered to the model")
	}
	for name := range tool.Properties {
		switch name {
		case "values", "include_values", "allow_values":
			t.Errorf("propose_rule takes a %q parameter; the values are the engine's to fill in", name)
		}
	}
}
