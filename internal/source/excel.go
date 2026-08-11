package source

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Excel deserves its own treatment rather than being converted to CSV and
// forgotten about. A spreadsheet carries structure a CSV cannot — hidden rows,
// merged cells, formulas, several tables on one sheet — and each of those is a
// way for a number to be wrong in a report while looking right on screen.
// Veritix records that structure here, then hands the plain values to the same
// ingest path a CSV takes.

// Workbook describes one Excel file.
type Workbook struct {
	Sheets []Sheet
	Notes  []Note
}

// Sheet describes one worksheet.
type Sheet struct {
	Name    string
	Index   int
	Visible bool

	// HeaderRow is the 1-based row the column names were found on. Anything
	// above it was a title block or a blank spacer.
	HeaderRow int
	// Columns are the header names as written.
	Columns []string
	// DataRows is the number of rows below the header.
	DataRows int
	// HiddenRows is how many data rows are hidden from a reader of the file.
	HiddenRows int
	// MergedRanges are merged cell ranges, which break the one-value-per-cell
	// assumption every downstream tool makes.
	MergedRanges []string
	// FormulaErrors maps an error code such as "#REF!" to how many cells hold it.
	FormulaErrors map[string]int
	// RaggedRows is how many rows have a different populated width than the header.
	RaggedRows int
	// BlankSeparators counts fully-blank rows inside the data, which usually
	// mean more than one table has been stacked onto a single sheet.
	BlankSeparators int

	Notes []Note
}

// excelErrorValues are the error strings Excel writes into a cell when a
// formula cannot be evaluated. They survive into exports and are read as
// ordinary text by anything downstream.
var excelErrorValues = []string{
	"#REF!", "#DIV/0!", "#VALUE!", "#N/A", "#NAME?", "#NULL!", "#NUM!", "#SPILL!", "#CALC!",
}

// InspectWorkbook opens an Excel file, describes every sheet, and writes each
// one out as a UTF-8 CSV in tmpDir for the ingest path to read.
//
// The returned paths are keyed by sheet name. The caller owns tmpDir and is
// responsible for removing it.
func InspectWorkbook(f File, tmpDir string) (*Workbook, map[string]string, error) {
	xl, err := excelize.OpenFile(f.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("source: opening %s: %w", f.Rel, err)
	}
	defer xl.Close() //nolint:errcheck // read-only

	wb := &Workbook{}
	paths := make(map[string]string)

	for i, name := range xl.GetSheetList() {
		sheet := Sheet{Name: name, Index: i, Visible: true}

		if vis, err := xl.GetSheetVisible(name); err == nil && !vis {
			sheet.Visible = false
			sheet.Notes = append(sheet.Notes, Note{
				Code: "excel.hidden_sheet",
				Message: fmt.Sprintf("worksheet %q is hidden; its contents are not visible "+
					"to someone opening the workbook", name),
			})
		}

		csvPath := filepath.Join(tmpDir, fmt.Sprintf("sheet_%03d.csv", i))
		if err := writeSheetCSV(xl, &sheet, csvPath); err != nil {
			wb.Notes = append(wb.Notes, Note{
				Code:    "excel.sheet_unreadable",
				Message: fmt.Sprintf("worksheet %q could not be read: %v", name, err),
			})
			continue
		}

		if sheet.DataRows == 0 {
			// An empty sheet is not an error, but it is not a table either.
			wb.Sheets = append(wb.Sheets, sheet)
			continue
		}

		if merged, err := xl.GetMergeCells(name); err == nil && len(merged) > 0 {
			for _, m := range merged {
				sheet.MergedRanges = append(sheet.MergedRanges, m.GetStartAxis()+":"+m.GetEndAxis())
			}
			sheet.Notes = append(sheet.Notes, Note{
				Code: "excel.merged_cells",
				Message: fmt.Sprintf("%d merged cell ranges; a merged cell holds one value but "+
					"covers several rows or columns, so every row but the first reads as empty",
					len(merged)),
			})
		}

		sheet.Notes = append(sheet.Notes, summarizeSheet(&sheet)...)
		wb.Sheets = append(wb.Sheets, sheet)
		paths[name] = csvPath
	}

	if len(wb.Sheets) == 0 {
		return wb, paths, fmt.Errorf("source: %s contains no worksheets", f.Rel)
	}
	return wb, paths, nil
}

// headerScanRows is how many rows are buffered to find the header. A worksheet
// can have any number of title, logo, and note rows above its real header, but
// not hundreds, and buffering must stay bounded for a million-row sheet.
const headerScanRows = 100

// bufferedRow is one row held during the header search.
type bufferedRow struct {
	cells  []string
	hidden bool
}

// writeSheetCSV streams a worksheet to a CSV file while recording the
// structural observations that only exist at this stage.
func writeSheetCSV(xl *excelize.File, sheet *Sheet, dest string) error {
	rows, err := xl.Rows(sheet.Name)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck // read-only

	out, err := os.Create(dest) //nolint:gosec // dest is a path Veritix chose
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // closed explicitly below

	w := csv.NewWriter(out)
	sheet.FormulaErrors = make(map[string]int)

	// The sheet's declared dimension is not trustworthy: a workbook written by
	// a tool other than Excel often declares "A1" regardless of its contents.
	// So buffer the opening rows, work out the real width and where the header
	// is, then stream the rest.
	buffer := make([]bufferedRow, 0, headerScanRows)
	rowNum := 0
	for rows.Next() && len(buffer) < headerScanRows {
		rowNum++
		cells, err := rows.Columns()
		if err != nil {
			return err
		}
		countFormulaErrors(cells, sheet.FormulaErrors)
		buffer = append(buffer, bufferedRow{cells: cells, hidden: rows.GetRowOpts().Hidden})
	}

	width, headerIdx := findHeader(buffer)
	if headerIdx < 0 {
		// Nothing in the buffered rows looks like a header, so the sheet holds
		// no table worth loading.
		w.Flush()
		return out.Close()
	}

	sheet.HeaderRow = headerIdx + 1
	sheet.Columns = pad(buffer[headerIdx].cells, width)
	if err := w.Write(sheet.Columns); err != nil {
		return err
	}
	for i := 0; i < headerIdx; i++ {
		if !isBlankRow(buffer[i].cells) {
			sheet.Notes = append(sheet.Notes, Note{
				Code: "excel.title_row",
				Message: fmt.Sprintf("row %d sits above the header and was skipped; it looks like "+
					"a title rather than data", i+1),
			})
		}
	}

	// pendingBlanks holds blank rows until data appears beneath them, so that
	// trailing blank rows at the end of a sheet are not mistaken for the gap
	// between two stacked tables.
	pendingBlanks := 0
	emit := func(br bufferedRow) error {
		if isBlankRow(br.cells) {
			pendingBlanks++
			return nil
		}
		sheet.BlankSeparators += pendingBlanks
		pendingBlanks = 0

		if len(br.cells) != len(sheet.Columns) && len(br.cells) != 0 {
			sheet.RaggedRows++
		}
		if br.hidden {
			sheet.HiddenRows++
		}
		if err := w.Write(pad(br.cells, width)); err != nil {
			return err
		}
		sheet.DataRows++
		return nil
	}

	for _, br := range buffer[headerIdx+1:] {
		if err := emit(br); err != nil {
			return err
		}
	}
	for rows.Next() {
		cells, err := rows.Columns()
		if err != nil {
			return err
		}
		countFormulaErrors(cells, sheet.FormulaErrors)
		if err := emit(bufferedRow{cells: cells, hidden: rows.GetRowOpts().Hidden}); err != nil {
			return err
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	if err := rows.Error(); err != nil {
		return err
	}
	return out.Close()
}

// findHeader locates the header among the buffered opening rows and reports
// the sheet's real width.
//
// A worksheet frequently opens with a title, a generated-on date, or a logo
// row: one or two populated cells sitting above the actual table. Those rows
// are narrower than the table beneath them, which is what distinguishes them.
// Reading a title row as the header is not a cosmetic error — it renames every
// column and shifts the whole table by a row.
func findHeader(buffer []bufferedRow) (width, headerIdx int) {
	for _, br := range buffer {
		if len(br.cells) > width {
			width = len(br.cells)
		}
	}
	if width == 0 {
		return 0, -1
	}

	// A row must be at least this wide to be the header rather than a title.
	// Narrow sheets (one or two columns) have no room for the heuristic, so
	// the first non-blank row is taken as written.
	threshold := 1
	if width >= 3 {
		threshold = (width * 3) / 5
	}

	for i, br := range buffer {
		if isBlankRow(br.cells) {
			continue
		}
		if len(br.cells) >= threshold {
			return width, i
		}
	}
	return width, -1
}

func pad(cells []string, width int) []string {
	if width <= len(cells) {
		return cells
	}
	out := make([]string, width)
	copy(out, cells)
	return out
}

func isBlankRow(cells []string) bool {
	for _, c := range cells {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

func countFormulaErrors(cells []string, into map[string]int) {
	for _, c := range cells {
		v := strings.TrimSpace(c)
		if v == "" || v[0] != '#' {
			continue
		}
		for _, e := range excelErrorValues {
			if v == e {
				into[e]++
				break
			}
		}
	}
}

// summarizeSheet turns the counters gathered during the walk into notes.
func summarizeSheet(s *Sheet) []Note {
	var notes []Note

	if s.HeaderRow > 1 {
		notes = append(notes, Note{
			Code: "excel.header_offset",
			Message: fmt.Sprintf("the header is on row %d rather than row 1; a tool that assumes "+
				"the first row is the header would read titles as column names", s.HeaderRow),
		})
	}
	if s.HiddenRows > 0 {
		notes = append(notes, Note{
			Code: "excel.hidden_rows",
			Message: fmt.Sprintf("%d of %d data rows are hidden; they are still present in the "+
				"file and still counted by anything that reads it", s.HiddenRows, s.DataRows),
		})
	}
	if s.RaggedRows > 0 {
		notes = append(notes, Note{
			Code:    "excel.ragged_rows",
			Message: fmt.Sprintf("%d rows have a different width than the header row", s.RaggedRows),
		})
	}
	if s.BlankSeparators > 0 {
		notes = append(notes, Note{
			Code: "excel.stacked_tables",
			Message: fmt.Sprintf("%d blank rows sit inside the data, which usually means more than "+
				"one table has been placed on a single sheet", s.BlankSeparators),
		})
	}
	if total := totalOf(s.FormulaErrors); total > 0 {
		notes = append(notes, Note{
			Code: "excel.formula_errors",
			Message: fmt.Sprintf("%d cells hold Excel error values (%s); these are read as ordinary "+
				"text by anything importing the file", total, describeCounts(s.FormulaErrors)),
		})
	}
	for i, name := range s.Columns {
		if strings.TrimSpace(name) == "" {
			notes = append(notes, Note{
				Code:    "excel.header_blank",
				Message: fmt.Sprintf("column %d has no name", i+1),
			})
		}
	}
	return notes
}

func totalOf(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func describeCounts(m map[string]int) string {
	// Iterate the canonical list so the message is stable between runs.
	var parts []string
	for _, k := range excelErrorValues {
		if n := m[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", k, n))
		}
	}
	return strings.Join(parts, ", ")
}
