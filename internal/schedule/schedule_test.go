package schedule

import (
	"testing"
	"time"
)

// london is the zone both daylight saving transitions are measured in. In 2027
// the clocks go forward on 28 March (01:00 GMT becomes 02:00 BST, so no local
// time between 01:00 and 01:59 exists) and back on 31 October (02:00 BST
// becomes 01:00 GMT, so every local time between 01:00 and 01:59 happens
// twice).
func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("loading the zone: %v", err)
	}
	return loc
}

func TestADailyScheduleKeepsItsWallClockAcrossADaylightSavingChange(t *testing.T) {
	loc := london(t)
	s := Schedule{Kind: KindDaily, AtMinute: 3 * 60, Location: loc}

	// 03:00 is outside the skipped hour, so the wall clock holds on every one
	// of these days and the elapsed time is what moves.
	at := time.Date(2027, 3, 27, 3, 0, 0, 0, loc)
	for _, want := range []string{
		"2027-03-28 03:00:00 +0100 BST",
		"2027-03-29 03:00:00 +0100 BST",
	} {
		next := s.Next(at)
		if got := next.String(); got != want {
			t.Errorf("next after %s = %s, want %s", at, got, want)
		}
		at = next
	}

	// The night the clocks went forward was 23 hours long, and a daily
	// schedule fired at the same wall clock time regardless. An interval
	// schedule is the other choice and does the opposite; see below.
	spring := time.Date(2027, 3, 27, 3, 0, 0, 0, loc)
	if d := s.Next(spring).Sub(spring); d != 23*time.Hour {
		t.Errorf("the short night was %s long, want 23h", d)
	}
	autumn := time.Date(2027, 10, 30, 3, 0, 0, 0, loc)
	if d := s.Next(autumn).Sub(autumn); d != 25*time.Hour {
		t.Errorf("the long night was %s long, want 25h", d)
	}
}

func TestADailyScheduleStillFiresOnTheNightThereIsNoSuchTime(t *testing.T) {
	loc := london(t)
	// 01:30 does not exist on 28 March 2027. A schedule set to it must not
	// silently skip that day: the next instant that does exist is the answer.
	s := Schedule{Kind: KindDaily, AtMinute: 90, Location: loc}

	next := s.Next(time.Date(2027, 3, 27, 1, 30, 0, 0, loc))
	if want := "2027-03-28 02:30:00 +0100 BST"; next.String() != want {
		t.Fatalf("the skipped night fired at %s, want %s", next, want)
	}
	// And the day after is back to the time that was asked for.
	if got, want := s.Next(next).String(), "2027-03-29 01:30:00 +0100 BST"; got != want {
		t.Errorf("the night after fired at %s, want %s", got, want)
	}
}

func TestADailyScheduleFiresExactlyOncePerDayThroughAWholeYear(t *testing.T) {
	loc := london(t)
	// 01:30 is the worst case on purpose: it is skipped in March and lived
	// through twice in October. Once a day is the whole promise.
	s := Schedule{Kind: KindDaily, AtMinute: 90, Location: loc}

	at := time.Date(2027, 1, 1, 0, 0, 0, 0, loc)
	end := time.Date(2028, 1, 1, 0, 0, 0, 0, loc)

	seen := make(map[string]int)
	prev := at
	for fires := 0; ; fires++ {
		next := s.Next(prev)
		if !next.After(prev) {
			t.Fatalf("next after %s went backwards to %s", prev, next)
		}
		if !next.Before(end) {
			break
		}
		seen[next.Format("2006-01-02")]++
		prev = next
		if fires > 400 {
			t.Fatal("the schedule did not reach the end of the year")
		}
	}

	if len(seen) != 365 {
		t.Errorf("fired on %d days of 2027, want 365", len(seen))
	}
	for day, n := range seen {
		if n != 1 {
			t.Errorf("fired %d times on %s, want once", n, day)
		}
	}
}

func TestAWeeklyScheduleLandsOnItsWeekdayAcrossAMonthAndAYearEnd(t *testing.T) {
	loc := time.UTC
	s := Schedule{Kind: KindWeekly, Weekday: time.Sunday, AtMinute: 2 * 60, Location: loc}

	at := time.Date(2027, 1, 28, 9, 0, 0, 0, loc) // a Thursday
	for _, want := range []string{
		"2027-01-31 02:00:00 +0000 UTC", // the last Sunday of the month
		"2027-02-07 02:00:00 +0000 UTC", // and over the month end
	} {
		next := s.Next(at)
		if got := next.String(); got != want {
			t.Errorf("next after %s = %s, want %s", at, got, want)
		}
		at = next
	}

	// Over a year end, which time.Date's normalization handles and which is
	// exactly the arithmetic nobody writes correctly by hand.
	at = time.Date(2027, 12, 26, 9, 0, 0, 0, loc) // itself a Sunday, past the time
	if got, want := s.Next(at).String(), "2028-01-02 02:00:00 +0000 UTC"; got != want {
		t.Errorf("next after %s = %s, want %s", at, got, want)
	}
}

func TestAScheduleNeverFiresTwiceInTheHourThatHappensTwice(t *testing.T) {
	loc := london(t)
	s := Schedule{Kind: KindDaily, AtMinute: 90, Location: loc}

	// 01:30 on 31 October 2027 happens twice: once at 00:30 UTC in BST, once
	// at 01:30 UTC in GMT. Whichever of the two the schedule picks, feeding it
	// back has to move to the next day rather than to the other reading.
	first := time.Date(2027, 10, 31, 0, 30, 0, 0, time.UTC).In(loc)
	second := time.Date(2027, 10, 31, 1, 30, 0, 0, time.UTC).In(loc)

	fired := s.Next(time.Date(2027, 10, 30, 12, 0, 0, 0, loc))
	if !fired.Equal(first) && !fired.Equal(second) {
		t.Fatalf("fired at %s, want one of the two readings of 01:30", fired)
	}
	if got := s.Next(fired); got.Format("2006-01-02") != "2027-11-01" {
		t.Errorf("the window after %s was %s, want the next day", fired, got)
	}
}

func TestAnIntervalCountsElapsedTimeWhereADailyScheduleCountsDays(t *testing.T) {
	loc := london(t)
	at := time.Date(2027, 3, 27, 3, 0, 0, 0, loc)

	every := Schedule{Kind: KindInterval, Every: 24 * time.Hour, Location: loc}
	if got, want := every.Next(at).String(), "2027-03-28 04:00:00 +0100 BST"; got != want {
		t.Errorf("every 24h from %s = %s, want %s", at, got, want)
	}

	daily := Schedule{Kind: KindDaily, AtMinute: 3 * 60, Location: loc}
	if got, want := daily.Next(at).String(), "2027-03-28 03:00:00 +0100 BST"; got != want {
		t.Errorf("daily at 03:00 from %s = %s, want %s", at, got, want)
	}
}

func TestNextIsAlwaysStrictlyAfterTheInstantItWasGiven(t *testing.T) {
	loc := london(t)
	schedules := []Schedule{
		{Kind: KindDaily, AtMinute: 0, Location: loc},
		{Kind: KindDaily, AtMinute: 90, Location: loc},
		{Kind: KindDaily, AtMinute: 23*60 + 59, Location: loc},
		{Kind: KindWeekly, Weekday: time.Sunday, AtMinute: 90, Location: loc},
		{Kind: KindWeekly, Weekday: time.Wednesday, AtMinute: 12 * 60, Location: loc},
		{Kind: KindInterval, Every: MinInterval, Location: loc},
	}

	// Every hour of both transition days, plus a plain one, and the exact
	// instant of a window fed straight back in.
	var instants []time.Time
	for _, day := range []time.Time{
		time.Date(2027, 3, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 10, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 6, 15, 0, 0, 0, 0, time.UTC),
	} {
		for h := range 24 {
			instants = append(instants, day.Add(time.Duration(h)*time.Hour))
		}
	}

	for _, s := range schedules {
		for _, at := range instants {
			next := s.Next(at)
			if !next.After(at) {
				t.Errorf("%s: next after %s = %s, which is not after it", s, at, next)
			}
			if again := s.Next(next); !again.After(next) {
				t.Errorf("%s: next after its own window %s = %s", s, next, again)
			}
		}
	}
}

func TestAnUnknownKindHasNoNextWindow(t *testing.T) {
	// Rather than a panic or a zero-length loop in whatever fires these: a
	// schedule that cannot be read is a schedule that never comes due, and
	// Validate is what refuses to store one in the first place.
	var s Schedule
	if got := s.Next(time.Now()); !got.IsZero() {
		t.Errorf("an empty schedule came due at %s", got)
	}
}

func TestValidateRefusesAScheduleThatCouldNotFire(t *testing.T) {
	loc := time.UTC
	tests := []struct {
		name string
		s    Schedule
		ok   bool
	}{
		{"daily", Schedule{Kind: KindDaily, AtMinute: 120, Location: loc}, true},
		{"midnight", Schedule{Kind: KindDaily, Location: loc}, true},
		{"last minute of the day", Schedule{Kind: KindDaily, AtMinute: 1439, Location: loc}, true},
		{"a minute past the day", Schedule{Kind: KindDaily, AtMinute: 1440, Location: loc}, false},
		{"before midnight", Schedule{Kind: KindDaily, AtMinute: -1, Location: loc}, false},
		{"weekly", Schedule{Kind: KindWeekly, Weekday: time.Saturday, AtMinute: 120}, true},
		{"an eighth day", Schedule{Kind: KindWeekly, Weekday: 7, AtMinute: 120}, false},
		{"interval", Schedule{Kind: KindInterval, Every: time.Hour}, true},
		{"too often", Schedule{Kind: KindInterval, Every: time.Second}, false},
		{"never", Schedule{Kind: KindInterval}, false},
		{"too rarely", Schedule{Kind: KindInterval, Every: MaxInterval + time.Hour}, false},
		{"no kind", Schedule{}, false},
		{"a kind nobody wrote", Schedule{Kind: "monthly", AtMinute: 120}, false},

		// A field the kind does not read is ignored rather than policed, so
		// that switching a schedule in the interface does not have to clear
		// the fields the previous kind used.
		{"a daily schedule that used to be weekly",
			Schedule{Kind: KindDaily, AtMinute: 120, Weekday: time.Friday, Every: time.Hour}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.s.Validate()
			if tt.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("accepted a schedule that could not fire")
			}
		})
	}
}

func TestAnEmptyZoneIsTheServersOwnAndNotUTC(t *testing.T) {
	// time.LoadLocation("") is UTC, which would move every schedule nobody
	// chose a zone for by an hour for half the year, in exactly the countries
	// that observe summer time and nowhere else.
	loc, err := LoadLocation("")
	if err != nil {
		t.Fatalf("loading the empty zone: %v", err)
	}
	if loc != time.Local {
		t.Errorf("the empty zone loaded as %s, want the server's own", loc)
	}

	var s Schedule
	if s.Loc() != time.Local {
		t.Errorf("a schedule with no zone reads its times in %s", s.Loc())
	}
	if got, want := s.Zone(), time.Local.String(); got != want {
		t.Errorf("zone name %q, want %q", got, want)
	}
}

func TestAZoneNameRoundTrips(t *testing.T) {
	for _, name := range []string{"Europe/London", "America/New_York", "UTC", "Local", ""} {
		loc, err := LoadLocation(name)
		if err != nil {
			t.Fatalf("loading %q: %v", name, err)
		}
		s := Schedule{Kind: KindDaily, Location: loc}
		again, err := LoadLocation(s.Zone())
		if err != nil {
			t.Fatalf("reloading %q as %q: %v", name, s.Zone(), err)
		}
		// By name, not by pointer: time.LoadLocation builds a fresh
		// *Location every call for a zone that is not one of the two
		// singletons, so two loads of Europe/London are not the same pointer
		// and never were.
		if again.String() != loc.String() {
			t.Errorf("%q round-tripped through %q to %s", name, s.Zone(), again)
		}
	}

	if _, err := LoadLocation("Middle/Earth"); err == nil {
		t.Error("a zone that does not exist loaded anyway")
	}
}

func TestScheduleDescribesItselfForALogLine(t *testing.T) {
	loc := time.UTC
	tests := []struct {
		s    Schedule
		want string
	}{
		{Schedule{Kind: KindDaily, AtMinute: 2 * 60, Location: loc}, "daily at 02:00 UTC"},
		{Schedule{Kind: KindDaily, AtMinute: 1439, Location: loc}, "daily at 23:59 UTC"},
		{Schedule{Kind: KindWeekly, Weekday: time.Sunday, AtMinute: 90, Location: loc},
			"weekly on Sunday at 01:30 UTC"},
		{Schedule{Kind: KindInterval, Every: 6 * time.Hour}, "every 6h0m0s"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}
