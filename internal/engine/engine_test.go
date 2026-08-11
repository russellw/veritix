package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/russellw/veritix/internal/config"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := Open(t.Context(), "", config.Default().Engine, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestOpenAndQuery(t *testing.T) {
	e := testEngine(t)

	var v string
	if err := e.ScanOne(t.Context(), "SELECT version()", []any{&v}); err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(v, "v") {
		t.Errorf("unexpected DuckDB version string %q", v)
	}
}

// Column names come out of customer files, so a name containing quotes,
// semicolons, or SQL keywords has to profile like any other.
func TestIdentQuotingResistsInjection(t *testing.T) {
	e := testEngine(t)
	ctx := t.Context()

	hostile := []string{
		`plain`,
		`with space`,
		`quote"inside`,
		`"; DROP TABLE t; --`,
		`select`,
		`Ünïcode`,
		`tab	inside`,
	}

	cols := make([]string, len(hostile))
	for i, h := range hostile {
		cols[i] = Ident(h) + " VARCHAR"
	}
	create := fmt.Sprintf("CREATE TABLE t (%s)", strings.Join(cols, ", "))
	if err := e.Exec(ctx, create); err != nil {
		t.Fatalf("create with hostile column names: %v", err)
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(hostile)), ", ")
	args := make([]any, len(hostile))
	for i := range args {
		args[i] = fmt.Sprintf("v%d", i)
	}
	insert := fmt.Sprintf("INSERT INTO t VALUES (%s)", placeholders)
	if err := e.Exec(ctx, insert, args...); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The table must still exist: a successful injection would have dropped it.
	ok, err := e.TableExists(ctx, "t")
	if err != nil {
		t.Fatalf("TableExists: %v", err)
	}
	if !ok {
		t.Fatal("table t is gone — identifier quoting failed to contain the injection")
	}

	for i, h := range hostile {
		var got string
		q := fmt.Sprintf("SELECT %s FROM t", Ident(h))
		if err := e.ScanOne(ctx, q, []any{&got}); err != nil {
			t.Errorf("selecting column %q: %v", h, err)
			continue
		}
		if want := fmt.Sprintf("v%d", i); got != want {
			t.Errorf("column %q = %q, want %q", h, got, want)
		}
	}
}

func TestLiteralQuoting(t *testing.T) {
	e := testEngine(t)

	for _, in := range []string{`plain`, `it's`, `''`, `a'; DROP TABLE x; --`} {
		var got string
		q := "SELECT " + Literal(in)
		if err := e.ScanOne(t.Context(), q, []any{&got}); err != nil {
			t.Errorf("Literal(%q): %v", in, err)
			continue
		}
		if got != in {
			t.Errorf("Literal round-trip: got %q, want %q", got, in)
		}
	}
}

func TestCollectCapsRows(t *testing.T) {
	e := testEngine(t)

	rs, err := e.Collect(t.Context(), "SELECT i FROM range(100) t(i)", 10)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rs.Rows) != 10 {
		t.Errorf("got %d rows, want the cap of 10", len(rs.Rows))
	}
	if !rs.Truncated {
		t.Error("a capped result must be reported as truncated")
	}

	rs, err = e.Collect(t.Context(), "SELECT i FROM range(3) t(i)", 10)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rs.Truncated {
		t.Error("a complete result must not be reported as truncated")
	}
	if len(rs.Rows) != 3 {
		t.Errorf("got %d rows, want 3", len(rs.Rows))
	}
}

func TestQueryErrorCarriesStatement(t *testing.T) {
	e := testEngine(t)

	_, err := e.Collect(t.Context(), "SELECT * FROM no_such_table", 1)
	if err == nil {
		t.Fatal("want an error")
	}
	var qe *QueryError
	if !asQueryError(err, &qe) {
		t.Fatalf("want a *QueryError, got %T", err)
	}
	if !strings.Contains(qe.Query, "no_such_table") {
		t.Errorf("the error should carry the failing statement, got %q", qe.Query)
	}
}

func asQueryError(err error, target **QueryError) bool {
	if qe, ok := err.(*QueryError); ok { //nolint:errorlint // deliberate direct check
		*target = qe
		return true
	}
	return false
}

func TestReadOnlyRefusesWrites(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "ds.duckdb")

	rw, err := Open(ctx, path, config.Default().Engine, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := rw.Exec(ctx, "CREATE TABLE t AS SELECT 1 AS a"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro, err := OpenReadOnly(ctx, path, config.Default().Engine, nil)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close() //nolint:errcheck // test cleanup

	var n int
	if err := ro.ScanOne(ctx, "SELECT count(*) FROM t", []any{&n}); err != nil {
		t.Fatalf("reads must still work: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d rows, want 1", n)
	}

	// Writes are refused by DuckDB, not by inspecting the query text.
	if err := ro.Exec(ctx, "CREATE TABLE evil AS SELECT 1"); err == nil {
		t.Error("a read-only engine must refuse DDL")
	}
	if err := ro.Exec(ctx, "DELETE FROM t"); err == nil {
		t.Error("a read-only engine must refuse DML")
	}
}

func TestQueryTimeout(t *testing.T) {
	cfg := config.Default().Engine
	cfg.QueryTimeout = 50 * time.Millisecond

	e, err := Open(t.Context(), "", cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close() //nolint:errcheck // test cleanup

	// A deliberately expensive cross join; it must be interrupted rather than
	// run to completion.
	start := time.Now()
	_, err = e.Collect(t.Context(),
		"SELECT count(*) FROM range(10000000) a, range(10000000) b", 1)
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("query ran for %v; the timeout did not interrupt it", elapsed)
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"orders.csv":             "orders_csv",
		"Q1 Sales Report.xlsx":   "q1_sales_report_xlsx",
		"2024-figures":           "t_2024_figures",
		"__weird__":              "weird",
		"":                       "table",
		"!!!":                    "table",
		"Sheet1":                 "sheet1",
		"customers (EMEA) FINAL": "customers_emea_final",
	}
	for in, want := range cases {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}

	long := strings.Repeat("a", 200)
	if got := SafeName(long); len(got) > 63 {
		t.Errorf("SafeName produced a %d-character identifier", len(got))
	}
}

func TestCountRows(t *testing.T) {
	e := testEngine(t)
	ctx := t.Context()

	if err := e.Exec(ctx, `CREATE TABLE "odd name" AS SELECT i FROM range(7) t(i)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := e.CountRows(ctx, "odd name")
	if err != nil {
		t.Fatalf("CountRows: %v", err)
	}
	if n != 7 {
		t.Errorf("CountRows = %d, want 7", n)
	}
}

// Lockdown is the boundary the agent's SQL tool sits behind. A SELECT that
// reaches outside the database is the way a read-only query becomes an
// exfiltration channel, so these are checked against DuckDB itself rather than
// against Veritix's opinion of what the statement said.
func TestLockdownClosesTheFilesystem(t *testing.T) {
	ctx := t.Context()
	e := testEngine(t)

	if err := e.Exec(ctx, "CREATE TABLE t AS SELECT 1 AS a"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Reading files has to work beforehand, or the test proves nothing: the
	// whole pipeline reads the customer's CSVs through DuckDB.
	var before string
	readFile := "SELECT content FROM read_text(" + Literal(writeTempFile(t, "hello")) + ")"
	if err := e.ScanOne(ctx, readFile, []any{&before}); err != nil {
		t.Fatalf("reading a file before lockdown: %v", err)
	}

	if err := e.Lockdown(ctx); err != nil {
		t.Fatalf("Lockdown: %v", err)
	}
	if !e.LockedDown() {
		t.Error("LockedDown reports false after Lockdown")
	}

	var after string
	if err := e.ScanOne(ctx, readFile, []any{&after}); err == nil {
		t.Error("a locked-down engine read a file off the host")
	}

	out := filepath.Join(t.TempDir(), "exfiltrated.csv")
	if err := e.Exec(ctx, "COPY t TO "+Literal(out)); err == nil {
		t.Error("a locked-down engine wrote a table to the host filesystem")
	}

	// The lock has to survive the agent asking for it back.
	if err := e.Exec(ctx, "SET enable_external_access = true"); err == nil {
		t.Error("filesystem access could be turned back on")
	}

	// None of which may cost the ability to query the data that is loaded.
	var n int
	if err := e.ScanOne(ctx, "SELECT count(*) FROM t", []any{&n}); err != nil || n != 1 {
		t.Errorf("querying loaded data after lockdown: n=%d err=%v", n, err)
	}

	// Twice is not an error: the pipeline locks down once per run, and a
	// caller should not have to track whether it already happened.
	if err := e.Lockdown(ctx); err != nil {
		t.Errorf("Lockdown is not idempotent: %v", err)
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "readable.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	return path
}

// Model-authored SQL is classified by DuckDB's own parser rather than by
// matching patterns in the text, and the classification decides whether a
// result is shown as a number or as a shape.
func TestAnalyzeSelectClassifiesOutputColumns(t *testing.T) {
	ctx := t.Context()
	e := testEngine(t)
	if err := e.Exec(ctx, `CREATE TABLE orders AS SELECT 'ACME' AS customer, 89.99 AS amount`); err != nil {
		t.Fatalf("create: %v", err)
	}

	cases := []struct {
		query string
		want  []bool
	}{
		{"SELECT count(*) FROM orders", []bool{true}},
		{"SELECT count(*) FILTER (WHERE amount > 0) FROM orders", []bool{true}},
		// An expression built only from aggregates and constants is still a
		// statistic; without this the useful half of an auditor's questions
		// would come back shaped.
		{"SELECT round(sum(amount) / count(*), 2) FROM orders", []bool{true}},
		{"SELECT 'label', count(*) FROM orders", []bool{true, true}},
		// A grouping key is a cell value, however it is dressed up.
		{"SELECT customer, count(*) FROM orders GROUP BY customer", []bool{false, true}},
		{"SELECT amount FROM orders", []bool{false}},
		{"SELECT * FROM orders", []bool{false}},
		{"SELECT upper(customer) FROM orders", []bool{false}},
		// A shape the parse-tree reader does not model must fail safe: no
		// column is called an aggregate, so every cell is shaped.
		{"SELECT count(*) FROM orders UNION ALL SELECT count(*) FROM orders", []bool{}},
	}

	for _, tc := range cases {
		a, err := e.AnalyzeSelect(ctx, tc.query)
		if err != nil {
			t.Errorf("AnalyzeSelect(%q): %v", tc.query, err)
			continue
		}
		if len(a.Aggregate) != len(tc.want) {
			t.Errorf("%q: got %d columns, want %d", tc.query, len(a.Aggregate), len(tc.want))
			continue
		}
		for i := range tc.want {
			if a.Aggregate[i] != tc.want[i] {
				t.Errorf("%q: column %d aggregate = %v, want %v", tc.query, i, a.Aggregate[i], tc.want[i])
			}
		}
	}
}

// Everything that is not one read-only statement is refused by the parser, so
// Veritix never has to hold an opinion about what a write looks like.
func TestAnalyzeSelectRefusesEverythingElse(t *testing.T) {
	ctx := t.Context()
	e := testEngine(t)
	if err := e.Exec(ctx, `CREATE TABLE orders AS SELECT 1 AS a`); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, q := range []string{
		"COPY orders TO '/tmp/exfiltrated.csv'",
		"DROP TABLE orders",
		"DELETE FROM orders",
		"INSERT INTO orders VALUES (2)",
		"ATTACH 'other.duckdb'",
		"SELECT 1; DROP TABLE orders",
		"INSTALL httpfs",
		"SET enable_external_access = true",
		"SELEC nonsense",
	} {
		if _, err := e.AnalyzeSelect(ctx, q); err == nil {
			t.Errorf("AnalyzeSelect accepted %q", q)
		}
	}

	// The table has to survive all of that.
	ok, err := e.TableExists(ctx, "orders")
	if err != nil || !ok {
		t.Errorf("orders is gone: ok=%v err=%v", ok, err)
	}
}
