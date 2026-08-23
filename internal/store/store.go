// Package store is Veritix's audit trail: which dataset was audited, by which
// run, when, and what was found.
//
// It is SQLite, deliberately separate from the DuckDB engine. DuckDB holds
// dataset content — large, disposable, and re-creatable from the customer's
// files. This holds the record of what was done, which is small, long-lived,
// and the thing somebody will want to produce six months later when asked why
// a number in a report looked wrong. Different lifetimes and different risk, so
// different databases.
//
// The store knows nothing about the shape of a report. A finished run's
// document is persisted as an opaque JSON blob, so that changing the report
// schema is not a database migration.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	// modernc.org/sqlite is the pure-Go SQLite driver. Veritix already needs
	// CGO for DuckDB, but keeping a second C library out of the build means
	// the run store cannot break the toolchain the engine depends on.
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when an id does not exist.
var ErrNotFound = errors.New("not found")

// Store is a handle on the run database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the run store at path. The parent directory must
// already exist. An empty path gives a private in-memory database, which is
// what the tests use.
func Open(path string) (*Store, error) {
	dsn := "file::memory:?cache=shared"
	if path != "" {
		// WAL lets the SSE handlers read a run's progress while the run that
		// is producing it writes. busy_timeout covers the remaining window
		// where two writers overlap, which is otherwise an instant
		// SQLITE_BUSY rather than a wait.
		dsn = "file:" + filepath.ToSlash(path) +
			"?_pragma=journal_mode(WAL)" +
			"&_pragma=busy_timeout(5000)" +
			"&_pragma=foreign_keys(1)" +
			"&_pragma=synchronous(NORMAL)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open run store: %w", err)
	}
	if path == "" {
		// A shared in-memory database exists only while a connection is open,
		// so the pool must not be allowed to drop to zero and lose the schema.
		db.SetMaxOpenConns(1)
	}

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// migrations are applied in order, and the count of those applied is recorded
// in SQLite's user_version. Adding a schema change means appending to this
// slice and never editing an earlier entry.
var migrations = []string{
	`CREATE TABLE datasets (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL,
		path       TEXT NOT NULL,
		uploaded   INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);
	CREATE UNIQUE INDEX datasets_by_path ON datasets(path);

	CREATE TABLE runs (
		id            TEXT PRIMARY KEY,
		dataset_id    TEXT NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
		status        TEXT NOT NULL,
		message       TEXT NOT NULL DEFAULT '',
		version       TEXT NOT NULL DEFAULT '',
		database_path TEXT NOT NULL DEFAULT '',
		created_at    TEXT NOT NULL,
		started_at    TEXT,
		finished_at   TEXT,
		duration_ms   INTEGER NOT NULL DEFAULT 0,
		errors        INTEGER NOT NULL DEFAULT 0,
		warnings      INTEGER NOT NULL DEFAULT 0,
		infos         INTEGER NOT NULL DEFAULT 0,
		document      BLOB
	);
	CREATE INDEX runs_by_time ON runs(created_at DESC);

	-- Only what the report document cannot serve: a finding's row query, which
	-- is deliberately absent from every report because running it returns raw
	-- customer data, plus enough identity to track one finding across runs.
	CREATE TABLE findings (
		run_id      TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		id          TEXT NOT NULL,
		ordinal     INTEGER NOT NULL,
		rule        TEXT NOT NULL,
		severity    TEXT NOT NULL,
		title       TEXT NOT NULL,
		table_name  TEXT NOT NULL DEFAULT '',
		column_name TEXT NOT NULL DEFAULT '',
		row_query   TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (run_id, id)
	);
	CREATE INDEX findings_by_rule ON findings(rule);`,

	// The agentic auditor's trace: exactly what was sent to a model and what
	// came back. It is a table of its own rather than a column on runs because
	// it is large, it is only read when somebody asks for it, and a run
	// without a model has none. Like the report document it is an opaque blob,
	// for the same reason: the shape of a trace changing should not be a
	// database migration.
	`CREATE TABLE traces (
		run_id     TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
		document   BLOB NOT NULL,
		created_at TEXT NOT NULL
	);`,

	// Rules the agent proposed. Unlike a finding, a proposal is not in the
	// report document in full: a one_of rule's permitted values are cell
	// values materialized from the data, and the report omits those the way it
	// omits every other value. So the store keeps the whole proposal and the
	// report keeps its shape, which is the same division as findings and their
	// row queries.
	//
	// The proposal itself is an opaque blob for the reason the report document
	// and the trace are: what the store knows is which run proposed what, and
	// changing the shape of a rule should not be a database migration.
	`CREATE TABLE proposals (
		run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		id         TEXT NOT NULL,
		ordinal    INTEGER NOT NULL,
		rule       TEXT NOT NULL,
		document   BLOB NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (run_id, id)
	);`,

	// A dataset's standing instruction to audit itself. It is a table of its
	// own rather than columns on datasets because a dataset without a schedule
	// has no row, which is the same shape traces and proposals have, and
	// because ON DELETE CASCADE then makes deleting the dataset delete the
	// commitment to keep auditing it.
	//
	// The columns are schedule.Schedule's fields spelled out rather than an
	// opaque blob, unlike the document columns above: this is a handful of
	// small values that an operator may want to read with sqlite3 when a
	// nightly audit did not happen, and "why did it not fire" is a question
	// answered by looking at next_due_at.
	`CREATE TABLE schedules (
		dataset_id  TEXT PRIMARY KEY REFERENCES datasets(id) ON DELETE CASCADE,
		kind        TEXT NOT NULL,
		at_minute   INTEGER NOT NULL DEFAULT 0,
		weekday     INTEGER NOT NULL DEFAULT 0,
		every_ms    INTEGER NOT NULL DEFAULT 0,
		location    TEXT NOT NULL DEFAULT '',
		notify      INTEGER NOT NULL DEFAULT 0,
		next_due_at TEXT NOT NULL,
		last_run_id TEXT NOT NULL DEFAULT '',
		last_error  TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL
	);`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf(
			"run store is at schema version %d but this build only knows %d: it was written by a newer Veritix",
			version, len(migrations))
	}

	for i := version; i < len(migrations); i++ {
		if _, err := s.db.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		// PRAGMA does not take a bind parameter, and i+1 is an int under our
		// control rather than anything a caller supplied.
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
	}
	return nil
}

// formatTime renders a timestamp for storage: UTC, RFC3339, so that rows sort
// chronologically as text and stay readable to anyone with a sqlite3 prompt.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s sql.NullString) time.Time {
	if !s.Valid || s.String == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return time.Time{}
	}
	return t
}
