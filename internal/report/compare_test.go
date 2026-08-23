package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/config"
)

// doc builds a small document by hand. The comparison is a function of two
// documents, so it is tested against documents rather than against runs: a
// fixture cannot be made to lose a column halfway through a test.
func doc(root string, findings []FindingInfo, tables []TableInfo) *Document {
	return &Document{
		Schema:  SchemaVersion,
		Run:     RunInfo{StartedAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)},
		Dataset: DatasetInfo{Root: root},

		Findings: findings,
		Tables:   tables,
	}
}

func f(id, rule, severity string, count int64) FindingInfo {
	return FindingInfo{
		ID: id, Rule: rule, Severity: severity, Count: count,
		Title: rule + " on something", Table: "orders_csv", Column: "amount",
	}
}

func TestCompareClassifiesEveryFinding(t *testing.T) {
	before := doc("/data", []FindingInfo{
		f("a", "column.missing_values", "error", 10),
		f("b", "column.mixed_date_formats", "warning", 4),
		f("c", "key.duplicate_values", "error", 2),
		f("d", "table.duplicate_rows", "info", 1),
	}, nil)
	after := doc("/data", []FindingInfo{
		f("a", "column.missing_values", "error", 20),      // worse
		f("b", "column.mixed_date_formats", "warning", 1), // better
		f("d", "table.duplicate_rows", "info", 1),         // unchanged
		f("e", "reference.orphan_values", "error", 3),     // new
		// c is gone: resolved
	}, nil)

	d := Compare(before, after)

	want := DeltaSummary{
		New: 1, Worsened: 1, Resolved: 1, Improved: 1, Unchanged: 1, NewErrors: 1,
	}
	if d.Summary != want {
		t.Errorf("summary = %+v, want %+v", d.Summary, want)
	}

	got := map[string]DeltaStatus{}
	for _, fd := range d.Findings {
		got[fd.ID] = fd.Status
	}
	for id, status := range map[string]DeltaStatus{
		"a": DeltaWorsened, "b": DeltaImproved, "c": DeltaResolved, "e": DeltaNew,
	} {
		if got[id] != status {
			t.Errorf("finding %s: status %q, want %q", id, got[id], status)
		}
	}

	// An unchanged finding is already in the document's own findings list, so
	// repeating it here would double the report to report nothing.
	if _, listed := got["d"]; listed {
		t.Error("an unchanged finding was repeated in the comparison")
	}
	if d.Summary.Unchanged != 1 {
		t.Errorf("the unchanged finding was not counted: %+v", d.Summary)
	}
}

// A regression is what a CI gate acts on, and the whole reason a team that
// cannot fix everything today can still refuse to make it worse.
func TestRegressionsCountNewAndWorseAtOrAboveTheSeverity(t *testing.T) {
	before := doc("/data", []FindingInfo{f("a", "r.a", "error", 10)}, nil)
	after := doc("/data", []FindingInfo{
		f("a", "r.a", "error", 11),  // worse, error
		f("b", "r.b", "warning", 1), // new, warning
		f("c", "r.c", "info", 1),    // new, info
	}, nil)
	d := Compare(before, after)

	for _, tc := range []struct {
		severity string
		want     int
	}{
		{"error", 1},
		{"warning", 2},
		{"info", 3},
	} {
		if got := d.Regressions(tc.severity); got != tc.want {
			t.Errorf("Regressions(%q) = %d, want %d", tc.severity, got, tc.want)
		}
	}

	// Nothing regressed against itself.
	if got := Compare(after, after).Regressions("info"); got != 0 {
		t.Errorf("a run compared with itself reported %d regressions", got)
	}
}

// A severity that moved is the one change to a finding that a count cannot
// show, and it happens whenever somebody edits a rule's severity.
func TestASeverityThatMovedIsReportedEvenWhenTheCountDidNot(t *testing.T) {
	before := doc("/data", []FindingInfo{f("a", "rule.mine", "warning", 3)}, nil)
	after := doc("/data", []FindingInfo{f("a", "rule.mine", "error", 3)}, nil)

	d := Compare(before, after)
	if len(d.Findings) != 1 {
		t.Fatalf("got %d findings, want the one whose severity moved", len(d.Findings))
	}
	got := d.Findings[0]
	if got.Status != DeltaUnchanged {
		t.Errorf("status = %q, want unchanged: the count did not move", got.Status)
	}
	if got.SeverityBefore != "warning" || got.Severity != "error" {
		t.Errorf("severity %q → %q, want warning → error", got.SeverityBefore, got.Severity)
	}
}

// Volume and schema drift is the half of the comparison no single-run check
// can see: an export that lost a third of its rows overnight is usually a
// broken extract, and nothing looking at one run can say so.
func TestCompareReportsVolumeAndSchemaDrift(t *testing.T) {
	before := doc("/data", nil, []TableInfo{
		{Name: "orders_csv", Source: "orders.csv", RowCount: 30000,
			Columns: []ColumnInfo{{Name: "id"}, {Name: "amount"}, {Name: "currency"}}},
		{Name: "legacy_csv", Source: "legacy.csv", RowCount: 5},
		{Name: "steady_csv", Source: "steady.csv", RowCount: 7,
			Columns: []ColumnInfo{{Name: "id"}}},
	})
	after := doc("/data", nil, []TableInfo{
		{Name: "orders_csv", Source: "orders.csv", RowCount: 20000,
			Columns: []ColumnInfo{{Name: "id"}, {Name: "amount"}, {Name: "region"}}},
		{Name: "steady_csv", Source: "steady.csv", RowCount: 7,
			Columns: []ColumnInfo{{Name: "id"}}},
		{Name: "new_csv", Source: "new.csv", RowCount: 12},
	})

	byName := map[string]TableDelta{}
	for _, td := range Compare(before, after).Tables {
		byName[td.Name] = td
	}

	if _, ok := byName["steady_csv"]; ok {
		t.Error("a table that did not move was listed as a change")
	}
	if td := byName["new_csv"]; td.Change != TableAdded || td.RowsAfter != 12 {
		t.Errorf("new_csv = %+v, want added with 12 rows", td)
	}
	if td := byName["legacy_csv"]; td.Change != TableRemoved || td.RowsBefore != 5 {
		t.Errorf("legacy_csv = %+v, want removed", td)
	}
	td := byName["orders_csv"]
	if td.Change != TableChanged || td.RowsBefore != 30000 || td.RowsAfter != 20000 {
		t.Errorf("orders_csv = %+v, want 30000 → 20000", td)
	}
	if len(td.ColumnsRemoved) != 1 || td.ColumnsRemoved[0] != "currency" {
		t.Errorf("columns removed = %v, want [currency]", td.ColumnsRemoved)
	}
	if len(td.ColumnsAdded) != 1 || td.ColumnsAdded[0] != "region" {
		t.Errorf("columns added = %v, want [region]", td.ColumnsAdded)
	}
}

// A finding is identified partly by where it sits, so a column that leaves the
// export takes its findings with it and they read as resolved. Reporting that
// as cleaned-up data would be the comparison telling a lie somebody acts on.
func TestARemovedColumnIsNotReportedAsCleanedUpData(t *testing.T) {
	before := doc("/data", []FindingInfo{f("a", "column.missing_values", "error", 9)},
		[]TableInfo{{Name: "orders_csv", RowCount: 5,
			Columns: []ColumnInfo{{Name: "id"}, {Name: "amount"}}}})
	after := doc("/data", nil,
		[]TableInfo{{Name: "orders_csv", RowCount: 5, Columns: []ColumnInfo{{Name: "id"}}}})

	d := Compare(before, after)
	if d.Summary.Resolved != 1 {
		t.Fatalf("resolved = %d, want 1", d.Summary.Resolved)
	}
	if len(d.Notes) == 0 {
		t.Fatal("a removed column produced no note beside the resolved finding")
	}
	if !strings.Contains(strings.Join(d.Notes, " "), "no longer in the export") {
		t.Errorf("notes = %v, want one saying the column is gone", d.Notes)
	}
}

func TestComparingTwoDifferentRootsSaysSo(t *testing.T) {
	d := Compare(doc("/data/january", nil, nil), doc("/data/february", nil, nil))
	if len(d.Notes) == 0 || !strings.Contains(d.Notes[0], "different roots") {
		t.Errorf("notes = %v, want one about the roots differing", d.Notes)
	}
}

// A first audit has nothing to compare against, and must read exactly as it
// did before this existed.
func TestWithoutABaselineNothingAboutTheReportChanges(t *testing.T) {
	res := run(t)

	var with, without bytes.Buffer
	if err := WriteText(&with, res, Options{Baseline: nil}); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if err := WriteText(&without, res, Options{}); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if with.String() != without.String() {
		t.Error("a nil baseline changed the report")
	}
	if strings.Contains(with.String(), "Since ") {
		t.Error("a report with no baseline printed a comparison section")
	}

	if d := Build(res, "test", Options{}); d.Comparison != nil {
		t.Error("a run with no baseline built a comparison")
	}
}

// The comparison repeats finding titles and column names, which is a second
// place for a cell value to escape into a file that gets emailed.
func TestTheComparisonCarriesNoRawValues(t *testing.T) {
	res := run(t)
	baseline := Build(res, "test", Options{})

	opts := Options{Indent: true, Baseline: &Baseline{Document: baseline, Source: "before.json"}}
	built := Build(res, "test", opts)
	if built.Comparison == nil {
		t.Fatal("a baseline produced no comparison")
	}

	for _, format := range []string{"json", "text", "html"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			var err error
			switch format {
			case "json":
				err = RenderJSON(&buf, built, opts)
			case "html":
				err = RenderHTML(&buf, built)
			default:
				err = RenderText(&buf, built, opts)
			}
			if err != nil {
				t.Fatalf("rendering %s: %v", format, err)
			}
			for _, raw := range rawValuesInFixture {
				if strings.Contains(buf.String(), raw) {
					t.Errorf("the %s comparison leaks the raw value %q", format, raw)
				}
			}
		})
	}
}

// A baseline is a file the customer kept, so the failure that matters is a
// path that is not a report at all.
func TestLoadDocumentRefusesSomethingThatIsNotAReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.json")
	if err := os.WriteFile(path, []byte(`{"hello": "world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDocument(path); err == nil {
		t.Error("a JSON file that is not a report was accepted as a baseline")
	} else if !strings.Contains(err.Error(), "veritix audit --format json") {
		t.Errorf("error %q does not say how to produce one", err)
	}

	if _, err := LoadDocument(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing baseline was accepted")
	}
}

// A baseline written by an earlier run has to survive a round trip through
// JSON, because that is the only form the CLI ever sees one in.
func TestABaselineSurvivesARoundTripThroughJSON(t *testing.T) {
	res, err := audit.Run(t.Context(), audit.Options{
		Paths: []string{fixtureDir}, Engine: config.Default().Engine,
	}, nil)
	if err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	var buf bytes.Buffer
	if err := WriteJSON(&buf, res, "test", Options{Indent: true}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadDocument(path)
	if err != nil {
		t.Fatalf("LoadDocument: %v", err)
	}

	built := Build(res, "test", Options{Baseline: &Baseline{Document: loaded, Source: path}})
	if built.Comparison == nil {
		t.Fatal("no comparison was built")
	}
	// The same run against itself: everything is unchanged and nothing moved.
	if built.Comparison.Summary.Changed() {
		t.Errorf("a run compared against its own report reported changes: %+v",
			built.Comparison.Summary)
	}
	if built.Comparison.Summary.Unchanged != len(built.Findings) {
		t.Errorf("unchanged = %d, want all %d findings",
			built.Comparison.Summary.Unchanged, len(built.Findings))
	}
	if len(built.Comparison.Tables) != 0 {
		t.Errorf("a run compared against itself reported table drift: %+v",
			built.Comparison.Tables)
	}
	if built.Comparison.Baseline.Source != path {
		t.Errorf("baseline source = %q, want %q", built.Comparison.Baseline.Source, path)
	}
}

// The text report is rendered from the document now rather than from the run,
// so the two have to agree about the headline numbers.
//
// Everything but the duration has to match exactly. The duration cannot: the
// document carries whole milliseconds, so a run of 197.6ms is 197ms here and
// 198ms in the run's own summary. That is the document being honest about the
// precision it has, and it is the reason this compares the two halves rather
// than the whole line.
func TestATextReportFromADocumentMatchesTheRun(t *testing.T) {
	res := run(t)
	got := summaryLine(Build(res, "test", Options{}))
	want := res.Summarize().String()

	const sep = " in "
	gotHead, gotTail, ok := strings.Cut(got, sep)
	if !ok {
		t.Fatalf("summary line %q has no duration", got)
	}
	wantHead, wantTail, _ := strings.Cut(want, sep)
	if gotHead != wantHead {
		t.Errorf("summary line\n got %q\nwant %q", gotHead, wantHead)
	}

	// The tail is the duration plus whatever follows it, so check the
	// remainder after the duration matches and that a duration was rendered.
	_, gotRest, _ := strings.Cut(gotTail, ")")
	_, wantRest, _ := strings.Cut(wantTail, ")")
	if gotRest != wantRest {
		t.Errorf("summary tail\n got %q\nwant %q", gotTail, wantTail)
	}
	if !strings.Contains(gotTail, "ms") {
		t.Errorf("summary line %q renders no duration", got)
	}
}

// The delta is part of the JSON contract, so it has to encode.
func TestTheComparisonEncodesAsJSON(t *testing.T) {
	d := Compare(
		doc("/data", []FindingInfo{f("a", "r.a", "error", 1)}, nil),
		doc("/data", []FindingInfo{f("a", "r.a", "error", 2)}, nil),
	)
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshaling the comparison: %v", err)
	}
	var back Delta
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshaling the comparison: %v", err)
	}
	if back.Summary != d.Summary || len(back.Findings) != len(d.Findings) {
		t.Errorf("the comparison did not survive a round trip: %+v", back)
	}
}
