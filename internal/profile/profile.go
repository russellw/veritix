// Package profile measures what a dataset actually contains.
//
// Profiling is the foundation everything else rests on. The deterministic
// checks read these numbers, the report shows them, and the agent reasons
// about them without ever seeing the underlying values. That last point shapes
// the design: a profile is built to be complete enough to diagnose a dataset
// while consisting almost entirely of counts, ratios, and derived shapes.
//
// Fields that do hold raw values are marked, so that the egress guard can
// strip them before anything reaches a model.
package profile

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/ingest"
)

// Kind is the type a column's values actually are, as distinct from the type
// its source declares or an importer would guess.
type Kind string

const (
	KindEmpty     Kind = "empty"
	KindInteger   Kind = "integer"
	KindDecimal   Kind = "decimal"
	KindBoolean   Kind = "boolean"
	KindDate      Kind = "date"
	KindTimestamp Kind = "timestamp"
	KindText      Kind = "text"
)

// conformanceThreshold is the share of values that must match a type for the
// column to be called that type. Below it the column is text.
//
// It is deliberately below 1.0. A column that is 99% dates and 1% "TBC" is a
// date column with a defect, and describing it as text would hide the defect
// rather than report it.
const conformanceThreshold = 0.95

// Dataset is the profile of every table in a dataset.
type Dataset struct {
	Tables []*Table
}

// Table is the profile of one table.
type Table struct {
	// Name is the SQL identifier.
	Name string
	// Display is the human-readable origin, e.g. "sales.xlsx#Q1".
	Display string
	// RowCount is the number of rows that loaded.
	RowCount int64
	// Columns are profiled in file order.
	Columns []*Column
	// Ingest carries how the table was read and what went wrong reading it.
	Ingest *ingest.Table
}

// Column is the profile of one column.
type Column struct {
	Name     string
	Original string
	Ordinal  int

	// DeclaredType is the type a conventional import would have inferred.
	// Comparing it against Inferred is how Veritix reports that an import
	// would have silently discarded values.
	DeclaredType string
	// Inferred is what the values actually are.
	Inferred Inference

	// Counts. Total is the table's row count; the rest partition it.
	Total    int64
	Nulls    int64
	Blanks   int64
	Distinct int64

	// Sentinels are values that stand in for "missing" without being null.
	// A column with 400 nulls and 300 "N/A" strings has 700 missing values,
	// and only reporting the first number understates the problem.
	Sentinels []ValueCount

	// Length statistics over non-null values.
	MinLength int64
	MaxLength int64
	AvgLength float64

	// Whitespace and case irregularities. These matter because they make
	// equal values compare unequal: " ACME" and "Acme" are one customer.
	LeadingWhitespace  int64
	TrailingWhitespace int64
	// DistinctNormalised is the distinct count after trimming and
	// lower-casing. Where it is below Distinct, the column holds variants of
	// the same value.
	DistinctNormalised int64

	// Shapes are value patterns with digits rendered as 9 and letters as X,
	// most frequent first. They describe the column's format without
	// disclosing any value, which is what lets an agent reason about a column
	// it is not allowed to read.
	Shapes []ValueCount

	// Numeric, Temporal, and Boolean are populated when the column's values
	// support them, regardless of the inferred kind: a mostly-text column
	// with a numeric minority is worth measuring too.
	Numeric  *NumericStats
	Temporal *TemporalStats

	// MinValue and MaxValue are the lexicographic extremes.
	//
	// RAW DATA: these are verbatim cell values and must not leave the process
	// under the default egress policy.
	MinValue string
	MaxValue string
	// TopValues are the most frequent values.
	//
	// RAW DATA: same restriction as above.
	TopValues []ValueCount
}

// Inference is the conclusion about a column's type.
//
// It is measured over the column's *data* values: non-null, non-blank, and not
// a recognised textual placeholder. Sentinels are accounted for as missing
// values instead, so a numeric column peppered with "N/A" is still reported as
// numeric rather than being demoted to text.
type Inference struct {
	Kind Kind
	// Conformance is the share of data values that match Kind.
	Conformance float64
	// Nonconforming is how many data values do not match. These are the
	// genuine type violations: values that are neither valid nor a known way
	// of writing "missing".
	Nonconforming int64
	// Candidates records the match count for every type that was tested,
	// which is what makes a near-miss visible.
	Candidates map[Kind]int64

	// BestCandidate and BestConformance describe the closest type match even
	// when the column was ultimately called text. A column that is 82% dates
	// is far more interesting than one that is 0% anything, and only these
	// fields can tell the two apart.
	BestCandidate   Kind
	BestConformance float64
}

// ValueCount is a value and how often it occurs.
type ValueCount struct {
	Value string
	Count int64
	// Share is Count as a fraction of the column's non-null values.
	Share float64
}

// NumericStats describes the numeric values in a column.
type NumericStats struct {
	Count    int64
	Min      float64
	Max      float64
	Mean     float64
	StdDev   float64
	P25      float64
	Median   float64
	P75      float64
	Negative int64
	Zero     int64
	// Outliers is how many values lie more than OutlierSigma standard
	// deviations from the mean.
	Outliers int64
}

// OutlierSigma is the distance from the mean at which a value is called an
// outlier. Six is deliberately conservative: the aim is to surface data-entry
// errors and unit mix-ups, not to flag a fat-tailed but legitimate
// distribution.
const OutlierSigma = 6.0

// TemporalStats describes the dates in a column.
type TemporalStats struct {
	Count int64
	// Min and Max are the earliest and latest parsed values, as text.
	Min string
	Max string
	// Formats maps a strptime pattern to how many values it parses. More than
	// one entry with a substantial count means the column mixes formats.
	Formats []FormatCount
	// Ambiguous is how many values parse under both day-first and month-first
	// readings while meaning different dates. These are the dangerous ones:
	// 03/04/2024 is silently either 3 April or 4 March.
	Ambiguous int64
	// Future is how many values are dated after the audit ran.
	Future int64
	// Implausible is how many values fall outside a sane business range.
	Implausible int64
}

// FormatCount is a date format and how many values match it.
type FormatCount struct {
	Format  string
	Example string
	Count   int64
}

// Options controls a profiling run.
type Options struct {
	// TopValues is how many of the most frequent values to keep per column.
	TopValues int
	// Shapes is how many distinct value shapes to keep per column.
	Shapes int
	// Parallelism caps concurrent column queries. Zero picks a default.
	Parallelism int
}

func (o Options) withDefaults() Options {
	if o.TopValues <= 0 {
		o.TopValues = 10
	}
	if o.Shapes <= 0 {
		o.Shapes = 8
	}
	if o.Parallelism <= 0 {
		o.Parallelism = 8
	}
	return o
}

// Run profiles every loaded table.
func Run(ctx context.Context, e *engine.Engine, loaded *ingest.Result, opts Options, log *slog.Logger) (*Dataset, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	opts = opts.withDefaults()

	ds := &Dataset{Tables: make([]*Table, 0, len(loaded.Tables))}

	for _, lt := range loaded.Tables {
		t := &Table{
			Name:     lt.Ref.Name,
			Display:  lt.Ref.Display,
			RowCount: lt.RowCount,
			Ingest:   lt,
			Columns:  make([]*Column, len(lt.Columns)),
		}

		// Columns are independent, and each one is a separate scan DuckDB can
		// run concurrently, so this is where the parallelism belongs.
		var (
			group errgroup.Group
			mu    sync.Mutex
			errs  int
		)
		group.SetLimit(opts.Parallelism)

		for i, lc := range lt.Columns {
			group.Go(func() error {
				col, err := profileColumn(ctx, e, t, lc, opts)
				if err != nil {
					// One unprofilable column should not lose the other
					// forty-nine; record a stub and carry on.
					log.Warn("could not profile column",
						"table", t.Display, "column", lc.Name, "error", err)
					mu.Lock()
					errs++
					mu.Unlock()
					col = &Column{
						Name:         lc.Name,
						Original:     lc.Original,
						Ordinal:      lc.Ordinal,
						DeclaredType: lc.SniffedType,
						Total:        t.RowCount,
					}
				}
				t.Columns[i] = col
				return nil
			})
		}
		if err := group.Wait(); err != nil {
			return nil, err
		}

		log.Debug("profiled table",
			"table", t.Display, "columns", len(t.Columns), "failed", errs)
		ds.Tables = append(ds.Tables, t)
	}

	sort.Slice(ds.Tables, func(i, j int) bool { return ds.Tables[i].Name < ds.Tables[j].Name })
	return ds, nil
}

// Column looks up a profiled column by name.
func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Missing is the total number of values that carry no information: nulls,
// blanks, and sentinel placeholders together.
func (c *Column) Missing() int64 {
	n := c.Nulls + c.Blanks
	for _, s := range c.Sentinels {
		n += s.Count
	}
	return n
}

// Populated is the number of values that do carry information.
func (c *Column) Populated() int64 {
	return c.Total - c.Missing()
}

// Unique reports whether every populated value in the column is distinct,
// which makes it a candidate key.
func (c *Column) Unique() bool {
	return c.Total > 0 && c.Distinct == c.Total-c.Nulls && c.Nulls == 0
}
