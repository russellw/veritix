package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/russellw/veritix/internal/schedule"
	"github.com/russellw/veritix/internal/store"
)

// scheduleJSON is a dataset's standing instruction to audit itself, on the
// wire.
//
// The time of day is "HH:MM" rather than a count of minutes because that is
// what an <input type="time"> produces and what a person reads, and the
// weekday is a name for the same reason. The alternative saves the server
// three lines and costs every client the same three.
type scheduleJSON struct {
	// Kind is "daily", "weekly" or "interval".
	Kind string `json:"kind"`
	// At is the time of day, "HH:MM", for daily and weekly.
	At string `json:"at,omitempty"`
	// Weekday is the day a weekly schedule fires on, lowercase English.
	Weekday string `json:"weekday,omitempty"`
	// EveryMinutes is the gap between windows, for an interval schedule.
	EveryMinutes int `json:"every_minutes,omitempty"`
	// Timezone is the IANA zone the time of day is read in. Empty is the
	// server's own zone, and comes back as "Local" to say so.
	Timezone string `json:"timezone,omitempty"`
	// Notify sends this dataset's regressions to the sink the operator
	// configured. Where that sink is is not settable here: it is an egress
	// decision, and it lives in the configuration with the model provider.
	Notify bool `json:"notify"`

	// The rest is what happened, and is ignored on the way in.
	NextDueAt *time.Time `json:"next_due_at,omitempty"`
	LastRunID string     `json:"last_run_id,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

func toScheduleJSON(sc *store.Schedule) *scheduleJSON {
	out := &scheduleJSON{
		Kind:     string(sc.Spec.Kind),
		Timezone: sc.Spec.Zone(),
		Notify:   sc.Notify,

		LastRunID: sc.LastRunID,
		LastError: sc.LastError,
	}
	switch sc.Spec.Kind {
	case schedule.KindDaily:
		out.At = sc.Spec.At()
	case schedule.KindWeekly:
		out.At = sc.Spec.At()
		out.Weekday = strings.ToLower(sc.Spec.Weekday.String())
	case schedule.KindInterval:
		out.EveryMinutes = int(sc.Spec.Every / time.Minute)
	}
	if !sc.NextDueAt.IsZero() {
		// In the schedule's own zone, so that the offset in the timestamp says
		// which 02:00 this is. A browser can still render it locally.
		due := sc.NextDueAt.In(sc.Spec.Loc())
		out.NextDueAt = &due
	}
	if !sc.CreatedAt.IsZero() {
		out.CreatedAt = &sc.CreatedAt
	}
	return out
}

// spec turns a request into a schedule, or says why it is not one.
func (j *scheduleJSON) spec() (schedule.Schedule, error) {
	s := schedule.Schedule{Kind: schedule.Kind(j.Kind)}

	loc, err := schedule.LoadLocation(j.Timezone)
	if err != nil {
		return s, newRunError(http.StatusBadRequest, "%s", err)
	}
	s.Location = loc

	switch s.Kind {
	case schedule.KindDaily, schedule.KindWeekly:
		if s.AtMinute, err = schedule.ParseAt(j.At); err != nil {
			return s, newRunError(http.StatusBadRequest, "%s", err)
		}
		if s.Kind == schedule.KindWeekly {
			if s.Weekday, err = schedule.ParseWeekday(j.Weekday); err != nil {
				return s, newRunError(http.StatusBadRequest, "%s", err)
			}
		}
	case schedule.KindInterval:
		s.Every = time.Duration(j.EveryMinutes) * time.Minute
	}

	if err := s.Validate(); err != nil {
		return s, newRunError(http.StatusBadRequest, "%s", err)
	}
	return s, nil
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.Dataset(r.Context(), r.PathValue("datasetId"))
	if err != nil {
		s.writeStoreError(w, err, "could not read the dataset")
		return
	}
	sc, err := s.store.Schedule(r.Context(), ds.ID)
	if err != nil {
		s.writeStoreError(w, err, "could not read the schedule")
		return
	}
	writeJSON(w, http.StatusOK, toScheduleJSON(sc))
}

func (s *Server) handleSetSchedule(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.Dataset(r.Context(), r.PathValue("datasetId"))
	if err != nil {
		s.writeStoreError(w, err, "could not read the dataset")
		return
	}
	// An upload is a copy of the data as it was at the moment somebody sent
	// it, and it does not change again. Auditing it every night would produce
	// the same report forever and a comparison that never says anything, which
	// would look like a working schedule.
	if ds.Uploaded {
		writeError(w, http.StatusBadRequest,
			"%s was uploaded, so it never changes; schedule a dataset registered by path instead",
			ds.Name)
		return
	}

	var req scheduleJSON
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	spec, err := req.spec()
	if err != nil {
		s.writeRunError(w, err)
		return
	}

	sc := &store.Schedule{
		DatasetID: ds.ID,
		Spec:      spec,
		Notify:    req.Notify,
		// From now, so that editing a schedule takes effect at once rather
		// than at whatever window the previous one was waiting for.
		NextDueAt: spec.Next(time.Now()),
	}
	// What already happened stays true across an edit: moving an audit from
	// 02:00 to 03:00 does not unmake last night's run, and a schedule screen
	// that forgot it would look like one that had never run.
	if prev, err := s.store.Schedule(r.Context(), ds.ID); err == nil {
		sc.CreatedAt = prev.CreatedAt
		sc.LastRunID = prev.LastRunID
		sc.LastError = prev.LastError
	} else if !errors.Is(err, store.ErrNotFound) {
		s.writeStoreError(w, err, "could not read the schedule")
		return
	}

	if err := s.store.SetSchedule(r.Context(), sc); err != nil {
		s.writeStoreError(w, err, "could not save the schedule")
		return
	}
	s.log.Info("scheduled an audit", "dataset", ds.ID, "schedule", spec.String(),
		"next_due", sc.NextDueAt, "notify", sc.Notify)

	writeJSON(w, http.StatusOK, toScheduleJSON(sc))
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.Dataset(r.Context(), r.PathValue("datasetId"))
	if err != nil {
		s.writeStoreError(w, err, "could not read the dataset")
		return
	}
	if err := s.store.DeleteSchedule(r.Context(), ds.ID); err != nil {
		s.writeStoreError(w, err, "could not delete the schedule")
		return
	}
	s.log.Info("stopped auditing on a schedule", "dataset", ds.ID)
	w.WriteHeader(http.StatusNoContent)
}
