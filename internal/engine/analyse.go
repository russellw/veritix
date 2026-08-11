package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Analysis is what DuckDB's parser says about a statement.
//
// It exists because model-authored SQL needs two questions answered before its
// results can be shown to anybody: is this one read-only statement, and which
// of its output columns are statistics rather than cell contents. Both are
// answered by parsing, not by matching patterns in the text — a regex that
// decides what SQL does is a regex somebody will eventually get past.
type Analysis struct {
	// Aggregate reports, per output column, whether the expression producing it
	// is built only from aggregates and constants and therefore cannot be a
	// value out of a row.
	//
	// It is conservative in the safe direction: an expression the parser
	// describes in a way this code does not recognise is reported as not an
	// aggregate, so it is shaped rather than disclosed.
	Aggregate []bool
}

// AnalyseSelect parses a statement and describes it, refusing anything that is
// not a single SELECT.
//
// The refusal is DuckDB's, not Veritix's: json_serialize_sql only serialises
// SELECT statements, so a COPY, an ATTACH, a DDL statement, or two statements
// separated by a semicolon fail here without Veritix needing an opinion about
// what those look like. What that does *not* cover — a SELECT that reads a file
// through a table function — is covered by Lockdown instead.
func (e *Engine) AnalyseSelect(ctx context.Context, query string) (*Analysis, error) {
	var raw string
	stmt := "SELECT CAST(json_serialize_sql(" + Literal(query) + ") AS VARCHAR)"
	if err := e.ScanOne(ctx, stmt, []any{&raw}); err != nil {
		return nil, fmt.Errorf("engine: the statement could not be parsed: %w", err)
	}

	var parsed struct {
		Error        bool              `json:"error"`
		ErrorMessage string            `json:"error_message"`
		Statements   []json.RawMessage `json:"statements"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("engine: reading the parse tree: %w", err)
	}
	if parsed.Error {
		msg := parsed.ErrorMessage
		if strings.Contains(msg, "Only SELECT statements") {
			msg = "only a single SELECT statement can be run here"
		}
		return nil, fmt.Errorf("engine: %s", msg)
	}
	if len(parsed.Statements) != 1 {
		return nil, fmt.Errorf("engine: expected one statement, got %d", len(parsed.Statements))
	}

	aggregates, err := e.aggregateFunctions(ctx)
	if err != nil {
		return nil, err
	}

	var stmtNode struct {
		Node struct {
			Type       string     `json:"type"`
			SelectList []exprNode `json:"select_list"`
		} `json:"node"`
	}
	if err := json.Unmarshal(parsed.Statements[0], &stmtNode); err != nil {
		return nil, fmt.Errorf("engine: reading the parse tree: %w", err)
	}

	// A node type this code does not model — a UNION, for instance — yields no
	// select list, and every column is then treated as not an aggregate, which
	// is the cautious answer.
	a := &Analysis{Aggregate: make([]bool, len(stmtNode.Node.SelectList))}
	for i, expr := range stmtNode.Node.SelectList {
		a.Aggregate[i] = isStatistic(expr, aggregates)
	}
	return a, nil
}

// exprNode is the part of DuckDB's serialised expression tree this code reads.
type exprNode struct {
	Class        string     `json:"class"`
	FunctionName string     `json:"function_name"`
	Children     []exprNode `json:"children"`
	Child        *exprNode  `json:"child"`
}

// isStatistic reports whether an expression can only produce a summary.
//
// A constant discloses nothing. An aggregate call reduces many rows to one
// number. And an expression built only from those — round(avg(x), 2),
// sum(bad) / count(*) — is still a summary, which is worth recognising because
// otherwise the useful half of what an auditor asks for comes back shaped.
// Anything else may be a cell value and is treated as one.
func isStatistic(e exprNode, aggregates map[string]bool) bool {
	switch e.Class {
	case "CONSTANT":
		return true

	case "FUNCTION", "OPERATOR", "CAST", "COMPARISON", "CONJUNCTION", "CASE":
		if aggregates[strings.ToLower(e.FunctionName)] {
			return true
		}
		children := e.Children
		if e.Child != nil {
			children = append(children, *e.Child)
		}
		if len(children) == 0 {
			return false
		}
		for _, c := range children {
			if !isStatistic(c, aggregates) {
				return false
			}
		}
		return true

	default:
		// COLUMN_REF, STAR, SUBQUERY, WINDOW, and anything else unrecognised.
		return false
	}
}

// aggregateFunctions reads DuckDB's own catalogue of aggregate functions.
//
// Reading the catalogue rather than keeping a list means the set cannot drift
// out of date with the engine, and a DuckDB upgrade that adds an aggregate does
// not quietly start shaping its results.
func (e *Engine) aggregateFunctions(ctx context.Context) (map[string]bool, error) {
	e.aggOnce.Do(func() {
		rs, err := e.Collect(ctx,
			`SELECT DISTINCT lower(function_name) FROM duckdb_functions()
			 WHERE function_type = 'aggregate'`, 10_000)
		if err != nil {
			e.aggErr = err
			return
		}
		set := make(map[string]bool, len(rs.Rows))
		for _, row := range rs.Rows {
			if name, ok := row[0].(string); ok {
				set[name] = true
			}
		}
		e.aggNames = set
	})
	if e.aggErr != nil {
		return nil, fmt.Errorf("engine: reading the aggregate catalogue: %w", e.aggErr)
	}
	return e.aggNames, nil
}

// aggregateCache is embedded in Engine.
type aggregateCache struct {
	aggOnce  sync.Once
	aggNames map[string]bool
	aggErr   error
}
