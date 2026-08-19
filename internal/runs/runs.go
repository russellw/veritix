// Package runs executes one audit and records it in the store.
//
// It exists for the same reason [audit.Run] does, one layer up. audit.Run is
// the pipeline; this is the bookkeeping that has to happen around it — mark
// the run started, build the report, release the engine, record the findings,
// save the trace — and the order of those steps is load-bearing. The engine is
// closed before the run is recorded as finished because the DuckDB file has to
// be flushed before anything reopens it read-only, and a run that was canceled
// has to be recorded as canceled on a context that outlives the cancellation.
//
// The HTTP API and the MCP server both drive this. Two callers each
// remembering that ordering for themselves is how one of them eventually
// forgets.
package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/report"
	"github.com/russellw/veritix/internal/store"
)

// Progress is one line of a run's own log, on its way to whoever is watching.
//
// It carries the same class of information as the server's diagnostic log —
// stages, table names, counts — and no cell values.
type Progress struct {
	Message string
	Time    time.Time
	Fields  map[string]any
}

// Options is everything one recorded run needs.
type Options struct {
	// Store is where the run is recorded. Required, and the run must already
	// exist in it: the caller creates it so that it has an id to hand back
	// before the work starts.
	Store *store.Store
	// RunID identifies that run.
	RunID string
	// Version is recorded in the report as the build that produced it.
	Version string
	// Audit configures the pipeline itself.
	Audit audit.Options
	// Report controls what the produced document may contain, which is where
	// the decision about verbatim cell values is taken.
	Report report.Options
	// Log receives diagnostics. Its handler is also what the pipeline logs
	// through, so Watch sees every stage this logger would have shown.
	Log *slog.Logger
	// Watch, when set, receives the pipeline's own log lines as they happen.
	// Nil means nobody is watching, which is the normal case for a caller that
	// waits for the result instead of streaming it.
	Watch func(Progress)
}

// Execute runs the audit and records what happened, returning the reason it
// did not finish or nil if it did.
//
// Every failure is recorded on the run before it is returned, so the store is
// the authority on the outcome whether or not the caller looks at the error.
func Execute(ctx context.Context, o Options) error {
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	// The store writes are done on a context that outlives cancellation: a
	// canceled run still has to be recorded as canceled.
	recordCtx := context.WithoutCancel(ctx)

	if err := o.Store.StartRun(recordCtx, o.RunID); err != nil {
		log.Error("could not mark the run as started", "run", o.RunID, "error", err)
		return err
	}

	runLog := log.With("run", o.RunID)
	if o.Watch != nil {
		runLog = slog.New(&progressHandler{inner: log.Handler(), watch: o.Watch}).With("run", o.RunID)
	}

	res, err := audit.Run(ctx, o.Audit, runLog)
	if err != nil {
		// A canceled context is reported as cancellation whatever error the
		// pipeline surfaced on the way out, because the layer that noticed
		// first varies with where the run had got to.
		status, message := store.StatusFailed, err.Error()
		if ctx.Err() != nil {
			status, message = store.StatusCanceled, "canceled"
		}
		if stopErr := o.Store.StopRun(recordCtx, o.RunID, status, message); stopErr != nil {
			log.Error("could not record the run's failure", "run", o.RunID, "error", stopErr)
		}
		return err
	}

	// The document is built, and then the engine is released, before anything
	// is stored: the DuckDB file has to be closed and flushed before the rows
	// endpoint can reopen it read-only.
	doc := report.Build(res, o.Version, o.Report)
	trace := res.Trace
	if err := res.Close(); err != nil {
		log.Warn("could not close the run's engine", "run", o.RunID, "error", err)
	}

	if ctx.Err() != nil {
		if err := o.Store.StopRun(recordCtx, o.RunID, store.StatusCanceled, "canceled"); err != nil {
			log.Error("could not record the cancellation", "run", o.RunID, "error", err)
		}
		return ctx.Err()
	}

	body, err := json.Marshal(doc)
	if err != nil {
		log.Error("could not encode the report", "run", o.RunID, "error", err)
		_ = o.Store.StopRun(recordCtx, o.RunID, store.StatusFailed, "the report could not be encoded")
		return err
	}

	counts := store.Counts{
		Errors:   doc.FindingSummary.Errors,
		Warnings: doc.FindingSummary.Warnings,
		Infos:    doc.FindingSummary.Info,
	}
	if err := o.Store.FinishRun(recordCtx, o.RunID, body, counts, storeFindings(res.Findings)); err != nil {
		log.Error("could not record the run's findings", "run", o.RunID, "error", err)
		_ = o.Store.StopRun(recordCtx, o.RunID, store.StatusFailed, "the results could not be stored")
		return err
	}

	// The trace is stored last and its failure does not fail the run: the
	// findings are already safe, and losing the record of how they were
	// investigated is worth a loud log line rather than throwing away a
	// completed audit.
	if trace != nil {
		if body, err := json.Marshal(trace); err != nil {
			log.Error("could not encode the agent trace", "run", o.RunID, "error", err)
		} else if err := o.Store.SaveTrace(recordCtx, o.RunID, body); err != nil {
			log.Error("could not record the agent trace", "run", o.RunID, "error", err)
		}
	}

	// No completion event is published here: audit.Run logs its own, and the
	// caller's terminal event carries the counts. Two "finished" lines in a row
	// is how a progress display ends up looking broken.
	return nil
}

// storeFindings reduces findings to what the store keeps: identity, plus the
// row query that no report is allowed to carry.
func storeFindings(set *finding.Set) []store.Finding {
	if set == nil {
		return nil
	}
	all := set.All()
	out := make([]store.Finding, 0, len(all))
	for i, f := range all {
		out = append(out, store.Finding{
			ID:       f.ID(),
			Ordinal:  i,
			Rule:     f.Rule,
			Severity: f.Severity.String(),
			Title:    f.Title,
			Table:    f.Location.Table,
			Column:   f.Location.Column,
			RowQuery: f.Evidence.RowQuery,
		})
	}
	return out
}

// DatabasePath is where a run's ingested dataset lives, created if it does not
// exist. It outlives the run so that a finding's offending rows can be fetched
// afterwards without reading the customer's files a second time.
func DatabasePath(dataDir, runID string) (string, error) {
	dir := filepath.Join(dataDir, "runs", runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("could not create the run directory: %w", err)
	}
	return filepath.Join(dir, "dataset.duckdb"), nil
}

// progressHandler turns the pipeline's own log lines into progress events.
//
// audit.Run already announces every stage it reaches, to a logger it is
// handed. Publishing from there rather than adding a second notification
// mechanism means the two cannot drift: a stage that is logged is a stage the
// watcher sees, and there is no way to add one and forget the other.
type progressHandler struct {
	inner slog.Handler
	watch func(Progress)
	attrs []slog.Attr
}

// Enabled accepts info and above even when the operator has turned the log
// down, because the progress stream is a user interface rather than a
// diagnostic. The inner handler is consulted separately in Handle.
func (h *progressHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo || h.inner.Enabled(ctx, level)
}

func (h *progressHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo {
		fields := make(map[string]any, r.NumAttrs()+len(h.attrs))
		for _, a := range h.attrs {
			addField(fields, a)
		}
		r.Attrs(func(a slog.Attr) bool {
			addField(fields, a)
			return true
		})
		if len(fields) == 0 {
			fields = nil
		}
		h.watch(Progress{Message: r.Message, Time: r.Time, Fields: fields})
	}

	if !h.inner.Enabled(ctx, r.Level) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *progressHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &progressHandler{
		inner: h.inner.WithAttrs(attrs),
		watch: h.watch,
		attrs: append(slices.Clip(h.attrs), attrs...),
	}
}

func (h *progressHandler) WithGroup(name string) slog.Handler {
	// Groups are passed to the diagnostic log but ignored for progress: the
	// pipeline does not use them, and flattening them would produce colliding
	// field names rather than a useful nesting.
	return &progressHandler{inner: h.inner.WithGroup(name), watch: h.watch, attrs: h.attrs}
}

// addField converts a log attribute into something that survives JSON.
// Anything exotic is rendered as text rather than risking an event that cannot
// be encoded and so silently never arrives.
func addField(fields map[string]any, a slog.Attr) {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		fields[a.Key] = v.String()
	case slog.KindInt64:
		fields[a.Key] = v.Int64()
	case slog.KindUint64:
		fields[a.Key] = v.Uint64()
	case slog.KindFloat64:
		fields[a.Key] = v.Float64()
	case slog.KindBool:
		fields[a.Key] = v.Bool()
	case slog.KindDuration:
		// Suffixed rather than left bare: a client receiving `duration: 208`
		// has no way to know what unit it is looking at.
		fields[a.Key+"_ms"] = v.Duration().Milliseconds()
	case slog.KindTime:
		fields[a.Key] = v.Time()
	default:
		fields[a.Key] = v.String()
	}
}
