package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/russellw/veritix/internal/schedule"
)

// Schedule is a dataset's standing instruction to audit itself.
//
// The store keeps the bookkeeping around a schedule — when it is next due,
// what happened last time — and [schedule.Schedule] keeps what the schedule
// means. Nothing here decides whether a window has arrived; that is arithmetic
// on time, and it lives in the package that does time.
type Schedule struct {
	DatasetID string
	// Spec is the schedule itself: how often, at what time, in which zone.
	Spec schedule.Schedule
	// Notify sends this dataset's regressions to the configured sink. It is
	// per dataset because whether a team wants telling is theirs to say; where
	// the message goes is the operator's, and lives in the configuration.
	Notify bool
	// NextDueAt is the window this schedule is waiting for. It is stored
	// rather than derived from the last run, so that changing a schedule takes
	// effect at once and a window that was missed is visible in the row rather
	// than inferred by whoever is wondering why.
	NextDueAt time.Time
	// LastRunID is the run the last window started, if it started one.
	LastRunID string
	// LastError is why the last window did not produce a run: the dataset's
	// path has gone, or an audit was already in flight. Empty is the normal
	// case and is not evidence that anything ran.
	LastError string
	CreatedAt time.Time
}

const scheduleColumns = `dataset_id, kind, at_minute, weekday, every_ms, location,
	notify, next_due_at, last_run_id, last_error, created_at`

func scanSchedule(sc interface{ Scan(...any) error }) (*Schedule, error) {
	var (
		s        Schedule
		kind     string
		weekday  int
		everyMS  int64
		location string
		created  sql.NullString
		nextDue  sql.NullString
	)
	err := sc.Scan(&s.DatasetID, &kind, &s.Spec.AtMinute, &weekday, &everyMS, &location,
		&s.Notify, &nextDue, &s.LastRunID, &s.LastError, &created)
	if err != nil {
		return nil, err
	}

	loc, err := schedule.LoadLocation(location)
	if err != nil {
		// A zone that resolved when the schedule was stored and does not
		// resolve now means the machine's zone database changed underneath it.
		// Refusing to read the row is right: firing it in the wrong zone would
		// audit at the wrong hour and say nothing about why.
		return nil, err
	}

	s.Spec.Kind = schedule.Kind(kind)
	s.Spec.Weekday = time.Weekday(weekday)
	s.Spec.Every = time.Duration(everyMS) * time.Millisecond
	s.Spec.Location = loc
	s.NextDueAt = parseTime(nextDue)
	s.CreatedAt = parseTime(created)
	return &s, nil
}

// SetSchedule stores a dataset's schedule, replacing whatever it had.
//
// The caller supplies NextDueAt, because when the next window falls is a
// question about time and this package does not answer those. CreatedAt
// survives a replacement: the schedule is the dataset's, and editing the time
// of day does not make it a new one.
func (s *Store) SetSchedule(ctx context.Context, sc *Schedule) error {
	if err := sc.Spec.Validate(); err != nil {
		return err
	}
	if sc.CreatedAt.IsZero() {
		sc.CreatedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO schedules (`+scheduleColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(dataset_id) DO UPDATE SET
		   kind = excluded.kind, at_minute = excluded.at_minute,
		   weekday = excluded.weekday, every_ms = excluded.every_ms,
		   location = excluded.location, notify = excluded.notify,
		   next_due_at = excluded.next_due_at,
		   last_run_id = excluded.last_run_id, last_error = excluded.last_error`,
		sc.DatasetID, string(sc.Spec.Kind), sc.Spec.AtMinute, int(sc.Spec.Weekday),
		sc.Spec.Every.Milliseconds(), sc.Spec.Zone(), sc.Notify,
		formatTime(sc.NextDueAt), sc.LastRunID, sc.LastError, formatTime(sc.CreatedAt))
	if err != nil {
		return fmt.Errorf("set schedule: %w", err)
	}
	return nil
}

// Schedule looks up one dataset's schedule.
func (s *Store) Schedule(ctx context.Context, datasetID string) (*Schedule, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE dataset_id = ?`, datasetID)
	sc, err := scanSchedule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("dataset %s has no schedule: %w", datasetID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read schedule: %w", err)
	}
	return sc, nil
}

// Schedules lists every schedule.
//
// There is deliberately no "which of these are due" query. Whether a window
// has arrived is a comparison between two instants, and SQLite would be
// comparing the text this package writes them as — RFC 3339 with the trailing
// zeros of the fraction removed, so "…:00Z" sorts after "…:00.5Z" and a
// schedule due on the second would wait a second longer for no reason anybody
// could find. A server has one schedule per dataset, which is tens of rows, so
// the caller reads them and compares times as times.
func (s *Store) Schedules(ctx context.Context) ([]*Schedule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scheduleColumns+` FROM schedules ORDER BY next_due_at`)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports what matters

	var out []*Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("read schedule: %w", err)
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ScheduleFired records what a window did: when the next one falls, the run it
// started, and why it did not start one.
//
// The three move together because they are one answer. A window that started a
// run clears the reason the last one did not, and a window that could not
// start one must still advance, or the same failure is retried every tick
// until somebody notices.
func (s *Store) ScheduleFired(
	ctx context.Context, datasetID string, nextDue time.Time, runID, reason string,
) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET next_due_at = ?, last_run_id = ?, last_error = ?
		 WHERE dataset_id = ?`,
		formatTime(nextDue), runID, reason, datasetID)
	if err != nil {
		return fmt.Errorf("record schedule fired: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("dataset %s has no schedule: %w", datasetID, ErrNotFound)
	}
	return nil
}

// DeleteSchedule stops auditing a dataset on a clock. Deleting a schedule that
// is not there is not an error: the caller asked for it gone and it is gone.
func (s *Store) DeleteSchedule(ctx context.Context, datasetID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM schedules WHERE dataset_id = ?`, datasetID); err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	return nil
}
