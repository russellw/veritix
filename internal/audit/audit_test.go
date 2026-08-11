package audit

import (
	"strings"
	"testing"

	"github.com/russellwallace/veritix/internal/agent"
	"github.com/russellwallace/veritix/internal/agent/llm/llmtest"
	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/finding"
)

const fixtureDir = "../../testdata/dirty-retail"

func run(t *testing.T, opts Options) *Result {
	t.Helper()
	opts.Paths = []string{fixtureDir}
	opts.Engine = config.Default().Engine

	res, err := Run(t.Context(), opts, nil)
	if err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	return res
}

// Without a model the pipeline is the deterministic auditor, unchanged. This
// matters more than it looks: it is what a customer gets by default, and the
// agent must not be load-bearing for any of it.
func TestThePipelineIsCompleteWithoutAModel(t *testing.T) {
	res := run(t, Options{})

	if res.Trace != nil {
		t.Error("a run with no model produced an agent trace")
	}
	if res.Findings.Len() == 0 {
		t.Fatal("the deterministic pass found nothing in the dirty fixtures")
	}
	for _, f := range res.Findings.All() {
		if f.Origin == finding.OriginAgent {
			t.Errorf("a run with no model produced an agent finding: %s", f.Rule)
		}
	}
	if res.Engine().LockedDown() {
		t.Error("the engine was locked down for a run that has no agent SQL to contain")
	}
}

// The agent's findings go through the same pipeline as everybody else's:
// recorded against the engine's measurement, then verified again with the
// deterministic findings before anything is reported.
func TestAgentFindingsAreVerifiedAlongsideTheRest(t *testing.T) {
	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{{
			Name: "record_finding",
			Input: map[string]any{
				"rule":     "negative_amount",
				"severity": "error",
				"table":    "orders_csv",
				"column":   "amount",
				"title":    "orders have a negative amount",
				"detail":   "a refund recorded as an order will understate revenue",
				"count_query": "SELECT count(*) FROM orders_csv " +
					"WHERE TRY_CAST(amount AS DOUBLE) < 0",
				"row_query":      "SELECT * FROM orders_csv WHERE TRY_CAST(amount AS DOUBLE) < 0",
				"affected_count": 1,
			},
		}}},
		llmtest.Turn{Text: "One finding recorded."},
	)

	res := run(t, Options{Agent: &agent.Options{Provider: script, MaxSteps: 5}})

	if res.Trace == nil {
		t.Fatal("the run produced no trace")
	}
	if res.Trace.Findings != 1 {
		t.Errorf("the agent recorded %d findings, want 1", res.Trace.Findings)
	}

	var found *finding.Finding
	for _, f := range res.Findings.All() {
		if f.Rule == "agent.negative_amount" {
			found = &f
			break
		}
	}
	if found == nil {
		t.Fatal("the agent's finding did not reach the result")
	}
	if !found.Verified {
		t.Error("the agent's finding was reported without being verified")
	}
	if found.Origin != finding.OriginAgent {
		t.Errorf("origin = %q, want agent", found.Origin)
	}
	if found.Count != 1 {
		t.Errorf("count = %d, want the 1 negative amount in the fixture", found.Count)
	}
	if found.Evidence.RowQuery == "" {
		t.Error("the row query was dropped, so nobody can inspect the offending rows")
	}

	// The deterministic findings are still all there: the agent adds, it does
	// not replace.
	var deterministic int
	for _, f := range res.Findings.All() {
		if f.Origin == finding.OriginCheck {
			deterministic++
		}
	}
	if deterministic == 0 {
		t.Error("the deterministic findings vanished when the agent ran")
	}
}

// The agent is told what has already been found, so it does not spend its
// budget rediscovering it.
func TestTheAgentIsBriefedOnWhatIsAlreadyKnown(t *testing.T) {
	script := llmtest.New(llmtest.Turn{Text: "Nothing to add."})
	res := run(t, Options{Agent: &agent.Options{Provider: script, MaxSteps: 3}})

	if res.Trace.Findings != 0 {
		t.Errorf("the agent recorded %d findings, want none", res.Trace.Findings)
	}

	brief := llmtest.Outbound(script.Requests())
	if !strings.Contains(brief, "Already found by the deterministic pass") {
		t.Error("the brief did not tell the agent what the deterministic pass found")
	}
	if !strings.Contains(brief, "orders_csv") {
		t.Error("the brief did not name the dataset's tables")
	}
}

// The engine is locked down before the model gets a query tool, and the
// pipeline is what guarantees it — not the caller remembering to.
func TestTheEngineIsLockedDownBeforeTheAgentRuns(t *testing.T) {
	probe := llmtest.New(llmtest.Turn{
		Calls: []llmtest.Call{{
			Name:  "run_sql",
			Input: map[string]any{"query": "SELECT count(*) FROM read_text('/etc/hostname')"},
		}},
	}, llmtest.Turn{Text: "done"})

	res := run(t, Options{Agent: &agent.Options{Provider: probe, MaxSteps: 3}})

	if !res.Engine().LockedDown() {
		t.Error("the engine was not locked down for the agent")
	}
	call := res.Trace.Steps[0].Calls[0]
	if !call.IsError {
		t.Errorf("the agent read a file off the host: %s", call.Result)
	}
}
