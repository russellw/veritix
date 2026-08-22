package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/agent/llm/llmtest"
	"github.com/russellw/veritix/internal/agent/redact"
	"github.com/russellw/veritix/internal/agent/tools"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/ingest"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/source"
)

const fixtureDir = "../../testdata/dirty-retail"

// rawValuesInFixture are verbatim contents of the fixture files. Under the
// default policy none of them may appear in anything sent to a model. It is
// deliberately the same list the report tests use: the report and the model are
// the two boundaries the product's promise is made at, and they should be held
// to it in the same terms.
var rawValuesInFixture = []string{
	"CUS-000001", "CUS-000005", "CUS-999999",
	"alice@example.com", "carol@example.com",
	"Alice Smith", "Frank Green",
	"Zürich", "München", "Montréal",
	"Doohickey", "Widget",
	"Quarterly Sales Report",
}

// fixture loads the dirty-retail dataset and locks the engine down, which is
// the state the agent always finds it in.
func fixture(t *testing.T) Input {
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
	if err := e.Lockdown(ctx); err != nil {
		t.Fatalf("Lockdown: %v", err)
	}

	return Input{Engine: e, Profile: prof, Root: ds.Root}
}

// tableNamed returns a profiled table whose SQL name contains the fragment.
func tableNamed(t *testing.T, in Input, fragment string) string {
	t.Helper()
	for _, tbl := range in.Profile.Tables {
		if strings.Contains(tbl.Name, fragment) {
			return tbl.Name
		}
	}
	t.Fatalf("no table matching %q in the fixture", fragment)
	return ""
}

// This is the guarantee the product is sold on. The model is given a real
// dirty dataset and told to look at everything; nothing it is sent may contain
// a value out of the files.
func TestNothingTheModelSeesContainsACellValue(t *testing.T) {
	in := fixture(t)

	// A model that calls every read-only tool against every table and column
	// it is told about — the widest surface the guard has to hold.
	var script llmtest.Provider
	script.Reply = func(req *llm.Request) llmtest.Turn {
		step := len(req.Messages) / 2
		switch step {
		case 0:
			return llmtest.Turn{Calls: []llmtest.Call{{Name: "list_tables"}}}
		case 1:
			var calls []llmtest.Call
			for _, tbl := range in.Profile.Tables {
				calls = append(calls, llmtest.Call{
					Name: "describe_table", Input: map[string]any{"table": tbl.Name},
				})
			}
			return llmtest.Turn{Calls: calls}
		case 2:
			var calls []llmtest.Call
			for _, tbl := range in.Profile.Tables {
				for _, c := range tbl.Columns {
					calls = append(calls,
						llmtest.Call{Name: "profile_column",
							Input: map[string]any{"table": tbl.Name, "column": c.Name}},
						llmtest.Call{Name: "sample_values",
							Input: map[string]any{"table": tbl.Name, "column": c.Name, "limit": 50}},
						llmtest.Call{Name: "check_candidate_key",
							Input: map[string]any{"table": tbl.Name, "columns": []string{c.Name}}},
					)
				}
			}
			return llmtest.Turn{Calls: calls}
		case 3:
			// The most direct attempt available: ask for the rows themselves.
			var calls []llmtest.Call
			for _, tbl := range in.Profile.Tables {
				calls = append(calls, llmtest.Call{
					Name:  "run_sql",
					Input: map[string]any{"query": "SELECT * FROM " + engine.Ident(tbl.Name) + " LIMIT 20"},
				})
			}
			return llmtest.Turn{Calls: calls}
		default:
			return llmtest.Turn{Text: "done"}
		}
	}

	res, err := Run(t.Context(), in, Options{Provider: &script, MaxSteps: 10}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	sent := llmtest.Outbound(script.Requests())
	if len(sent) < 5000 {
		t.Fatalf("only %d bytes were sent to the model; the test is not exercising anything", len(sent))
	}
	for _, raw := range rawValuesInFixture {
		if strings.Contains(sent, raw) {
			t.Errorf("the value %q was sent to the model", raw)
		}
	}

	// The shapes have to actually be there, or the tools returned nothing and
	// the scan above passed vacuously.
	if !strings.Contains(sent, "⟨XXX-999999⟩") {
		t.Error("no delimited shape reached the model; the tools may have returned nothing")
	}
	if res.Trace.Redaction.Passed != 0 {
		t.Errorf("%d values were passed through unshaped under the default policy",
			res.Trace.Redaction.Passed)
	}

	// And the same must hold for what was stored about the run, since the
	// trace is served back over HTTP.
	stored, err := json.Marshal(res.Trace)
	if err != nil {
		t.Fatalf("encoding the trace: %v", err)
	}
	for _, raw := range rawValuesInFixture {
		if strings.Contains(string(stored), raw) {
			t.Errorf("the value %q was written into the run's trace", raw)
		}
	}
}

// The mechanism the whole design turns on: a claim is only a finding if the
// engine reproduces it, and a claim the engine contradicts is refused rather
// than quietly corrected.
//
// Quiet correction is not enough, and the title is why. It is model-authored
// prose and it usually carries the figure, so a finding headed "9,999 orders"
// above a count of 1 would look like Veritix vouching for the 9,999.
func TestTheEngineDecidesWhatIsTrue(t *testing.T) {
	in := fixture(t)
	orders := tableNamed(t, in, "order")

	// The fixture spells one currency "gbp" against nine "GBP".
	caseQuery := "SELECT count(*) FROM " + engine.Ident(orders) +
		" WHERE currency IS NOT NULL AND currency <> upper(currency)"

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{
			// A real problem, with a wildly inflated count.
			{Name: "record_finding", Input: map[string]any{
				"rule":           "currency_case",
				"severity":       "warning",
				"table":          orders,
				"column":         "currency",
				"title":          "9,999 orders record their currency in the wrong case",
				"detail":         "grouping by currency will report one currency as two",
				"count_query":    caseQuery,
				"affected_count": 9999,
			}},
			// A problem that does not exist at all.
			{Name: "record_finding", Input: map[string]any{
				"rule":           "invented_problem",
				"severity":       "error",
				"table":          orders,
				"title":          "every order is duplicated",
				"detail":         "this was made up",
				"count_query":    "SELECT count(*) FROM " + engine.Ident(orders) + " WHERE 1 = 0",
				"affected_count": 10,
			}},
		}},
		// Told the real figure, the model records it correctly — with a title
		// that now says the same thing the evidence does.
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "record_finding", Input: map[string]any{
				"rule":           "currency_case",
				"severity":       "warning",
				"table":          orders,
				"column":         "currency",
				"title":          "1 order records its currency in the wrong case",
				"detail":         "grouping by currency will report one currency as two",
				"count_query":    caseQuery,
				"affected_count": 1,
			}},
		}},
		llmtest.Turn{Text: "finished"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 6}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("recorded %d findings, want 1: %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if f.Rule != "agent.currency_case" {
		t.Errorf("rule = %q", f.Rule)
	}
	if f.Origin != finding.OriginAgent {
		t.Errorf("origin = %q, want agent", f.Origin)
	}
	if f.Count != 1 {
		t.Errorf("count = %d, want 1", f.Count)
	}
	if strings.Contains(f.Title, "9,999") {
		t.Errorf("a title carrying the discredited figure was recorded: %q", f.Title)
	}
	if f.Location.Column != "currency" {
		t.Errorf("column = %q, want currency", f.Location.Column)
	}
	if res.Trace.Refused != 2 {
		t.Errorf("refused = %d, want 2: the inflated claim and the invented one",
			res.Trace.Refused)
	}

	// Both refusals have to tell the model something it can act on.
	var refusals []string
	for _, s := range res.Trace.Steps {
		for _, c := range s.Calls {
			if c.IsError {
				refusals = append(refusals, c.Result)
			}
		}
	}
	if len(refusals) != 2 {
		t.Fatalf("got %d refusals, want 2", len(refusals))
	}
	if !strings.Contains(refusals[0], "the count_query returned 1") {
		t.Errorf("the inflated claim was not corrected with the real figure: %q", refusals[0])
	}
	if !strings.Contains(refusals[1], "does not reproduce") {
		t.Errorf("the invented finding's refusal did not explain itself: %q", refusals[1])
	}
}

// A SELECT is a way out of the process unless something stops it. Nothing here
// depends on Veritix recognizing a dangerous statement — the engine refuses.
func TestTheAgentCannotReachTheHost(t *testing.T) {
	in := fixture(t)
	orders := tableNamed(t, in, "order")

	attempts := []string{
		"SELECT content FROM read_text('/etc/passwd')",
		"SELECT * FROM read_csv('/etc/passwd')",
		"COPY " + engine.Ident(orders) + " TO '/tmp/veritix-exfiltrated.csv'",
		"DROP TABLE " + engine.Ident(orders),
		"DELETE FROM " + engine.Ident(orders),
		"SELECT 1; DROP TABLE " + engine.Ident(orders),
		"SET enable_external_access = true",
		"INSTALL httpfs",
		"ATTACH 'http://example.com/x.duckdb'",
	}

	var calls []llmtest.Call
	for _, q := range attempts {
		calls = append(calls, llmtest.Call{Name: "run_sql", Input: map[string]any{"query": q}})
	}
	script := llmtest.New(llmtest.Turn{Calls: calls}, llmtest.Turn{Text: "done"})

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 5}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for i, c := range res.Trace.Steps[0].Calls {
		if !c.IsError {
			t.Errorf("%q was allowed to run and returned: %s", attempts[i], c.Result)
		}
		if strings.Contains(c.Result, "root:") {
			t.Errorf("%q read a file off the host", attempts[i])
		}
	}

	// The dataset must be intact afterwards.
	n, err := in.Engine.CountRows(t.Context(), orders)
	if err != nil || n == 0 {
		t.Errorf("the orders table did not survive: %d rows, err=%v", n, err)
	}
}

// A model that gets things wrong is ordinary, not fatal. The run keeps going
// and the mistakes are visible in the trace.
func TestMistakesAreToldToTheModelRatherThanEndingTheRun(t *testing.T) {
	in := fixture(t)

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "no_such_tool"},
			{Name: "describe_table", Input: map[string]any{"table": "not_a_table"}},
			{Name: "run_sql", Input: map[string]any{"query": "SELECT nonexistent_column FROM nowhere"}},
			{Name: "record_finding", Input: map[string]any{"rule": "x", "severity": "critical",
				"table": "not_a_table", "title": "t", "detail": "d", "count_query": "SELECT 1"}},
		}},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "list_tables"}}},
		llmtest.Turn{Text: "recovered"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 5}, nil)
	if err != nil {
		t.Fatalf("a run must not fail because the model made mistakes: %v", err)
	}
	if res.Trace.Stopped != StoppedModelFinished {
		t.Errorf("stopped = %q, want finished", res.Trace.Stopped)
	}
	if len(res.Trace.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(res.Trace.Steps))
	}
	for i, c := range res.Trace.Steps[0].Calls {
		if !c.IsError {
			t.Errorf("call %d (%s) should have failed", i, c.Tool)
		}
		if c.Result == "" {
			t.Errorf("call %d (%s) told the model nothing", i, c.Tool)
		}
	}
	// The recovery step must have worked, or the loop is not really continuing.
	if res.Trace.Steps[1].Calls[0].IsError {
		t.Error("the run did not recover after the failed calls")
	}
}

// A model that never stops has to be stopped.
func TestTheStepBudgetEndsARunawayRun(t *testing.T) {
	in := fixture(t)

	var script llmtest.Provider
	script.Reply = func(*llm.Request) llmtest.Turn {
		return llmtest.Turn{
			Calls: []llmtest.Call{{Name: "list_tables"}},
			Usage: llm.Usage{Input: 100, Output: 50},
		}
	}

	res, err := Run(t.Context(), in, Options{Provider: &script, MaxSteps: 4}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Trace.Stopped != StoppedStepBudget {
		t.Errorf("stopped = %q, want step_budget", res.Trace.Stopped)
	}
	if len(res.Trace.Steps) != 4 {
		t.Errorf("ran %d steps, want the cap of 4", len(res.Trace.Steps))
	}
	if res.Trace.Stopped.Complete() {
		t.Error("a run cut short must not report itself as complete")
	}
}

func TestTheTokenBudgetEndsAnExpensiveRun(t *testing.T) {
	in := fixture(t)

	var script llmtest.Provider
	script.Reply = func(*llm.Request) llmtest.Turn {
		return llmtest.Turn{
			Calls: []llmtest.Call{{Name: "list_tables"}},
			Usage: llm.Usage{Input: 400, Output: 100},
		}
	}

	res, err := Run(t.Context(), in,
		Options{Provider: &script, MaxSteps: 100, TokenBudget: 1200}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Trace.Stopped != StoppedTokenBudget {
		t.Errorf("stopped = %q, want token_budget", res.Trace.Stopped)
	}
	if res.Trace.Usage.Total() < 1200 || res.Trace.Usage.Total() > 2000 {
		t.Errorf("spent %d tokens against a budget of 1200", res.Trace.Usage.Total())
	}
}

// The opt-in has to actually work, or it is a lie in the configuration file.
func TestSampleValuesFollowThePolicy(t *testing.T) {
	in := fixture(t)
	customers := tableNamed(t, in, "customer")

	call := llmtest.Call{Name: "sample_values", Input: map[string]any{
		"table": customers, "column": "email", "limit": 5,
	}}

	shaped := llmtest.New(llmtest.Turn{Calls: []llmtest.Call{call}}, llmtest.Turn{Text: "."})
	res, err := Run(t.Context(), in, Options{Provider: shaped, MaxSteps: 3}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	result := res.Trace.Steps[0].Calls[0].Result
	if res.Trace.Steps[0].Calls[0].IsError {
		t.Fatalf("sample_values failed: %s", result)
	}
	if strings.Contains(result, "@example.com") {
		t.Errorf("the default policy returned an address: %s", result)
	}
	if !strings.Contains(result, `"values_are_shapes":true`) {
		t.Errorf("the result did not say the values were shapes: %s", result)
	}

	allowed := llmtest.New(llmtest.Turn{Calls: []llmtest.Call{call}}, llmtest.Turn{Text: "."})
	res, err = Run(t.Context(), in, Options{
		Provider: allowed, MaxSteps: 3,
		Policy: redact.Policy{AllowValues: true},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	result = res.Trace.Steps[0].Calls[0].Result
	// Addresses are masked even when values are permitted, so what proves the
	// opt-in worked is the mask appearing where a shape used to be.
	if !strings.Contains(result, "[email]") {
		t.Errorf("--allow-sample-values did not change what was returned: %s", result)
	}
	if !res.Trace.ValuesAllowed {
		t.Error("the trace does not record that values were permitted")
	}
}

// Verbatim from a qwen3-4b run that ended this way: three complete
// record_finding payloads emitted as message content, the first of them a real
// finding, and not one tool call made. Trimmed to two.
const proseInsteadOfCalls = `I have finished. Here are the findings:
{ "rule": "orphaned_region_reference", "severity": "error", "table": "%[1]s",
  "title": "2 rows in %[1]s.region have no matching region_code in regions_csv",
  "detail": "The sales data references a region that does not exist.",
  "count_query": "SELECT count(*) FROM \"%[1]s\" WHERE trim(\"region\") NOT IN (SELECT trim(\"region_code\") FROM \"regions_csv\")",
  "affected_count": 2 },
{ "rule": "improper_margin_data", "severity": "warning", "table": "%[1]s",
  "title": "1 value in margin is an Excel error", "detail": "margin cannot be summed.",
  "count_query": "SELECT count(*) FROM \"%[1]s\" WHERE 1 = 0", "affected_count": 1 }`

// A model that writes its tool call out as prose has done the work and fumbled
// the handover. The loop used to read that as a model with nothing left to say
// and end the run with nothing recorded.
func TestAToolCallWrittenAsProseIsHandedBack(t *testing.T) {
	in := fixture(t)
	q1 := tableNamed(t, in, "q1")
	prose := fmt.Sprintf(proseInsteadOfCalls, q1)

	// Told what happened, the model makes the call — the real one only. The
	// invented margin claim is left behind, which is the model's own doing and
	// exactly what the correction must leave room for.
	script := llmtest.New(
		llmtest.Turn{Text: prose},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "record_finding", Input: map[string]any{
			"rule": "orphaned_region_reference", "severity": "error", "table": q1,
			"column": "region",
			"title":  "2 rows in sales.xlsx#Q1.region have no matching region_code",
			"detail": "the sales data references a region that does not exist",
			"count_query": `SELECT count(*) FROM ` + engine.Ident(q1) +
				` WHERE trim("region") NOT IN (SELECT trim("region_code") FROM "regions_csv")`,
			"affected_count": 2,
		}}}},
		llmtest.Turn{Text: "done"},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 5}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Findings) != 1 {
		t.Fatalf("recorded %d findings, want the 1 the model went on to call for: %+v",
			len(res.Findings), res.Findings)
	}
	if res.Findings[0].Count != 2 {
		t.Errorf("count = %d, want the 2 the engine measured", res.Findings[0].Count)
	}

	// What was sent back is in the trace, because the trace is the answer to
	// "what was the model sent" and this is neither brief nor tool result.
	correction := res.Trace.Steps[0].Correction
	if !strings.Contains(correction, "record_finding") {
		t.Errorf("the correction does not name the tool that was written out: %q", correction)
	}
	if !strings.Contains(correction, "nothing needs to be invented") {
		t.Errorf("the correction does not leave room for an empty result: %q", correction)
	}

	// And it is said once. A model that keeps writing prose is finished.
	stubborn := llmtest.New(
		llmtest.Turn{Text: prose},
		llmtest.Turn{Text: prose},
		llmtest.Turn{Text: prose},
	)
	res, err = Run(t.Context(), in, Options{Provider: stubborn, MaxSteps: 5}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Trace.Steps) != 2 {
		t.Errorf("took %d steps, want 2: one prose turn, one correction, then done",
			len(res.Trace.Steps))
	}
	if res.Trace.Stopped != StoppedModelFinished {
		t.Errorf("stopped = %q, want %q", res.Trace.Stopped, StoppedModelFinished)
	}
	if len(res.Findings) != 0 {
		t.Errorf("prose was turned into %d findings; nothing written as text may be "+
			"recorded without going through the tool that checks it", len(res.Findings))
	}
}

// Ordinary prose must not be mistaken for a fumbled call, or every run would
// end with a pointless extra model call.
func TestASummaryIsNotMistakenForAToolCall(t *testing.T) {
	in := fixture(t)
	registry := tools.New(&tools.World{
		Engine: in.Engine, Profile: in.Profile, Guard: redact.New(redact.Policy{}),
	})
	defs := registry.Definitions()

	for _, text := range []string{
		"I looked at the seven tables and found nothing beyond the deterministic pass.",
		"",
		// A tool result quoted back: its keys are not any tool's parameters.
		`The result was {"child":"orders_csv.customer_id","orphans":1,"orphan_share":0.125}`,
		// An incomplete call is not one: record_finding needs seven arguments.
		`I would record {"rule": "x"} but the count is wrong.`,
	} {
		if name, ok := writtenCall(text, defs); ok {
			t.Errorf("writtenCall(%q) = %q: ordinary prose was read as a call", text, name)
		}
	}

	// The known cost of a schema-driven test, stated rather than hidden: a
	// quoted {"table": "..."} is indistinguishable from describe_table's
	// arguments, so it draws a correction. That is one step, once in a run,
	// against a whole run's work being dropped — and the correction says
	// plainly that stopping is a legitimate answer.
	if _, ok := writtenCall(`for instance {"table": "orders_csv"}`, defs); !ok {
		t.Error("the single-argument case has stopped matching; the comment above is stale")
	}

	if name, ok := writtenCall(fmt.Sprintf(proseInsteadOfCalls, "t"), defs); !ok ||
		name != "record_finding" {
		t.Errorf("writtenCall on a written-out record_finding = %q, %v", name, ok)
	}
}

// Orientation used to cost a third of the budget: both local models measured
// spent eight of twenty-four steps calling list_tables and describe_table
// before doing any work. The profile is in the brief now, so the first thing
// the model sees is what those calls would have returned.
func TestTheBriefCarriesTheProfileSoOrientationCostsNoSteps(t *testing.T) {
	in := fixture(t)
	in.Known = []finding.Finding{{
		Rule:     "column.type_violation",
		Severity: finding.Error,
		Title:    "amount holds 1 value(s) that are not numbers",
		Location: finding.Location{Table: tableNamed(t, in, "q1"), Display: "sales.xlsx#Q1"},
	}}

	script := llmtest.New(llmtest.Turn{Text: "nothing to add"})
	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 2}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Trace.Steps) != 1 {
		t.Fatalf("the scripted model took %d steps, want 1", len(res.Trace.Steps))
	}

	reqs := script.Requests()
	if len(reqs) == 0 {
		t.Fatal("the model was never called")
	}
	sent := reqs[0].Messages[0].Text()

	// Everything describe_table would have answered, before a single step.
	for _, want := range []string{
		"customers_csv", "sales_xlsx_q1", // every table, not just the first
		`"declared_type"`, `"inferred_kind"`, `"conformance"`,
		`"distinct"`, `"shapes"`,
		"⟨XXX-999999⟩", // a shape, delimited like any other
		"amount holds 1 value(s) that are not numbers", // and what is already known
	} {
		if !strings.Contains(sent, want) {
			t.Errorf("the brief does not carry %q", want)
		}
	}

	// It is customer-derived content on the one path that is not a tool result,
	// so it has to have been through the guard like everything else.
	for _, raw := range rawValuesInFixture {
		if strings.Contains(sent, raw) {
			t.Errorf("the brief carries the cell value %q", raw)
		}
	}
	if res.Trace.Redaction.Sealed == 0 {
		t.Error("the brief did not go through the guard: nothing was sealed")
	}
}

// A dataset can be wider than any context window, so the profile in the brief
// is bounded and says what it left out rather than silently describing half a
// dataset as if it were all of it.
func TestABriefTooLargeToFitNamesWhatItOmitted(t *testing.T) {
	in := fixture(t)
	guard := redact.New(redact.Policy{})
	registry := tools.New(&tools.World{
		Engine: in.Engine, Profile: in.Profile, Guard: guard,
	})

	sealed, err := registry.Overview(1) // room for the first table and no more
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	var out struct {
		Tables  []struct{ Table string } `json:"tables"`
		Omitted []string                 `json:"described_on_request"`
		Note    string                   `json:"note"`
	}
	if err := json.Unmarshal([]byte(sealed.String()), &out); err != nil {
		t.Fatalf("decoding the overview: %v", err)
	}

	if len(out.Tables) != 1 {
		t.Errorf("described %d tables on a 1-byte budget, want 1 — a brief that "+
			"describes nothing is worse than one over budget", len(out.Tables))
	}
	if len(out.Omitted)+len(out.Tables) != len(in.Profile.Tables) {
		t.Errorf("%d described + %d omitted, want %d tables accounted for",
			len(out.Tables), len(out.Omitted), len(in.Profile.Tables))
	}
	if !strings.Contains(out.Note, "describe_table") {
		t.Errorf("the note does not tell the model how to get the rest: %q", out.Note)
	}
}

// The agent's value over the deterministic pass is the relationship that pass
// never proposed, and against dirty-retail a small model measured exactly that,
// said nothing, and moved on. A check that lands on new ground has to say so
// where the model is looking, and a check that lands on known ground has to say
// that too, or the budget goes on re-verifying the brief.
func TestACheckSaysWhetherTheDefectIsAlreadyKnown(t *testing.T) {
	in := fixture(t)
	orders := tableNamed(t, in, "order")
	customers := tableNamed(t, in, "customer")
	q1 := tableNamed(t, in, "q1")
	regions := tableNamed(t, in, "region")

	// The one relationship the deterministic pass reports here. The agent is
	// told about it in the brief; the point of this test is that the tool says
	// it again, at the moment the model is looking at the number.
	in.Known = []finding.Finding{{
		Rule:     "reference.orphan_values",
		Severity: finding.Error,
		Origin:   finding.OriginCheck,
		Title:    "1 value in orders.csv.customer_id has no matching row in customers.csv",
		Location: finding.Location{
			Table: orders, Display: "orders.csv", Column: "customer_id",
		},
	}}

	// The second pair is the one relate.go never proposes, because "region" and
	// "region_code" do not match by name.
	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "check_referential_integrity", Input: map[string]any{
				"child_table": orders, "child_column": "customer_id",
				"parent_table": customers, "parent_column": "customer_id",
			}},
			{Name: "check_referential_integrity", Input: map[string]any{
				"child_table": q1, "child_column": "region",
				"parent_table": regions, "parent_column": "region_code",
			}},
		}},
		llmtest.Turn{Text: "."},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 3}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := res.Trace.Steps[0].Calls
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(calls))
	}

	type reference struct {
		Child   string `json:"child"`
		Orphans int64  `json:"orphans"`
		Note    string `json:"note"`
	}
	results := make([]reference, len(calls))
	for i, c := range calls {
		if c.IsError {
			t.Fatalf("call %d failed: %s", i, c.Result)
		}
		if err := json.Unmarshal([]byte(c.Result), &results[i]); err != nil {
			t.Fatalf("call %d result: %v", i, err)
		}
	}

	known, discovered := results[0], results[1]

	// If the fixture ever stops planting these, the notes below would pass
	// vacuously by being absent.
	if known.Orphans == 0 || discovered.Orphans == 0 {
		t.Fatalf("the fixture no longer has orphans in both relationships: %+v", results)
	}

	if !strings.Contains(known.Note, "already reports this as reference.orphan_values") {
		t.Errorf("a defect the deterministic pass reports was not named as known: %q", known.Note)
	}
	if strings.Contains(known.Note, "record_finding") {
		t.Errorf("the model was invited to re-report a known defect: %q", known.Note)
	}

	if !strings.Contains(discovered.Note, "record_finding") {
		t.Errorf("a defect no check reports did not tell the model to record it: %q",
			discovered.Note)
	}
	if !strings.Contains(discovered.Note, "no deterministic finding covers this") {
		t.Errorf("the note did not say the ground was new: %q", discovered.Note)
	}
}

// slowProvider blocks until the call's context ends, then reports what a real
// provider reports for a dead connection — which is what an expired deadline
// looks like from inside an HTTP client.
type slowProvider struct {
	calls atomic.Int32
}

func (p *slowProvider) Name() string  { return "slow" }
func (p *slowProvider) Model() string { return "slow-model" }

func (p *slowProvider) Complete(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
	p.calls.Add(1)
	<-ctx.Done()
	return nil, &llm.Error{
		Provider: p.Name(), Message: ctx.Err().Error(), Retryable: true, Err: ctx.Err(),
	}
}

// A slow model is not a broken one, and the difference matters because the two
// need opposite responses. Retrying an expired deadline asks the identical
// question of the same endpoint with the same deadline: it cannot succeed, and
// it costs the timeout again for every attempt.
//
// Found against a local model on a CPU, where a step legitimately took longer
// than llm.request_timeout: half of a 56-minute run was the same request sent
// three times, and the run then failed rather than stopping cleanly.
func TestAnExpiredDeadlineIsNotRetried(t *testing.T) {
	in := fixture(t)

	var slow slowProvider
	res, err := Run(t.Context(), in,
		Options{Provider: &slow, MaxSteps: 4, RequestTimeout: 20 * time.Millisecond}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := slow.calls.Load(); n != 1 {
		t.Errorf("the model was called %d times; an expired deadline must not be retried", n)
	}
	if res.Trace.Stopped != StoppedProviderError {
		t.Errorf("stopped = %q, want provider_error", res.Trace.Stopped)
	}
	// The operator has to be able to tell "too slow" from "unreachable", because
	// only one of them is fixed by changing a setting.
	if !strings.Contains(res.Trace.Error, "llm.request_timeout") {
		t.Errorf("the error does not name the setting to change: %s", res.Trace.Error)
	}
}

// The other half of the same rule: a failure that really is about the moment
// still gets another go, or one dropped connection ends an audit.
func TestATransientFailureIsRetried(t *testing.T) {
	in := fixture(t)

	var attempts atomic.Int32
	var script llmtest.Provider
	script.Reply = func(*llm.Request) llmtest.Turn {
		if attempts.Add(1) == 1 {
			return llmtest.Turn{Err: &llm.Error{
				Provider: "scripted", Message: "connection reset", Retryable: true,
			}}
		}
		return llmtest.Turn{Text: "Nothing further."}
	}

	res, err := Run(t.Context(), in, Options{Provider: &script, MaxSteps: 4}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if attempts.Load() != 2 {
		t.Errorf("the model was called %d times; a retryable failure should be retried", attempts.Load())
	}
	if res.Trace.Stopped == StoppedProviderError {
		t.Errorf("the run failed on an error it had already recovered from: %s", res.Trace.Error)
	}
}

// Configure is what the CLI and the server both go through, so the default has
// to be the safe one.
func TestNoModelIsConfiguredByDefault(t *testing.T) {
	opts, err := Configure(config.Default().LLM)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if opts != nil {
		t.Error("a default installation must not be configured to talk to a model")
	}

	opts, err = Configure(config.LLM{Provider: config.ProviderAnthropic, MaxSteps: 5})
	if err != nil {
		t.Fatalf("Configure(anthropic): %v", err)
	}
	if opts == nil || opts.Provider == nil {
		t.Fatal("configuring anthropic produced no provider")
	}
	if opts.Policy.AllowValues {
		t.Error("values must not be permitted unless the operator asks")
	}

	if _, err := Configure(config.LLM{Provider: "wishful-thinking"}); err == nil {
		t.Error("an unknown provider should be refused")
	}
}

// TestAnIdenticalRefusedCallIsCalledOut is a livelock the loop could not see.
//
// On a dirty-logistics run, gpt-oss-120b sent the same propose_rule four times
// running — a one_of rule naming no column — and got the same correct refusal
// each time. At around five minutes a step against a budget of twenty-four,
// that is a sixth of the run spent on one fixable mistake. Nothing in the
// refusal distinguished "you got this wrong" from "you got this wrong in
// exactly the same way you just did", which is the distinction that would have
// made it change something.
//
// The fix is a note, not a stop: the budget is still the backstop, and what to
// do next is still the model's decision. What the loop owes it is the fact that
// it is repeating itself.
func TestAnIdenticalRefusedCallIsCalledOut(t *testing.T) {
	in := fixture(t)

	bad := llmtest.Call{Name: "propose_rule", Input: map[string]any{
		"rule": "status_domain", "description": "status is a fixed vocabulary",
		"table": "customers_csv", "expect": "one_of", "violations_now": 0,
	}}

	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{bad}},
		llmtest.Turn{Calls: []llmtest.Call{bad}},
		llmtest.Turn{Calls: []llmtest.Call{bad}},
		llmtest.Turn{Text: "I will leave that one."},
	)

	res, err := Run(t.Context(), in, Options{Provider: script, MaxSteps: 6}, nil)
	if err != nil {
		t.Fatalf("a repeated mistake must not fail the run: %v", err)
	}

	first := res.Trace.Steps[0].Calls[0]
	if !first.IsError {
		t.Fatal("a one_of rule naming no column should have been refused")
	}
	if strings.Contains(first.Result, "attempt 2") {
		t.Error("the first attempt was described as a repeat")
	}

	// The second and third have to say so, and say what the options are.
	for i, step := range res.Trace.Steps[1:3] {
		got := step.Calls[0].Result
		if !strings.Contains(got, fmt.Sprintf("attempt %d", i+2)) {
			t.Errorf("repeat %d was not named as one: %s", i+2, got)
		}
		// A correction that reads as "you were supposed to succeed at this" is
		// how a model burns a budget rather than spending it.
		if !strings.Contains(got, "legitimate answer") {
			t.Errorf("repeat %d does not say that moving on is allowed: %s", i+2, got)
		}
		// The original refusal has to survive, or the note replaces the only
		// thing that says what is actually wrong.
		if !strings.Contains(got, "applies to a column") {
			t.Errorf("repeat %d lost the reason it was refused: %s", i+2, got)
		}
	}

	// A different call is not a repeat, so nothing here should have leaked into
	// the run's own accounting.
	if res.Trace.Proposals != 0 {
		t.Errorf("a refused proposal was recorded anyway: %d", res.Trace.Proposals)
	}
}

// The engine's query timeout has to be sized for Veritix's own measurements —
// profiling one column of a twenty-million-row table takes minutes, and a
// limit below that silently drops the column. That leaves the model's SQL,
// which is the one statement in the process nobody reviewed, needing a bound
// of its own; World.QueryTimeout is it, applied to every tool call rather
// than to each tool, so a tool added later cannot miss it.
func TestAToolCallIsBoundedApartFromTheEngine(t *testing.T) {
	in := fixture(t)
	registry := tools.New(&tools.World{
		Engine:       in.Engine,
		Profile:      in.Profile,
		Guard:        redact.New(redact.Policy{}),
		QueryTimeout: time.Millisecond,
	})

	args := []byte(`{"sql":"SELECT count(*) FROM range(200000000) t(i) WHERE i::VARCHAR LIKE '%7%'"}`)
	res := registry.Invoke(t.Context(), "run_sql", args)
	if !res.IsError {
		t.Fatalf("a query far beyond the tool's own timeout was allowed to finish")
	}
}
