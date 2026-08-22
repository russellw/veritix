package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/report"
)

// proposeStatusDomain is the tool call a model makes to propose the fixture's
// status vocabulary. It names no values, because it has never been shown one.
func proposeStatusDomain() string {
	return toolCallReply("propose_rule", map[string]any{
		"rule":           "status_domain",
		"description":    "status is drawn from a fixed vocabulary",
		"rationale":      "status drives billing, so a new spelling of it is a billing defect",
		"table":          "customers_csv",
		"column":         "status",
		"expect":         "one_of",
		"ignore_case":    true,
		"allow_missing":  true,
		"violations_now": 0,
	})
}

func proposalsOf(ts *testServer, runID string) []report.ProposalInfo {
	ts.t.Helper()
	var body struct {
		Proposals []report.ProposalInfo `json:"proposals"`
	}
	ts.decode(ts.get("/api/v1/runs/"+runID+"/proposals"), http.StatusOK, &body)
	return body.Proposals
}

// The claim the whole milestone rests on, end to end over HTTP: a model
// proposes a rule on one run, a person accepts it with the misspelling struck
// out, and the *next* run finds the defect with no model involved at all.
//
// That conversion is the product's answer to "why would I pay for a model to
// audit my data". You pay once per class of defect, not once per audit.
func TestAnAcceptedRuleIsEnforcedWithoutTheModel(t *testing.T) {
	model := newStubModel(t, proposeStatusDomain())
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()

	run := ts.startRun(map[string]any{"dataset_id": datasetID, "agent": true})

	proposals := proposalsOf(ts, run.ID)
	if len(proposals) != 1 {
		t.Fatalf("the run proposed %d rules, want 1", len(proposals))
	}
	p := proposals[0]
	if p.Expect != "one_of" || p.Target != "customers.csv.status" {
		t.Fatalf("the proposal is not the one that was made: %+v", p)
	}
	if p.PermittedValueCount == 0 {
		t.Error("the proposal permits nothing, so its values were never materialized")
	}
	if len(p.PermittedValues) != 0 {
		t.Errorf("the list response carries the values themselves: %q", p.PermittedValues)
	}

	// One proposal, fetched by id, does carry them: that is the review.
	var one struct {
		Proposal report.ProposalInfo `json:"proposal"`
		YAML     string              `json:"yaml"`
		Note     string              `json:"values_note"`
	}
	ts.decode(ts.get("/api/v1/runs/"+run.ID+"/proposals/"+p.ID), http.StatusOK, &one)
	if !contains(one.Proposal.PermittedValues, "Actve") {
		t.Fatalf("the proposal does not carry what it would permit: %q",
			one.Proposal.PermittedValues)
	}
	if one.Note == "" {
		t.Error("nothing tells the reviewer what those values are")
	}
	if !strings.Contains(one.YAML, "expect: one_of") {
		t.Errorf("the proposal does not render as a rule: %s", one.YAML)
	}

	// Accept it with the typo struck out, which is what the review is for.
	var accepted struct {
		Rule  report.ProposalInfo `json:"rule"`
		Count int                 `json:"rules_in_force"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/v1/datasets/"+datasetID+"/rules", map[string]any{
		"run_id":      run.ID,
		"proposal_id": p.ID,
		"severity":    "error",
		"values":      []string{"Active", "Inactive", "Suspended", "Closed"},
	}), http.StatusCreated, &accepted)
	if accepted.Count != 1 {
		t.Fatalf("rules in force = %d, want 1", accepted.Count)
	}

	// A second run, with no model at all. The defect the agent's proposal was
	// about is now found by the deterministic pass.
	second := ts.startRun(map[string]any{"dataset_id": datasetID})

	var doc report.Document
	ts.decode(ts.get("/api/v1/runs/"+second.ID+"/report"), http.StatusOK, &doc)

	if doc.Agent != nil {
		t.Fatal("the second run used a model")
	}
	var fired *report.FindingInfo
	for i, f := range doc.Findings {
		if f.Rule == "rule.status_domain" {
			fired = &doc.Findings[i]
		}
	}
	if fired == nil {
		t.Fatal("the accepted rule did not run on the next audit")
	}
	if fired.Count != 1 {
		t.Errorf("the rule caught %d rows, want the 1 misspelled status", fired.Count)
	}
	if fired.Severity != "error" {
		t.Errorf("severity = %q, want the error the reviewer chose", fired.Severity)
	}
	if fired.Column != "status" {
		t.Errorf("the finding is at %s.%s", fired.Table, fired.Column)
	}
}

// The list is described, not reproduced. A proposal's permitted set is
// materialized from the customer's column, so it may reach a client only when
// one named proposal is asked for.
func TestProposalListsCarryNoCellValues(t *testing.T) {
	model := newStubModel(t, proposeStatusDomain())
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()
	run := ts.startRun(map[string]any{"dataset_id": datasetID, "agent": true})

	p := proposalsOf(ts, run.ID)[0]
	ts.decode(ts.do(http.MethodPost, "/api/v1/datasets/"+datasetID+"/rules", map[string]any{
		"run_id": run.ID, "proposal_id": p.ID,
	}), http.StatusCreated, &struct{}{})

	for _, path := range []string{
		"/api/v1/runs/" + run.ID + "/proposals",
		"/api/v1/datasets/" + datasetID + "/rules",
		"/api/v1/runs/" + run.ID + "/report",
	} {
		body := string(ts.get(path).Body)
		for _, value := range []string{"Actve", "Inactive", "ACTIVE"} {
			if strings.Contains(body, value) {
				t.Errorf("%s carries the cell value %q", path, value)
			}
		}
	}
}

// A rule measured against one dataset must not be installed against another,
// where nothing has ever run it.
func TestAProposalIsOnlyAcceptableForItsOwnDataset(t *testing.T) {
	model := newStubModel(t, proposeStatusDomain())
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()
	run := ts.startRun(map[string]any{"dataset_id": datasetID, "agent": true})
	p := proposalsOf(ts, run.ID)[0]

	var other struct {
		ID string `json:"id"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/v1/datasets", map[string]any{
		"path": "../../testdata/dirty-logistics",
	}), http.StatusCreated, &other)

	resp := ts.do(http.MethodPost, "/api/v1/datasets/"+other.ID+"/rules", map[string]any{
		"run_id": run.ID, "proposal_id": p.ID,
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("accepting another dataset's proposal returned %d", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "different dataset") {
		t.Errorf("the refusal does not say why: %s", resp.Body)
	}
}

// Two rules of the same name in one file is a file that will not load, and a
// dataset whose rules do not load is a dataset that can no longer be audited.
func TestARuleNameCannotBeAcceptedTwice(t *testing.T) {
	model := newStubModel(t, proposeStatusDomain())
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()
	run := ts.startRun(map[string]any{"dataset_id": datasetID, "agent": true})
	p := proposalsOf(ts, run.ID)[0]

	accept := map[string]any{"run_id": run.ID, "proposal_id": p.ID}
	ts.decode(ts.do(http.MethodPost, "/api/v1/datasets/"+datasetID+"/rules", accept),
		http.StatusCreated, &struct{}{})

	resp := ts.do(http.MethodPost, "/api/v1/datasets/"+datasetID+"/rules", accept)
	if resp.Status != http.StatusConflict {
		t.Fatalf("accepting the same rule twice returned %d", resp.Status)
	}

	// Renamed, it is a different rule and goes in beside the first.
	renamed := map[string]any{"run_id": run.ID, "proposal_id": p.ID, "id": "status_domain_strict"}
	var second struct {
		Count int `json:"rules_in_force"`
	}
	ts.decode(ts.do(http.MethodPost, "/api/v1/datasets/"+datasetID+"/rules", renamed),
		http.StatusCreated, &second)
	if second.Count != 2 {
		t.Errorf("rules in force = %d, want 2", second.Count)
	}
}

// A run with no model proposed nothing, and says so with an empty list rather
// than a 404 a client has to special-case.
func TestARunWithoutAModelHasNoProposals(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()
	run := ts.startRun(map[string]any{"dataset_id": datasetID})

	if got := proposalsOf(ts, run.ID); len(got) != 0 {
		t.Errorf("a deterministic run proposed %d rules", len(got))
	}
	if resp := ts.get("/api/v1/runs/" + run.ID + "/proposals/nope"); resp.Status != http.StatusNotFound {
		t.Errorf("an unknown proposal returned %d", resp.Status)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
