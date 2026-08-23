package api

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/russellw/veritix/internal/schedule"
)

func TestAScheduleIsSetReadAndDeleted(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()

	var got scheduleJSON
	ts.decode(ts.do(http.MethodPut, "/api/v1/datasets/"+ds+"/schedule", map[string]any{
		"kind": "daily", "at": "02:00", "timezone": "Europe/London", "notify": true,
	}), http.StatusOK, &got)

	if got.Kind != "daily" || got.At != "02:00" || got.Timezone != "Europe/London" || !got.Notify {
		t.Errorf("stored schedule came back as %+v", got)
	}
	if got.NextDueAt == nil || !got.NextDueAt.After(time.Now()) {
		t.Fatalf("next window is %v, want one in the future", got.NextDueAt)
	}
	// In the schedule's own zone, so the offset in the timestamp says which
	// 02:00 this is.
	if h, m, _ := got.NextDueAt.Clock(); h != 2 || m != 0 {
		t.Errorf("next window is at %02d:%02d local to the schedule, want 02:00", h, m)
	}

	var read scheduleJSON
	ts.decode(ts.do(http.MethodGet, "/api/v1/datasets/"+ds+"/schedule", nil), http.StatusOK, &read)
	if read.Kind != got.Kind || !read.NextDueAt.Equal(*got.NextDueAt) {
		t.Errorf("read back %+v, want %+v", read, got)
	}

	if resp := ts.do(http.MethodDelete, "/api/v1/datasets/"+ds+"/schedule", nil); resp.Status != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", resp.Status, resp.Body)
	}
	if resp := ts.do(http.MethodGet, "/api/v1/datasets/"+ds+"/schedule", nil); resp.Status != http.StatusNotFound {
		t.Errorf("reading a deleted schedule = %d, want 404: %s", resp.Status, resp.Body)
	}
	// Asking for it gone again is not an error.
	if resp := ts.do(http.MethodDelete, "/api/v1/datasets/"+ds+"/schedule", nil); resp.Status != http.StatusNoContent {
		t.Errorf("deleting twice = %d, want 204", resp.Status)
	}
}

func TestEachKindOfScheduleRoundTripsOverHTTP(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()

	tests := []struct {
		name string
		body map[string]any
		want scheduleJSON
	}{
		{"daily", map[string]any{"kind": "daily", "at": "23:59"},
			scheduleJSON{Kind: "daily", At: "23:59", Timezone: "Local"}},
		{"weekly", map[string]any{"kind": "weekly", "at": "02:00", "weekday": "sunday", "timezone": "UTC"},
			scheduleJSON{Kind: "weekly", At: "02:00", Weekday: "sunday", Timezone: "UTC"}},
		{"interval", map[string]any{"kind": "interval", "every_minutes": 360, "timezone": "UTC"},
			scheduleJSON{Kind: "interval", EveryMinutes: 360, Timezone: "UTC"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got scheduleJSON
			ts.decode(ts.do(http.MethodPut, "/api/v1/datasets/"+ds+"/schedule", tt.body),
				http.StatusOK, &got)
			if got.Kind != tt.want.Kind || got.At != tt.want.At ||
				got.Weekday != tt.want.Weekday || got.EveryMinutes != tt.want.EveryMinutes ||
				got.Timezone != tt.want.Timezone {
				t.Errorf("came back as %+v, want %+v", got, tt.want)
			}
			if got.NextDueAt == nil || !got.NextDueAt.After(time.Now()) {
				t.Errorf("next window is %v, want one in the future", got.NextDueAt)
			}
		})
	}
}

func TestAScheduleThatCouldNotFireIsRefusedWithWhatToChange(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()

	tests := []struct {
		name string
		body map[string]any
	}{
		{"no kind", map[string]any{"at": "02:00"}},
		{"a kind nobody wrote", map[string]any{"kind": "monthly", "at": "02:00"}},
		{"no time of day", map[string]any{"kind": "daily"}},
		{"a time that is not one", map[string]any{"kind": "daily", "at": "2am"}},
		{"an hour that does not exist", map[string]any{"kind": "daily", "at": "25:00"}},
		{"weekly with no day", map[string]any{"kind": "weekly", "at": "02:00"}},
		{"a day nobody has", map[string]any{"kind": "weekly", "at": "02:00", "weekday": "funday"}},
		{"an interval of nothing", map[string]any{"kind": "interval"}},
		{"an interval too short to matter", map[string]any{"kind": "interval", "every_minutes": 0}},
		{"a zone that does not exist", map[string]any{"kind": "daily", "at": "02:00", "timezone": "Middle/Earth"}},
		{"a field nobody has", map[string]any{"kind": "daily", "at": "02:00", "notifi": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := ts.do(http.MethodPut, "/api/v1/datasets/"+ds+"/schedule", tt.body)
			if resp.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.Status, resp.Body)
			}
			// Every message out of rules.Validate says what to change
			// rather than only what is wrong, and a schedule is no
			// different: this is a person in a browser, not a log file.
			var body errorBody
			ts.decode(resp, http.StatusBadRequest, &body)
			if len(body.Error) < 20 {
				t.Errorf("refusal said only %q", body.Error)
			}
		})
	}

	// And nothing was stored by any of them.
	if resp := ts.do(http.MethodGet, "/api/v1/datasets/"+ds+"/schedule", nil); resp.Status != http.StatusNotFound {
		t.Errorf("a refused schedule was stored anyway: %d %s", resp.Status, resp.Body)
	}
}

func TestAnUploadedDatasetCannotBeScheduled(t *testing.T) {
	ts := newTestServer(t, "")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("files", "customers.csv")
	if err != nil {
		t.Fatalf("build upload: %v", err)
	}
	if _, err := part.Write([]byte("id,name\n1,Ada\n")); err != nil {
		t.Fatalf("build upload: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("build upload: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/api/v1/datasets", &body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	var ds datasetJSON
	ts.decode(ts.send(req), http.StatusCreated, &ds)

	// An upload is a copy of the data as it was. Auditing it nightly would
	// produce the same report forever and a comparison that never says
	// anything, which looks exactly like a schedule that is working.
	resp := ts.do(http.MethodPut, "/api/v1/datasets/"+ds.ID+"/schedule",
		map[string]any{"kind": "daily", "at": "02:00"})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Status, resp.Body)
	}
	if !bytes.Contains(resp.Body, []byte("registered by path")) {
		t.Errorf("refusal does not say what to do instead: %s", resp.Body)
	}
}

func TestEditingAScheduleKeepsWhatAlreadyHappened(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()
	ctx := context.Background()

	ts.decode(ts.do(http.MethodPut, "/api/v1/datasets/"+ds+"/schedule", map[string]any{
		"kind": "daily", "at": "02:00", "timezone": "UTC",
	}), http.StatusOK, nil)

	// A window fires, as the clock will do in its own right shortly.
	sc, err := ts.st.Schedule(ctx, ds)
	if err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if err := ts.st.ScheduleFired(ctx, ds, sc.Spec.Next(sc.NextDueAt), "run-42", ""); err != nil {
		t.Fatalf("record fired: %v", err)
	}

	var got scheduleJSON
	ts.decode(ts.do(http.MethodPut, "/api/v1/datasets/"+ds+"/schedule", map[string]any{
		"kind": "daily", "at": "03:00", "timezone": "UTC",
	}), http.StatusOK, &got)

	// Moving an audit from 02:00 to 03:00 does not unmake last night's run,
	// and a screen that forgot it would look like one that had never run.
	if got.LastRunID != "run-42" {
		t.Errorf("last run came back as %q, want run-42", got.LastRunID)
	}
	// But the window itself moves at once, rather than waiting for whatever
	// the schedule it replaced was waiting for.
	if got.NextDueAt == nil {
		t.Fatal("no next window")
	}
	if h, m, _ := got.NextDueAt.Clock(); h != 3 || m != 0 {
		t.Errorf("next window is at %02d:%02d, want 03:00", h, m)
	}
}

func TestAScheduleForADatasetNobodyRegisteredIs404(t *testing.T) {
	ts := newTestServer(t, "")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		var body map[string]any
		if method == http.MethodPut {
			body = map[string]any{"kind": "daily", "at": "02:00"}
		}
		resp := ts.do(method, "/api/v1/datasets/nope/schedule", body)
		if resp.Status != http.StatusNotFound {
			t.Errorf("%s = %d, want 404: %s", method, resp.Status, resp.Body)
		}
	}
}

func TestASchedulesWindowSurvivesTheStore(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()

	var put scheduleJSON
	ts.decode(ts.do(http.MethodPut, "/api/v1/datasets/"+ds+"/schedule", map[string]any{
		"kind": "weekly", "at": "01:30", "weekday": "sunday", "timezone": "Europe/London",
	}), http.StatusOK, &put)

	sc, err := ts.st.Schedule(context.Background(), ds)
	if err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if sc.Spec.Kind != schedule.KindWeekly || sc.Spec.Weekday != time.Sunday {
		t.Errorf("stored spec is %s", sc.Spec)
	}
	if !sc.NextDueAt.Equal(*put.NextDueAt) {
		t.Errorf("stored window %s, answered %s", sc.NextDueAt, put.NextDueAt)
	}
	// The row is what a ticker will read, so it has to be found by the query
	// that reads all of them rather than only by dataset id.
	all, err := ts.st.Schedules(context.Background())
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}
	if len(all) != 1 || all[0].DatasetID != ds {
		t.Errorf("listed %d schedules, want the one just set", len(all))
	}
}
