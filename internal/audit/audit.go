// Package audit runs the whole pipeline: discover the files, load them,
// profile them, and (from M2) check them.
//
// It exists so that the CLI, the HTTP API, and the MCP server all drive
// exactly the same sequence. Three entry points that each assemble the
// pipeline slightly differently is how a tool ends up reporting different
// results depending on how it was invoked.
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/russellwallace/veritix/internal/agent"
	"github.com/russellwallace/veritix/internal/checks"
	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/ingest"
	"github.com/russellwallace/veritix/internal/profile"
	"github.com/russellwallace/veritix/internal/rules"
	"github.com/russellwallace/veritix/internal/source"
)

// Options controls a run.
type Options struct {
	// Paths are the files and directories to audit as one dataset.
	Paths []string
	// Engine configures the DuckDB instance and its resource limits.
	Engine config.Engine
	// Profile controls the depth of profiling.
	Profile profile.Options
	// DatabasePath, when set, keeps the loaded dataset in a DuckDB file
	// instead of memory, so a later run can query it without re-reading the
	// source files.
	DatabasePath string
	// Rules are the customer's own expectations, applied after the built-in
	// checks.
	Rules *rules.File
	// Agent, when set, runs the model-driven investigation after the
	// deterministic pass. Nil means no model is configured, which is the
	// default and a complete audit in its own right.
	Agent *agent.Options
}

// Result is everything a run produced.
type Result struct {
	// Dataset is what was discovered on disk.
	Dataset *source.Dataset
	// Loaded is how each file was read and what went wrong reading it.
	Loaded *ingest.Result
	// Profile is what the data actually contains.
	Profile *profile.Dataset
	// Findings are the problems found, most severe first.
	Findings *finding.Set
	// Trace records what the agent did, when one ran. Nil when no model was
	// configured.
	Trace *agent.Trace
	// StartedAt and Duration describe the run itself.
	StartedAt time.Time
	Duration  time.Duration

	engine *engine.Engine
}

// Engine exposes the engine the run used, so a caller can keep querying the
// loaded dataset. It is only valid until Close.
func (r *Result) Engine() *engine.Engine { return r.engine }

// Close releases the engine.
func (r *Result) Close() error {
	if r == nil || r.engine == nil {
		return nil
	}
	return r.engine.Close()
}

// Run executes the pipeline. The caller must Close the result.
func Run(ctx context.Context, opts Options, log *slog.Logger) (*Result, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	started := time.Now()

	ds, err := source.Discover(opts.Paths)
	if err != nil {
		return nil, err
	}
	log.Info("discovered dataset",
		"root", ds.Root, "files", len(ds.Files), "skipped", len(ds.Skipped))

	e, err := engine.Open(ctx, opts.DatabasePath, opts.Engine, log)
	if err != nil {
		return nil, err
	}

	// From here on the engine has to be released on every path out.
	res := &Result{Dataset: ds, StartedAt: started, engine: e}

	loaded, err := ingest.Load(ctx, e, ds, ingest.Options{}, log)
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	res.Loaded = loaded

	var rows int64
	for _, t := range loaded.Tables {
		rows += t.RowCount
	}
	log.Info("loaded dataset", "tables", len(loaded.Tables), "rows", rows)

	prof, err := profile.Run(ctx, e, loaded, opts.Profile, log)
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	res.Profile = prof

	found, err := checks.Run(ctx, e, prof, log)
	if err != nil {
		_ = e.Close()
		return nil, err
	}

	ruleFindings, err := rules.Evaluate(ctx, e, prof, opts.Rules, log)
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	found.AddAll(ruleFindings)

	// The agent runs last, over everything the deterministic pass established,
	// and before Verify — so its proposals are checked by exactly the same
	// mechanism as everything else rather than by a lenient path of their own.
	//
	// The engine is locked down first. From here on nothing needs to touch the
	// filesystem, and the SQL about to be executed was written by a language
	// model, so this is the moment to take the capability away.
	if opts.Agent != nil {
		if err := e.Lockdown(ctx); err != nil {
			_ = e.Close()
			return nil, err
		}

		agentRes, err := agent.Run(ctx, agent.Input{
			Engine:  e,
			Profile: prof,
			Known:   found.All(),
			Root:    ds.Root,
		}, *opts.Agent, log)
		if err != nil {
			_ = e.Close()
			return nil, err
		}
		found.AddAll(agentRes.Findings)
		res.Trace = agentRes.Trace
	}

	// Re-run every finding's evidence before reporting it. For the built-in
	// checks this is close to a tautology. It is not one for the agent's
	// findings, and the rule applies to all of them equally rather than being
	// switched on for the ones we already distrust: a check whose evidence
	// stopped reproducing is as much a defect as a model that made something
	// up, and neither should reach a customer's report.
	dropped, err := found.Verify(ctx, e)
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	for _, d := range dropped {
		log.Warn("dropped a finding that did not reproduce",
			"rule", d.Rule, "location", d.Location.String())
	}
	res.Findings = found

	res.Duration = time.Since(started)
	log.Info("audit complete", "duration", res.Duration.Round(time.Millisecond))

	return res, nil
}

// Summary is a short description of a run, used in reports and logs.
type Summary struct {
	Root      string
	Files     int
	Skipped   int
	Tables    int
	Columns   int
	Rows      int64
	Rejected  int64
	Duration  time.Duration
	StartedAt time.Time
}

// Summarize reduces a run to its headline numbers.
func (r *Result) Summarize() Summary {
	s := Summary{
		Root:      r.Dataset.Root,
		Files:     len(r.Dataset.Files),
		Skipped:   len(r.Dataset.Skipped),
		Tables:    len(r.Loaded.Tables),
		Duration:  r.Duration,
		StartedAt: r.StartedAt,
	}
	for _, t := range r.Loaded.Tables {
		s.Rows += t.RowCount
		s.Rejected += t.RejectCount
		s.Columns += len(t.Columns)
	}
	return s
}

// String renders a summary as one line.
func (s Summary) String() string {
	out := fmt.Sprintf("%d files, %d tables, %d columns, %d rows in %s",
		s.Files, s.Tables, s.Columns, s.Rows, s.Duration.Round(time.Millisecond))
	if s.Rejected > 0 {
		out += fmt.Sprintf(" (%d rows unreadable)", s.Rejected)
	}
	if s.Skipped > 0 {
		out += fmt.Sprintf(", %d files skipped", s.Skipped)
	}
	return out
}
