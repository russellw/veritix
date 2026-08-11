package tools

import (
	"context"
	"encoding/json"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/agent/redact"
	"github.com/russellw/veritix/internal/profile"
)

// The tools in this file answer from the profile the deterministic pass already
// built, so they cost no query. That is deliberate: the agent's first several
// turns are orientation, and orientation should be cheap.

func listTables() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "list_tables",
			Description: "List every table in the dataset with its row and column counts. " +
				"Start here: it is the map of what there is to audit.",
		},
		invoke: func(_ context.Context, w *World, _ json.RawMessage) (any, error) {
			type tableInfo struct {
				Name           string `json:"name"`
				Source         string `json:"source"`
				Rows           int64  `json:"rows"`
				Columns        int    `json:"columns"`
				UnreadableRows int64  `json:"unreadable_rows,omitempty"`
			}
			out := struct {
				Tables []tableInfo `json:"tables"`
			}{}

			for _, t := range w.Profile.Tables {
				info := tableInfo{
					Name:    t.Name,
					Source:  t.Display,
					Rows:    t.RowCount,
					Columns: len(t.Columns),
				}
				if t.Ingest != nil {
					info.UnreadableRows = t.Ingest.RejectCount
				}
				out.Tables = append(out.Tables, info)
			}
			return out, nil
		},
	}
}

// columnInfo is the summary of a column in describe_table: enough to decide
// whether the column is worth a closer look, and nothing out of any row.
type columnInfo struct {
	Name string `json:"name"`
	// Original is the heading in the source file, when it differs from the SQL
	// name.
	Original     string `json:"original_name,omitempty"`
	DeclaredType string `json:"declared_type,omitempty"`
	// InferredKind is what the values actually are, which is the interesting
	// number when it disagrees with declared_type.
	InferredKind  string  `json:"inferred_kind"`
	Conformance   float64 `json:"conformance"`
	Nonconforming int64   `json:"nonconforming,omitempty"`
	// BestCandidate names the type a text column nearly is, which is what
	// separates a column that is 82% dates from one that is 0% anything.
	BestCandidate   string  `json:"near_type,omitempty"`
	BestConformance float64 `json:"near_type_conformance,omitempty"`

	Rows     int64 `json:"rows"`
	Nulls    int64 `json:"nulls,omitempty"`
	Blanks   int64 `json:"blanks,omitempty"`
	Missing  int64 `json:"missing_total,omitempty"`
	Distinct int64 `json:"distinct"`
	// DistinctNormalized is the distinct count after trimming and lower-casing.
	// Below Distinct means the column holds variants of the same value.
	DistinctNormalized int64 `json:"distinct_normalized,omitempty"`
	Unique             bool  `json:"unique,omitempty"`

	LeadingWhitespace  int64 `json:"leading_whitespace,omitempty"`
	TrailingWhitespace int64 `json:"trailing_whitespace,omitempty"`

	// Shapes describe the column's formats without disclosing a value.
	Shapes []countedText `json:"shapes,omitempty"`
	// Sentinels are recognized placeholders — "N/A", "-", "unknown". They are
	// sent verbatim because they come from Veritix's own vocabulary of ways to
	// write "missing", not from the customer's data.
	Sentinels []countedText `json:"sentinels,omitempty"`

	Numeric  *numericInfo  `json:"numeric,omitempty"`
	Temporal *temporalInfo `json:"temporal,omitempty"`
}

type numericInfo struct {
	Count    int64   `json:"count"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Mean     float64 `json:"mean"`
	StdDev   float64 `json:"stddev"`
	P25      float64 `json:"p25"`
	Median   float64 `json:"median"`
	P75      float64 `json:"p75"`
	Negative int64   `json:"negative,omitempty"`
	Zero     int64   `json:"zero,omitempty"`
	Outliers int64   `json:"outliers_beyond_6_sigma,omitempty"`
}

type temporalInfo struct {
	Count int64 `json:"count"`
	// Min and Max are dates the column contains. They are values, so they are
	// shaped like any other; the range is still legible as a shape when the
	// question is whether a column holds dates at all.
	Min redact.Text `json:"earliest"`
	Max redact.Text `json:"latest"`
	// Formats maps a strptime pattern to how many values it parses. More than
	// one substantial entry means the column mixes formats, which is the
	// defect that silently misreads days as months.
	Formats []formatInfo `json:"formats,omitempty"`
	// Ambiguous values read as valid but different dates under day-first and
	// month-first conventions.
	Ambiguous   int64 `json:"ambiguous,omitempty"`
	Future      int64 `json:"future,omitempty"`
	Implausible int64 `json:"implausible,omitempty"`
}

type formatInfo struct {
	Format  string      `json:"format"`
	Example redact.Text `json:"example,omitempty"`
	Count   int64       `json:"count"`
}

func summarizeColumn(g *redact.Guard, c *profile.Column, full bool) columnInfo {
	info := columnInfo{
		Name:               c.Name,
		DeclaredType:       c.DeclaredType,
		InferredKind:       string(c.Inferred.Kind),
		Conformance:        round2(c.Inferred.Conformance),
		Nonconforming:      c.Inferred.Nonconforming,
		Rows:               c.Total,
		Nulls:              c.Nulls,
		Blanks:             c.Blanks,
		Missing:            c.Missing(),
		Distinct:           c.Distinct,
		DistinctNormalized: c.DistinctNormalized,
		Unique:             c.Unique(),
		LeadingWhitespace:  c.LeadingWhitespace,
		TrailingWhitespace: c.TrailingWhitespace,
	}
	if c.Inferred.Kind == profile.KindText && c.Inferred.BestCandidate != "" {
		info.BestCandidate = string(c.Inferred.BestCandidate)
		info.BestConformance = round2(c.Inferred.BestConformance)
	}
	if c.Original != c.Name {
		// The heading as it appears in the file. It matters because a finding
		// about "order_ref" has to be recognizable to somebody looking at a
		// spreadsheet column headed "Order Ref.".
		info.Original = c.Original
	}

	shapes := c.Shapes
	if !full && len(shapes) > 4 {
		shapes = shapes[:4]
	}
	for _, s := range shapes {
		info.Shapes = append(info.Shapes, countedText{
			// Already a shape: the profiler derived it in SQL, and shaping it
			// again would be identity.
			Value: g.Derived(s.Value),
			Count: s.Count,
			Share: round2(s.Share),
		})
	}
	for _, s := range c.Sentinels {
		info.Sentinels = append(info.Sentinels, countedText{
			Value: g.Derived(s.Value), Count: s.Count, Share: round2(s.Share),
		})
	}

	if n := c.Numeric; n != nil {
		info.Numeric = &numericInfo{
			Count: n.Count, Min: n.Min, Max: n.Max, Mean: n.Mean, StdDev: n.StdDev,
			P25: n.P25, Median: n.Median, P75: n.P75,
			Negative: n.Negative, Zero: n.Zero, Outliers: n.Outliers,
		}
	}
	if tm := c.Temporal; tm != nil {
		info.Temporal = &temporalInfo{
			Count:       tm.Count,
			Min:         g.Value(tm.Min),
			Max:         g.Value(tm.Max),
			Ambiguous:   tm.Ambiguous,
			Future:      tm.Future,
			Implausible: tm.Implausible,
		}
		for _, f := range tm.Formats {
			info.Temporal.Formats = append(info.Temporal.Formats, formatInfo{
				Format:  f.Format,
				Example: g.Value(f.Example),
				Count:   f.Count,
			})
		}
	}

	return info
}

func describeTable() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "describe_table",
			Description: "Describe every column of a table: the type it declares, the type its " +
				"values actually are, how many are missing, how many distinct, and the shapes " +
				"its values take. Shapes render digits as 9 and letters as X, so 'CUS-004417' " +
				"appears as 'XXX-999999'. This is the fastest way to find a column worth " +
				"investigating.",
			Properties: map[string]any{
				"table": str("the table name, as given by list_tables"),
			},
			Required: []string{"table"},
		},
		invoke: func(_ context.Context, w *World, args json.RawMessage) (any, error) {
			var in struct {
				Table string `json:"table"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			t, err := w.table(in.Table)
			if err != nil {
				return nil, err
			}

			out := struct {
				Table   string       `json:"table"`
				Source  string       `json:"source"`
				Rows    int64        `json:"rows"`
				Columns []columnInfo `json:"columns"`
			}{Table: t.Name, Source: t.Display, Rows: t.RowCount}

			for _, c := range t.Columns {
				out.Columns = append(out.Columns, summarizeColumn(w.Guard, c, false))
			}
			return out, nil
		},
	}
}

func profileColumn() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "profile_column",
			Description: "Everything measured about one column: the full shape distribution, " +
				"numeric distribution and outlier count, date formats and ambiguous dates, " +
				"whitespace and case irregularities. Use it once describe_table has pointed " +
				"you at a column.",
			Properties: map[string]any{
				"table":  str("the table name"),
				"column": str("the column name"),
			},
			Required: []string{"table", "column"},
		},
		invoke: func(_ context.Context, w *World, args json.RawMessage) (any, error) {
			var in struct {
				Table  string `json:"table"`
				Column string `json:"column"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			t, err := w.table(in.Table)
			if err != nil {
				return nil, err
			}
			c, err := w.column(t, in.Column)
			if err != nil {
				return nil, err
			}

			return struct {
				Table  string     `json:"table"`
				Column columnInfo `json:"column"`
			}{Table: t.Name, Column: summarizeColumn(w.Guard, c, true)}, nil
		},
	}
}
