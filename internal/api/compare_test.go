package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/russellw/veritix/internal/report"
)

// registerCopy registers a writable copy of the fixture, so that a test can
// change the data between two runs. The dataset the rest of the suite audits
// is read from the repository and must stay as committed.
func (ts *testServer) registerCopy(dir string) string {
	ts.t.Helper()

	var ds datasetJSON
	ts.decode(ts.do(http.MethodPost, "/api/v1/datasets",
		map[string]any{"path": dir}), http.StatusCreated, &ds)
	return ds.ID
}

// copyFixture makes a writable copy of the dirty-retail CSVs. The workbook and
// the manifest are left behind: this is about two runs over changing data, and
// three CSVs are enough to change.
func copyFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for _, name := range []string{"customers.csv", "orders.csv", "regions.csv"} {
		body, err := os.ReadFile(filepath.Join(fixtureDataset, name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

func (ts *testServer) document(runID string) *report.Document {
	ts.t.Helper()

	resp := ts.get("/api/v1/runs/" + runID + "/report")
	if resp.Status != http.StatusOK {
		ts.t.Fatalf("report for %s: %d", runID, resp.Status)
	}
	var doc report.Document
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		ts.t.Fatalf("decode report: %v", err)
	}
	return &doc
}

// The comparison is what makes Veritix something a business runs every week
// rather than once. Over HTTP nobody asks for it: a second run of a dataset
// carries it because there is something to compare against.
func TestASecondRunSaysWhatChangedSinceTheFirst(t *testing.T) {
	ts := newTestServer(t, "")
	dir := copyFixture(t)
	ds := ts.registerCopy(dir)

	first := ts.startRun(map[string]any{"dataset_id": ds})
	if doc := ts.document(first.ID); doc.Comparison != nil {
		t.Fatal("the first audit of a dataset compared itself against something")
	}

	// One more order referencing a customer who does not exist, so an existing
	// finding gets worse rather than a new one appearing.
	orders := filepath.Join(dir, "orders.csv")
	body, err := os.ReadFile(orders)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orders,
		append(body, []byte("9999,CUS-999999,2024-03-01,10.00,GBP\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	second := ts.startRun(map[string]any{"dataset_id": ds})
	doc := ts.document(second.ID)
	if doc.Comparison == nil {
		t.Fatal("the second audit of a dataset carried no comparison")
	}

	if doc.Comparison.Baseline.RunID != first.ID {
		t.Errorf("compared against run %q, want the first run %q",
			doc.Comparison.Baseline.RunID, first.ID)
	}
	if !doc.Comparison.Summary.Changed() {
		t.Errorf("adding an orphan reference changed nothing: %+v", doc.Comparison.Summary)
	}

	var worsened *report.FindingDelta
	for i, f := range doc.Comparison.Findings {
		if f.Rule == "reference.orphan_values" {
			worsened = &doc.Comparison.Findings[i]
		}
	}
	if worsened == nil {
		t.Fatalf("no orphan-reference finding moved: %+v", doc.Comparison.Findings)
	}
	if worsened.Status != report.DeltaWorsened {
		t.Errorf("orphan references: status %q, want worsened", worsened.Status)
	}
	if worsened.CountAfter <= worsened.CountBefore {
		t.Errorf("orphan references: %d → %d, want the count to have risen",
			worsened.CountBefore, worsened.CountAfter)
	}

	// One table gained a row, and the comparison is the only thing in the
	// product that can see it.
	var grew bool
	for _, td := range doc.Comparison.Tables {
		if td.RowsAfter == td.RowsBefore+1 {
			grew = true
		}
	}
	if !grew {
		t.Errorf("no table reported the row it gained: %+v", doc.Comparison.Tables)
	}
}

// A third run compares against the second, not against the first: "the
// previous audit" has to keep moving, or a weekly report would drift further
// from the truth every week.
func TestARunComparesAgainstTheMostRecentEarlierRun(t *testing.T) {
	ts := newTestServer(t, "")
	ds := ts.registerCopy(copyFixture(t))

	ts.startRun(map[string]any{"dataset_id": ds})
	second := ts.startRun(map[string]any{"dataset_id": ds})
	third := ts.startRun(map[string]any{"dataset_id": ds})

	doc := ts.document(third.ID)
	if doc.Comparison == nil {
		t.Fatal("the third run carried no comparison")
	}
	if doc.Comparison.Baseline.RunID != second.ID {
		t.Errorf("compared against %q, want the run immediately before it, %q",
			doc.Comparison.Baseline.RunID, second.ID)
	}
	// Nothing about the data changed between them.
	if doc.Comparison.Summary.Changed() {
		t.Errorf("two runs over unchanged files reported changes: %+v", doc.Comparison.Summary)
	}
}

// A failed run has no report, so it cannot be a baseline. Resetting the
// comparison every time an audit crashed would be worse than no comparison.
func TestAFailedRunIsNotUsedAsABaseline(t *testing.T) {
	ts := newTestServer(t, "")
	dir := copyFixture(t)
	ds := ts.registerCopy(dir)

	first := ts.startRun(map[string]any{"dataset_id": ds})

	// Take the data away, so the next run fails rather than finding nothing.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}

	var failed runJSON
	ts.decode(ts.do(http.MethodPost, "/api/v1/runs", map[string]any{"dataset_id": ds}),
		http.StatusAccepted, &failed)
	if done := ts.awaitDone(failed.ID); done.Status == "succeeded" {
		t.Fatalf("the audit of an empty directory succeeded; this test needs it to fail")
	}

	// Put it back and run again: the comparison should reach past the failure.
	for _, name := range []string{"customers.csv", "orders.csv", "regions.csv"} {
		body, err := os.ReadFile(filepath.Join(fixtureDataset, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	third := ts.startRun(map[string]any{"dataset_id": ds})
	doc := ts.document(third.ID)
	if doc.Comparison == nil {
		t.Fatal("no comparison was made across a failed run")
	}
	if doc.Comparison.Baseline.RunID != first.ID {
		t.Errorf("compared against %q, want the last run that produced a report, %q",
			doc.Comparison.Baseline.RunID, first.ID)
	}
}

// Two datasets that happen to be audited by the same server are not each
// other's history.
func TestARunIsOnlyComparedWithItsOwnDataset(t *testing.T) {
	ts := newTestServer(t, "")

	other := ts.registerCopy(copyFixture(t))
	ts.startRun(map[string]any{"dataset_id": other})

	mine := ts.registerCopy(copyFixture(t))
	run := ts.startRun(map[string]any{"dataset_id": mine})

	if doc := ts.document(run.ID); doc.Comparison != nil {
		t.Errorf("a dataset's first audit was compared against another dataset's run: %q",
			doc.Comparison.Baseline.RunID)
	}
}
