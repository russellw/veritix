// Package schedule says when an audit is next due.
//
// It is deliberately pure: no store, no server, no clock of its own. A
// schedule is a description of windows and [Schedule.Next] is a function from
// one instant to the next window after it, which is what makes the daylight
// saving and month-end behavior testable without waiting for a year to pass.
//
// There is no cron syntax here on purpose. The people this product is for
// cannot write one, a parser for it is a dependency this repo would have to
// justify, and daily, weekly and every-so-often cover what a data export
// actually does. If an operator ever needs cron it is a fourth [Kind] on the
// same struct rather than a different mechanism.
package schedule

import (
	"fmt"
	"time"

	// A schedule names an IANA zone, and a distroless image carries no zone
	// database. Without this a zone that works in development is an error in
	// the container, which is the worst place to find out. It costs about
	// 450 KB and it is the whole of the fix.
	_ "time/tzdata"
)

// Kind is how a schedule's windows are spaced.
type Kind string

const (
	// KindDaily fires once a day at a wall clock time.
	KindDaily Kind = "daily"
	// KindWeekly fires once a week on one weekday at a wall clock time.
	KindWeekly Kind = "weekly"
	// KindInterval fires a fixed duration after the previous window.
	KindInterval Kind = "interval"
)

// MinInterval and MaxInterval bound [Schedule.Every].
//
// The floor is a minute rather than something more sober because an audit that
// overlaps its own next window is already handled by refusing to start one, and
// a short interval is how the browser tests watch a schedule fire without
// waiting for the hour. The ceiling is a sanity bound: past a year it is not a
// schedule anybody would notice had stopped.
const (
	MinInterval = time.Minute
	MaxInterval = 366 * 24 * time.Hour
)

// Schedule is when a dataset is due to be audited.
//
// Fields the [Kind] does not use are ignored rather than policed, so that an
// interface switching a schedule from weekly to daily does not have to clear
// the weekday to be accepted.
type Schedule struct {
	Kind Kind

	// AtMinute is minutes past midnight, read in Location. It is a minute
	// count rather than an hour and a minute because that is one number to
	// store, to validate, and to get wrong.
	AtMinute int

	// Weekday is the day KindWeekly fires on.
	Weekday time.Weekday

	// Every is the gap between windows, for KindInterval. It is counted from
	// the previous window and not from when the run actually started, so an
	// audit that took longer than usual does not push the schedule later.
	Every time.Duration

	// Location is the zone AtMinute is read in. Nil means the server's own
	// zone, which is what an operator setting "02:00" without thinking about
	// it means. It is part of the schedule rather than a server setting
	// because "overnight" is a fact about the business whose export this is,
	// not about the machine Veritix happens to run on.
	Location *time.Location
}

// Next returns the first window strictly after the given instant.
//
// Strictly after, so that a caller storing the result and passing it back gets
// the following window rather than the same one twice. That is the whole of
// the promise that a schedule does not double-fire on the night a clock goes
// back and an hour is lived through twice.
func (s Schedule) Next(after time.Time) time.Time {
	switch s.Kind {
	case KindInterval:
		return after.Add(s.Every)
	case KindDaily, KindWeekly:
		return s.nextWallClock(after)
	default:
		return time.Time{}
	}
}

// nextWallClock finds the next instant matching the schedule's wall clock
// time. It steps by whole days in the schedule's own zone rather than by
// twenty-four hours, because a step across a daylight saving transition has to
// keep the wall clock: "02:00 every day" means 02:00 on the day that is 23
// hours long too.
func (s Schedule) nextWallClock(after time.Time) time.Time {
	loc := s.Loc()
	local := after.In(loc)
	y, m, d := local.Date()

	step := 1
	if s.Kind == KindWeekly {
		// Move to the schedule's own weekday first: the right time on the
		// wrong day is not a window.
		d += int(s.Weekday-local.Weekday()+7) % 7
		step = 7
	}

	// time.Date normalizes a day past the end of a month and a minute past the
	// end of an hour, so month ends, year ends and AtMinute need no arithmetic
	// of their own. It also resolves a wall clock time that does not exist —
	// the hour a spring forward skips — to the next instant that does, which is
	// the honest answer for "01:30 every day" on the one night there is no
	// 01:30.
	next := time.Date(y, m, d, 0, s.AtMinute, 0, 0, loc)
	if !next.After(after) {
		next = time.Date(y, m, d+step, 0, s.AtMinute, 0, 0, loc)
	}
	return next
}

// Loc is the zone this schedule's times are read in.
func (s Schedule) Loc() *time.Location {
	if s.Location == nil {
		return time.Local
	}
	return s.Location
}

// Zone is the name of that zone, as it is stored and as it round-trips through
// [LoadLocation]. A schedule with no zone reports "Local", which is what it
// means.
func (s Schedule) Zone() string { return s.Loc().String() }

// LoadLocation resolves a stored zone name.
//
// An empty name is the server's own zone. That is worth a function of its own
// because time.LoadLocation("") returns UTC, which is a reasonable reading of
// the standard library's contract and the wrong reading of a row where nobody
// chose a zone: it would move every unconfigured schedule by an hour for half
// the year, silently, and only in the countries that observe summer time.
func LoadLocation(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("schedule: unknown time zone %q: %w", name, err)
	}
	return loc, nil
}

// Validate reports a schedule that could never fire sensibly.
func (s Schedule) Validate() error {
	switch s.Kind {
	case KindDaily, KindWeekly:
		if s.AtMinute < 0 || s.AtMinute >= 24*60 {
			return fmt.Errorf(
				"schedule: the time of day is %d minutes past midnight; it has to be between 0 and 1439",
				s.AtMinute)
		}
		if s.Kind == KindWeekly && (s.Weekday < time.Sunday || s.Weekday > time.Saturday) {
			return fmt.Errorf("schedule: %d is not a weekday; it has to be between 0 (Sunday) and 6", s.Weekday)
		}
	case KindInterval:
		if s.Every < MinInterval || s.Every > MaxInterval {
			return fmt.Errorf(
				"schedule: an interval of %s is out of range; it has to be between %s and %s",
				s.Every, MinInterval, MaxInterval)
		}
	default:
		return fmt.Errorf(
			"schedule: %q is not a kind of schedule; it has to be %q, %q or %q",
			s.Kind, KindDaily, KindWeekly, KindInterval)
	}
	return nil
}

// String describes the schedule for a log line. The interface writes its own
// words for a person.
func (s Schedule) String() string {
	switch s.Kind {
	case KindDaily:
		return fmt.Sprintf("daily at %s %s", clock(s.AtMinute), s.Zone())
	case KindWeekly:
		return fmt.Sprintf("weekly on %s at %s %s", s.Weekday, clock(s.AtMinute), s.Zone())
	case KindInterval:
		return fmt.Sprintf("every %s", s.Every)
	default:
		return string(s.Kind)
	}
}

func clock(atMinute int) string {
	return fmt.Sprintf("%02d:%02d", atMinute/60%24, atMinute%60)
}
