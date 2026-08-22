package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	// A file rather than :memory: so the tests exercise the same WAL and
	// pragma configuration the server runs with.
	s, err := Open(filepath.Join(t.TempDir(), "veritix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRunLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "dirty-retail", "/data/dirty-retail", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}

	run, err := s.CreateRun(ctx, ds.ID, "v0.1.0", "/data/runs/1/dataset.duckdb")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.Status != StatusPending {
		t.Errorf("new run status = %q, want %q", run.Status, StatusPending)
	}

	if err := s.StartRun(ctx, run.ID); err != nil {
		t.Fatalf("start run: %v", err)
	}

	doc := json.RawMessage(`{"schema":"veritix.audit/v1"}`)
	findings := []Finding{{
		ID: "abc123", Ordinal: 0, Rule: "column.mixed_date_formats",
		Severity: "warning", Title: "signup_date has 2 date formats",
		Table: "customers", Column: "signup_date",
		RowQuery: `SELECT * FROM "customers" LIMIT 50`,
	}}

	if err := s.FinishRun(ctx, run.ID, doc, Counts{Warnings: 1}, findings); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	got, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got.Status != StatusSucceeded {
		t.Errorf("status = %q, want %q", got.Status, StatusSucceeded)
	}
	if got.Total() != 1 || got.Warnings != 1 {
		t.Errorf("counts = %+v, want one warning", got)
	}
	if got.FinishedAt.IsZero() {
		t.Error("finished_at was not recorded")
	}

	stored, err := s.Document(ctx, run.ID)
	if err != nil {
		t.Fatalf("read document: %v", err)
	}
	if string(stored) != string(doc) {
		t.Errorf("document = %s, want %s", stored, doc)
	}

	f, err := s.Finding(ctx, run.ID, "abc123")
	if err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if f.RowQuery != findings[0].RowQuery {
		t.Errorf("row query = %q, want %q", f.RowQuery, findings[0].RowQuery)
	}
}

// A run left mid-flight by a previous process has to be closed out at startup:
// nothing is executing it any more, so an SSE stream would wait forever.
func TestMarkInterruptedFailsRunningRuns(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "d", "/data/d", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	run, err := s.CreateRun(ctx, ds.ID, "v0", "")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := s.StartRun(ctx, run.ID); err != nil {
		t.Fatalf("start run: %v", err)
	}

	n, err := s.MarkInterrupted(ctx)
	if err != nil {
		t.Fatalf("mark interrupted: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked %d runs, want 1", n)
	}

	got, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if !got.Status.Terminal() {
		t.Errorf("status = %q, want a terminal status", got.Status)
	}
	if got.Message == "" {
		t.Error("an interrupted run must say why it stopped")
	}
}

// The same folder audited twice is one dataset with two runs, not two
// datasets, or the run history fragments and stops being usable.
func TestCreateDatasetIsIdempotentByPath(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	first, err := s.CreateDataset(ctx, "retail", "/data/retail", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	second, err := s.CreateDataset(ctx, "retail again", "/data/retail", false)
	if err != nil {
		t.Fatalf("create dataset again: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("got two ids for one path: %s and %s", first.ID, second.ID)
	}

	all, err := s.Datasets(ctx)
	if err != nil {
		t.Fatalf("list datasets: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("listed %d datasets, want 1", len(all))
	}
}

func TestMissingIDsReportNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.Run(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Run(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.Dataset(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Dataset(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.Finding(ctx, "nope", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Finding(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.Document(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Document(missing) = %v, want ErrNotFound", err)
	}
}

// Deleting a dataset must take its runs and findings with it; a findings row
// pointing at a run that no longer exists would surface as a phantom in any
// cross-run view.
func TestDeleteDatasetCascades(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "d", "/data/d", true)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	run, err := s.CreateRun(ctx, ds.ID, "v0", "")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := s.StartRun(ctx, run.ID); err != nil {
		t.Fatalf("start run: %v", err)
	}
	err = s.FinishRun(ctx, run.ID, json.RawMessage(`{}`), Counts{Errors: 1},
		[]Finding{{ID: "f1", Rule: "r", Severity: "error", Title: "t"}})
	if err != nil {
		t.Fatalf("finish run: %v", err)
	}

	if err := s.DeleteDataset(ctx, ds.ID); err != nil {
		t.Fatalf("delete dataset: %v", err)
	}
	if _, err := s.Run(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("run survived its dataset: %v", err)
	}
	fs, err := s.Findings(ctx, run.ID)
	if err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(fs) != 0 {
		t.Errorf("findings survived their run: %d left", len(fs))
	}
}

// Reopening an existing store must be a no-op, not a re-run of the migrations.
func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "veritix.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := first.CreateDataset(context.Background(), "d", "/data/d", false); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close() //nolint:errcheck // the test has already reported what matters

	all, err := second.Datasets(context.Background())
	if err != nil {
		t.Fatalf("list datasets: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("listed %d datasets after reopen, want 1", len(all))
	}
}

// Proposals are kept whole, unlike findings and unlike the report. A proposed
// one_of rule permits values materialized from the customer's data, and the
// report deliberately does not carry those, so if the store did not keep them
// there would be nothing to accept from later.
func TestProposalsAreStoredWholeAndGoWithTheirRun(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	ds, err := s.CreateDataset(ctx, "dirty-retail", "/data/dirty-retail", false)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	run, err := s.CreateRun(ctx, ds.ID, "v1", "")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	body := json.RawMessage(`{"rule":{"id":"status_domain","values":["Active","Actve"]}}`)
	proposals := []Proposal{
		{ID: "aaaa1111", Ordinal: 0, Rule: "status_domain", Document: body},
		{ID: "bbbb2222", Ordinal: 1, Rule: "amount_range", Document: json.RawMessage(`{}`)},
	}
	if err := s.SaveProposals(ctx, run.ID, proposals); err != nil {
		t.Fatalf("save proposals: %v", err)
	}

	got, err := s.Proposals(ctx, run.ID)
	if err != nil {
		t.Fatalf("list proposals: %v", err)
	}
	if len(got) != 2 || got[0].ID != "aaaa1111" || got[1].Rule != "amount_range" {
		t.Fatalf("read back %+v", got)
	}
	if string(got[0].Document) != string(body) {
		t.Errorf("the document came back as %s", got[0].Document)
	}

	one, err := s.Proposal(ctx, run.ID, "bbbb2222")
	if err != nil || one.Ordinal != 1 {
		t.Fatalf("read one proposal: %+v, %v", one, err)
	}
	if _, err := s.Proposal(ctx, run.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown proposal returned %v", err)
	}

	// Saving again replaces rather than accumulates: a re-recorded run has the
	// proposals it made this time.
	if err := s.SaveProposals(ctx, run.ID, proposals[:1]); err != nil {
		t.Fatalf("save proposals again: %v", err)
	}
	if got, _ := s.Proposals(ctx, run.ID); len(got) != 1 {
		t.Errorf("saving again left %d proposals", len(got))
	}

	// And they belong to the run: deleting the dataset takes them with it,
	// rather than leaving customer values behind with nothing pointing at them.
	if err := s.DeleteDataset(ctx, ds.ID); err != nil {
		t.Fatalf("delete dataset: %v", err)
	}
	if got, _ := s.Proposals(ctx, run.ID); len(got) != 0 {
		t.Errorf("%d proposals survived their dataset", len(got))
	}
}
