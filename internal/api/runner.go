package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/russellwallace/veritix/internal/audit"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/report"
	"github.com/russellwallace/veritix/internal/store"
)

// Event is one item on a run's stream.
//
// It carries the same class of information as the server's diagnostic log —
// stages, table names, counts — and no cell values. The one endpoint that
// serves those is the per-finding rows endpoint, deliberately and alone.
type Event struct {
	// Seq numbers the run's progress from 1. The terminal event has no
	// sequence number: it is read from the store rather than replayed from the
	// stream, so it is not at any position in it.
	Seq     int            `json:"seq,omitempty"`
	Type    string         `json:"type"`
	Time    time.Time      `json:"time"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
	// Run is set on the terminal event, so a client that only cares about the
	// outcome can ignore everything before it.
	Run *runJSON `json:"run,omitempty"`
}

const (
	eventProgress = "progress"
	eventDone     = "done"
)

// activeRun is one audit executing in this process, and the fan-out of its
// progress to whoever is watching.
type activeRun struct {
	cancel context.CancelFunc

	mu      sync.Mutex
	history []Event
	subs    map[chan Event]struct{}
	closed  bool
}

// subscribe returns everything published so far plus a channel of what comes
// next. Both come from under one lock: a subscriber that read the backlog and
// then registered would miss anything published in between.
func (a *activeRun) subscribe() ([]Event, chan Event) {
	a.mu.Lock()
	defer a.mu.Unlock()

	backlog := slices.Clone(a.history)
	if a.closed {
		return backlog, nil
	}

	// Buffered because a slow browser must not stall the audit. If a
	// subscriber falls this far behind it loses progress lines, which is
	// recoverable: the outcome is read back from the store when the stream
	// ends, not carried by the last event.
	ch := make(chan Event, 64)
	if a.subs == nil {
		a.subs = make(map[chan Event]struct{})
	}
	a.subs[ch] = struct{}{}
	return backlog, ch
}

func (a *activeRun) unsubscribe(ch chan Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.subs[ch]; ok {
		delete(a.subs, ch)
		close(ch)
	}
}

func (a *activeRun) publish(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}

	ev.Seq = len(a.history) + 1
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	a.history = append(a.history, ev)

	for ch := range a.subs {
		select {
		case ch <- ev:
		default: // see subscribe: dropping progress is survivable
		}
	}
}

// close ends the stream. Subscribers see their channel close and then read the
// finished run from the store, so the outcome cannot be lost to a full buffer.
func (a *activeRun) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	for ch := range a.subs {
		close(ch)
	}
	a.subs = nil
}

// runner owns the runs executing in this process.
type runner struct {
	srv *Server

	mu     sync.Mutex
	active map[string]*activeRun
	wg     sync.WaitGroup
}

func newRunner(s *Server) *runner {
	return &runner{srv: s, active: make(map[string]*activeRun)}
}

func (rn *runner) get(id string) *activeRun {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return rn.active[id]
}

// cancel stops a run if it is executing here, and reports whether it was.
func (rn *runner) cancel(id string) bool {
	if a := rn.get(id); a != nil {
		a.cancel()
		return true
	}
	return false
}

// shutdown cancels every run and waits for them to unwind, so that the process
// does not exit with a DuckDB handle still open on a half-written file.
func (rn *runner) shutdown() {
	rn.mu.Lock()
	for _, a := range rn.active {
		a.cancel()
	}
	rn.mu.Unlock()
	rn.wg.Wait()
}

// start launches a run in the background and returns immediately.
//
// The run's context is deliberately not the request's: POST /runs returns as
// soon as the run is accepted, and an audit that died because the browser that
// started it navigated away would be worse than useless. Cancellation is an
// explicit act, through POST /runs/{id}/cancel or shutdown.
func (rn *runner) start(run *store.Run, opts audit.Options, reportOpts report.Options) {
	ctx, cancel := context.WithCancel(context.Background())

	a := &activeRun{cancel: cancel}

	rn.mu.Lock()
	rn.active[run.ID] = a
	rn.mu.Unlock()

	rn.wg.Add(1)
	go func() {
		defer rn.wg.Done()
		defer cancel()

		rn.execute(ctx, a, run, opts, reportOpts)

		// Deregister before closing the stream: a subscriber that sees the
		// channel close goes to the store for the outcome, and by then the run
		// must no longer look active.
		rn.mu.Lock()
		delete(rn.active, run.ID)
		rn.mu.Unlock()
		a.close()
	}()
}

func (rn *runner) execute(
	ctx context.Context,
	a *activeRun,
	run *store.Run,
	opts audit.Options,
	reportOpts report.Options,
) {
	s := rn.srv

	// The store write is done on a context that outlives cancellation: a
	// canceled run still has to be recorded as canceled.
	recordCtx := context.WithoutCancel(ctx)

	if err := s.store.StartRun(recordCtx, run.ID); err != nil {
		s.log.Error("could not mark the run as started", "run", run.ID, "error", err)
		return
	}
	a.publish(Event{Type: eventProgress, Message: "starting the audit"})

	log := slog.New(&progressHandler{inner: s.log.Handler(), run: a}).With("run", run.ID)

	res, err := audit.Run(ctx, opts, log)
	if err != nil {
		// A canceled context is reported as cancellation whatever error the
		// pipeline surfaced on the way out, because the layer that noticed
		// first varies with where the run had got to.
		status, message := store.StatusFailed, err.Error()
		if ctx.Err() != nil {
			status, message = store.StatusCanceled, "canceled"
		}
		if err := s.store.StopRun(recordCtx, run.ID, status, message); err != nil {
			s.log.Error("could not record the run's failure", "run", run.ID, "error", err)
		}
		return
	}

	// The document is built, and then the engine is released, before anything
	// is stored: the DuckDB file has to be closed and flushed before the rows
	// endpoint can reopen it read-only.
	doc := report.Build(res, s.version, reportOpts)
	trace := res.Trace
	if err := res.Close(); err != nil {
		s.log.Warn("could not close the run's engine", "run", run.ID, "error", err)
	}

	if ctx.Err() != nil {
		if err := s.store.StopRun(recordCtx, run.ID, store.StatusCanceled, "canceled"); err != nil {
			s.log.Error("could not record the cancellation", "run", run.ID, "error", err)
		}
		return
	}

	body, err := json.Marshal(doc)
	if err != nil {
		s.log.Error("could not encode the report", "run", run.ID, "error", err)
		_ = s.store.StopRun(recordCtx, run.ID, store.StatusFailed, "the report could not be encoded")
		return
	}

	counts := store.Counts{
		Errors:   doc.FindingSummary.Errors,
		Warnings: doc.FindingSummary.Warnings,
		Infos:    doc.FindingSummary.Info,
	}
	if err := s.store.FinishRun(recordCtx, run.ID, body, counts, storeFindings(res.Findings)); err != nil {
		s.log.Error("could not record the run's findings", "run", run.ID, "error", err)
		_ = s.store.StopRun(recordCtx, run.ID, store.StatusFailed, "the results could not be stored")
		return
	}

	// The trace is stored last and its failure does not fail the run: the
	// findings are already safe, and losing the record of how they were
	// investigated is worth a loud log line rather than throwing away a
	// completed audit.
	if trace != nil {
		if body, err := json.Marshal(trace); err != nil {
			s.log.Error("could not encode the agent trace", "run", run.ID, "error", err)
		} else if err := s.store.SaveTrace(recordCtx, run.ID, body); err != nil {
			s.log.Error("could not record the agent trace", "run", run.ID, "error", err)
		}
	}

	// No completion event is published here: audit.Run logs its own, and the
	// terminal `done` event carries the counts. Two "finished" lines in a row
	// is how a progress display ends up looking broken.
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

// runDatabasePath is where a run's ingested dataset lives. It outlives the run
// so that a finding's offending rows can be fetched afterwards without reading
// the customer's files a second time.
func (s *Server) runDatabasePath(runID string) (string, error) {
	dir := filepath.Join(s.cfg.Server.DataDir, "runs", runID)
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
// browser sees, and there is no way to add one and forget the other.
type progressHandler struct {
	inner slog.Handler
	run   *activeRun
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
		h.run.publish(Event{
			Type: eventProgress, Message: r.Message, Time: r.Time, Fields: fields,
		})
	}

	if !h.inner.Enabled(ctx, r.Level) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *progressHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &progressHandler{
		inner: h.inner.WithAttrs(attrs),
		run:   h.run,
		attrs: append(slices.Clip(h.attrs), attrs...),
	}
}

func (h *progressHandler) WithGroup(name string) slog.Handler {
	// Groups are passed to the diagnostic log but ignored for progress: the
	// pipeline does not use them, and flattening them would produce colliding
	// field names rather than a useful nesting.
	return &progressHandler{inner: h.inner.WithGroup(name), run: h.run, attrs: h.attrs}
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
