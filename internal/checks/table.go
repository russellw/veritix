package checks

import (
	"context"
	"fmt"
	"strings"

	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
)

// checkEmptyTable reports a file that produced no rows.
func checkEmptyTable(tc *tableContext) []finding.Finding {
	if tc.table.RowCount > 0 {
		return nil
	}
	return []finding.Finding{{
		Rule:     "table.empty",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s has a header but no data rows", tc.table.Display),
		Detail: "The file describes a shape but contains nothing. An empty export is " +
			"usually a failed extract rather than a genuine absence of data, and it will " +
			"be indistinguishable from \"nothing happened\" in any report built from it.",
		Remedy:   "Confirm the extract that produced this file completed successfully.",
		Location: tc.location(nil),
		Evidence: finding.Evidence{
			CountQuery: fmt.Sprintf("SELECT count(*) FROM %s", tc.quoted),
			Expected:   "at least one data row",
			Observed:   "none",
		},
	}}
}

// checkUnreadableRows reports rows the parser could not read at all.
//
// This is the most under-appreciated defect in a data file: the rows are not
// wrong, they are absent. Nothing downstream can notice a row that was never
// loaded, so every count, sum, and reconciliation is quietly short.
func checkUnreadableRows(tc *tableContext) []finding.Finding {
	lt := tc.table.Ingest
	if lt == nil || lt.RejectCount == 0 {
		return nil
	}

	lines := make([]string, 0, 3)
	for _, r := range lt.Rejects {
		if len(lines) == 3 {
			break
		}
		lines = append(lines, fmt.Sprintf("line %d (%s)", r.Line, strings.ToLower(r.ErrorType)))
	}

	total := tc.table.RowCount + lt.RejectCount
	return []finding.Finding{{
		Rule:     "table.unreadable_rows",
		Severity: finding.Error,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%d row(s) in %s could not be read and were skipped",
			lt.RejectCount, tc.table.Display),
		Detail: fmt.Sprintf(
			"These rows have a different number of fields than the header declares, so "+
				"they were not loaded: %s. They are absent from every count and total "+
				"computed from this file, and nothing downstream can detect a row that "+
				"was never read. The usual cause is an unquoted separator inside a value.",
			strings.Join(lines, ", ")),
		Remedy: "Re-export the file with proper quoting, or repair the offending lines. " +
			"Until then, treat every total from this file as understated.",
		Location: tc.location(nil),
		Count:    lt.RejectCount,
		Total:    total,
		Evidence: finding.Evidence{
			Expected: fmt.Sprintf("all %d rows readable", total),
			Observed: fmt.Sprintf("%d rejected", lt.RejectCount),
		},
	}}
}

// checkDuplicateRows reports rows that are identical across every column.
func checkDuplicateRows(ctx context.Context, e *engine.Engine, tc *tableContext) ([]finding.Finding, error) {
	if tc.table.RowCount < 2 || len(tc.table.Columns) == 0 {
		return nil, nil
	}

	names := make([]string, len(tc.table.Columns))
	for i, c := range tc.table.Columns {
		names[i] = engine.Ident(c.Name)
	}
	all := strings.Join(names, ", ")

	// Count the surplus rows rather than the number of duplicated groups: the
	// surplus is how many rows would disappear on de-duplication, which is the
	// figure that matters to anyone reconciling totals.
	q := fmt.Sprintf(
		"SELECT coalesce(sum(n - 1), 0) FROM (SELECT count(*) AS n FROM %s GROUP BY %s HAVING count(*) > 1)",
		tc.quoted, all)

	var surplus int64
	if err := e.ScanOne(ctx, q, []any{&surplus}); err != nil {
		return nil, err
	}
	if surplus == 0 {
		return nil, nil
	}

	return []finding.Finding{{
		Rule:     "table.duplicate_rows",
		Severity: finding.Error,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s has %d fully duplicated row(s)", tc.table.Display, surplus),
		Detail: "These rows are identical in every column, so nothing distinguishes them. " +
			"Either the same record was exported twice, or a genuine repeat has no " +
			"identifier to tell the two occurrences apart. Any sum or count over this " +
			"file double-counts them.",
		Remedy: "Establish whether the repeats are real events or an export artifact. " +
			"If they are real, the file needs a column that distinguishes them.",
		Location: tc.location(nil),
		Count:    surplus,
		Total:    tc.table.RowCount,
		Evidence: finding.Evidence{
			CountQuery: q,
			RowQuery: fmt.Sprintf(
				"SELECT %s, count(*) AS occurrences FROM %s GROUP BY %s HAVING count(*) > 1 LIMIT 100",
				all, tc.quoted, all),
			Expected: "every row distinct",
			Observed: fmt.Sprintf("%d surplus copies", surplus),
		},
	}}, nil
}

// checkNoCandidateKey reports a table with no column that identifies a row.
func checkNoCandidateKey(tc *tableContext) []finding.Finding {
	if tc.table.RowCount < 2 {
		return nil
	}
	for _, c := range tc.table.Columns {
		if c.Unprofiled != "" {
			// "No column identifies a row" is a claim about every column,
			// and one of them was not measured. The unmeasured column is
			// reported in its own right; saying this as well would be
			// reporting the same gap as a defect in the data.
			return nil
		}
	}
	for _, c := range tc.table.Columns {
		if c.Unique() {
			return nil
		}
	}
	return []finding.Finding{{
		Rule:     "table.no_candidate_key",
		Severity: finding.Info,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("no single column identifies a row in %s", tc.table.Display),
		Detail: "No column holds a distinct, complete value for every row, so there is no " +
			"way to refer to a particular record. Corrections, joins, and incremental " +
			"loads all need one; without it, the only way to match a row is to compare " +
			"every field.",
		Remedy:   "Confirm whether a key exists and was omitted from the export, or whether several columns form one.",
		Location: tc.location(nil),
		Total:    tc.table.RowCount,
		Evidence: finding.Evidence{
			Expected: "at least one unique, fully populated column",
			Observed: "none",
		},
	}}
}

// structuralSeverity maps an observation made while reading a file to how much
// it matters.
//
// The messages are written where the observation is made, because that is
// where the context is. This table only decides how loudly to say them and
// what to do about it.
var structuralSeverity = map[string]struct {
	severity finding.Severity
	remedy   string
}{
	"csv.header_duplicate": {finding.Warning,
		"Give the columns distinct names at source."},
	"csv.header_blank": {finding.Warning,
		"Name the column, or drop it from the export."},
	"csv.header_whitespace": {finding.Warning,
		"Trim the header names at source."},
	"csv.encoding_not_utf8": {finding.Warning,
		"Re-export the file as UTF-8."},
	"csv.delimiter_disagreement": {finding.Error,
		"Re-export with proper quoting so the delimiter is unambiguous."},
	"csv.inconsistent_width": {finding.Error,
		"Re-export with proper quoting; values containing the separator must be quoted."},
	"csv.width_disagreement": {finding.Error,
		"Repair the rows whose field count differs from the header."},
	"csv.dialect_undetectable": {finding.Error,
		"Re-export the file in a standard CSV dialect."},
	"csv.no_header": {finding.Info,
		"Add a header row so columns are identified by name rather than position."},
	"csv.bom":                 {finding.Info, ""},
	"csv.delimiter_ambiguous": {finding.Info, ""},

	"excel.hidden_sheet": {finding.Warning,
		"Confirm whether the hidden sheet should be part of this dataset."},
	"excel.hidden_rows": {finding.Error,
		"Unhide the rows, or remove them, so that what is on screen matches what is in the file."},
	"excel.merged_cells": {finding.Warning,
		"Unmerge the cells and repeat the value in every row it covers."},
	"excel.formula_errors": {finding.Error,
		"Fix the broken formulas before exporting; the error text is being read as data."},
	"excel.stacked_tables": {finding.Warning,
		"Put each table on its own sheet."},
	"excel.header_offset": {finding.Info,
		"Consider exporting without the title rows."},
	"excel.title_row":    {finding.Info, ""},
	"excel.ragged_rows":  {finding.Warning, "Check the rows that are wider or narrower than the header."},
	"excel.header_blank": {finding.Warning, "Name every column."},
	"ingest.no_rows":     {finding.Warning, "Confirm the extract completed."},
	"excel.sheet_unreadable": {finding.Error,
		"The worksheet could not be read; check the file is not corrupt."},
}

// checkStructure promotes the observations made while reading a file into
// findings.
func checkStructure(tc *tableContext) []finding.Finding {
	lt := tc.table.Ingest
	if lt == nil {
		return nil
	}

	var out []finding.Finding
	for _, n := range lt.Notes {
		// Rejected rows get their own, fuller finding.
		if n.Code == "ingest.rejected_rows" {
			continue
		}
		meta, known := structuralSeverity[n.Code]
		if !known {
			meta.severity = finding.Info
		}
		out = append(out, finding.Finding{
			Rule:     n.Code,
			Severity: meta.severity,
			Origin:   finding.OriginCheck,
			Title:    summarize(n.Message),
			Detail:   n.Message,
			Remedy:   meta.remedy,
			Location: tc.location(nil),
		})
	}
	return out
}

// summarize reduces a long explanation to a headline. The messages are written
// as "what happened; why it matters", so the first clause is the headline.
func summarize(msg string) string {
	for _, sep := range []string{"; ", ", which ", ". "} {
		if i := strings.Index(msg, sep); i > 0 {
			return msg[:i]
		}
	}
	const max = 100
	if len(msg) > max {
		return msg[:max] + "…"
	}
	return msg
}
