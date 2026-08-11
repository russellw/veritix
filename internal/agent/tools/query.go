package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/russellwallace/veritix/internal/agent/llm"
	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/profile"
)

func runSQL() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "run_sql",
			Description: "Run one read-only SELECT against the dataset and get its result. " +
				"Aggregates — count, sum, avg, min over numbers, and expressions built from " +
				"them — come back as numbers. Anything else is a cell value and comes back as " +
				"its shape, with digits as 9 and letters as X, so ask for counts rather than " +
				"rows: 'SELECT count(*) FROM orders WHERE status IS NULL' tells you what you " +
				"need; 'SELECT * FROM orders LIMIT 10' will not. Only one SELECT statement is " +
				"accepted.",
			Properties: map[string]any{
				"query":  str("a single SELECT statement"),
				"reason": str("one line on what you are trying to establish; it is recorded in the audit trail"),
			},
			Required: []string{"query"},
		},
		invoke: func(ctx context.Context, w *World, args json.RawMessage) (any, error) {
			var in struct {
				Query  string `json:"query"`
				Reason string `json:"reason"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			if in.Query == "" {
				return nil, fmt.Errorf("no query was given")
			}

			w.log().Info("agent query", "reason", in.Reason)

			rs, analysis, err := w.query(ctx, in.Query)
			if err != nil {
				return nil, err
			}

			type resultSet struct {
				Columns   []string `json:"columns"`
				Aggregate []bool   `json:"is_aggregate"`
				Rows      [][]any  `json:"rows"`
				RowCount  int      `json:"row_count"`
				Truncated bool     `json:"truncated,omitempty"`
				Note      string   `json:"note,omitempty"`
			}

			out := resultSet{
				Columns:   rs.Columns,
				Aggregate: analysis.Aggregate,
				RowCount:  len(rs.Rows),
				Truncated: rs.Truncated,
			}
			for _, row := range rs.Rows {
				cells := make([]any, len(row))
				for i, cell := range row {
					cells[i] = w.Guard.Cell(cell, i < len(analysis.Aggregate) && analysis.Aggregate[i])
				}
				out.Rows = append(out.Rows, cells)
			}
			if hasShapedColumn(analysis.Aggregate) {
				out.Note = "columns marked false in is_aggregate are cell values and are shown as shapes"
			}
			return out, nil
		},
	}
}

func hasShapedColumn(aggregate []bool) bool {
	for _, a := range aggregate {
		if !a {
			return true
		}
	}
	return len(aggregate) == 0
}

// query parses, runs, and caps one model-authored statement.
//
// The parse is what makes the statement safe to run at all, and it happens
// before execution rather than after: AnalyseSelect refuses anything that is
// not a single SELECT, and the engine's Lockdown has already taken away its
// access to the filesystem.
func (w *World) query(ctx context.Context, q string) (*engine.ResultSet, *engine.Analysis, error) {
	analysis, err := w.Engine.AnalyseSelect(ctx, q)
	if err != nil {
		return nil, nil, fmt.Errorf("that statement was refused: %v", w.Guard.EngineError(err))
	}
	rs, err := w.Engine.Collect(ctx, q, w.MaxRows)
	if err != nil {
		return nil, nil, fmt.Errorf("that query failed: %v", w.Guard.EngineError(err))
	}
	return rs, analysis, nil
}

// scanCount runs a statement Veritix built itself and reads one number from it.
func (w *World) scanCount(ctx context.Context, q string) (int64, error) {
	var n int64
	if err := w.Engine.ScanOne(ctx, q, []any{&n}); err != nil {
		return 0, fmt.Errorf("%v", w.Guard.EngineError(err))
	}
	return n, nil
}

func checkCandidateKey() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "check_candidate_key",
			Description: "Test whether one or more columns uniquely identify a row. Returns the " +
				"row count, the number of distinct combinations, how many rows are involved in " +
				"a duplicate, and how many have a missing part of the key. Use it before " +
				"trusting a column as an identifier, and before checking a relationship that " +
				"depends on one.",
			Properties: map[string]any{
				"table":   str("the table name"),
				"columns": stringList("the columns that together should be unique"),
			},
			Required: []string{"table", "columns"},
		},
		invoke: func(ctx context.Context, w *World, args json.RawMessage) (any, error) {
			var in struct {
				Table   string   `json:"table"`
				Columns []string `json:"columns"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}
			if len(in.Columns) == 0 {
				return nil, fmt.Errorf("name at least one column")
			}
			t, err := w.table(in.Table)
			if err != nil {
				return nil, err
			}
			cols := make([]*profile.Column, 0, len(in.Columns))
			for _, name := range in.Columns {
				c, err := w.column(t, name)
				if err != nil {
					return nil, err
				}
				cols = append(cols, c)
			}

			quoted := quoteColumns(cols)
			table := engine.Ident(t.Name)

			var missing string
			for i, c := range cols {
				if i > 0 {
					missing += " OR "
				}
				missing += fmt.Sprintf("NOT %s", profile.SQLNonBlank(engine.Ident(c.Name)))
			}

			// One pass produces all four numbers, so the answer cannot be
			// internally inconsistent the way four separate queries could be.
			q := fmt.Sprintf(`
				SELECT count(*),
				       count(DISTINCT (%[1]s)),
				       count(*) FILTER (WHERE %[3]s)
				FROM %[2]s`, quoted, table, missing)

			rs, err := w.Engine.Collect(ctx, q, 1)
			if err != nil {
				return nil, fmt.Errorf("%v", w.Guard.EngineError(err))
			}
			if len(rs.Rows) != 1 {
				return nil, fmt.Errorf("the uniqueness check returned nothing")
			}
			rows, distinct, incomplete := asInt(rs.Rows[0][0]), asInt(rs.Rows[0][1]), asInt(rs.Rows[0][2])

			// The count query is handed back so that a finding can be recorded
			// against the same measurement rather than a re-derived one.
			duplicated := fmt.Sprintf(
				`SELECT coalesce(sum(n), 0) FROM (SELECT count(*) AS n FROM %s GROUP BY %s HAVING count(*) > 1)`,
				table, quoted)
			duplicateRows, err := w.scanCount(ctx, duplicated)
			if err != nil {
				return nil, err
			}

			return struct {
				Table         string   `json:"table"`
				Columns       []string `json:"columns"`
				Rows          int64    `json:"rows"`
				Distinct      int64    `json:"distinct_combinations"`
				DuplicateRows int64    `json:"rows_in_a_duplicate"`
				Incomplete    int64    `json:"rows_missing_part_of_the_key"`
				Unique        bool     `json:"is_a_key"`
				EvidenceQuery string   `json:"evidence_query"`
				RowQuery      string   `json:"row_query"`
			}{
				Table: t.Name, Columns: in.Columns,
				Rows: rows, Distinct: distinct,
				DuplicateRows: duplicateRows, Incomplete: incomplete,
				Unique:        duplicateRows == 0 && incomplete == 0,
				EvidenceQuery: duplicated,
				RowQuery: fmt.Sprintf(
					`SELECT * FROM %[1]s WHERE (%[2]s) IN (SELECT %[2]s FROM %[1]s GROUP BY %[2]s HAVING count(*) > 1)`,
					table, quoted),
			}, nil
		},
	}
}

func checkReferentialIntegrity() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "check_referential_integrity",
			Description: "Count the rows in one table whose reference does not exist in another: " +
				"orders pointing at a customer that is not in the customer file, and the like. " +
				"Most real defects in a folder of exports live in the relationships between the " +
				"files rather than inside any one of them, so this is worth reaching for early.",
			Properties: map[string]any{
				"child_table":   str("the table holding the reference"),
				"child_column":  str("the referring column"),
				"parent_table":  str("the table that should contain it"),
				"parent_column": str("the column it should match"),
			},
			Required: []string{"child_table", "child_column", "parent_table", "parent_column"},
		},
		invoke: func(ctx context.Context, w *World, args json.RawMessage) (any, error) {
			var in struct {
				ChildTable   string `json:"child_table"`
				ChildColumn  string `json:"child_column"`
				ParentTable  string `json:"parent_table"`
				ParentColumn string `json:"parent_column"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}

			child, err := w.table(in.ChildTable)
			if err != nil {
				return nil, err
			}
			childCol, err := w.column(child, in.ChildColumn)
			if err != nil {
				return nil, err
			}
			parent, err := w.table(in.ParentTable)
			if err != nil {
				return nil, err
			}
			parentCol, err := w.column(parent, in.ParentColumn)
			if err != nil {
				return nil, err
			}

			var (
				ct = engine.Ident(child.Name)
				cc = engine.Ident(childCol.Name)
				pt = engine.Ident(parent.Name)
				pc = engine.Ident(parentCol.Name)
			)

			// Trimmed on both sides: a reference that only fails to match
			// because of stray whitespace is a real defect, but it is a
			// different one, and reporting it as a missing customer would send
			// somebody looking for a customer who exists.
			orphans := fmt.Sprintf(`
				SELECT count(*) FROM %[1]s
				WHERE %[3]s
				  AND trim(%[2]s) NOT IN (SELECT trim(%[5]s) FROM %[4]s WHERE %[6]s)`,
				ct, cc, profile.SQLNonBlank(cc), pt, pc, profile.SQLNonBlank(pc))

			orphanCount, err := w.scanCount(ctx, orphans)
			if err != nil {
				return nil, err
			}

			referencing, err := w.scanCount(ctx, fmt.Sprintf(
				`SELECT count(*) FROM %s WHERE %s`, ct, profile.SQLNonBlank(cc)))
			if err != nil {
				return nil, err
			}

			share := 0.0
			if referencing > 0 {
				share = float64(orphanCount) / float64(referencing)
			}

			return struct {
				Child         string  `json:"child"`
				Parent        string  `json:"parent"`
				Referencing   int64   `json:"rows_with_a_reference"`
				Orphans       int64   `json:"orphans"`
				OrphanShare   float64 `json:"orphan_share"`
				EvidenceQuery string  `json:"evidence_query"`
				RowQuery      string  `json:"row_query"`
			}{
				Child:         child.Name + "." + childCol.Name,
				Parent:        parent.Name + "." + parentCol.Name,
				Referencing:   referencing,
				Orphans:       orphanCount,
				OrphanShare:   round2(share),
				EvidenceQuery: orphans,
				RowQuery: fmt.Sprintf(`
					SELECT * FROM %[1]s
					WHERE %[3]s
					  AND trim(%[2]s) NOT IN (SELECT trim(%[5]s) FROM %[4]s WHERE %[6]s)`,
					ct, cc, profile.SQLNonBlank(cc), pt, pc, profile.SQLNonBlank(pc)),
			}, nil
		},
	}
}

func sampleValues() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "sample_values",
			Description: "The most frequent distinct entries in a column, with their counts. " +
				"By default the entries come back as shapes rather than values, which is enough " +
				"to see that a column mixes two formats but not what any customer is called. " +
				"If the operator has permitted values for this run, they arrive as written, " +
				"with obvious identifiers masked; the result says which you are looking at.",
			Properties: map[string]any{
				"table":  str("the table name"),
				"column": str("the column name"),
				"limit":  integer("how many entries to return (default 10, maximum 50)"),
			},
			Required: []string{"table", "column"},
		},
		invoke: func(ctx context.Context, w *World, args json.RawMessage) (any, error) {
			var in struct {
				Table  string `json:"table"`
				Column string `json:"column"`
				Limit  int    `json:"limit"`
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

			limit := in.Limit
			if limit <= 0 {
				limit = 10
			}
			if limit > 50 {
				limit = 50
			}

			col := engine.Ident(c.Name)
			q := fmt.Sprintf(`
				SELECT %[1]s AS v, count(*) AS n
				FROM %[2]s
				WHERE %[3]s
				GROUP BY 1 ORDER BY n DESC, v
				LIMIT %[4]d`, col, engine.Ident(t.Name), profile.SQLNonBlank(col), limit)

			rs, err := w.Engine.Collect(ctx, q, limit)
			if err != nil {
				return nil, fmt.Errorf("%v", w.Guard.EngineError(err))
			}

			out := struct {
				Table    string        `json:"table"`
				Column   string        `json:"column"`
				Redacted bool          `json:"values_are_shapes"`
				Values   []countedText `json:"values"`
			}{Table: t.Name, Column: c.Name, Redacted: !w.Guard.Policy().AllowValues}

			populated := c.Total - c.Nulls
			for _, row := range rs.Rows {
				text, _ := row[0].(string)
				count := asInt(row[1])
				vc := countedText{Value: w.Guard.Value(text), Count: count}
				if populated > 0 {
					vc.Share = round2(float64(count) / float64(populated))
				}
				out.Values = append(out.Values, vc)
			}
			return out, nil
		},
	}
}

// asInt normalises the integer types DuckDB returns for counts, which vary by
// expression: count() gives int64 and some aggregates give uint64.
func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case uint64:
		return int64(n) //nolint:gosec // row counters are far below the overflow point
	case uint32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
