package checks

import (
	"fmt"
	"strings"
	"testing"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/ingest"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/source"
)

// Checks have to keep working as a table grows, and the committed fixtures
// cannot show whether they do: at nine rows every defect is a large share of
// the column. Two placeholders among nine values is 22%, and a check that asks
// for 5% passes. The same defect in the file a customer actually audits is a
// fiftieth of a percent, and a proportional threshold goes quiet exactly where
// nobody is going to notice by eye — which is the only place any of this
// matters.
//
// These tests build a column two hundred thousand rows deep from DuckDB's own
// range(), so they cost milliseconds and say what a two-million-row export
// would say.
const scaleRows = 200_000

// profileOneColumn creates a single-column table from an expression over
// range(scaleRows) and runs the real profiler and the real checks over it.
func profileOneColumn(t *testing.T, column, expr string) *finding.Set {
	t.Helper()
	e, prof := profileColumnOnly(t, column, expr)
	return runChecksOn(t, e, prof)
}

func profileColumnOnly(t *testing.T, column, expr string) (*engine.Engine, *profile.Dataset) {
	t.Helper()
	ctx := t.Context()

	e, err := engine.Open(ctx, "", config.Default().Engine, nil)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	const table = "big"
	create := fmt.Sprintf("CREATE TABLE %s AS SELECT %s AS %s FROM range(%d) t(i)",
		engine.Ident(table), expr, engine.Ident(column), scaleRows)
	if err := e.Exec(ctx, create); err != nil {
		t.Fatalf("creating the table: %v", err)
	}

	loaded := &ingest.Result{Tables: []*ingest.Table{{
		Ref:      source.TableRef{Name: table, Display: "big.csv"},
		Columns:  []ingest.Column{{Name: column, Original: column, Ordinal: 1, SniffedType: "VARCHAR"}},
		RowCount: scaleRows,
	}}}

	prof, err := profile.Run(ctx, e, loaded, profile.Options{}, nil)
	if err != nil {
		t.Fatalf("profile.Run: %v", err)
	}
	return e, prof
}

func runChecksOn(t *testing.T, e *engine.Engine, prof *profile.Dataset) *finding.Set {
	t.Helper()
	set, err := Run(t.Context(), e, prof, nil)
	if err != nil {
		t.Fatalf("checks.Run: %v", err)
	}

	// Every finding is held to its own evidence here as everywhere else. A
	// check that reports a count its query cannot produce is a check that
	// disagrees with itself, and at this size nobody would spot it by reading
	// the report.
	dropped, err := set.Verify(t.Context(), e)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, f := range dropped {
		t.Errorf("finding %s did not reproduce from its own evidence: %s", f.Rule, f.Title)
	}
	return set
}

func found(set *finding.Set, rule string) *finding.Finding {
	for _, f := range set.All() {
		if f.Rule == rule {
			return &f
		}
	}
	return nil
}

// A placeholder is not a rate. Ten "N/A"s in two hundred thousand rows defeat
// a null check exactly as thoroughly as two in nine, and are harder to see.
func TestARarePlaceholderIsFoundInALargeColumn(t *testing.T) {
	set := profileOneColumn(t, "region",
		"CASE WHEN i % 20000 = 0 THEN 'N/A' ELSE 'R' || lpad((i % 24)::VARCHAR, 2, '0') END")

	f := found(set, "column.missing_values")
	if f == nil {
		t.Fatalf("a column with 10 placeholders in %d rows reported no missing values", scaleRows)
	}
	if f.Count != scaleRows/20000 {
		t.Errorf("counted %d placeholders, want %d", f.Count, scaleRows/20000)
	}
	if !strings.Contains(f.Detail, "placeholder") {
		t.Errorf("the detail does not mention the placeholders: %s", f.Detail)
	}
}

// The other direction, which is what makes the first affordable. Every id in a
// column of unique ids occurs once, including the one that happens to be
// 999999, and a magic number that is no more repeated than the data around it
// is a number.
func TestAnIncidentalMagicNumberIsNotAPlaceholder(t *testing.T) {
	set := profileOneColumn(t, "order_id", "(i + 1)::VARCHAR")

	if f := found(set, "column.missing_values"); f != nil {
		t.Errorf("a column of distinct ids was reported as missing values: %s", f.Title)
	}
}

// A magic number on the wrong side of zero is a placeholder however rare it
// is, and however popular some legitimate value happens to be. 1000 here is a
// default that occurs far more often than -999, so frequency alone would miss
// it; nothing else in the column is negative.
func TestANegativeMagicNumberIsAPlaceholder(t *testing.T) {
	set := profileOneColumn(t, "credit_limit", `CASE
		WHEN i % 20000 = 0 THEN '-999'
		WHEN i % 3 = 0 THEN '1000'
		ELSE (1000 + i % 50000)::VARCHAR END`)

	f := found(set, "column.missing_values")
	if f == nil {
		t.Fatalf("-999 among credit limits of 1000 and up was not reported")
	}
	if f.Count != scaleRows/20000 {
		t.Errorf("counted %d placeholders, want %d", f.Count, scaleRows/20000)
	}
}

// The other direction again, and the reason the test is sign rather than
// distance. The largest value of a large column of numbers is data, and it has
// to be some number: a rule that called a magic number a placeholder for
// sitting past the rest of the column would report the maximum of every
// uniform column in every export.
func TestTheTopOfTheRangeIsNotAPlaceholder(t *testing.T) {
	set := profileOneColumn(t, "reading",
		"CASE WHEN i = 0 THEN '999999' ELSE (i % 900000)::VARCHAR END")

	if f := found(set, "column.missing_values"); f != nil {
		t.Errorf("the largest value in a column of readings was called missing: %s", f.Title)
	}
}

// A second date format is rare by nature in a large file — the exports were
// concatenated, or one system's rows were appended to another's — and every
// one of those rows is read as the wrong day by a reader that assumes the
// majority format.
func TestARareSecondDateFormatIsFoundInALargeColumn(t *testing.T) {
	set := profileOneColumn(t, "signup_date", `CASE
		WHEN i % 10000 = 0 THEN strftime(DATE '2019-01-01' + i::INTEGER, '%d/%m/%Y')
		ELSE strftime(DATE '2019-01-01' + i::INTEGER, '%Y-%m-%d') END`)

	if f := found(set, "column.mixed_date_formats"); f == nil {
		t.Fatalf("a column with %d dates in a second format reported one format",
			scaleRows/10000)
	}
}

// And the reason the threshold was proportional in the first place: a column
// written entirely day-first is also parsed by the month-first pattern
// wherever the day is 12 or less, so counting formats over the whole column
// reports two. One format is not a mixture, at any size.
func TestOneDateFormatIsNotReportedAsTwo(t *testing.T) {
	set := profileOneColumn(t, "signup_date",
		"strftime(DATE '2019-01-01' + (i % 3650)::INTEGER, '%d/%m/%Y')")

	if f := found(set, "column.mixed_date_formats"); f != nil {
		t.Errorf("a column written in one format was reported as mixed: %s", f.Title)
	}
}

// A measurement that did not run has to be reported as one. The stub a failed
// column leaves behind reads as a clean column — no nulls, no placeholders, no
// type violations — and on the largest table in a dataset, which is where a
// per-query timeout actually runs out, nobody is going to notice that a column
// simply has no findings. Measured: every column of a twenty-million-row table
// timed out against the default two-minute limit, and the audit reported the
// other four tables and called itself complete.
func TestAColumnThatCouldNotBeMeasuredSaysSo(t *testing.T) {
	e, prof := profileColumnOnly(t, "region", "'R' || lpad((i % 24)::VARCHAR, 2, '0')")
	prof.Tables[0].Columns[0].Unprofiled = profile.UnprofiledTimeout

	set := runChecksOn(t, e, prof)

	f := found(set, "column.not_profiled")
	if f == nil {
		t.Fatalf("an unmeasured column produced no finding at all")
	}
	if f.Severity != finding.Warning {
		t.Errorf("severity %v, want warning: it is the audit declining to make a claim, "+
			"not a defect in the data", f.Severity)
	}
	// Table-level checks still run: they query the data rather than read the
	// column's measurements. What must not appear is a second finding about
	// this column, since every one of those would be drawn from measurements
	// that do not exist.
	for _, other := range set.All() {
		if other.Location.Column != "" && other.Rule != "column.not_profiled" {
			t.Errorf("an unmeasured column produced %s, which is a claim drawn from "+
				"measurements that do not exist: %s", other.Rule, other.Title)
		}
	}
	if f := found(set, "table.no_candidate_key"); f != nil {
		t.Errorf("a table with an unmeasured column was reported as having no key, " +
			"which is a claim about the column nothing measured")
	}
}
