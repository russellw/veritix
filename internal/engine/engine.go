// Package engine wraps the embedded DuckDB instance that does all of
// Veritix's measuring.
//
// Everything the profiler, the checks, and the agent want to know about a
// dataset is expressed as a SQL aggregate over tables in this engine. Keeping
// that in one place means there is exactly one component that touches data,
// one place where resource limits are enforced, and one place where the SQL
// that backs a finding can be re-run to verify it.
package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2" // database/sql driver "duckdb"

	"github.com/russellw/veritix/internal/config"
)

// Engine is a handle on one DuckDB database.
type Engine struct {
	db       *sql.DB
	cfg      config.Engine
	log      *slog.Logger
	path     string
	readOnly bool
	// locked records that Lockdown has run. It is written once, before the
	// agent starts, and only read afterwards.
	locked bool

	// aggregateCache memoizes DuckDB's aggregate-function catalog, which
	// AnalyzeSelect consults for every model-authored query.
	aggregateCache
}

// Open starts an engine. An empty path gives a transient in-memory database;
// a path gives a DuckDB file that survives the process, which is how the
// server caches an ingested dataset between runs.
func Open(ctx context.Context, path string, cfg config.Engine, log *slog.Logger) (*Engine, error) {
	return open(ctx, path, cfg, log, false)
}

// OpenReadOnly opens an existing database file with writes refused by DuckDB
// itself, which is how a finished run's dataset is reopened to serve a
// finding's rows.
//
// It is not what protects the database from the agent: the agent queries the
// live engine mid-run, when the file is already open for writing by this
// process. Lockdown is that boundary.
func OpenReadOnly(ctx context.Context, path string, cfg config.Engine, log *slog.Logger) (*Engine, error) {
	if path == "" {
		return nil, fmt.Errorf("engine: a read-only engine needs a database file, not in-memory")
	}
	return open(ctx, path, cfg, log, true)
}

func open(ctx context.Context, path string, cfg config.Engine, log *slog.Logger, readOnly bool) (*Engine, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	dsn := path
	if readOnly {
		dsn += "?" + url.Values{"access_mode": []string{"read_only"}}.Encode()
	}

	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, fmt.Errorf("engine: opening duckdb: %w", err)
	}

	e := &Engine{db: db, cfg: cfg, log: log, path: path, readOnly: readOnly}
	if err := e.applyLimits(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return e, nil
}

// applyLimits pushes the configured caps into DuckDB. One pathological file or
// one runaway agent query must not be able to exhaust the host, and a customer
// running Veritix beside their own workloads needs to be able to box it in.
func (e *Engine) applyLimits(ctx context.Context) error {
	type setting struct {
		name  string
		value string
	}
	var settings []setting

	if e.cfg.MemoryLimit != "" {
		settings = append(settings, setting{"memory_limit", e.cfg.MemoryLimit})
	}
	if e.cfg.Threads > 0 {
		settings = append(settings, setting{"threads", fmt.Sprint(e.cfg.Threads)})
	}
	if e.cfg.TempDir != "" {
		settings = append(settings, setting{"temp_directory", e.cfg.TempDir})
	}

	for _, s := range settings {
		// Settings are not parameterizable, so quote the value as a literal.
		stmt := fmt.Sprintf("SET %s = %s", s.name, Literal(s.value))
		if _, err := e.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("engine: applying %s=%s: %w", s.name, s.value, err)
		}
	}
	return nil
}

// Lockdown takes away DuckDB's access to the filesystem and then locks the
// configuration so it cannot be given back.
//
// It is called once the dataset is loaded and before any model-authored SQL is
// executed. Without it, a single SELECT is a way out of the process:
// `read_text('/etc/passwd')` reads a file the audit was never pointed at, and
// `COPY orders TO '/tmp/x.csv'` writes customer data somewhere the egress
// guard cannot see it. Neither is prevented by opening the database read-only,
// because both are about the host's filesystem rather than the database's.
//
// This is DuckDB refusing, not Veritix pattern-matching the query text, which
// is the only version of "read-only" worth relying on when the query was
// written by a language model. The statement guard in agent/tools is the
// second layer, not the first.
//
// It is irreversible by design: lock_configuration means that a later
// SET enable_external_access = true is rejected, so an agent that talks
// Veritix into running one gains nothing.
func (e *Engine) Lockdown(ctx context.Context) error {
	if e.locked {
		return nil
	}
	for _, stmt := range []string{
		"SET enable_external_access = false",
		"SET lock_configuration = true",
	} {
		if _, err := e.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("engine: locking down: %w", err)
		}
	}
	e.locked = true
	e.log.Debug("engine locked down: no filesystem access, configuration frozen")
	return nil
}

// LockedDown reports whether Lockdown has been applied.
func (e *Engine) LockedDown() bool { return e.locked }

// Close releases the database.
func (e *Engine) Close() error {
	if e == nil || e.db == nil {
		return nil
	}
	return e.db.Close()
}

// DB exposes the underlying handle for callers that need database/sql
// directly, such as bulk loading.
func (e *Engine) DB() *sql.DB { return e.db }

// Path reports the backing file, or "" for an in-memory database.
func (e *Engine) Path() string { return e.path }

// ReadOnly reports whether DuckDB will refuse writes on this handle.
func (e *Engine) ReadOnly() bool { return e.readOnly }

// withTimeout applies the configured per-query timeout unless the caller's
// context already has an earlier deadline.
func (e *Engine) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.cfg.QueryTimeout <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= e.cfg.QueryTimeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.cfg.QueryTimeout)
}

// Exec runs a statement that returns no rows.
func (e *Engine) Exec(ctx context.Context, query string, args ...any) error {
	ctx, cancel := e.withTimeout(ctx)
	defer cancel()

	start := time.Now()
	_, err := e.db.ExecContext(ctx, query, args...)
	e.trace(ctx, query, start, err)
	if err != nil {
		return &QueryError{Query: query, Err: err}
	}
	return nil
}

// runQuery starts a statement and hands back its rows together with the
// function that releases the query's timeout.
//
// The timeout has to outlive the call that started the query, so it cannot be
// released by a defer here; the caller owns it until the rows are drained.
// Nothing outside this package sees rows: Collect is the exported way to run a
// statement, and it does the iterating, the row cap, and the Err check that a
// caller holding raw rows would have to remember for itself. That is the same
// reason there is no exported QueryRow.
func (e *Engine) runQuery(
	ctx context.Context, statement string, args ...any,
) (*sql.Rows, context.CancelFunc, error) {
	ctx, cancel := e.withTimeout(ctx)

	start := time.Now()
	rows, err := e.db.QueryContext(ctx, statement, args...) //nolint:rowserrcheck // drained and checked by Collect
	e.trace(ctx, statement, start, err)
	if err != nil {
		cancel()
		return nil, nil, &QueryError{Query: statement, Err: err}
	}
	return rows, cancel, nil
}

// scanRow runs a statement expected to return one row and scans it while the
// query's timeout is still armed.
//
// There is deliberately no exported QueryRow: sql.Row defers its work to
// Scan, so handing one back to a caller after this function's timeout has been
// released would cancel the query out from under them.
func (e *Engine) scanRow(ctx context.Context, query string, dest []any, args ...any) error {
	ctx, cancel := e.withTimeout(ctx)
	defer cancel()

	start := time.Now()
	err := e.db.QueryRowContext(ctx, query, args...).Scan(dest...)
	e.trace(ctx, query, start, err)
	return err
}

func (e *Engine) trace(_ context.Context, query string, start time.Time, err error) {
	if !e.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	attrs := []any{
		slog.String("sql", collapse(query)),
		slog.Duration("took", time.Since(start)),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	e.log.Debug("engine query", attrs...)
}

// collapse squashes a statement onto one line so debug logs stay readable.
func collapse(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	const max = 400
	if len(q) > max {
		return q[:max] + "…"
	}
	return q
}

// QueryError carries the statement that failed. Veritix builds a lot of SQL,
// and an error without the offending statement is close to undebuggable.
type QueryError struct {
	Query string
	Err   error
}

func (e *QueryError) Error() string {
	return fmt.Sprintf("engine: query failed: %v\n  sql: %s", e.Err, collapse(e.Query))
}

func (e *QueryError) Unwrap() error { return e.Err }
