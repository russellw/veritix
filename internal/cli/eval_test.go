package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runEvalCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newEvalCmd(newEnv())
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	// Not a one-line return: Go evaluates return operands left to right, so
	// out.String() would be read before the command had written anything.
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// With no model this is what CI runs, and it is a complete measurement of the
// deterministic auditor: everything planted found, nothing fired where the data
// is clean.
func TestEvalScoresTheCheckSuiteWithNoModel(t *testing.T) {
	out, err := runEvalCmd(t, fixtureDir)
	if err != nil {
		t.Fatalf("eval: %v\n%s", err, out)
	}
	if !strings.Contains(out, "0 false positives") {
		t.Errorf("the scorecard does not report the clean half:\n%s", out)
	}
	if !strings.Contains(out, "no check proposes") {
		t.Errorf("the scorecard does not distinguish the agent's targets:\n%s", out)
	}
}

// The gate is the reason this can live in CI: a manifest the checks no longer
// satisfy fails the build without anybody having to ask for it, because the
// manifest is not an opinion.
func TestEvalFailsWhenTheChecksMissAPlantedDefect(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "veritix-manifest.yaml")
	err := os.WriteFile(manifest, []byte(`
version: 1
dataset: invented
defects:
  - id: not.real
    where: orders.csv.amount
    why: nothing plants this, so nothing can find it
    caught_by: column.no_such_check
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	out, err := runEvalCmd(t, fixtureDir, "--manifest", manifest)
	if err == nil {
		t.Fatalf("a missed planted defect did not fail the eval:\n%s", out)
	}
	if !strings.Contains(err.Error(), "missed 1 planted defect") {
		t.Errorf("the failure does not say what went wrong: %v", err)
	}
	// The scorecard is written before the gate decides, for the same reason the
	// report is: the run that fails a build is the one somebody has to read.
	if !strings.Contains(out, "not.real") {
		t.Errorf("the scorecard was not written before the failure:\n%s", out)
	}
}

// --min-recall is about the model, so asking for it without one is a mistake
// worth naming rather than a threshold that silently passes at zero runs.
func TestMinRecallNeedsAModel(t *testing.T) {
	_, err := runEvalCmd(t, fixtureDir, "--min-recall", "0.5")
	if err == nil {
		t.Fatal("--min-recall with no model was accepted")
	}
	if !strings.Contains(err.Error(), "--llm") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
}

// A scored run has to be an ordinary run: the same pipeline, the same egress
// policy, the same trace. Anything else and the score is for a configuration
// nobody ships.
func TestEvalScoresARealAgentRun(t *testing.T) {
	base := stubChatModel(t,
		`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"record_finding","arguments":`+
			`"{\"rule\":\"orphaned_reference\",\"severity\":\"error\",\"table\":\"customers_csv\",`+
			`\"column\":\"region\",\"title\":\"4 customers have a region code that resolves to nothing\",`+
			`\"detail\":\"a regional total built by joining these will silently omit them\",`+
			`\"count_query\":\"SELECT count(*) FROM customers_csv WHERE region NOT IN (SELECT region_code FROM regions_csv)\",`+
			`\"affected_count\":4}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"Nothing further."}}]}`,
	)

	out, err := runEvalCmd(t, fixtureDir, "--format", "json",
		"--llm", "openai-compatible", "--llm-base-url", base, "--llm-model", "stub")
	if err != nil {
		t.Fatalf("eval: %v\n%s", err, out)
	}

	var doc struct {
		Model string `json:"model"`
		Agent struct {
			MeanRecall float64 `json:"mean_recall"`
			Coverage   float64 `json:"coverage"`
			Targets    []struct {
				ID   string `json:"id"`
				Hits int    `json:"hits"`
			} `json:"targets"`
		} `json:"agent"`
		Runs []struct {
			Detected []string `json:"detected"`
			Trace    *struct {
				Findings int `json:"findings"`
			} `json:"trace"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the JSON scorecard does not parse: %v\n%s", err, out)
	}

	if doc.Model != "stub" {
		t.Errorf("model = %q, want the model that was evaluated", doc.Model)
	}
	if doc.Agent.MeanRecall != 0.5 || doc.Agent.Coverage != 0.5 {
		t.Errorf("one of two targets found should score 0.5 both ways, got %v and %v",
			doc.Agent.MeanRecall, doc.Agent.Coverage)
	}
	if len(doc.Runs) != 1 || len(doc.Runs[0].Detected) != 1 ||
		doc.Runs[0].Detected[0] != "customers.region_orphans" {
		t.Fatalf("the run did not credit the defect the model found: %+v", doc.Runs)
	}
	// The trace rides along, so a scorecard kept for months still answers what
	// the model was actually sent.
	if doc.Runs[0].Trace == nil || doc.Runs[0].Trace.Findings != 1 {
		t.Errorf("the run's trace is missing from the scorecard: %+v", doc.Runs[0].Trace)
	}
}

// An eval never lifts the egress policy, whatever the configuration says. A
// score obtained by showing the model cell values is not a score for the
// product anybody ships.
func TestEvalWillNotShowTheModelCellValues(t *testing.T) {
	base := stubChatModel(t)

	e := newEnv()
	e.cfg.LLM.AllowSampleValues = true
	cmd := newEvalCmd(e)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{fixtureDir, "--format", "json",
		"--llm", "openai-compatible", "--llm-base-url", base, "--llm-model", "stub"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("eval: %v\n%s", err, out.String())
	}

	var doc struct {
		Runs []struct {
			Trace struct {
				ValuesAllowed bool `json:"values_allowed"`
			} `json:"trace"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("the JSON scorecard does not parse: %v", err)
	}
	if len(doc.Runs) != 1 {
		t.Fatalf("expected one run, got %d", len(doc.Runs))
	}
	if doc.Runs[0].Trace.ValuesAllowed {
		t.Error("the eval ran with cell values allowed")
	}
}
