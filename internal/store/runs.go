package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status is where a run has got to.
type Status string

const (
	// StatusPending means the run is queued but has not started.
	StatusPending Status = "pending"
	// StatusRunning means the pipeline is executing.
	StatusRunning Status = "running"
	// StatusSucceeded means the audit completed and produced a report. It says
	// nothing about whether the data was clean.
	StatusSucceeded Status = "succeeded"
	// StatusFailed means the audit could not be completed.
	StatusFailed Status = "failed"
	// StatusCanceled means a caller stopped the run.
	StatusCanceled Status = "canceled"
)

// Terminal reports whether a run has stopped moving, which is what tells an
// SSE stream it can close.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCanceled
}

// Dataset is a registered dataset root.
type Dataset struct {
	ID   string
	Name string
	// Path is an absolute path on the server's filesystem. It is resolved
	// before it reaches the store; nothing here should ever join a
	// caller-supplied fragment onto a directory.
	Path string
	// Uploaded distinguishes data the server wrote into its own data
	// directory from a path an operator registered in place. Only the former
	// is safe to delete.
	Uploaded  bool
	CreatedAt time.Time
}

// Run is one audit of one dataset.
type Run struct {
	ID        string
	DatasetID string
	Status    Status
	// Message carries the failure reason when Status is failed.
	Message string
	Version string
	// DatabasePath is the DuckDB file holding the ingested dataset. It
	// outlives the run so that a finding's rows can be fetched afterwards
	// without re-reading the customer's files.
	DatabasePath string

	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration

	Errors   int
	Warnings int
	Infos    int
}

// Total is how many findings the run produced.
func (r *Run) Total() int { return r.Errors + r.Warnings + r.Infos }

// Finding is the part of a finding the store keeps: enough to address it, plus
// the row query that no report is allowed to carry.
type Finding struct {
	ID       string
	Ordinal  int
	Rule     string
	Severity string
	Title    string
	Table    string
	Column   string
	// RowQuery returns the offending rows themselves. It is stored so that the
	// API can serve them on explicit request and never by accident: it is
	// reachable only by asking for one finding's rows by id.
	RowQuery string
}

// newID returns a time-ordered identifier, so that rows sort by creation even
// before their timestamps are consulted.
func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return id.String(), nil
}

// CreateDataset registers a dataset root. Registering a path that is already
// known returns the existing record rather than a duplicate, because the same
// folder audited twice is one dataset with two runs.
func (s *Store) CreateDataset(ctx context.Context, name, path string, uploaded bool) (*Dataset, error) {
	if existing, err := s.DatasetByPath(ctx, path); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	ds := &Dataset{ID: id, Name: name, Path: path, Uploaded: uploaded, CreatedAt: time.Now()}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO datasets (id, name, path, uploaded, created_at) VALUES (?, ?, ?, ?, ?)`,
		ds.ID, ds.Name, ds.Path, ds.Uploaded, formatTime(ds.CreatedAt))
	if err != nil {
		return nil, fmt.Errorf("create dataset: %w", err)
	}
	return ds, nil
}

const datasetColumns = `id, name, path, uploaded, created_at`

func scanDataset(sc interface{ Scan(...any) error }) (*Dataset, error) {
	var (
		ds      Dataset
		created sql.NullString
	)
	if err := sc.Scan(&ds.ID, &ds.Name, &ds.Path, &ds.Uploaded, &created); err != nil {
		return nil, err
	}
	ds.CreatedAt = parseTime(created)
	return &ds, nil
}

// Dataset looks up one dataset by id.
func (s *Store) Dataset(ctx context.Context, id string) (*Dataset, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+datasetColumns+` FROM datasets WHERE id = ?`, id)
	ds, err := scanDataset(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("dataset %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read dataset: %w", err)
	}
	return ds, nil
}

// DatasetByPath looks up one dataset by its path.
func (s *Store) DatasetByPath(ctx context.Context, path string) (*Dataset, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+datasetColumns+` FROM datasets WHERE path = ?`, path)
	ds, err := scanDataset(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("dataset at %s: %w", path, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read dataset: %w", err)
	}
	return ds, nil
}

// Datasets lists registered datasets, most recent first.
func (s *Store) Datasets(ctx context.Context) ([]*Dataset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+datasetColumns+` FROM datasets ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list datasets: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports what matters

	var out []*Dataset
	for rows.Next() {
		ds, err := scanDataset(rows)
		if err != nil {
			return nil, fmt.Errorf("read dataset: %w", err)
		}
		out = append(out, ds)
	}
	return out, rows.Err()
}

// DeleteDataset removes a dataset and, by cascade, its runs and findings. It
// does not touch the filesystem: deciding whether the bytes may be deleted is
// the caller's business, since a registered path belongs to the operator.
func (s *Store) DeleteDataset(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM datasets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete dataset: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("dataset %s: %w", id, ErrNotFound)
	}
	return nil
}

// CreateRun records a queued run.
func (s *Store) CreateRun(ctx context.Context, datasetID, version, databasePath string) (*Run, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	r := &Run{
		ID:           id,
		DatasetID:    datasetID,
		Status:       StatusPending,
		Version:      version,
		DatabasePath: databasePath,
		CreatedAt:    time.Now(),
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO runs (id, dataset_id, status, version, database_path, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.DatasetID, string(r.Status), r.Version, r.DatabasePath, formatTime(r.CreatedAt))
	if err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	return r, nil
}

// SetRunDatabase records where the run's ingested dataset was written.
//
// It is a second step because the path is named after the run id, and the id
// is not known until the row exists.
func (s *Store) SetRunDatabase(ctx context.Context, id, path string) error {
	return s.update(ctx, `UPDATE runs SET database_path = ? WHERE id = ?`, path, id)
}

// StartRun marks a run as executing.
func (s *Store) StartRun(ctx context.Context, id string) error {
	return s.update(ctx, `UPDATE runs SET status = ?, started_at = ? WHERE id = ?`,
		string(StatusRunning), formatTime(time.Now()), id)
}

// Counts summarizes a run's findings by severity.
type Counts struct {
	Errors   int
	Warnings int
	Infos    int
}

// FinishRun records a successful run: its report document, its finding counts,
// and the findings themselves. All of it lands in one transaction, so a reader
// never sees a run marked complete whose findings have not arrived.
func (s *Store) FinishRun(
	ctx context.Context,
	id string,
	document json.RawMessage,
	counts Counts,
	findings []Finding,
) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var started sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT started_at FROM runs WHERE id = ?`, id).
		Scan(&started); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s: %w", id, ErrNotFound)
		}
		return fmt.Errorf("finish run: %w", err)
	}

	finished := time.Now()
	var duration time.Duration
	if t := parseTime(started); !t.IsZero() {
		duration = finished.Sub(t)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, duration_ms = ?,
		        errors = ?, warnings = ?, infos = ?, document = ?
		 WHERE id = ?`,
		string(StatusSucceeded), formatTime(finished), duration.Milliseconds(),
		counts.Errors, counts.Warnings, counts.Infos, []byte(document), id)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}

	// Re-running an audit into the same run id replaces its findings rather
	// than accumulating two generations of them.
	if _, err = tx.ExecContext(ctx, `DELETE FROM findings WHERE run_id = ?`, id); err != nil {
		return fmt.Errorf("finish run: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO findings
		   (run_id, id, ordinal, rule, severity, title, table_name, column_name, row_query)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	defer stmt.Close() //nolint:errcheck // the transaction outcome is what matters

	for _, f := range findings {
		if _, err = stmt.ExecContext(ctx,
			id, f.ID, f.Ordinal, f.Rule, f.Severity, f.Title, f.Table, f.Column, f.RowQuery,
		); err != nil {
			return fmt.Errorf("record finding %s: %w", f.Rule, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

// StopRun records a run that ended without a report. The message is shown to
// the operator, so it should say what went wrong rather than name a symptom.
func (s *Store) StopRun(ctx context.Context, id string, status Status, message string) error {
	if !status.Terminal() {
		return fmt.Errorf("stop run: %q is not a terminal status", status)
	}

	r, err := s.Run(ctx, id)
	if err != nil {
		return err
	}
	finished := time.Now()
	var duration time.Duration
	if !r.StartedAt.IsZero() {
		duration = finished.Sub(r.StartedAt)
	}

	return s.update(ctx,
		`UPDATE runs SET status = ?, message = ?, finished_at = ?, duration_ms = ? WHERE id = ?`,
		string(status), message, formatTime(finished), duration.Milliseconds(), id)
}

// MarkInterrupted fails every run left mid-flight by a previous process.
//
// A run is executed in memory by the process that started it, so one that
// survives a restart in "running" is a lie that would otherwise sit in the
// history forever, and an SSE stream would wait on it indefinitely.
func (s *Store) MarkInterrupted(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, message = ?, finished_at = ?
		 WHERE status IN (?, ?)`,
		string(StatusFailed), "interrupted: the server stopped while this run was in progress",
		formatTime(time.Now()), string(StatusPending), string(StatusRunning))
	if err != nil {
		return 0, fmt.Errorf("reset interrupted runs: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) update(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("run: %w", ErrNotFound)
	}
	return nil
}

const runColumns = `id, dataset_id, status, message, version, database_path,
	created_at, started_at, finished_at, duration_ms, errors, warnings, infos`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var (
		r                          Run
		status                     string
		created, started, finished sql.NullString
		durationMS                 int64
	)
	err := sc.Scan(&r.ID, &r.DatasetID, &status, &r.Message, &r.Version, &r.DatabasePath,
		&created, &started, &finished, &durationMS, &r.Errors, &r.Warnings, &r.Infos)
	if err != nil {
		return nil, err
	}
	r.Status = Status(status)
	r.CreatedAt = parseTime(created)
	r.StartedAt = parseTime(started)
	r.FinishedAt = parseTime(finished)
	r.Duration = time.Duration(durationMS) * time.Millisecond
	return &r, nil
}

// Run looks up one run.
func (s *Store) Run(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read run: %w", err)
	}
	return r, nil
}

// PreviousRun returns the most recent successful audit of the same dataset
// started before this one, or ErrNotFound when this is the first.
//
// It is what a run is compared against. "The previous audit" has to mean the
// previous *successful* one: a failed run has no report to compare with, and
// skipping over it silently is right — a comparison that reset itself every
// time an audit crashed would be worse than no comparison at all.
//
// The bound is on created_at rather than on the id alone, because a run that
// started earlier can finish later and "the previous audit" is about when the
// data was looked at.
func (s *Store) PreviousRun(ctx context.Context, runID string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs
		 WHERE dataset_id = (SELECT dataset_id FROM runs WHERE id = ?)
		   AND status = ?
		   AND created_at < (SELECT created_at FROM runs WHERE id = ?)
		 ORDER BY created_at DESC LIMIT 1`,
		runID, string(StatusSucceeded), runID)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no earlier run of this dataset: %w", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read previous run: %w", err)
	}
	return r, nil
}

// ActiveRun returns a run of this dataset that has not finished, or
// ErrNotFound when none has.
//
// The store is the authority rather than the runner's own map of what is
// executing here, because a run recorded as in flight by another process
// sharing this data directory is just as good a reason not to start a second
// audit of the same files. MarkInterrupted is what keeps that honest across a
// restart.
func (s *Store) ActiveRun(ctx context.Context, datasetID string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs
		 WHERE dataset_id = ? AND status IN (?, ?)
		 ORDER BY created_at DESC LIMIT 1`,
		datasetID, string(StatusPending), string(StatusRunning))
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no run of dataset %s is in flight: %w", datasetID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read active run: %w", err)
	}
	return r, nil
}

// DiscardableRunDatabases returns finished runs whose ingested data may be
// deleted: those that finished before the given instant and are not the most
// recent run of their dataset still holding one.
//
// The floor matters more than the cutoff. A dataset audited once, six months
// ago, would otherwise lose the only copy of the data its findings were
// computed from, and the rows behind a finding are the most useful thing the
// interface shows.
//
// The caller checks the cutoff again in Go. This comparison is between the
// texts formatTime writes, and RFC 3339 Nano drops the trailing zeros of the
// fraction, so the boundary is fuzzy by under a second — which does not matter
// for a cutoff measured in days, and a run wrongly left out here is picked up
// on the next pass anyway.
func (s *Store) DiscardableRunDatabases(ctx context.Context, before time.Time) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs r
		 WHERE r.database_path <> ''
		   AND r.finished_at IS NOT NULL AND r.finished_at <> ''
		   AND r.finished_at < ?
		   AND r.id <> (SELECT id FROM runs
		                WHERE dataset_id = r.dataset_id AND database_path <> ''
		                ORDER BY created_at DESC LIMIT 1)
		 ORDER BY r.finished_at`,
		formatTime(before))
	if err != nil {
		return nil, fmt.Errorf("list discardable run databases: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports what matters

	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("read run: %w", err)
		}
		if r.FinishedAt.Before(before) {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ClearRunDatabase records that a run's ingested data is gone. The run itself
// stays: it is the audit trail, and what was discarded is a copy of the
// customer's files that can be made again by auditing them again.
func (s *Store) ClearRunDatabase(ctx context.Context, id string) error {
	return s.update(ctx, `UPDATE runs SET database_path = '' WHERE id = ?`, id)
}

// Runs lists run history, most recent first. An empty datasetID lists every
// dataset's runs.
func (s *Store) Runs(ctx context.Context, datasetID string, limit int) ([]*Run, error) {
	if limit <= 0 {
		limit = 50
	}

	query := `SELECT ` + runColumns + ` FROM runs`
	args := []any{}
	if datasetID != "" {
		query += ` WHERE dataset_id = ?`
		args = append(args, datasetID)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports what matters

	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("read run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Document returns the stored report document for a completed run.
func (s *Store) Document(ctx context.Context, id string) (json.RawMessage, error) {
	var doc []byte
	err := s.db.QueryRowContext(ctx, `SELECT document FROM runs WHERE id = ?`, id).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read run document: %w", err)
	}
	if len(doc) == 0 {
		return nil, fmt.Errorf("run %s has no report yet: %w", id, ErrNotFound)
	}
	return doc, nil
}

// Finding returns one finding of one run, including its row query.
func (s *Store) Finding(ctx context.Context, runID, findingID string) (*Finding, error) {
	var f Finding
	err := s.db.QueryRowContext(ctx,
		`SELECT id, ordinal, rule, severity, title, table_name, column_name, row_query
		 FROM findings WHERE run_id = ? AND id = ?`, runID, findingID).
		Scan(&f.ID, &f.Ordinal, &f.Rule, &f.Severity, &f.Title, &f.Table, &f.Column, &f.RowQuery)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("finding %s in run %s: %w", findingID, runID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read finding: %w", err)
	}
	return &f, nil
}

// Findings lists a run's findings in report order.
func (s *Store) Findings(ctx context.Context, runID string) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ordinal, rule, severity, title, table_name, column_name, row_query
		 FROM findings WHERE run_id = ? ORDER BY ordinal`, runID)
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports what matters

	var out []Finding
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.ID, &f.Ordinal, &f.Rule, &f.Severity, &f.Title,
			&f.Table, &f.Column, &f.RowQuery); err != nil {
			return nil, fmt.Errorf("read finding: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SaveTrace records what the agentic auditor did during a run.
//
// It is written after FinishRun rather than inside it. A trace is worth having
// even when the report could not be stored — it is the record of what left the
// machine — but a run whose findings failed to save should not also fail
// because of its trace.
func (s *Store) SaveTrace(ctx context.Context, runID string, document json.RawMessage) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO traces (run_id, document, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET document = excluded.document`,
		runID, []byte(document), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("save trace: %w", err)
	}
	return nil
}

// Proposal is one rule the agent proposed during a run, as the store keeps it:
// its identity, and the proposal itself as an opaque document.
type Proposal struct {
	ID      string
	Ordinal int
	Rule    string
	// Document is the proposal in full, including anything the report omits.
	Document json.RawMessage
}

// SaveProposals records what a run proposed, replacing anything already stored
// for that run.
//
// Like the trace it is written after FinishRun and its failure does not fail
// the run: the findings are already safe, and a lost proposal is a suggestion
// nobody sees rather than an audit nobody has.
func (s *Store) SaveProposals(ctx context.Context, runID string, ps []Proposal) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save proposals: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rolled back only if Commit did not happen

	if _, err := tx.ExecContext(ctx, `DELETE FROM proposals WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("save proposals: %w", err)
	}
	now := formatTime(time.Now())
	for _, p := range ps {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO proposals (run_id, id, ordinal, rule, document, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			runID, p.ID, p.Ordinal, p.Rule, []byte(p.Document), now); err != nil {
			return fmt.Errorf("save proposal %s: %w", p.ID, err)
		}
	}
	return tx.Commit()
}

// Proposals lists what a run proposed, in the order it proposed it.
func (s *Store) Proposals(ctx context.Context, runID string) ([]Proposal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ordinal, rule, document FROM proposals WHERE run_id = ? ORDER BY ordinal`,
		runID)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports what matters

	var out []Proposal
	for rows.Next() {
		var p Proposal
		var doc []byte
		if err := rows.Scan(&p.ID, &p.Ordinal, &p.Rule, &doc); err != nil {
			return nil, fmt.Errorf("read proposal: %w", err)
		}
		p.Document = doc
		out = append(out, p)
	}
	return out, rows.Err()
}

// Proposal returns one proposal of one run.
func (s *Store) Proposal(ctx context.Context, runID, id string) (*Proposal, error) {
	var p Proposal
	var doc []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, ordinal, rule, document FROM proposals WHERE run_id = ? AND id = ?`,
		runID, id).Scan(&p.ID, &p.Ordinal, &p.Rule, &doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("proposal %s in run %s: %w", id, runID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read proposal: %w", err)
	}
	p.Document = doc
	return &p, nil
}

// Trace returns a run's agent trace.
func (s *Store) Trace(ctx context.Context, runID string) (json.RawMessage, error) {
	var doc []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT document FROM traces WHERE run_id = ?`, runID).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %s has no agent trace: %w", runID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}
	return doc, nil
}
