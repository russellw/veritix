package api

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/russellw/veritix/internal/store"
)

// scheduler starts the audits nobody pressed, and clears up after them.
//
// It runs in the process that serves, and nowhere else. internal/mcp
// deliberately does not call store.MarkInterrupted, because a subprocess must
// not declare another process's work dead; the same argument says a
// veritix mcp sharing a data directory must not also be firing windows, and
// since it never builds a Server it never does.
//
// A scheduled run is an ordinary run: it goes through the same startRun that
// POST /runs uses, so it streams on the same event stream, can be canceled
// from the same interface, has the same accepted rules in force, and is
// compared against the same baseline. Anything else would be a fourth entry
// point assembling the pipeline slightly differently.
type scheduler struct {
	srv  *Server
	tick time.Duration
	// fire is whether this process starts due audits. Discarding expired run
	// databases happens either way: it is disk hygiene, and an operator who
	// turned the clock off because another process owns it has not asked for
	// the disk to fill up.
	fire bool
	// retain is how long a finished run's ingested data is kept. Zero keeps
	// all of it.
	retain time.Duration
	// now is the clock, injected so that the tests do not have to sleep
	// through a window.
	now func() time.Time

	wg sync.WaitGroup
}

func newScheduler(s *Server) *scheduler {
	// Validate only bounds the tick when the clock is on, and time.NewTicker
	// panics on a zero one. Discarding expired databases uses the same ticker
	// and does not care how often it fires.
	tick := s.cfg.Schedule.Tick
	if tick <= 0 {
		tick = 30 * time.Second
	}
	return &scheduler{
		srv:    s,
		tick:   tick,
		fire:   s.cfg.Schedule.Enabled,
		retain: s.cfg.Server.RetainDatabases,
		now:    time.Now,
	}
}

// needed reports whether there is anything for the ticker to do.
func (sc *scheduler) needed() bool { return sc.fire || sc.retain > 0 }

// start runs the clock until the context is canceled or the server is closing.
func (sc *scheduler) start(ctx context.Context) {
	sc.wg.Add(1)
	go func() {
		defer sc.wg.Done()

		t := time.NewTicker(sc.tick)
		defer t.Stop()

		// The first sweep is one tick in rather than immediate: a window that
		// is already due has waited this long and can wait thirty seconds
		// more, and a server that has just started has other things to do.
		for {
			select {
			case <-ctx.Done():
				return
			case <-sc.srv.stopping:
				return
			case <-t.C:
				sc.tickOnce(ctx)
			}
		}
	}()
}

// tickOnce is one turn of the clock: start what is due, then clear up after
// what is finished.
func (sc *scheduler) tickOnce(ctx context.Context) {
	if sc.fire {
		sc.sweep(ctx)
	}
	if sc.retain > 0 {
		sc.discard(ctx)
	}
}

// wait blocks until the clock has stopped. Server.Close closes stopping and
// then calls this, before it shuts the running audits down: a scheduler still
// ticking could otherwise start one after they had all been stopped.
func (sc *scheduler) wait() { sc.wg.Wait() }

// sweep starts what is due.
//
// At most one audit is started per sweep. That is the whole of the stagger: a
// server coming back after a weekend with fifteen datasets due does not start
// fifteen audits at once, it starts one a tick, and nothing starves because a
// schedule that fires moves its window to the next one.
func (sc *scheduler) sweep(ctx context.Context) {
	now := sc.now()

	all, err := sc.srv.store.Schedules(ctx)
	if err != nil {
		sc.srv.log.Error("could not read the schedules", "error", err)
		return
	}

	started := false
	for _, s := range all {
		if s.NextDueAt.After(now) {
			continue
		}

		// Whatever happens to this window, the next one is the first in the
		// future. That is what makes a missed window fire once rather than
		// once for every window that was missed.
		next := s.Spec.Next(now)

		if reason := sc.blocked(ctx, s, now); reason != "" {
			sc.record(ctx, s, next, "", reason)
			continue
		}
		if started {
			// Left due on purpose: the next tick will take it.
			continue
		}

		// No model and no cell values: createRunRequest's zero value is the
		// deterministic auditor, which is what a nightly audit has to be.
		// Sending a dataset's metadata to a model unattended, every night,
		// forever is exactly the decision the per-run switch exists to make
		// deliberately.
		run, err := sc.srv.startRun(ctx,
			createRunRequest{DatasetID: s.DatasetID},
			startOptions{Notify: s.Notify})
		if err != nil {
			// A dataset whose path has gone, or one that cannot be prepared.
			// Recording it is the point: a schedule that has been failing
			// quietly for a month is worse than no schedule at all.
			sc.srv.log.Error("a scheduled audit could not start",
				"dataset", s.DatasetID, "error", err)
			sc.record(ctx, s, next, "", err.Error())
			continue
		}

		sc.srv.log.Info("started a scheduled audit",
			"dataset", s.DatasetID, "run", run.ID, "schedule", s.Spec.String(), "next_due", next)
		sc.record(ctx, s, next, run.ID, "")
		started = true
	}
}

// blocked reports why this window must not start an audit, or "" to go ahead.
func (sc *scheduler) blocked(ctx context.Context, s *store.Schedule, now time.Time) string {
	// A window older than the schedule's own period was not missed once, it
	// was missed repeatedly, and auditing week-old state the moment the server
	// comes back is not what "daily at 02:00" asked for. The schedule resumes
	// at its next window, which is at most one period away.
	if w := s.Spec.Window(); w > 0 && now.Sub(s.NextDueAt) > w {
		return "missed while nothing was running to fire it"
	}

	// An audit of two gigabytes takes fifteen minutes, and an hourly schedule
	// on something slower must not stack.
	switch run, err := sc.srv.store.ActiveRun(ctx, s.DatasetID); {
	case err == nil:
		return "an audit of this dataset was already running (" + run.ID + ")"
	case !errors.Is(err, store.ErrNotFound):
		return err.Error()
	}
	return ""
}

// record advances the window and says what it did. A window that could not
// start a run still advances, or the same failure is retried on every tick
// until somebody notices.
func (sc *scheduler) record(
	ctx context.Context, s *store.Schedule, next time.Time, runID, reason string,
) {
	if reason != "" {
		sc.srv.log.Warn("a scheduled audit did not start",
			"dataset", s.DatasetID, "reason", reason, "next_due", next)
	}
	if err := sc.srv.store.ScheduleFired(ctx, s.DatasetID, next, runID, reason); err != nil {
		sc.srv.log.Error("could not record what a schedule did",
			"dataset", s.DatasetID, "error", err)
	}
}

// discard deletes the ingested data of runs old enough to have lost it, and
// records that it is gone.
//
// What goes is the DuckDB file a run left behind so that a finding's rows
// could be fetched afterwards — a copy of the customer's own files, which can
// be made again by auditing them again. What stays is the run, its report, its
// findings and its trace: that is the audit trail, it is small, and it is the
// thing somebody wants six months later.
//
// The comparison a report is built from reads the stored document and never
// this file, so discarding one cannot make a run-over-run comparison stop
// working. That is what makes the split possible at all.
func (sc *scheduler) discard(ctx context.Context) {
	cutoff := sc.now().Add(-sc.retain)

	expired, err := sc.srv.store.DiscardableRunDatabases(ctx, cutoff)
	if err != nil {
		sc.srv.log.Error("could not list the run databases to discard", "error", err)
		return
	}

	for _, run := range expired {
		var bytes int64
		if info, err := os.Stat(run.DatabasePath); err == nil { //nolint:gosec // server-generated path
			bytes = info.Size()
		}

		// removeRunFiles refuses anything that is not under DataDir/runs, so a
		// row edited by hand cannot make this delete something else.
		//
		// The store is not told the data is gone unless it is gone. On Windows
		// a file something holds open cannot be deleted — a rows request in
		// flight, or a virus scanner reading a 700 MB database — where on
		// Linux the unlink succeeds regardless and this can never fail.
		// Recording a discard that did not happen would take the run out of
		// this list forever and leave the bytes on the disk: retained and
		// unreachable at once, which is both halves wrong, and silent, in the
		// one job whose whole purpose is that the disk does not fill up. The
		// next sweep tries again, because a held file is a transient thing.
		if err := sc.srv.removeRunFiles(run); err != nil {
			continue
		}

		if err := sc.srv.store.ClearRunDatabase(ctx, run.ID); err != nil {
			sc.srv.log.Error("could not record a discarded run database",
				"run", run.ID, "error", err)
			continue
		}
		sc.srv.log.Info("discarded a run's ingested data",
			"run", run.ID, "dataset", run.DatasetID, "finished", run.FinishedAt, "bytes", bytes)
	}
}
