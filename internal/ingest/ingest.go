// Package ingest loads a discovered dataset into the DuckDB engine.
//
// Every column is loaded as text, deliberately. A normal import guesses a type
// per column and then quietly discards whatever does not fit: a stray "N/A" in
// a numeric column becomes NULL, a European decimal comma becomes NULL, a date
// in the wrong format becomes NULL. Those discarded values are exactly what an
// auditor needs to see. Veritix keeps the original strings and works out the
// types afterwards, so it can report the difference between what a column
// claims to be and what it actually contains.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/source"
)

// Reject table names shared by every scan in a run. DuckDB tags each scan with
// its own id, so one pair of tables serves the whole dataset.
const (
	rejectErrorsTable = "veritix_reject_errors"
	rejectScansTable  = "veritix_reject_scans"
)

// Options controls a load.
type Options struct {
	// TempDir is where Excel worksheets are materialized. If empty, a
	// directory is created and removed automatically.
	TempDir string
}

// Result is everything the load produced.
type Result struct {
	Dataset *source.Dataset
	Tables  []*Table
}

// Table is one loaded table and how it was read.
type Table struct {
	Ref     source.TableRef
	Columns []Column
	// RowCount is the number of rows successfully loaded.
	RowCount int64
	// Dialect describes how a delimited file was parsed. Nil for Excel.
	Dialect *source.CSVDialect
	// Sheet describes the worksheet a table came from. Nil for CSV.
	Sheet *source.Sheet
	// Rejects are rows the parser could not read as the declared shape.
	Rejects []Reject
	// RejectCount is the total number of rejected rows, which can exceed
	// len(Rejects) because only a sample is retained.
	RejectCount int64
	// Notes are observations about how this table was read.
	Notes []source.Note

	// readPath is the file DuckDB actually scanned. For CSV it is the source
	// file; for Excel it is the materialized worksheet, which is what DuckDB
	// records against a rejected row.
	readPath string
}

// Column is one loaded column.
type Column struct {
	// Name is the identifier the column has in DuckDB. It can differ from
	// Original when a file repeats a header name.
	Name string
	// Original is the name exactly as written in the file.
	Original string
	// Ordinal is the 1-based position in the file.
	Ordinal int
	// SniffedType is the type a conventional import would have guessed. It is
	// compared against what the values actually are during profiling.
	SniffedType string
	// Renamed reports that Name and Original differ.
	Renamed bool
}

// Reject is a row the CSV parser could not read.
type Reject struct {
	Line      int64
	Column    string
	ErrorType string
	Message   string
	// RawLine is the offending line verbatim. It is raw customer data and
	// must never be sent to a model; the egress guard treats it accordingly.
	RawLine string
}

// maxRejectSamples bounds how many rejected rows are retained per table. The
// count is always exact; the samples are for a human reading the report.
const maxRejectSamples = 20

// Load reads every file in the dataset into the engine.
func Load(ctx context.Context, e *engine.Engine, ds *source.Dataset, opts Options, log *slog.Logger) (*Result, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	tmpDir := opts.TempDir
	if tmpDir == "" {
		dir, err := os.MkdirTemp("", "veritix-ingest-")
		if err != nil {
			return nil, fmt.Errorf("ingest: creating temp dir: %w", err)
		}
		defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup
		tmpDir = dir
	}

	plan, err := planTables(ctx, ds, tmpDir, log)
	if err != nil {
		return nil, err
	}
	assignRefs(plan)

	if err := prepareRejectTables(ctx, e); err != nil {
		return nil, err
	}

	// Sniffing runs through DuckDB and the loads are DDL, so both are kept
	// sequential; the parallel work happened in planTables, where Excel
	// workbooks were decoded. DuckDB parallelises each individual scan across
	// cores anyway, so a serial outer loop is not the bottleneck.
	res := &Result{Dataset: ds}
	for i := range plan {
		p := &plan[i]
		if err := loadOne(ctx, e, p, log); err != nil {
			return nil, err
		}
		res.Tables = append(res.Tables, p.table)
	}

	if err := attachRejects(ctx, e, res.Tables); err != nil {
		return nil, err
	}

	sort.Slice(res.Tables, func(i, j int) bool {
		return res.Tables[i].Ref.Name < res.Tables[j].Ref.Name
	})
	return res, nil
}

// planned is one table about to be loaded, together with the path the data
// will actually be read from. For Excel that is a materialized CSV rather than
// the workbook itself.
type planned struct {
	table    *Table
	readPath string
	display  string
	file     source.File
	sheet    string
}

// assignRefs gives every planned table its final, unique SQL name.
func assignRefs(plan []planned) {
	refs := make([]source.TableRef, len(plan))
	for i := range plan {
		refs[i] = source.TableRef{
			Display: plan[i].display,
			File:    plan[i].file,
			Sheet:   plan[i].sheet,
		}
	}
	source.AssignNames(refs)
	for i := range plan {
		plan[i].table.Ref = refs[i]
	}
}

// planTables works out which tables exist and, for Excel, decodes the
// workbooks. Workbook decoding is pure Go and CPU-bound, so it runs in
// parallel across files.
func planTables(ctx context.Context, ds *source.Dataset, tmpDir string, log *slog.Logger) ([]planned, error) {
	var (
		mu    sync.Mutex
		plan  []planned
		group errgroup.Group
	)
	group.SetLimit(maxParallelWorkbooks())

	for _, f := range ds.Files {
		switch f.Kind {
		case source.KindCSV:
			plan = append(plan, planned{
				table:    &Table{},
				readPath: f.Path,
				display:  f.Rel,
				file:     f,
			})

		case source.KindExcel:
			group.Go(func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				dir := filepath.Join(tmpDir, engine.SafeName(f.Rel))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return fmt.Errorf("ingest: %w", err)
				}
				wb, paths, err := source.InspectWorkbook(f, dir)
				if err != nil {
					// A workbook Veritix cannot open is reported, not fatal:
					// the rest of the dataset is still worth auditing.
					log.Warn("skipping unreadable workbook", "file", f.Rel, "error", err)
					mu.Lock()
					ds.Skipped = append(ds.Skipped, source.SkippedFile{
						Path: f.Path, Rel: f.Rel, Reason: err.Error(),
					})
					mu.Unlock()
					return nil
				}

				mu.Lock()
				defer mu.Unlock()
				for i := range wb.Sheets {
					sh := wb.Sheets[i]
					path, ok := paths[sh.Name]
					if !ok || sh.DataRows == 0 {
						continue
					}
					plan = append(plan, planned{
						table: &Table{
							Sheet: &wb.Sheets[i],
							Notes: append(append([]source.Note{}, wb.Notes...), sh.Notes...),
						},
						readPath: path,
						display:  f.Rel + "#" + sh.Name,
						file:     f,
						sheet:    sh.Name,
					})
				}
				return nil
			})
		}
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	sort.Slice(plan, func(i, j int) bool { return plan[i].display < plan[j].display })
	return plan, nil
}

func maxParallelWorkbooks() int {
	// Excel decoding is memory-hungry: excelize holds a workbook's shared
	// string table in memory. A modest cap keeps a directory of large
	// workbooks from exhausting the host.
	return 4
}
