package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/russellw/veritix/internal/store"
)

// scheduler starts the audits nobody pressed.
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
	// now is the clock, injected so that the tests do not have to sleep
	// through a window.
	now func() time.Time

	wg sync.WaitGroup
}

func newScheduler(s *Server) *scheduler {
	return &scheduler{srv: s, tick: s.cfg.Schedule.Tick, now: time.Now}
}

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
				sc.sweep(ctx)
			}
		}
	}()
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

		run, err := sc.srv.startRun(ctx, createRunRequest{DatasetID: s.DatasetID})
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
