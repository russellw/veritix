package engine

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// Veritix builds SQL from identifiers that come out of customer files, which
// are untrusted input: a column can be named `"; DROP TABLE x; --` and Veritix
// still has to profile it. Every identifier and literal that reaches a
// statement goes through the helpers below, and nothing else interpolates into
// SQL except values bound as parameters.

// Ident quotes a table or column name for use in a statement.
func Ident(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Idents quotes a list of names and joins them with ", ".
func Idents(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = Ident(n)
	}
	return strings.Join(quoted, ", ")
}

// Literal quotes a string for use as a SQL literal. Prefer a bound parameter;
// this exists for the handful of places DuckDB does not accept one, such as
// SET statements and table functions taking file paths.
func Literal(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// Qualify renders table.column with both parts quoted.
func Qualify(table, column string) string {
	return Ident(table) + "." + Ident(column)
}

// SafeName turns an arbitrary string, typically a file or worksheet name, into
// a readable identifier. The result is only a suggestion: it still goes
// through Ident before reaching SQL, and the ingest layer resolves collisions.
func SafeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastUnderscore = false
		case !lastUnderscore && b.Len() > 0:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	name := strings.Trim(b.String(), "_")
	if name == "" {
		return "table"
	}
	// An identifier starting with a digit is legal once quoted but reads
	// badly in reports and in agent-authored SQL.
	if unicode.IsDigit(rune(name[0])) {
		name = "t_" + name
	}
	const maxLen = 63
	if len(name) > maxLen {
		name = strings.Trim(name[:maxLen], "_")
	}
	return name
}

// ResultSet is a fully-read query result. Results are materialised rather than
// streamed because they are small by construction: every caller either caps
// rows or is running an aggregate that returns a handful of them.
type ResultSet struct {
	Columns []string
	Types   []string
	Rows    [][]any
	// Truncated reports that the query produced more rows than the cap
	// allowed, so a caller does not mistake a partial answer for a whole one.
	Truncated bool
}

// Collect runs a query and reads the entire result, refusing to return more
// than maxRows. A maxRows of zero or less means "use the engine's configured
// cap"; the cap always applies, because an unbounded result from an
// agent-authored query is a way to exhaust memory.
func (e *Engine) Collect(ctx context.Context, query string, maxRows int, args ...any) (*ResultSet, error) {
	if maxRows <= 0 {
		maxRows = e.cfg.MaxResultRows
	}

	rows, err := e.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read path; the read error below is the one that matters

	cols, err := rows.Columns()
	if err != nil {
		return nil, &QueryError{Query: query, Err: err}
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, &QueryError{Query: query, Err: err}
	}

	rs := &ResultSet{Columns: cols, Types: make([]string, len(types))}
	for i, t := range types {
		rs.Types[i] = t.DatabaseTypeName()
	}

	for rows.Next() {
		if len(rs.Rows) >= maxRows {
			rs.Truncated = true
			break
		}
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, &QueryError{Query: query, Err: err}
		}
		rs.Rows = append(rs.Rows, cells)
	}
	if err := rows.Err(); err != nil {
		return nil, &QueryError{Query: query, Err: err}
	}
	return rs, nil
}

// ScanOne runs a query expected to yield exactly one row and scans it.
func (e *Engine) ScanOne(ctx context.Context, query string, dest []any, args ...any) error {
	if err := e.scanRow(ctx, query, dest, args...); err != nil {
		return &QueryError{Query: query, Err: err}
	}
	return nil
}

// TableExists reports whether a table of that name is present.
func (e *Engine) TableExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := e.ScanOne(ctx,
		`SELECT count(*) FROM duckdb_tables() WHERE table_name = ?`,
		[]any{&n}, name)
	return n > 0, err
}

// CountRows returns the row count of a table.
func (e *Engine) CountRows(ctx context.Context, table string) (int64, error) {
	var n int64
	q := fmt.Sprintf("SELECT count(*) FROM %s", Ident(table))
	if err := e.ScanOne(ctx, q, []any{&n}); err != nil {
		return 0, err
	}
	return n, nil
}
