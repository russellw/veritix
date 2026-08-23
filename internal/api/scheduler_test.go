package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/schedule"
	"github.com/russellw/veritix/internal/store"
)

// dueSchedule gives a dataset a daily schedule whose window fell `ago` in the
// past, which is what the clock would have found had it been running then.
func (ts *testServer) dueSchedule(datasetID string, ago time.Duration) *store.Schedule {
	ts.t.Helper()

	spec := schedule.Schedule{Kind: schedule.KindDaily, AtMinute: 2 * 60, Location: time.UTC}
	sc := &store.Schedule{
		DatasetID: datasetID,
		Spec:      spec,
		NextDueAt: time.Now().Add(-ago),
	}
	if err := ts.st.SetSchedule(context.Background(), sc); err != nil {
		ts.t.Fatalf("set schedule: %v", err)
	}
	return sc
}

func (ts *testServer) schedule(datasetID string) *store.Schedule {
	ts.t.Helper()
	sc, err := ts.st.Schedule(context.Background(), datasetID)
	if err != nil {
		ts.t.Fatalf("read schedule: %v", err)
	}
	return sc
}

// A due window starts a real audit, and it is an ordinary run: in the history,
// on the event stream, with a report at the end.
func TestADueWindowStartsAnOrdinaryAudit(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()
	before := ts.dueSchedule(ds, time.Hour)

	ts.srv.sched.sweep(context.Background())

	var runs struct {
		Runs []*runJSON `json:"runs"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs?dataset_id="+ds, nil), http.StatusOK, &runs)
	if len(runs.Runs) != 1 {
		t.Fatalf("the sweep started %d runs, want 1", len(runs.Runs))
	}
	run := ts.awaitDone(runs.Runs[0].ID)
	if run.Status != string(store.StatusSucceeded) {
		t.Fatalf("scheduled run: %s (%s)", run.Status, run.Message)
	}
	ts.decode(ts.get("/api/v1/runs/"+run.ID+"/report"), http.StatusOK, nil)

	after := ts.schedule(ds)
	if after.LastRunID != run.ID {
		t.Errorf("the schedule recorded run %q, want %q", after.LastRunID, run.ID)
	}
	if after.LastError != "" {
		t.Errorf("the schedule recorded an error: %q", after.LastError)
	}
	if !after.NextDueAt.After(time.Now()) {
		t.Errorf("the window did not advance: still %s", after.NextDueAt)
	}
	if !after.NextDueAt.After(before.NextDueAt) {
		t.Errorf("the window went backwards")
	}
}

// The window that was missed fires once, not once for every tick that finds it.
func TestAMissedWindowFiresOnceAndOnlyOnce(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()
	ts.dueSchedule(ds, 3*time.Hour)

	ts.srv.sched.sweep(context.Background())
	ts.srv.sched.sweep(context.Background())
	ts.srv.sched.sweep(context.Background())

	var runs struct {
		Runs []*runJSON `json:"runs"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs?dataset_id="+ds, nil), http.StatusOK, &runs)
	if len(runs.Runs) != 1 {
		t.Fatalf("three sweeps started %d runs, want 1", len(runs.Runs))
	}
	ts.awaitDone(runs.Runs[0].ID)
}

// A window missed by more than the schedule's own period was not missed once.
// Auditing week-old state the moment the server comes back is not what "daily
// at 02:00" asked for, and the reason has to be visible or it looks like a
// schedule that silently stopped.
func TestAWindowMissedForLongerThanItsPeriodIsNotCaughtUp(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()
	ts.dueSchedule(ds, 3*24*time.Hour)

	ts.srv.sched.sweep(context.Background())

	var runs struct {
		Runs []*runJSON `json:"runs"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs?dataset_id="+ds, nil), http.StatusOK, &runs)
	if len(runs.Runs) != 0 {
		t.Fatalf("the sweep started %d runs, want none", len(runs.Runs))
	}

	after := ts.schedule(ds)
	if after.LastError == "" {
		t.Error("nothing recorded why the audit did not happen")
	}
	if !after.NextDueAt.After(time.Now()) {
		t.Errorf("the window did not advance: still %s", after.NextDueAt)
	}
}

// An audit of two gigabytes takes fifteen minutes and an hourly schedule on
// something slower must not stack.
func TestAWindowDoesNotStartASecondAuditOfTheSameDataset(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerFixture()
	ts.dueSchedule(ds, time.Hour)
	ctx := context.Background()

	// A run recorded as in flight, without executing one: this is about the
	// decision, and a real audit of the fixture would be over before the
	// sweep could see it.
	inflight, err := ts.st.CreateRun(ctx, ds, "test", filepath.Join(t.TempDir(), "x.duckdb"))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ts.st.StartRun(ctx, inflight.ID); err != nil {
		t.Fatalf("start run: %v", err)
	}

	ts.srv.sched.sweep(ctx)

	var runs struct {
		Runs []*runJSON `json:"runs"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs?dataset_id="+ds, nil), http.StatusOK, &runs)
	if len(runs.Runs) != 1 || runs.Runs[0].ID != inflight.ID {
		t.Fatalf("the sweep started a second audit: %d runs", len(runs.Runs))
	}

	after := ts.schedule(ds)
	if after.LastError == "" {
		t.Error("nothing recorded why the window was skipped")
	}
	if after.LastRunID != "" {
		t.Errorf("a skipped window claimed run %q", after.LastRunID)
	}
	if !after.NextDueAt.After(time.Now()) {
		t.Error("a skipped window did not advance, so it will be retried on every tick")
	}
}

// A server coming back after a weekend with several datasets due does not
// start them all at once.
func TestOneAuditStartsPerSweep(t *testing.T) {
	ts := newTestServer(t, "")
	ctx := context.Background()

	first := ts.registerFixture()
	second := ts.registerCopy(t.TempDir())
	ts.dueSchedule(first, 2*time.Hour)
	ts.dueSchedule(second, time.Hour)

	count := func(ds string) int {
		var runs struct {
			Runs []*runJSON `json:"runs"`
		}
		ts.decode(ts.do(http.MethodGet, "/api/v1/runs?dataset_id="+ds, nil), http.StatusOK, &runs)
		return len(runs.Runs)
	}

	ts.srv.sched.sweep(ctx)
	if got := count(first) + count(second); got != 1 {
		t.Fatalf("the first sweep started %d audits, want 1", got)
	}
	// The one left behind is still due, so it goes on the next tick rather
	// than waiting for tomorrow.
	ts.srv.sched.sweep(ctx)
	if count(first) != 1 || count(second) != 1 {
		t.Fatalf("after two sweeps: %d and %d audits", count(first), count(second))
	}
}

// The clock is wired up, not just callable: this drives the real ticker.
func TestTheClockFiresWithoutBeingAsked(t *testing.T) {
	ts := newTestServerTuned(t, "", nil, func(cfg *config.Config) {
		cfg.Schedule.Tick = 50 * time.Millisecond
	})
	ds := ts.registerFixture()
	ts.dueSchedule(ds, time.Hour)

	deadline := time.Now().Add(30 * time.Second)
	for {
		if sc := ts.schedule(ds); sc.LastRunID != "" {
			ts.awaitDone(sc.LastRunID)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the clock never fired a window that was due")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTheClockCanBeTurnedOff(t *testing.T) {
	ts := newTestServerTuned(t, "", nil, func(cfg *config.Config) {
		cfg.Schedule.Enabled = false
	})
	ds := ts.registerFixture()
	ts.dueSchedule(ds, time.Hour)

	// The ticker is still there, because discarding expired run databases uses
	// it and is not something an operator turned off. It fires no windows.
	ts.srv.sched.tickOnce(context.Background())

	var runs struct {
		Runs []*runJSON `json:"runs"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs?dataset_id="+ds, nil), http.StatusOK, &runs)
	if len(runs.Runs) != 0 {
		t.Errorf("the clock started %d audits with schedule.enabled off", len(runs.Runs))
	}

	// The schedule is still stored and still readable: turning the clock off
	// is about this process, not about the commitment.
	ts.decode(ts.do(http.MethodGet, "/api/v1/datasets/"+ds+"/schedule", nil), http.StatusOK, nil)
}

// A nightly audit that sent this dataset's metadata to a model, unattended,
// forever, is exactly the decision the per-run switch exists to make
// deliberate. A schedule does not carry one.
func TestAScheduledAuditRunsNoModel(t *testing.T) {
	model := newStubModel(t)
	ts := newTestServerTuned(t, "", nil, func(cfg *config.Config) {
		cfg.LLM.Provider = config.ProviderOpenAICompatible
		cfg.LLM.BaseURL = model.URL
		cfg.LLM.Model = "stub"
	})
	ds := ts.registerFixture()
	ts.dueSchedule(ds, time.Hour)

	ts.srv.sched.sweep(context.Background())

	sc := ts.schedule(ds)
	if sc.LastRunID == "" {
		t.Fatalf("no audit started: %q", sc.LastError)
	}
	run := ts.awaitDone(sc.LastRunID)
	if run.Status != string(store.StatusSucceeded) {
		t.Fatalf("scheduled run: %s (%s)", run.Status, run.Message)
	}

	if n := model.count(); n != 0 {
		t.Errorf("a scheduled audit sent %d requests to the model", n)
	}
	// And nothing to show in the trace, because there was nothing to record.
	if resp := ts.get("/api/v1/runs/" + run.ID + "/trace"); resp.Status != http.StatusNotFound {
		t.Errorf("trace = %d, want 404 for a run with no model", resp.Status)
	}
}

// Retention is what makes scheduling survivable: every run leaves a DuckDB
// file behind at roughly a third of the dataset's size, and a nightly audit of
// a two gigabyte export is 700 MB a night.
func TestOldRunDataIsDiscardedAndTheAuditTrailIsNot(t *testing.T) {
	ts := newTestServerTuned(t, "", nil, retention(time.Hour))
	ctx := context.Background()
	ds := ts.registerFixture()

	old := ts.startRun(map[string]any{"dataset_id": ds})
	recent := ts.startRun(map[string]any{"dataset_id": ds})

	// Move the clock past the cutoff rather than editing the runs: it ages
	// every run at once, which is also the harder case — the most recent run
	// is then over the cutoff too, and has to be kept anyway.
	ts.srv.sched.now = func() time.Time { return time.Now().Add(48 * time.Hour) }

	oldRun, err := ts.st.Run(ctx, old.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	dbPath := oldRun.DatabasePath
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("the run kept no database to discard: %v", err)
	}

	ts.srv.sched.tickOnce(ctx)

	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("the expired run database is still there: %v", err)
	}
	if dir := filepath.Dir(dbPath); dirExists(t, dir) {
		t.Errorf("the run directory %s survived", dir)
	}

	// The audit trail is untouched: the run, its counts, its report.
	var run runJSON
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs/"+old.ID, nil), http.StatusOK, &run)
	if run.Status != string(store.StatusSucceeded) || run.Findings.Total == 0 {
		t.Errorf("the run itself changed: %+v", run)
	}
	ts.decode(ts.get("/api/v1/runs/"+old.ID+"/report"), http.StatusOK, nil)

	// And the most recent run keeps its data whatever the cutoff says, so the
	// newest audit's rows are always there to look at.
	after, err := ts.st.Run(ctx, recent.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if after.DatabasePath == "" {
		t.Fatal("the most recent run lost its data")
	}
	if _, err := os.Stat(after.DatabasePath); err != nil {
		t.Errorf("the most recent run's database is gone: %v", err)
	}
}

// A run whose data has been discarded says so, rather than looking like a
// server that is broken on old runs.
func TestAskingForTheRowsOfADiscardedRunSaysSo(t *testing.T) {
	ts := newTestServerTuned(t, "", nil, retention(time.Hour))
	ds := ts.registerFixture()

	old := ts.startRun(map[string]any{"dataset_id": ds})
	ts.startRun(map[string]any{"dataset_id": ds}) // so the old one is not the newest

	var doc struct {
		Findings []struct {
			ID string `json:"id"`
		} `json:"findings"`
	}
	ts.decode(ts.get("/api/v1/runs/"+old.ID+"/report"), http.StatusOK, &doc)
	var findingID string
	for _, f := range doc.Findings {
		resp := ts.do(http.MethodGet, "/api/v1/runs/"+old.ID+"/findings/"+f.ID+"/rows", nil)
		if resp.Status == http.StatusOK {
			findingID = f.ID
			break
		}
	}
	if findingID == "" {
		t.Fatal("no finding of the fixture has rows to show")
	}

	ts.srv.sched.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	ts.srv.sched.tickOnce(context.Background())

	resp := ts.do(http.MethodGet, "/api/v1/runs/"+old.ID+"/findings/"+findingID+"/rows", nil)
	if resp.Status != http.StatusGone {
		t.Fatalf("rows for a discarded run = %d, want 410: %s", resp.Status, resp.Body)
	}
	if !bytes.Contains(resp.Body, []byte("discarded")) {
		t.Errorf("the refusal does not say what happened: %s", resp.Body)
	}
}

func TestKeepingEverythingDiscardsNothing(t *testing.T) {
	ts := newTestServerTuned(t, "", nil, retention(0))
	ctx := context.Background()
	ds := ts.registerFixture()

	old := ts.startRun(map[string]any{"dataset_id": ds})
	ts.startRun(map[string]any{"dataset_id": ds})
	ts.srv.sched.now = func() time.Time { return time.Now().Add(10 * 365 * 24 * time.Hour) }

	ts.srv.sched.tickOnce(ctx)

	after, err := ts.st.Run(ctx, old.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if after.DatabasePath == "" {
		t.Fatal("a run was discarded with retention off")
	}
	if _, err := os.Stat(after.DatabasePath); err != nil {
		t.Errorf("its database is gone: %v", err)
	}
}

// retention configures how long run data is kept, and slows the ticker right
// down so that a test can drive it by hand without the clock racing it.
func retention(d time.Duration) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Server.RetainDatabases = d
		cfg.Schedule.Tick = time.Hour
	}
}

func dirExists(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(dir)
	return err == nil
}
