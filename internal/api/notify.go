package api

import (
	"context"
	"encoding/json"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/notify"
	"github.com/russellw/veritix/internal/report"
	"github.com/russellw/veritix/internal/store"
)

// notifyRun tells the configured sink what a scheduled audit found.
//
// Everything in the message is read back from the store, not from the audit
// that just finished: the run's terminal status is the store's to say, and the
// document is the one every other reader gets. A message that disagreed with
// the report it points at would be worse than no message.
//
// Nothing here can fail a run. The audit is over and recorded; a report
// nobody was told about is a great deal better than an audit that died because
// a chat server was down.
func (s *Server) notifyRun(ctx context.Context, started *store.Run) {
	if s.notify == nil {
		return
	}

	run, err := s.store.Run(ctx, started.ID)
	if err != nil {
		s.log.Error("could not read a run to notify about", "run", started.ID, "error", err)
		return
	}

	// A failed run has no document, which is itself the reason it is worth a
	// message: a run with no report cannot be shown not to have regressed.
	var doc *report.Document
	if raw, err := s.store.Document(ctx, run.ID); err == nil {
		var d report.Document
		if err := json.Unmarshal(raw, &d); err == nil {
			doc = &d
		} else {
			s.log.Error("could not read a run's report to notify about",
				"run", run.ID, "error", err)
		}
	}

	event, wanted := s.notify.Wanted(string(run.Status), doc, s.notify.MinSeverity())
	if !wanted {
		return
	}

	m := notify.Message{
		Event:      event,
		DatasetID:  run.DatasetID,
		RunID:      run.ID,
		Status:     string(run.Status),
		Reason:     run.Message,
		FinishedAt: run.FinishedAt,
		URL:        s.notify.RunURL(run.ID),
		Version:    run.Version,
		Findings: &notify.Counts{
			Total: run.Total(), Errors: run.Errors, Warnings: run.Warnings, Info: run.Infos,
		},
	}
	if ds, err := s.store.Dataset(ctx, run.DatasetID); err == nil {
		m.Dataset = ds.Name
	}
	if doc != nil && doc.Comparison != nil {
		summary := doc.Comparison.Summary
		m.Changes = &summary
		if s.notify.Detail() == config.NotifyDetailFindings {
			m.Regressions = regressions(doc.Comparison, s.notify.MinSeverity())
		}
	}

	if err := s.notify.Send(ctx, m); err != nil {
		// The schedule's own record is about its windows, not about a webhook,
		// so this is a log line. A sink that has been refusing messages for a
		// month is a problem in somebody else's system.
		s.log.Error("could not notify about a run", "run", run.ID, "error", err)
	}
}

// regressions is what the delta says got worse, in the report's own words.
//
// Delta.Regressed is the one definition, the same set --fail-on-regression
// counts at the same threshold, so a message and a build gate cannot disagree
// about what a regression is.
func regressions(d *report.Delta, minSeverity string) []notify.Regression {
	var out []notify.Regression
	for _, f := range d.Regressed(minSeverity) {
		out = append(out, notify.Regression{
			Rule:     f.Rule,
			Severity: f.Severity,
			Status:   string(f.Status),
			Title:    f.Title,
			Table:    f.Table,
			Column:   f.Column,
			Before:   f.CountBefore,
			After:    f.CountAfter,
		})
	}
	return out
}
