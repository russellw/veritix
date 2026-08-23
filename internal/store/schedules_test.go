package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/russellw/veritix/internal/schedule"
)

func TestAScheduleRoundTripsThroughTheStore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "nightly", "/data/nightly", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}

	london, err := schedule.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	spec := schedule.Schedule{
		Kind: schedule.KindWeekly, Weekday: time.Sunday, AtMinute: 2 * 60, Location: london,
	}
	due := spec.Next(time.Date(2027, 3, 1, 0, 0, 0, 0, london))

	want := &Schedule{DatasetID: ds.ID, Spec: spec, Notify: true, NextDueAt: due}
	if err := s.SetSchedule(ctx, want); err != nil {
		t.Fatalf("set schedule: %v", err)
	}

	got, err := s.Schedule(ctx, ds.ID)
	if err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if got.Spec.Kind != spec.Kind || got.Spec.Weekday != spec.Weekday ||
		got.Spec.AtMinute != spec.AtMinute {
		t.Errorf("schedule came back as %+v, want %+v", got.Spec, spec)
	}
	if got.Spec.Zone() != "Europe/London" {
		t.Errorf("zone came back as %q", got.Spec.Zone())
	}
	if !got.Notify {
		t.Error("notify came back off")
	}
	if !got.NextDueAt.Equal(due) {
		t.Errorf("next due came back as %s, want %s", got.NextDueAt, due)
	}
	// The window has to survive the round trip as an instant a comparison can
	// use, and it has to still be read in the schedule's own zone.
	// Equal and not ==: the reloaded zone is a different *time.Location than
	// the one that was stored, so two identical instants are not identical
	// structs.
	if !got.Spec.Next(got.NextDueAt).Equal(spec.Next(due)) {
		t.Errorf("the window after the stored one moved to %s, want %s",
			got.Spec.Next(got.NextDueAt), spec.Next(due))
	}
}

func TestReplacingAScheduleKeepsTheDatasetsOwn(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "nightly", "/data/nightly", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}

	created := time.Now().Add(-72 * time.Hour).Truncate(time.Millisecond)
	first := &Schedule{
		DatasetID: ds.ID,
		Spec:      schedule.Schedule{Kind: schedule.KindDaily, AtMinute: 120},
		NextDueAt: time.Now(),
		CreatedAt: created,
		LastRunID: "run-1",
	}
	if err := s.SetSchedule(ctx, first); err != nil {
		t.Fatalf("set schedule: %v", err)
	}

	second := &Schedule{
		DatasetID: ds.ID,
		Spec:      schedule.Schedule{Kind: schedule.KindInterval, Every: 6 * time.Hour},
		NextDueAt: time.Now().Add(time.Hour),
		CreatedAt: created,
	}
	if err := s.SetSchedule(ctx, second); err != nil {
		t.Fatalf("replace schedule: %v", err)
	}

	got, err := s.Schedule(ctx, ds.ID)
	if err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if got.Spec.Kind != schedule.KindInterval || got.Spec.Every != 6*time.Hour {
		t.Errorf("schedule came back as %s, want the replacement", got.Spec)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("created at %s, want the original %s", got.CreatedAt, created)
	}
	// Editing the time of day does not mean the last window never happened,
	// but it does not carry a stale run either: the caller says what it knows.
	if got.LastRunID != "" {
		t.Errorf("last run came back as %q after a replacement that named none", got.LastRunID)
	}
}

func TestTheStoreRefusesAScheduleThatCouldNotFire(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "nightly", "/data/nightly", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}

	// The API validates too, but a row nothing can read is worse than a
	// refused write: Schedules is read by a ticker with nobody watching.
	bad := &Schedule{DatasetID: ds.ID, Spec: schedule.Schedule{Kind: "monthly"}}
	if err := s.SetSchedule(ctx, bad); err == nil {
		t.Fatal("stored a schedule that has no windows")
	}
}

func TestAWindowRecordsWhatItDid(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "nightly", "/data/nightly", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	spec := schedule.Schedule{Kind: schedule.KindDaily, AtMinute: 120, Location: time.UTC}
	due := spec.Next(time.Now())
	if err := s.SetSchedule(ctx, &Schedule{DatasetID: ds.ID, Spec: spec, NextDueAt: due}); err != nil {
		t.Fatalf("set schedule: %v", err)
	}

	next := spec.Next(due)
	if err := s.ScheduleFired(ctx, ds.ID, next, "run-7", ""); err != nil {
		t.Fatalf("record fired: %v", err)
	}
	got, err := s.Schedule(ctx, ds.ID)
	if err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if !got.NextDueAt.Equal(next) || got.LastRunID != "run-7" || got.LastError != "" {
		t.Errorf("after firing: due %s, run %q, error %q", got.NextDueAt, got.LastRunID, got.LastError)
	}

	// A window that could not start a run still advances, or the same failure
	// is retried on every tick forever.
	after := spec.Next(next)
	if err := s.ScheduleFired(ctx, ds.ID, after, "", "the dataset path has gone"); err != nil {
		t.Fatalf("record failed window: %v", err)
	}
	got, err = s.Schedule(ctx, ds.ID)
	if err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if !got.NextDueAt.Equal(after) || got.LastRunID != "" || got.LastError == "" {
		t.Errorf("after a failed window: due %s, run %q, error %q",
			got.NextDueAt, got.LastRunID, got.LastError)
	}

	if err := s.ScheduleFired(ctx, "no-such-dataset", after, "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("recording against a dataset with no schedule = %v, want ErrNotFound", err)
	}
}

func TestSchedulesAreListedAndDeleted(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	now := time.Now()
	for i, name := range []string{"a", "b", "c"} {
		ds, err := s.CreateDataset(ctx, name, "/data/"+name, false)
		if err != nil {
			t.Fatalf("create dataset: %v", err)
		}
		if name == "c" {
			continue // a dataset with no schedule has no row
		}
		sc := &Schedule{
			DatasetID: ds.ID,
			Spec:      schedule.Schedule{Kind: schedule.KindDaily, AtMinute: 60 * i, Location: time.UTC},
			NextDueAt: now.Add(time.Duration(2-i) * time.Hour),
		}
		if err := s.SetSchedule(ctx, sc); err != nil {
			t.Fatalf("set schedule: %v", err)
		}
	}

	all, err := s.Schedules(ctx)
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d schedules, want 2", len(all))
	}
	if !all[0].NextDueAt.Before(all[1].NextDueAt) {
		t.Errorf("listed out of order: %s then %s", all[0].NextDueAt, all[1].NextDueAt)
	}

	if err := s.DeleteSchedule(ctx, all[0].DatasetID); err != nil {
		t.Fatalf("delete schedule: %v", err)
	}
	if _, err := s.Schedule(ctx, all[0].DatasetID); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading a deleted schedule = %v, want ErrNotFound", err)
	}
	// Asking for it gone twice is not an error: it is gone.
	if err := s.DeleteSchedule(ctx, all[0].DatasetID); err != nil {
		t.Errorf("deleting a schedule twice: %v", err)
	}
}

func TestDeletingADatasetStopsAuditingIt(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "nightly", "/data/nightly", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	sc := &Schedule{
		DatasetID: ds.ID,
		Spec:      schedule.Schedule{Kind: schedule.KindDaily, AtMinute: 120, Location: time.UTC},
		NextDueAt: time.Now(),
	}
	if err := s.SetSchedule(ctx, sc); err != nil {
		t.Fatalf("set schedule: %v", err)
	}

	if err := s.DeleteDataset(ctx, ds.ID); err != nil {
		t.Fatalf("delete dataset: %v", err)
	}

	// The cascade is the point: a dataset that is gone must not leave behind a
	// standing instruction to audit it every night.
	all, err := s.Schedules(ctx)
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("%d schedules survived the dataset", len(all))
	}
}
