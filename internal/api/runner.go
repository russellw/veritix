package api

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/report"
	"github.com/russellw/veritix/internal/runs"
	"github.com/russellw/veritix/internal/store"
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

		a.publish(Event{Type: eventProgress, Message: "starting the audit"})

		// The outcome is not inspected: it has already been recorded on the
		// run, and the store is what every reader of this run consults.
		_ = runs.Execute(ctx, runs.Options{
			Store:   rn.srv.store,
			RunID:   run.ID,
			Version: rn.srv.version,
			Audit:   opts,
			Report:  reportOpts,
			Log:     rn.srv.log,
			Watch: func(p runs.Progress) {
				a.publish(Event{
					Type: eventProgress, Message: p.Message, Time: p.Time, Fields: p.Fields,
				})
			},
		})

		// Deregister before closing the stream: a subscriber that sees the
		// channel close goes to the store for the outcome, and by then the run
		// must no longer look active.
		rn.mu.Lock()
		delete(rn.active, run.ID)
		rn.mu.Unlock()
		a.close()
	}()
}
