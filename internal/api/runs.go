package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/russellwallace/veritix/internal/agent"
	"github.com/russellwallace/veritix/internal/audit"
	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/profile"
	"github.com/russellwallace/veritix/internal/report"
	"github.com/russellwallace/veritix/internal/rules"
	"github.com/russellwallace/veritix/internal/store"
)

// allRuns is the ceiling used where every run of a dataset is wanted rather
// than a page of them.
const allRuns = 100_000

// runJSON is a run on the wire.
type runJSON struct {
	ID         string        `json:"id"`
	DatasetID  string        `json:"dataset_id"`
	Status     string        `json:"status"`
	Message    string        `json:"message,omitempty"`
	Version    string        `json:"version,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	StartedAt  *time.Time    `json:"started_at,omitempty"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
	DurationMS int64         `json:"duration_ms"`
	Findings   findingCounts `json:"findings"`
}

type findingCounts struct {
	Total    int `json:"total"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
}

func toRunJSON(r *store.Run) *runJSON {
	out := &runJSON{
		ID: r.ID, DatasetID: r.DatasetID, Status: string(r.Status),
		Message: r.Message, Version: r.Version,
		CreatedAt: r.CreatedAt, DurationMS: r.Duration.Milliseconds(),
		Findings: findingCounts{
			Total: r.Total(), Errors: r.Errors, Warnings: r.Warnings, Info: r.Infos,
		},
	}
	if !r.StartedAt.IsZero() {
		out.StartedAt = &r.StartedAt
	}
	if !r.FinishedAt.IsZero() {
		out.FinishedAt = &r.FinishedAt
	}
	return out
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be a number between 1 and 500")
			return
		}
		limit = n
	}

	all, err := s.store.Runs(r.Context(), r.URL.Query().Get("dataset_id"), limit)
	if err != nil {
		s.writeStoreError(w, err, "could not list runs")
		return
	}

	out := make([]*runJSON, 0, len(all))
	for _, run := range all {
		out = append(out, toRunJSON(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.Run(r.Context(), r.PathValue("runId"))
	if err != nil {
		s.writeStoreError(w, err, "could not read the run")
		return
	}
	writeJSON(w, http.StatusOK, toRunJSON(run))
}

type createRunRequest struct {
	DatasetID string `json:"dataset_id"`
	// IncludeValues permits verbatim cell values in this run's report. It is
	// per-run rather than per-server: the decision belongs to whoever is about
	// to send the report somewhere, at the moment they produce it.
	IncludeValues bool   `json:"include_values"`
	TopValues     *int   `json:"top_values"`
	Rules         string `json:"rules"`
	// Agent runs the model-driven investigation for this run. It is per-run
	// and defaults off even when a provider is configured: sending a dataset's
	// metadata to a model is a decision somebody should take deliberately, not
	// one they inherit from a config file they set up months ago.
	Agent bool `json:"agent"`
	// AllowSampleValues lifts the egress policy for this run alone.
	AllowSampleValues bool `json:"allow_sample_values"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var req createRunRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	if req.DatasetID == "" {
		writeError(w, http.StatusBadRequest, "dataset_id is required")
		return
	}

	topValues := 10
	if req.TopValues != nil {
		if *req.TopValues < 0 || *req.TopValues > 100 {
			writeError(w, http.StatusBadRequest, "top_values must be between 0 and 100")
			return
		}
		topValues = *req.TopValues
	}

	ds, err := s.store.Dataset(r.Context(), req.DatasetID)
	if err != nil {
		s.writeStoreError(w, err, "could not read the dataset")
		return
	}

	// Rules are loaded now rather than inside the run, so that a typo in a
	// rule file fails the request the operator is watching instead of a
	// background run they will have to go and read the history to find.
	var ruleFile *rules.File
	if req.Rules != "" {
		if ruleFile, err = rules.Load(req.Rules); err != nil {
			writeError(w, http.StatusBadRequest, "could not read the rules: %s", err)
			return
		}
	}

	// The agent is configured before the run is created, so that a
	// misconfigured provider fails the request the operator is watching rather
	// than a background run they have to go and find in the history.
	var agentOpts *agent.Options
	if req.Agent {
		if s.cfg.LLM.Provider == "" || s.cfg.LLM.Provider == config.ProviderNone {
			writeError(w, http.StatusBadRequest,
				"no model is configured; set llm.provider to anthropic or openai-compatible")
			return
		}
		cfg := s.cfg.LLM
		if req.AllowSampleValues {
			cfg.AllowSampleValues = true
		}
		if agentOpts, err = agent.Configure(cfg); err != nil {
			writeError(w, http.StatusBadRequest, "the model is not configured correctly: %s", err)
			return
		}
		agentOpts.MaxRows = s.cfg.Engine.MaxResultRows
	}

	run, err := s.store.CreateRun(r.Context(), ds.ID, s.version, "")
	if err != nil {
		s.writeStoreError(w, err, "could not create the run")
		return
	}

	dbPath, err := s.runDatabasePath(run.ID)
	if err != nil {
		s.log.Error("could not prepare the run directory", "run", run.ID, "error", err)
		_ = s.store.StopRun(r.Context(), run.ID, store.StatusFailed, err.Error())
		writeError(w, http.StatusInternalServerError, "could not prepare the run")
		return
	}
	if err := s.store.SetRunDatabase(r.Context(), run.ID, dbPath); err != nil {
		s.writeStoreError(w, err, "could not create the run")
		return
	}
	run.DatabasePath = dbPath

	s.runs.start(run,
		audit.Options{
			Paths:        []string{ds.Path},
			Engine:       s.cfg.Engine,
			DatabasePath: dbPath,
			Rules:        ruleFile,
			Agent:        agentOpts,
			Profile:      profile.Options{TopValues: topValues},
		},
		report.Options{IncludeValues: req.IncludeValues},
	)

	writeJSON(w, http.StatusAccepted, toRunJSON(run))
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")

	run, err := s.store.Run(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err, "could not read the run")
		return
	}
	if run.Status.Terminal() {
		writeError(w, http.StatusConflict, "run %s has already finished (%s)", id, run.Status)
		return
	}
	if !s.runs.cancel(id) {
		// Recorded as in flight but not executing here: the process that owned
		// it is gone. Close it out rather than leave it running forever.
		if err := s.store.StopRun(r.Context(), id, store.StatusFailed,
			"interrupted: no process is running this audit"); err != nil {
			s.writeStoreError(w, err, "could not stop the run")
			return
		}
	}

	// Read it back rather than reporting the status optimistically: the run
	// unwinds asynchronously and the caller should see where it actually got to.
	run, err = s.store.Run(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err, "could not read the run")
		return
	}
	writeJSON(w, http.StatusAccepted, toRunJSON(run))
}

func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	doc, err := s.store.Document(r.Context(), r.PathValue("runId"))
	if err != nil {
		s.reportNotAvailable(w, r, err)
		return
	}

	// The stored document is served verbatim. Re-encoding it here would be a
	// second chance for the API and the JSON report to disagree.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(doc) //nolint:gosec // JSON this server encoded, served as JSON
}

// handleGetTrace serves the record of what a model was sent and what it
// answered.
//
// This is the endpoint that makes the egress promise checkable rather than
// merely stated: a customer can read every payload that left the process,
// verbatim, instead of taking Veritix's word for what was in it. It is served
// like the report — stored bytes, written straight out — because re-encoding
// it would be a chance for the served trace and the recorded one to differ.
func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")

	doc, err := s.store.Trace(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Distinguish "no such run" from "that run had no model", because
			// they mean very different things to whoever is asking.
			if _, runErr := s.store.Run(r.Context(), id); runErr == nil {
				writeError(w, http.StatusNotFound,
					"run %s was audited without a model, so there is no trace", id)
				return
			}
		}
		s.writeStoreError(w, err, "could not read the trace")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(doc) //nolint:gosec // JSON this server encoded, served as JSON
}

func (s *Server) handleGetReportHTML(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")

	body, err := s.store.Document(r.Context(), id)
	if err != nil {
		s.reportNotAvailable(w, r, err)
		return
	}

	var doc report.Document
	if err := json.Unmarshal(body, &doc); err != nil {
		s.log.Error("could not decode the stored report", "run", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not render the report")
		return
	}

	// Rendered from the stored document, so the page carries exactly the
	// redaction the run was asked for and matches the JSON byte for byte in
	// what it discloses.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="veritix-report-`+id+`.html"`)
	if err := report.RenderHTML(w, &doc); err != nil {
		// The response is already committed, so this can only be logged.
		s.log.Error("could not render the report", "run", id, "error", err)
	}
}

// reportNotAvailable distinguishes "no such run" from "that run has no report
// yet", because the second is a normal state a UI polls through and the first
// is a mistake.
func (s *Server) reportNotAvailable(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, store.ErrNotFound) {
		s.writeStoreError(w, err, "could not read the report")
		return
	}

	id := r.PathValue("runId")
	run, runErr := s.store.Run(r.Context(), id)
	if runErr != nil {
		writeError(w, http.StatusNotFound, "no run %s", id)
		return
	}
	writeError(w, http.StatusNotFound,
		"run %s has no report: it is %s", id, run.Status)
}
