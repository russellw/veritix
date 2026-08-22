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
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/checks"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/ingest"
	"github.com/russellw/veritix/internal/mcpclient"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/rules"
	"github.com/russellw/veritix/internal/source"
	"github.com/russellw/veritix/internal/telemetry"
)

// Options controls a run.
type Options struct {
	// Paths are the files and directories to audit as one dataset.
	Paths []string
	// Engine configures the DuckDB instance and its resource limits.
	Engine config.Engine
	// Profile controls the depth of profiling.
	Profile profile.Options
	// DatabasePath, when set, keeps the loaded dataset in a DuckDB file that
	// outlives the run, so a later request can query it without re-reading the
	// source files. That is what the HTTP API and the MCP server want, since
	// the per-finding rows endpoint reopens it afterwards.
	//
	// Empty does not mean memory. A run with no path gets a DuckDB file in a
	// temporary directory that Result.Close removes, because measurement is
	// what an audit spends its time on and DuckDB's own storage is compressed
	// where its in-memory tables are not. Measured on a 400 MB dataset: 185s
	// and 1.07 GiB against 283s and 1.67 GiB — faster *and* smaller, so there
	// is no trade to offer the caller. See docs/scale.md.
	DatabasePath string
	// Rules are the customer's own expectations, applied after the built-in
	// checks.
	Rules *rules.File
	// Agent, when set, runs the model-driven investigation after the
	// deterministic pass. Nil means no model is configured, which is the
	// default and a complete audit in its own right.
	Agent *agent.Options
	// Context names the MCP servers the agent may read the customer's own
	// documents from. It is connected here rather than by the caller for the
	// reason internal/runs exists: four entry points each remembering to
	// connect, and to close afterwards, is how one of them eventually forgets.
	// It is ignored when Agent is nil, since nothing else reads a document.
	Context config.Context
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
	// Proposals are rules the agent suggested for future audits. They are not
	// findings, are not applied, and go to a person to accept or discard.
	Proposals []rules.Proposal
	// StartedAt and Duration describe the run itself.
	StartedAt time.Time
	Duration  time.Duration

	engine *engine.Engine
	// scratch is the temporary directory holding this run's DuckDB file, when
	// the caller did not name one of its own. Close removes it.
	scratch string
}

// Engine exposes the engine the run used, so a caller can keep querying the
// loaded dataset. It is only valid until Close.
func (r *Result) Engine() *engine.Engine { return r.engine }

// Close releases the engine.
func (r *Result) Close() error {
	if r == nil {
		return nil
	}
	var err error
	if r.engine != nil {
		err = r.engine.Close()
	}
	if r.scratch != "" {
		// After the engine, never before: the file has to be flushed and
		// released before the directory under it goes away.
		if rmErr := os.RemoveAll(r.scratch); err == nil {
			err = rmErr
		}
		r.scratch = ""
	}
	return err
}

// stage runs one step of the pipeline inside its own span.
//
// The span carries the stage name and whatever the stage counted, and nothing
// else: no table name, no column name, no path. See telemetry.Start for why
// that line is where it is, and TestNoSpanCarriesCustomerData for what holds
// it there.
func stage[T any](ctx context.Context, log *slog.Logger, name string, f func(context.Context) (T, error),
	attrs func(T) []attribute.KeyValue,
) (T, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "audit."+name,
		oteltrace.WithAttributes(telemetry.AttrStage.String(name)))
	defer span.End()

	// Announced before as well as after, because on a dataset worth auditing
	// a stage is minutes long and everything downstream of these lines — the
	// terminal, and the browser's progress stream, which is these same lines —
	// otherwise shows nothing at all while it runs. The duration is the other
	// half: an audit that takes an hour is a question about which stage took
	// it, and a log that only says a stage finished cannot answer.
	started := time.Now()
	log.Info("stage starting", "stage", name)

	out, err := f(ctx)
	if err != nil {
		log.Info("stage failed", "stage", name, "duration", time.Since(started).Round(time.Millisecond))
		// The message is the stage that failed, not the error text: an engine
		// error can carry a cell value, which is the whole reason
		// redact.Guard.EngineError exists.
		span.SetStatus(codes.Error, name+" failed")
		return out, err
	}
	log.Info("stage complete", "stage", name, "duration", time.Since(started).Round(time.Millisecond))
	if attrs != nil {
		span.SetAttributes(attrs(out)...)
	}
	return out, nil
}

// Run executes the pipeline. The caller must Close the result.
func Run(ctx context.Context, opts Options, log *slog.Logger) (*Result, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	started := time.Now()

	// One trace per audit, with a span per stage under it. An audit is a
	// minutes-long operation somebody asked for by name, so the useful shape
	// is one trace that shows where the minutes went.
	ctx, span := telemetry.Tracer().Start(ctx, "audit.run",
		oteltrace.WithAttributes(attribute.Bool("veritix.agent.enabled", opts.Agent != nil)))
	defer span.End()

	outcome := "error"
	defer func() { recordRun(ctx, outcome, time.Since(started)) }()

	ds, err := stage(ctx, log, "discover", func(context.Context) (*source.Dataset, error) {
		return source.Discover(opts.Paths)
	}, func(ds *source.Dataset) []attribute.KeyValue {
		return []attribute.KeyValue{
			attribute.Int("veritix.files", len(ds.Files)),
			attribute.Int("veritix.files.skipped", len(ds.Skipped)),
		}
	})
	if err != nil {
		span.SetStatus(codes.Error, "discovery failed")
		return nil, err
	}
	log.Info("discovered dataset",
		"root", ds.Root, "files", len(ds.Files), "skipped", len(ds.Skipped))

	dbPath, scratch := opts.DatabasePath, ""
	if dbPath == "" {
		// Failing to get one is not a reason to fail the audit: in-memory is
		// what this did until it was measured, and it still works.
		if dir, err := os.MkdirTemp(opts.Engine.TempDir, "veritix-run-"); err == nil {
			scratch = dir
			dbPath = filepath.Join(dir, "dataset.duckdb")
		} else {
			log.Warn("could not make a scratch directory, so the dataset will be held "+
				"in memory", "error", err)
		}
	}

	e, err := engine.Open(ctx, dbPath, opts.Engine, log)
	if err != nil {
		if scratch != "" {
			_ = os.RemoveAll(scratch)
		}
		span.SetStatus(codes.Error, "the engine would not open")
		return nil, err
	}

	// From here on the engine has to be released on every path out.
	res := &Result{Dataset: ds, StartedAt: started, engine: e, scratch: scratch}

	loaded, err := stage(ctx, log, "ingest", func(ctx context.Context) (*ingest.Result, error) {
		return ingest.Load(ctx, e, ds, ingest.Options{}, log)
	}, func(l *ingest.Result) []attribute.KeyValue {
		var rows, rejected int64
		for _, t := range l.Tables {
			rows += t.RowCount
			rejected += t.RejectCount
		}
		return []attribute.KeyValue{
			attribute.Int("veritix.tables", len(l.Tables)),
			attribute.Int64("veritix.rows", rows),
			attribute.Int64("veritix.rows.rejected", rejected),
		}
	})
	if err != nil {
		span.SetStatus(codes.Error, "ingest failed")
		_ = e.Close()
		return nil, err
	}
	res.Loaded = loaded

	var rows int64
	for _, t := range loaded.Tables {
		rows += t.RowCount
	}
	log.Info("loaded dataset", "tables", len(loaded.Tables), "rows", rows)

	prof, err := stage(ctx, log, "profile", func(ctx context.Context) (*profile.Dataset, error) {
		return profile.Run(ctx, e, loaded, opts.Profile, log)
	}, func(pr *profile.Dataset) []attribute.KeyValue {
		cols := 0
		for _, t := range pr.Tables {
			cols += len(t.Columns)
		}
		return []attribute.KeyValue{attribute.Int("veritix.columns", cols)}
	})
	if err != nil {
		span.SetStatus(codes.Error, "profiling failed")
		_ = e.Close()
		return nil, err
	}
	res.Profile = prof

	found, err := stage(ctx, log, "checks", func(ctx context.Context) (*finding.Set, error) {
		return checks.Run(ctx, e, prof, log)
	}, func(f *finding.Set) []attribute.KeyValue {
		return []attribute.KeyValue{attribute.Int("veritix.findings", len(f.All()))}
	})
	if err != nil {
		span.SetStatus(codes.Error, "the checks failed")
		_ = e.Close()
		return nil, err
	}

	ruleFindings, err := stage(ctx, log, "rules", func(ctx context.Context) ([]finding.Finding, error) {
		return rules.Evaluate(ctx, e, prof, opts.Rules, log)
	}, func(fs []finding.Finding) []attribute.KeyValue {
		return []attribute.KeyValue{attribute.Int("veritix.findings", len(fs))}
	})
	if err != nil {
		span.SetStatus(codes.Error, "the rules failed")
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
			span.SetStatus(codes.Error, "lockdown failed")
			_ = e.Close()
			return nil, err
		}

		// The context servers are connected after lockdown and before the
		// model, so that a subprocess Veritix started cannot be reached
		// through DuckDB either. A server that will not answer is a warning
		// and not a failure: an audit that dies because a data dictionary was
		// down is worse than one that runs unaided.
		var lib *mcpclient.Library
		if len(opts.Context.Servers) > 0 {
			lib, err = stage(ctx, log, "context", func(ctx context.Context) (*mcpclient.Library, error) {
				return mcpclient.Connect(ctx, mcpclient.Options{
					Servers:          contextServers(opts.Context.Servers),
					MaxDocumentBytes: opts.Context.MaxDocumentBytes,
					ConnectTimeout:   opts.Context.ConnectTimeout,
					Log:              log,
				})
			}, func(l *mcpclient.Library) []attribute.KeyValue {
				// A count, never a name: a document title is the customer's
				// own vocabulary, which is the schema half of the rule in
				// telemetry.Start.
				return []attribute.KeyValue{
					attribute.Int("veritix.context.documents", len(l.Catalog())),
				}
			})
			if err != nil {
				span.SetStatus(codes.Error, "the context servers could not be reached")
				_ = e.Close()
				return nil, err
			}
			// Closed as soon as the agent is done rather than with the run:
			// these are subprocesses, and a report that takes a minute to
			// render is a minute of a ticket system's connection held for
			// nothing.
			defer func() {
				if closeErr := lib.Close(); closeErr != nil {
					log.Warn("a context server did not shut down cleanly", "error", closeErr)
				}
			}()
		}

		agentRes, err := stage(ctx, log, "agent", func(ctx context.Context) (*agent.Result, error) {
			return agent.Run(ctx, agent.Input{
				Engine:  e,
				Profile: prof,
				Known:   found.All(),
				Rules:   opts.Rules,
				Context: lib,
				Root:    ds.Root,
			}, *opts.Agent, log)
		}, func(a *agent.Result) []attribute.KeyValue {
			return []attribute.KeyValue{
				attribute.Int("veritix.findings", len(a.Findings)),
				attribute.Int("veritix.proposals", len(a.Proposals)),
			}
		})
		if err != nil {
			span.SetStatus(codes.Error, "the agent failed")
			_ = e.Close()
			return nil, err
		}
		found.AddAll(agentRes.Findings)
		res.Proposals = agentRes.Proposals
		res.Trace = agentRes.Trace
		recordAgent(ctx, agentRes.Trace)
	}

	// Re-run every finding's evidence before reporting it. For the built-in
	// checks this is close to a tautology. It is not one for the agent's
	// findings, and the rule applies to all of them equally rather than being
	// switched on for the ones we already distrust: a check whose evidence
	// stopped reproducing is as much a defect as a model that made something
	// up, and neither should reach a customer's report.
	dropped, err := stage(ctx, log, "verify", func(ctx context.Context) ([]finding.Finding, error) {
		return found.Verify(ctx, e)
	}, func(d []finding.Finding) []attribute.KeyValue {
		return []attribute.KeyValue{attribute.Int("veritix.findings.dropped", len(d))}
	})
	if err != nil {
		span.SetStatus(codes.Error, "verification failed")
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

	outcome = "ok"
	recordFindings(ctx, found.All())
	span.SetAttributes(attribute.Int("veritix.findings", len(found.All())))

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

// recordRun, recordFindings and recordAgent are here rather than in
// internal/telemetry because they read Veritix's own types, and telemetry is
// imported by the packages those types live in. A metrics helper that forced
// an import cycle would be a helper that costs more than it saves.
func recordRun(ctx context.Context, outcome string, d time.Duration) {
	in := telemetry.Metrics()
	attrs := metric.WithAttributes(telemetry.AttrOutcome.String(outcome))
	in.Runs.Add(ctx, 1, attrs)
	in.RunDuration.Record(ctx, d.Seconds(), attrs)
}

// recordFindings counts what the audit reported, by severity and by what
// produced it. Both halves matter to an operator: severity is what they act
// on, and origin is whether the model is earning its keep.
func recordFindings(ctx context.Context, fs []finding.Finding) {
	in := telemetry.Metrics()
	for _, f := range fs {
		in.Findings.Add(ctx, 1, metric.WithAttributes(
			telemetry.AttrSeverity.String(f.Severity.String()),
			telemetry.AttrOrigin.String(string(f.Origin)),
		))
	}
}

// recordAgent reports what one agentic run cost. Tokens are the number an
// operator converts to money against their own contract, which is why the
// trace records them and no price list is kept anywhere in this repo.
func recordAgent(ctx context.Context, t *agent.Trace) {
	if t == nil {
		return
	}
	in := telemetry.Metrics()
	in.AgentSteps.Record(ctx, int64(len(t.Steps)), metric.WithAttributes(
		telemetry.AttrStopped.String(string(t.Stopped)),
	))
	// Every direction the provider reported, separately. Cached input is a
	// fifth to a tenth of the price of uncached input, and an operator working
	// out what a run cost cannot do it from a single total.
	for dir, n := range map[string]int{
		"input":       t.Usage.Input,
		"output":      t.Usage.Output,
		"cache_read":  t.Usage.CacheRead,
		"cache_write": t.Usage.CacheWrite,
	} {
		if n > 0 {
			in.AgentTokens.Add(ctx, int64(n),
				metric.WithAttributes(telemetry.AttrDirection.String(dir)))
		}
	}
}

// contextServers converts the configured servers into what the client wants.
//
// The two types are deliberately separate: config.ContextServer is what a
// person writes in a YAML file, and mcpclient.Server carries a Transport that
// a config file has no way to express and that tests use to connect to a
// server in the same process.
func contextServers(servers []config.ContextServer) []mcpclient.Server {
	out := make([]mcpclient.Server, 0, len(servers))
	for _, s := range servers {
		out = append(out, mcpclient.Server{
			Name:    s.Name,
			Command: s.Command,
			Args:    s.Args,
			Env:     s.Env,
		})
	}
	return out
}
