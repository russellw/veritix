package ingest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/source"
)

const fixtureDir = "../../testdata/dirty-retail"

func loadFixture(t *testing.T) (*engine.Engine, *Result) {
	t.Helper()
	ctx := t.Context()

	e, err := engine.Open(ctx, "", config.Default().Engine, nil)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	ds, err := source.Discover([]string{fixtureDir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	res, err := Load(ctx, e, ds, Options{}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return e, res
}

func tableByDisplay(res *Result, display string) *Table {
	for _, t := range res.Tables {
		if t.Ref.Display == display {
			return t
		}
	}
	return nil
}

func TestDiscoverySkipsAndReports(t *testing.T) {
	ds, err := source.Discover([]string{fixtureDir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	got := make(map[string]bool)
	for _, f := range ds.Files {
		got[f.Rel] = true
	}
	for _, want := range []string{"customers.csv", "orders.csv", "products.tsv", "regions.csv", "sales.xlsx"} {
		if !got[want] {
			t.Errorf("Discover missed %s", want)
		}
	}

	// A file Veritix declines to read is itself worth telling the user about.
	skipped := make(map[string]string)
	for _, s := range ds.Skipped {
		skipped[s.Rel] = s.Reason
	}
	if r, ok := skipped["legacy.xls"]; !ok {
		t.Error("legacy .xls should be reported as skipped, not silently ignored")
	} else if !strings.Contains(r, ".xls") {
		t.Errorf("unhelpful skip reason for legacy.xls: %q", r)
	}
	if _, ok := skipped["~$sales.xlsx"]; !ok {
		t.Error("an Excel lock file should be reported as skipped")
	}
}

func TestLoadAllFixtureTables(t *testing.T) {
	_, res := loadFixture(t)

	want := []string{
		"customers.csv",
		"orders.csv",
		"products.tsv",
		"regions.csv",
		"sales.xlsx#Q1",
		"sales.xlsx#Reference",
		"sales.xlsx#Archive",
	}
	for _, w := range want {
		if tableByDisplay(res, w) == nil {
			var got []string
			for _, tb := range res.Tables {
				got = append(got, tb.Ref.Display)
			}
			t.Fatalf("missing table %q; loaded: %v", w, got)
		}
	}

	// Table names must be unique, since they become SQL identifiers.
	seen := make(map[string]string)
	for _, tb := range res.Tables {
		if prev, dup := seen[tb.Ref.Name]; dup {
			t.Errorf("duplicate table name %q for both %s and %s", tb.Ref.Name, prev, tb.Ref.Display)
		}
		seen[tb.Ref.Name] = tb.Ref.Display
	}
}

// Everything must land as text: a normal import would turn "N/A" in a numeric
// column into NULL, which is precisely the evidence an audit needs to keep.
func TestEverythingLoadsAsText(t *testing.T) {
	e, res := loadFixture(t)

	for _, tb := range res.Tables {
		q := fmt.Sprintf(
			`SELECT count(*) FROM duckdb_columns() WHERE table_name = %s AND data_type <> 'VARCHAR'`,
			engine.Literal(tb.Ref.Name))
		var n int
		if err := e.ScanOne(t.Context(), q, []any{&n}); err != nil {
			t.Fatalf("checking column types of %s: %v", tb.Ref.Name, err)
		}
		if n != 0 {
			t.Errorf("table %s has %d non-VARCHAR columns", tb.Ref.Display, n)
		}
	}

	orders := tableByDisplay(res, "orders.csv")
	var amount string
	q := fmt.Sprintf(`SELECT amount FROM %s WHERE order_id = '1008'`, engine.Ident(orders.Ref.Name))
	if err := e.ScanOne(t.Context(), q, []any{&amount}); err != nil {
		t.Fatalf("reading a sentinel value: %v", err)
	}
	if amount != "N/A" {
		t.Errorf(`amount for order 1008 = %q, want the literal "N/A" preserved`, amount)
	}
}

func TestDuplicateHeaderIsPreservedAndReported(t *testing.T) {
	_, res := loadFixture(t)
	customers := tableByDisplay(res, "customers.csv")

	// customers.csv repeats the "region" header. DuckDB renames the second one
	// so its schema stays valid; Veritix must still know the file said "region"
	// twice, and must say so.
	var renamed *Column
	for i := range customers.Columns {
		if customers.Columns[i].Renamed {
			renamed = &customers.Columns[i]
		}
	}
	if renamed == nil {
		t.Fatal("the repeated header column should be marked as renamed")
	}
	if renamed.Original != "region" {
		t.Errorf("original name = %q, want %q", renamed.Original, "region")
	}
	if renamed.Name == renamed.Original {
		t.Error("the loaded name should differ from the original for a repeated header")
	}

	if !hasNote(customers.Notes, "csv.header_duplicate") {
		t.Errorf("expected a csv.header_duplicate note, got %v", codesOf(customers.Notes))
	}
}

func TestRaggedRowsAreRejectedAndCounted(t *testing.T) {
	_, res := loadFixture(t)
	orders := tableByDisplay(res, "orders.csv")

	// orders.csv has one short row (1005) and one over-long row (1006, where a
	// European decimal comma split the amount into two fields).
	if orders.RejectCount != 2 {
		t.Errorf("RejectCount = %d, want 2", orders.RejectCount)
	}
	if len(orders.Rejects) != 2 {
		t.Fatalf("got %d reject samples, want 2", len(orders.Rejects))
	}

	kinds := make(map[string]bool)
	for _, r := range orders.Rejects {
		kinds[r.ErrorType] = true
		if r.Line == 0 {
			t.Error("a rejected row must carry its line number so a user can find it")
		}
	}
	for _, want := range []string{"MISSING COLUMNS", "TOO MANY COLUMNS"} {
		if !kinds[want] {
			t.Errorf("missing reject type %q; got %v", want, kinds)
		}
	}

	if !hasNote(orders.Notes, "ingest.rejected_rows") {
		t.Error("rejected rows must produce a note: they are silently absent from every total")
	}

	// The eight readable rows must still have loaded.
	if orders.RowCount != 8 {
		t.Errorf("RowCount = %d, want 8 readable rows", orders.RowCount)
	}
}

func TestTabSeparatedAndHeaderlessFile(t *testing.T) {
	_, res := loadFixture(t)
	products := tableByDisplay(res, "products.tsv")

	if products.Dialect == nil {
		t.Fatal("a delimited file must carry its dialect")
	}
	if products.Dialect.Delimiter != "\t" {
		t.Errorf("delimiter = %q, want a tab", products.Dialect.Delimiter)
	}
	if products.RowCount != 3 {
		t.Errorf("RowCount = %d, want 3", products.RowCount)
	}
	if products.Dialect.HasHeader {
		t.Error("products.tsv has no header row and should not be read as having one")
	}
	if !hasNote(products.Notes, "csv.no_header") {
		t.Error("a headerless file should be reported: columns are then positional")
	}
}

// A Latin-1 file read as UTF-8 mangles accented characters, which silently
// turns one customer into two during de-duplication.
func TestLatin1FileDecodesCorrectly(t *testing.T) {
	e, res := loadFixture(t)
	regions := tableByDisplay(res, "regions.csv")

	if regions.Dialect.Encoding != source.EncodingLatin1 {
		t.Errorf("encoding = %q, want latin-1", regions.Dialect.Encoding)
	}
	if !hasNote(regions.Notes, "csv.encoding_not_utf8") {
		t.Error("a non-UTF-8 file should be reported")
	}

	rs, err := e.Collect(t.Context(),
		fmt.Sprintf("SELECT city FROM %s ORDER BY city", engine.Ident(regions.Ref.Name)), 10)
	if err != nil {
		t.Fatalf("reading cities: %v", err)
	}
	var cities []string
	for _, r := range rs.Rows {
		cities = append(cities, fmt.Sprint(r[0]))
	}
	joined := strings.Join(cities, ",")
	for _, want := range []string{"Zürich", "München", "Montréal"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q among the decoded cities, got %v", want, cities)
		}
	}
	if strings.Contains(joined, "�") {
		t.Errorf("decoding produced replacement characters: %v", cities)
	}
}

func TestExcelStructureIsCaptured(t *testing.T) {
	_, res := loadFixture(t)

	q1 := tableByDisplay(res, "sales.xlsx#Q1")
	if q1.Sheet == nil {
		t.Fatal("an Excel table must carry its sheet description")
	}
	s := q1.Sheet

	// The header is on row 3, under two title rows.
	if s.HeaderRow != 3 {
		t.Errorf("HeaderRow = %d, want 3", s.HeaderRow)
	}
	if !hasNote(q1.Notes, "excel.header_offset") {
		t.Error("a header below row 1 should be reported")
	}
	if !hasNote(q1.Notes, "excel.merged_cells") {
		t.Error("merged cells should be reported")
	}
	if !hasNote(q1.Notes, "excel.formula_errors") {
		t.Error("Excel error values left in cells should be reported")
	}
	if s.HiddenRows == 0 {
		t.Error("the hidden data row should be counted")
	}
	if !hasNote(q1.Notes, "excel.hidden_rows") {
		t.Error("hidden rows should be reported: they are invisible but still counted downstream")
	}
	if s.FormulaErrors["#DIV/0!"] == 0 || s.FormulaErrors["#REF!"] == 0 {
		t.Errorf("expected both #DIV/0! and #REF! to be counted, got %v", s.FormulaErrors)
	}
	if s.BlankSeparators == 0 {
		t.Error("the blank row before the stacked TOTAL table should be counted")
	}

	archive := tableByDisplay(res, "sales.xlsx#Archive")
	if archive.Sheet.Visible {
		t.Error("the Archive sheet is hidden and should be recorded as such")
	}
	if !hasNote(archive.Notes, "excel.hidden_sheet") {
		t.Error("a hidden worksheet should be reported")
	}
}

// Reading the same directory twice must produce identical table names, or
// reports and stored findings will not line up between runs.
func TestLoadIsDeterministic(t *testing.T) {
	_, first := loadFixture(t)
	_, second := loadFixture(t)

	if len(first.Tables) != len(second.Tables) {
		t.Fatalf("table counts differ: %d then %d", len(first.Tables), len(second.Tables))
	}
	for i := range first.Tables {
		a, b := first.Tables[i], second.Tables[i]
		if a.Ref.Name != b.Ref.Name || a.Ref.Display != b.Ref.Display {
			t.Errorf("table %d differs: %s/%s then %s/%s",
				i, a.Ref.Name, a.Ref.Display, b.Ref.Name, b.Ref.Display)
		}
		if a.RowCount != b.RowCount {
			t.Errorf("%s row count differs: %d then %d", a.Ref.Display, a.RowCount, b.RowCount)
		}
	}
}

func hasNote(notes []source.Note, code string) bool {
	for _, n := range notes {
		if n.Code == code {
			return true
		}
	}
	return false
}

func codesOf(notes []source.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.Code)
	}
	return out
}
