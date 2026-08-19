package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/report"
	"github.com/russellw/veritix/internal/runs"
	"github.com/russellw/veritix/internal/store"
)

// maxFindings caps what audit_dataset and list_findings return in one call. A
// dataset in genuinely bad shape can produce hundreds, and a caller that asked
// "audit this" wants the report rather than a context window full of it. The
// count omitted is reported so the caller knows to page rather than assuming
// it has seen everything.
const maxFindings = 50

// datasetOut is a dataset on the wire.
type datasetOut struct {
	ID        string    `json:"id" jsonschema:"the dataset's id, for use with audit_dataset"`
	Name      string    `json:"name" jsonschema:"a human-readable name"`
	Path      string    `json:"path" jsonschema:"where the dataset lives on this machine"`
	Uploaded  bool      `json:"uploaded" jsonschema:"true if the files were uploaded to this instance rather than registered in place"`
	CreatedAt time.Time `json:"created_at" jsonschema:"when the dataset was registered"`
}

func toDatasetOut(d *store.Dataset) datasetOut {
	return datasetOut{
		ID: d.ID, Name: d.Name, Path: d.Path,
		Uploaded: d.Uploaded, CreatedAt: d.CreatedAt,
	}
}

// runOut is a run on the wire.
type runOut struct {
	ID         string                `json:"id" jsonschema:"the run's id"`
	DatasetID  string                `json:"dataset_id" jsonschema:"the dataset that was audited"`
	Status     string                `json:"status" jsonschema:"pending, running, succeeded, failed, or canceled"`
	Message    string                `json:"message,omitempty" jsonschema:"why the run did not succeed, when it did not"`
	CreatedAt  time.Time             `json:"created_at"`
	DurationMS int64                 `json:"duration_ms" jsonschema:"how long the audit took"`
	Findings   report.FindingSummary `json:"finding_summary" jsonschema:"how many problems were found, by severity"`
}

func toRunOut(r *store.Run) runOut {
	return runOut{
		ID: r.ID, DatasetID: r.DatasetID, Status: string(r.Status), Message: r.Message,
		CreatedAt: r.CreatedAt, DurationMS: r.Duration.Milliseconds(),
		Findings: report.FindingSummary{
			Total: r.Total(), Errors: r.Errors, Warnings: r.Warnings, Info: r.Infos,
		},
	}
}

// --- list_datasets ---

type listDatasetsIn struct{}

type listDatasetsOut struct {
	Datasets []datasetOut `json:"datasets"`
}

func (s *Server) listDatasets(ctx context.Context, _ *sdk.CallToolRequest, _ listDatasetsIn) (*sdk.CallToolResult, listDatasetsOut, error) {
	all, err := s.opts.Store.Datasets(ctx)
	if err != nil {
		return nil, listDatasetsOut{}, fmt.Errorf("could not list datasets: %w", err)
	}
	out := listDatasetsOut{Datasets: make([]datasetOut, 0, len(all))}
	for _, d := range all {
		out.Datasets = append(out.Datasets, toDatasetOut(d))
	}
	return nil, out, nil
}

// --- register_dataset ---

type registerDatasetIn struct {
	Path string `json:"path" jsonschema:"a directory of data files, or a single CSV or Excel file, on the machine running Veritix"`
	Name string `json:"name,omitempty" jsonschema:"what to call it; defaults to the name of the directory or file"`
}

func (s *Server) registerDataset(ctx context.Context, _ *sdk.CallToolRequest, in registerDatasetIn) (*sdk.CallToolResult, datasetOut, error) {
	d, err := s.dataset(ctx, in.Path, in.Name)
	if err != nil {
		return nil, datasetOut{}, err
	}
	return nil, toDatasetOut(d), nil
}

// dataset registers a path, or returns the record a previous registration
// made. Which path this server may read is the operator's decision — it is
// single-tenant and runs with their privileges — so the path is checked for
// existence and not confined to a root.
func (s *Server) dataset(ctx context.Context, path, name string) (*store.Dataset, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("path is required: give a directory or file on the machine running Veritix")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("could not resolve %q: %w", path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", abs, err)
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	d, err := s.opts.Store.CreateDataset(ctx, name, abs, false)
	if err != nil {
		return nil, fmt.Errorf("could not register the dataset: %w", err)
	}
	return d, nil
}

// --- audit_dataset ---

type auditIn struct {
	Path      string `json:"path,omitempty" jsonschema:"a directory of data files, or a single CSV or Excel file, on the machine running Veritix; give this or dataset_id"`
	DatasetID string `json:"dataset_id,omitempty" jsonschema:"the id of a dataset already registered here; give this or path"`
	Name      string `json:"name,omitempty" jsonschema:"what to call the dataset, when registering it by path for the first time"`
}

type auditOut struct {
	Run      runOut               `json:"run" jsonschema:"the recorded audit"`
	Dataset  datasetOut           `json:"dataset" jsonschema:"what was audited"`
	Findings []report.FindingInfo `json:"findings" jsonschema:"the problems found, most severe first"`
	Omitted  int                  `json:"findings_omitted,omitempty" jsonschema:"findings not included here; read them with list_findings or get_report"`
	Note     string               `json:"note,omitempty" jsonschema:"anything the caller should know about this result"`
}

func (s *Server) auditDataset(ctx context.Context, _ *sdk.CallToolRequest, in auditIn) (*sdk.CallToolResult, auditOut, error) {
	if (in.Path == "") == (in.DatasetID == "") {
		return nil, auditOut{}, errors.New("give either path or dataset_id, not both and not neither")
	}

	var (
		ds  *store.Dataset
		err error
	)
	if in.DatasetID != "" {
		if ds, err = s.opts.Store.Dataset(ctx, in.DatasetID); err != nil {
			return nil, auditOut{}, storeErr(err, "dataset", in.DatasetID)
		}
	} else if ds, err = s.dataset(ctx, in.Path, in.Name); err != nil {
		return nil, auditOut{}, err
	}

	agentOpts, err := agentOptions(s.opts)
	if err != nil {
		return nil, auditOut{}, err
	}

	run, err := s.opts.Store.CreateRun(ctx, ds.ID, s.opts.Version, "")
	if err != nil {
		return nil, auditOut{}, fmt.Errorf("could not create the run: %w", err)
	}

	// The dataset's DuckDB file outlives the run, as it does over HTTP, so
	// that the web interface can show a finding's offending rows afterwards.
	// The same history serves both.
	dbPath, err := runs.DatabasePath(s.opts.Config.Server.DataDir, run.ID)
	if err != nil {
		_ = s.opts.Store.StopRun(ctx, run.ID, store.StatusFailed, err.Error())
		return nil, auditOut{}, err
	}
	if err := s.opts.Store.SetRunDatabase(ctx, run.ID, dbPath); err != nil {
		return nil, auditOut{}, fmt.Errorf("could not create the run: %w", err)
	}

	s.log.Info("auditing", "run", run.ID, "dataset", ds.ID, "path", ds.Path)

	// Synchronous, on the caller's context: an assistant asked a question and
	// is waiting for the answer, and a tool call that returned an id to poll
	// would spend the caller's turns on bookkeeping. Cancellation follows the
	// call, so a client that gives up stops the audit.
	runErr := runs.Execute(ctx, runs.Options{
		Store:   s.opts.Store,
		RunID:   run.ID,
		Version: s.opts.Version,
		Log:     s.log,
		Audit: audit.Options{
			Paths:        []string{ds.Path},
			Engine:       s.opts.Config.Engine,
			DatabasePath: dbPath,
			Rules:        s.opts.Rules,
			Agent:        agentOpts,
			Profile:      profile.Options{TopValues: s.opts.TopValues},
		},
		Report: report.Options{IncludeValues: s.opts.IncludeValues},
	})

	// The run is read back rather than reported from what was just done: the
	// store is the authority on how it ended, and it is what every other tool
	// here will show for the same id.
	finished, err := s.opts.Store.Run(context.WithoutCancel(ctx), run.ID)
	if err != nil {
		return nil, auditOut{}, fmt.Errorf("could not read the finished run: %w", err)
	}

	out := auditOut{Run: toRunOut(finished), Dataset: toDatasetOut(ds)}
	if runErr != nil {
		// Reported as a tool error so the caller sees it as something that
		// went wrong, with the run id so it can still be looked up.
		return nil, out, fmt.Errorf("the audit did not finish (run %s, %s): %w",
			run.ID, finished.Status, runErr)
	}

	doc, err := s.document(context.WithoutCancel(ctx), run.ID)
	if err != nil {
		return nil, out, err
	}
	out.Findings, out.Omitted = trim(doc.Findings)
	out.Note = summarize(doc, out.Omitted)
	return nil, out, nil
}

// --- list_runs ---

type listRunsIn struct {
	DatasetID string `json:"dataset_id,omitempty" jsonschema:"only runs of this dataset"`
	Limit     int    `json:"limit,omitempty" jsonschema:"how many to return, 1 to 500; defaults to 20"`
}

type listRunsOut struct {
	Runs []runOut `json:"runs"`
}

func (s *Server) listRuns(ctx context.Context, _ *sdk.CallToolRequest, in listRunsIn) (*sdk.CallToolResult, listRunsOut, error) {
	limit := in.Limit
	switch {
	case limit == 0:
		limit = 20
	case limit < 1 || limit > 500:
		return nil, listRunsOut{}, errors.New("limit must be between 1 and 500")
	}

	all, err := s.opts.Store.Runs(ctx, in.DatasetID, limit)
	if err != nil {
		return nil, listRunsOut{}, fmt.Errorf("could not list runs: %w", err)
	}
	out := listRunsOut{Runs: make([]runOut, 0, len(all))}
	for _, r := range all {
		out.Runs = append(out.Runs, toRunOut(r))
	}
	return nil, out, nil
}

// --- get_run ---

type runIn struct {
	RunID string `json:"run_id" jsonschema:"the id of an audit, as returned by audit_dataset or list_runs"`
}

func (s *Server) getRun(ctx context.Context, _ *sdk.CallToolRequest, in runIn) (*sdk.CallToolResult, runOut, error) {
	r, err := s.opts.Store.Run(ctx, in.RunID)
	if err != nil {
		return nil, runOut{}, storeErr(err, "run", in.RunID)
	}
	return nil, toRunOut(r), nil
}

// --- list_findings ---

type listFindingsIn struct {
	RunID    string `json:"run_id" jsonschema:"the id of an audit"`
	Severity string `json:"severity,omitempty" jsonschema:"only findings of this severity: error, warning, or info"`
	Offset   int    `json:"offset,omitempty" jsonschema:"skip this many, for reading past the first page"`
}

type listFindingsOut struct {
	Findings []report.FindingInfo  `json:"findings" jsonschema:"the problems found, most severe first"`
	Summary  report.FindingSummary `json:"finding_summary" jsonschema:"how many the run found in total, by severity"`
	Omitted  int                   `json:"findings_omitted,omitempty" jsonschema:"how many more match; raise offset to read them"`
}

func (s *Server) listFindings(ctx context.Context, _ *sdk.CallToolRequest, in listFindingsIn) (*sdk.CallToolResult, listFindingsOut, error) {
	doc, err := s.document(ctx, in.RunID)
	if err != nil {
		return nil, listFindingsOut{}, err
	}

	matching := doc.Findings
	if in.Severity != "" {
		want := strings.ToLower(strings.TrimSpace(in.Severity))
		switch want {
		case "error", "warning", "info":
		default:
			return nil, listFindingsOut{}, fmt.Errorf("severity must be error, warning, or info, not %q", in.Severity)
		}
		matching = nil
		for _, f := range doc.Findings {
			if f.Severity == want {
				matching = append(matching, f)
			}
		}
	}

	if in.Offset < 0 {
		return nil, listFindingsOut{}, errors.New("offset cannot be negative")
	}
	if in.Offset > len(matching) {
		matching = nil
	} else {
		matching = matching[in.Offset:]
	}

	page, omitted := trim(matching)
	return nil, listFindingsOut{
		Findings: page, Summary: doc.FindingSummary, Omitted: omitted,
	}, nil
}

// --- get_report ---

func (s *Server) getReport(ctx context.Context, _ *sdk.CallToolRequest, in runIn) (*sdk.CallToolResult, *report.Document, error) {
	doc, err := s.document(ctx, in.RunID)
	if err != nil {
		return nil, nil, err
	}
	return nil, doc, nil
}

// --- shared ---

// document reads back the stored report.
//
// It is decoded from the blob the run recorded rather than rebuilt, for the
// same reason the HTTP API serves those bytes verbatim: one document, built
// once, so that what an assistant is told and what the web interface displays
// cannot drift apart.
func (s *Server) document(ctx context.Context, runID string) (*report.Document, error) {
	raw, err := s.opts.Store.Document(ctx, runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Distinguish the two, because they call for different next steps:
			// a run that failed is not going to acquire a report by waiting.
			if r, rerr := s.opts.Store.Run(ctx, runID); rerr == nil {
				return nil, fmt.Errorf("run %s has no report: it is %s", runID, r.Status)
			}
		}
		return nil, storeErr(err, "run", runID)
	}

	var doc report.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("the stored report for run %s could not be read: %w", runID, err)
	}
	return &doc, nil
}

// trim caps a page of findings and reports how many it left behind.
func trim(all []report.FindingInfo) ([]report.FindingInfo, int) {
	if len(all) <= maxFindings {
		return all, 0
	}
	return all[:maxFindings], len(all) - maxFindings
}

// summarize is the sentence an assistant reads first. It says what the numbers
// mean rather than repeating them: that a clean audit is a real result, and
// where the rest of a long list is.
func summarize(doc *report.Document, omitted int) string {
	var parts []string
	if doc.FindingSummary.Total == 0 {
		parts = append(parts, "Every check ran and none of them found a problem.")
	}
	if omitted > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d further findings are not shown; read them with list_findings and an offset, or get_report.",
			omitted))
	}
	if doc.Agent != nil {
		parts = append(parts, fmt.Sprintf(
			"An agentic pass ran on this instance (%s %s) and contributed %d of the findings.",
			doc.Agent.Provider, doc.Agent.Model, doc.Agent.Findings))
	}
	if !doc.Redacted.ValuesIncluded {
		parts = append(parts, "Cell values are withheld: a column is described by the shape its contents take, such as XXX-999999.")
	}
	return strings.Join(parts, " ")
}

// agentOptions configures Veritix's own agentic pass, or returns nil when the
// operator has not asked for one.
func agentOptions(opts Options) (*agent.Options, error) {
	if !opts.Agent {
		return nil, nil
	}
	if opts.Config.LLM.Provider == "" || opts.Config.LLM.Provider == config.ProviderNone {
		return nil, errors.New(
			"the agentic pass is enabled but no model is configured; set llm.provider to anthropic or openai-compatible")
	}
	a, err := agent.Configure(opts.Config.LLM)
	if err != nil {
		return nil, fmt.Errorf("the model is not configured correctly: %w", err)
	}
	a.MaxRows = opts.Config.Engine.MaxResultRows
	return a, nil
}

// storeErr turns a lookup failure into something a caller can act on: a
// missing id is a mistake it can correct, anything else is this server's
// problem and says so.
func storeErr(err error, what, id string) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("there is no %s with id %q", what, id)
	}
	return fmt.Errorf("could not read the %s: %w", what, err)
}
