package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/report"
	"github.com/russellw/veritix/internal/rules"
	"github.com/russellw/veritix/internal/runs"
	"github.com/russellw/veritix/internal/store"
)

// The rule-proposal endpoints: what the agent suggested, and how a person
// accepts one.
//
// Accepting is the point of the whole feature. A defect the model found on one
// run is found on every run once the rule that catches it is in force, and
// from then on it costs no model and no tokens. Nothing here is automatic: an
// accepted rule raises errors on future data and can fail a build, which is
// not a thing a model gets to do unattended.
//
// One proposal at a time is the rule for the values. A proposed one_of rule
// permits a list materialized from the customer's own column, so it is cell
// values, and this is the second place in Veritix where those cross the wire —
// the per-finding rows endpoint being the first. Both are bounded the same
// way: it takes a deliberate request for one named thing, the values never
// appear in a list response, and nothing about them is logged. Showing
// somebody what they are about to bless is the whole of the review step; an
// accept screen that cannot show the list is theater.

// handleListProposals lists what a run proposed, described rather than
// reproduced: the same rendering the report carries, and no values.
func (s *Server) handleListProposals(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runId")
	if _, err := s.store.Run(r.Context(), runID); err != nil {
		s.writeStoreError(w, err, "could not read the run")
		return
	}

	stored, err := s.store.Proposals(r.Context(), runID)
	if err != nil {
		s.writeStoreError(w, err, "could not read the proposals")
		return
	}

	out := make([]report.ProposalInfo, 0, len(stored))
	for _, sp := range stored {
		p, err := decodeProposal(sp)
		if err != nil {
			s.log.Error("could not decode a stored proposal",
				"run", runID, "proposal", sp.ID, "error", err)
			continue
		}
		out = append(out, report.DescribeProposal(p, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": out})
}

// handleGetProposal serves one proposal in full, including the values it would
// permit.
//
// This is the deliberate exception, and it is deliberately per-proposal: an
// automated caller walking every proposal of every run is a different thing
// from a person opening the one they are deciding about, which is the same
// reason the rows endpoint is shaped the way it is.
func (s *Server) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runId")
	if _, err := s.store.Run(r.Context(), runID); err != nil {
		s.writeStoreError(w, err, "could not read the run")
		return
	}

	sp, err := s.store.Proposal(r.Context(), runID, r.PathValue("proposalId"))
	if err != nil {
		s.writeStoreError(w, err, "could not read the proposal")
		return
	}
	p, err := decodeProposal(*sp)
	if err != nil {
		s.log.Error("could not decode a stored proposal",
			"run", runID, "proposal", sp.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the proposal")
		return
	}

	// The values are the reason this endpoint exists, so they are here and the
	// note says what they are: a description of what the column held, mistakes
	// included, not a vocabulary anybody chose.
	body := map[string]any{
		"proposal": report.DescribeProposal(p, true),
		"yaml":     renderOne(p),
	}
	if len(p.Rule.Values) > 0 {
		body["values_note"] = "These are the values the column held when the audit ran, " +
			"not a vocabulary anybody chose. Remove anything that is a mistake rather " +
			"than a category before accepting."
	}
	writeJSON(w, http.StatusOK, body)
}

// acceptRequest is what the accept screen sends back.
//
// The edits are the point. A vocabulary materialized from the data contains
// whatever the data contains — on the fixture that includes a misspelled
// status — so accepting one unread would bless the typo forever. Values,
// severity and the description are all the reviewer's to change.
type acceptRequest struct {
	RunID      string `json:"run_id"`
	ProposalID string `json:"proposal_id"`

	// ID renames the rule. It has to be unique among the rules in force, and
	// it is what a future finding is reported under.
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Severity    string   `json:"severity"`
	Values      []string `json:"values"`
}

// handleAcceptProposal writes an accepted rule into the dataset's own rules
// file, which every later audit of that dataset loads.
func (s *Server) handleAcceptProposal(w http.ResponseWriter, r *http.Request) {
	datasetID := r.PathValue("datasetId")

	var req acceptRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	if req.RunID == "" || req.ProposalID == "" {
		writeError(w, http.StatusBadRequest, "run_id and proposal_id are required")
		return
	}

	if _, err := s.store.Dataset(r.Context(), datasetID); err != nil {
		s.writeStoreError(w, err, "could not read the dataset")
		return
	}

	// The proposal has to have been made about this dataset. Without the
	// check, a rule measured against one customer's data could be installed
	// against another's, where nothing has ever run it.
	run, err := s.store.Run(r.Context(), req.RunID)
	if err != nil {
		s.writeStoreError(w, err, "could not read the run")
		return
	}
	if run.DatasetID != datasetID {
		writeError(w, http.StatusBadRequest,
			"run %s audited a different dataset, so its proposals are not about this one", req.RunID)
		return
	}

	sp, err := s.store.Proposal(r.Context(), req.RunID, req.ProposalID)
	if err != nil {
		s.writeStoreError(w, err, "could not read the proposal")
		return
	}
	p, err := decodeProposal(*sp)
	if err != nil {
		s.log.Error("could not decode a stored proposal",
			"run", req.RunID, "proposal", sp.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the proposal")
		return
	}

	rule := p.Rule
	if req.ID != "" {
		rule.ID = req.ID
	}
	if req.Description != "" {
		rule.Description = req.Description
	}
	if req.Severity != "" {
		severity, err := finding.ParseSeverity(req.Severity)
		if err != nil {
			writeError(w, http.StatusBadRequest, "%s", err)
			return
		}
		rule.Severity = &severity
	}
	if req.Values != nil {
		if rule.Expect != rules.ExpectOneOf {
			writeError(w, http.StatusBadRequest,
				"values apply to a one_of rule; this one expects %s", rule.Expect)
			return
		}
		rule.Values = req.Values
	}

	accepted, err := s.acceptRule(datasetID, rule)
	if err != nil {
		var conflict *ruleConflict
		if errors.As(err, &conflict) {
			writeError(w, http.StatusConflict, "%s", conflict)
			return
		}
		s.log.Error("could not accept the rule", "dataset", datasetID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not accept the rule")
		return
	}

	// Logged without the values: an accepted rule is a decision worth
	// recording, and its permitted set is still cell values.
	s.log.Info("accepted a proposed rule",
		"dataset", datasetID, "run", req.RunID, "rule", rule.ID,
		"expect", string(rule.Expect), "rules_in_force", accepted)

	writeJSON(w, http.StatusCreated, map[string]any{
		"rule":           report.DescribeProposal(rules.Proposal{Rule: rule, Display: p.Display}, false),
		"rules_in_force": accepted,
	})
}

// handleGetDatasetRules lists the rules in force for a dataset.
//
// Described, not reproduced, like everything else that lists rules: an
// accepted one_of rule's permitted set is still the customer's data.
func (s *Server) handleGetDatasetRules(w http.ResponseWriter, r *http.Request) {
	datasetID := r.PathValue("datasetId")
	if _, err := s.store.Dataset(r.Context(), datasetID); err != nil {
		s.writeStoreError(w, err, "could not read the dataset")
		return
	}

	file, err := runs.AcceptedRules(s.cfg.Server.DataDir, datasetID)
	if err != nil {
		s.log.Error("could not read the accepted rules", "dataset", datasetID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the rules in force")
		return
	}

	out := make([]report.ProposalInfo, 0)
	if file != nil {
		for _, rule := range file.Rules {
			out = append(out, report.DescribeProposal(rules.Proposal{Rule: rule}, false))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

// ruleConflict is an accept that would redefine a rule already in force.
type ruleConflict struct{ id string }

func (e *ruleConflict) Error() string {
	return fmt.Sprintf("a rule called %q is already in force for this dataset; "+
		"rename this one or remove the existing rule", e.id)
}

// acceptRule appends a rule to the dataset's file and returns how many are
// then in force.
//
// The file is read, extended, validated and replaced under a lock, because two
// accept requests arriving together would otherwise each write the file they
// read and one of the rules would vanish. It is validated before it is written
// and loaded again after, so a file that would fail the next audit fails the
// request that created it instead.
func (s *Server) acceptRule(datasetID string, rule rules.Rule) (int, error) {
	s.rulesMu.Lock()
	defer s.rulesMu.Unlock()

	path, err := runs.DatasetRulesPath(s.cfg.Server.DataDir, datasetID)
	if err != nil {
		return 0, err
	}

	file, err := runs.AcceptedRules(s.cfg.Server.DataDir, datasetID)
	if err != nil {
		return 0, err
	}
	if file == nil {
		file = &rules.File{Version: 1}
	}
	for _, existing := range file.Rules {
		if strings.EqualFold(existing.ID, rule.ID) {
			return 0, &ruleConflict{id: rule.ID}
		}
	}
	file.Rules = append(file.Rules, rule)
	if err := file.Validate(); err != nil {
		return 0, fmt.Errorf("the rule cannot be applied: %w", err)
	}

	var body strings.Builder
	header := "Rules accepted for this dataset in the Veritix interface. Every audit of\n" +
		"this dataset applies them. Edit or delete them here; Veritix only appends."
	proposals := make([]rules.Proposal, 0, len(file.Rules))
	for _, r := range file.Rules {
		proposals = append(proposals, rules.Proposal{Rule: r})
	}
	if err := rules.RenderProposals(&body, proposals, header); err != nil {
		return 0, err
	}

	// Written beside the target and renamed over it: a crash halfway through
	// must not leave a dataset with a rules file that no longer parses, since
	// every later audit of it would then fail to start.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body.String()), 0o600); err != nil { //nolint:gosec // server-generated path
		return 0, fmt.Errorf("could not write the rules: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil { //nolint:gosec // path is DataDir + a store-generated id, checked by runs.DatasetRulesPath
		_ = os.Remove(tmp) //nolint:gosec // the same path with a suffix
		return 0, fmt.Errorf("could not write the rules: %w", err)
	}

	// Read back through the same path an audit uses. A rule that writes but
	// does not load is a dataset that can no longer be audited.
	reloaded, err := rules.Load(path)
	if err != nil {
		return 0, fmt.Errorf("the rules were written but do not load: %w", err)
	}
	return len(reloaded.Rules), nil
}

// renderOne renders a single proposal as the YAML a person would paste into
// their own rules file.
func renderOne(p rules.Proposal) string {
	var b strings.Builder
	if err := rules.RenderProposals(&b, []rules.Proposal{p}, ""); err != nil {
		return ""
	}
	return b.String()
}

func decodeProposal(sp store.Proposal) (rules.Proposal, error) {
	var p rules.Proposal
	if err := json.Unmarshal(sp.Document, &p); err != nil {
		return p, fmt.Errorf("proposal %s: %w", sp.ID, err)
	}
	return p, nil
}
