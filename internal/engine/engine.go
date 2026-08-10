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

	"github.com/russellwallace/veritix/internal/config"
)

// Engine is a handle on one DuckDB database.
type Engine struct {
	db       *sql.DB
	cfg      config.Engine
	log      *slog.Logger
	path     string
	readOnly bool
}

// Open starts an engine. An empty path gives a transient in-memory database;
// a path gives a DuckDB file that survives the process, which is how the
// server caches an ingested dataset between runs.
func Open(ctx context.Context, path string, cfg config.Engine, log *slog.Logger) (*Engine, error) {
	return open(ctx, path, cfg, log, false)
}

// OpenReadOnly opens an existing database file with writes refused by DuckDB
// itself. Agent-authored SQL runs through a handle like this, so "read-only"
// is enforced by the engine rather than by pattern-matching the query text.
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
		// Settings are not parameterisable, so quote the value as a literal.
		stmt := fmt.Sprintf("SET %s = %s", s.name, Literal(s.value))
		if _, err := e.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("engine: applying %s=%s: %w", s.name, s.value, err)
		}
	}
	return nil
}

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

// Rows is a result set whose timeout stays armed until it is closed. The
// timeout has to outlive the call that started the query, so it is released by
// Close rather than by a defer at the call site.
type Rows struct {
	*sql.Rows
	cancel context.CancelFunc
}

// Close releases both the rows and the query's timeout.
func (r *Rows) Close() error {
	err := r.Rows.Close()
	r.cancel()
	return err
}

// Query runs a statement and returns its rows. The caller must close them.
func (e *Engine) Query(ctx context.Context, query string, args ...any) (*Rows, error) {
	ctx, cancel := e.withTimeout(ctx)

	start := time.Now()
	rows, err := e.db.QueryContext(ctx, query, args...)
	e.trace(ctx, query, start, err)
	if err != nil {
		cancel()
		return nil, &QueryError{Query: query, Err: err}
	}
	return &Rows{Rows: rows, cancel: cancel}, nil
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
