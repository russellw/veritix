package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/source"
)

// excelDialect is how a materialized worksheet is read back. The CSV written
// by the Excel reader is always UTF-8, comma-separated, and RFC 4180 quoted,
// so nothing needs sniffing.
func excelDialect() source.CSVDialect {
	return source.CSVDialect{
		Delimiter: ",",
		Quote:     `"`,
		Escape:    `"`,
		HasHeader: true,
		Encoding:  source.EncodingUTF8,
		// NewLine is left unset: DuckDB detects the line ending itself, and
		// its new_line option wants the escaped text "\n" rather than an
		// actual newline character.
	}
}

// loadOne reads a single table into the engine.
func loadOne(ctx context.Context, e *engine.Engine, p *planned, log *slog.Logger) error {
	t := p.table
	t.readPath = p.readPath

	dialect := excelDialect()
	if p.sheet == "" {
		sniffed, err := source.SniffCSV(ctx, e, source.File{
			Path: p.readPath, Rel: p.display, Kind: source.KindCSV,
		})
		if err != nil {
			return err
		}
		dialect = sniffed
		t.Dialect = &dialect
		t.Notes = append(t.Notes, dialect.Notes...)
	}

	// Excel sheets always have a header written by the materializer, but the
	// names come from the worksheet, so sniff the schema to learn them.
	cols, err := columnsFor(ctx, e, p, dialect)
	if err != nil {
		return err
	}
	t.Columns = cols

	stmt := createTableStmt(t.Ref.Name, p.readPath, dialect, cols)
	if err := e.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("ingest: loading %s: %w", p.display, err)
	}

	n, err := e.CountRows(ctx, t.Ref.Name)
	if err != nil {
		return err
	}
	t.RowCount = n

	log.Debug("loaded table",
		"table", t.Ref.Name, "source", p.display, "rows", n, "columns", len(cols))

	if n == 0 {
		t.Notes = append(t.Notes, source.Note{
			Code:    "ingest.no_rows",
			Message: "the file has a header but no data rows",
		})
	}
	return nil
}

// columnsFor determines the column names and the types a conventional import
// would have inferred.
func columnsFor(ctx context.Context, e *engine.Engine, p *planned, d source.CSVDialect) ([]Column, error) {
	sniffed := d.SniffedTypes
	if len(sniffed) == 0 {
		// The Excel path has no sniff result yet; ask DuckDB about the
		// materialized CSV so that guessed types are available for comparison.
		var err error
		sniffed, err = sniffMaterialized(ctx, e, p.readPath)
		if err != nil {
			return nil, err
		}
	}

	cols := make([]Column, 0, len(sniffed))
	for i, s := range sniffed {
		name := s.Name
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("column_%d", i+1)
		}
		original := name
		switch {
		case p.sheet != "" && p.table.Sheet != nil && i < len(p.table.Sheet.Columns):
			original = p.table.Sheet.Columns[i]
		case i < len(d.HeaderNames):
			original = d.HeaderNames[i]
		}

		cols = append(cols, Column{
			Name:        name,
			Original:    original,
			Ordinal:     i + 1,
			SniffedType: s.Type,
			Renamed:     strings.TrimSpace(original) != "" && original != name,
		})
	}

	if len(cols) == 0 {
		return nil, fmt.Errorf("ingest: %s has no columns", p.display)
	}
	return cols, nil
}

func sniffMaterialized(ctx context.Context, e *engine.Engine, path string) ([]source.SniffedColumn, error) {
	q := "SELECT unnest(Columns).name AS name, unnest(Columns).type AS type FROM sniff_csv(" +
		engine.Literal(path) + ")"
	rs, err := e.Collect(ctx, q, 10_000)
	if err != nil {
		return nil, fmt.Errorf("ingest: sniffing materialized sheet: %w", err)
	}
	out := make([]source.SniffedColumn, 0, len(rs.Rows))
	for _, r := range rs.Rows {
		out = append(out, source.SniffedColumn{
			Name: fmt.Sprint(r[0]),
			Type: fmt.Sprint(r[1]),
		})
	}
	return out, nil
}

// createTableStmt builds the CREATE TABLE ... AS SELECT that loads a file.
//
// Every column is declared VARCHAR so that nothing is coerced or dropped on
// the way in, and auto-detection is switched off so the dialect Veritix
// reports is exactly the dialect it used. Structural failures (rows with the
// wrong number of fields) are diverted to the reject tables instead of
// aborting the load, because a file with three bad rows out of a million is
// still worth auditing — and those three rows are themselves a finding.
func createTableStmt(table, path string, d source.CSVDialect, cols []Column) string {
	var schema strings.Builder
	schema.WriteByte('{')
	for i, c := range cols {
		if i > 0 {
			schema.WriteString(", ")
		}
		schema.WriteString(engine.Literal(c.Name))
		schema.WriteString(": 'VARCHAR'")
	}
	schema.WriteByte('}')

	opts := []string{
		"auto_detect=false",
		"columns=" + schema.String(),
		"header=" + boolLiteral(d.HasHeader),
		"null_padding=false",
		"ignore_errors=true",
		"store_rejects=true",
		"rejects_table=" + engine.Literal(rejectErrorsTable),
		"rejects_scan=" + engine.Literal(rejectScansTable),
	}
	if d.Delimiter != "" {
		opts = append(opts, "delim="+engine.Literal(d.Delimiter))
	}
	if d.Quote != "" {
		opts = append(opts, "quote="+engine.Literal(d.Quote))
	}
	if d.Escape != "" {
		opts = append(opts, "escape="+engine.Literal(d.Escape))
	}
	if d.NewLine != "" {
		// DuckDB reports and expects line endings in escaped form ("\n"), so
		// a real control character has to be converted back before it is
		// handed to read_csv.
		opts = append(opts, "new_line="+engine.Literal(escapeNewline(d.NewLine)))
	}
	if d.Comment != "" {
		opts = append(opts, "comment="+engine.Literal(d.Comment))
	}
	if d.SkipRows > 0 {
		opts = append(opts, fmt.Sprintf("skip=%d", d.SkipRows))
	}
	if d.Encoding != "" && d.Encoding != source.EncodingUTF8 {
		opts = append(opts, "encoding="+engine.Literal(string(d.Encoding)))
	}

	return fmt.Sprintf("CREATE OR REPLACE TABLE %s AS SELECT * FROM read_csv(%s, %s)",
		engine.Ident(table), engine.Literal(path), strings.Join(opts, ", "))
}

// escapeNewline renders control characters the way DuckDB's new_line option
// expects them. A string that is already escaped passes through untouched.
func escapeNewline(s string) string {
	r := strings.NewReplacer("\r\n", `\r\n`, "\n", `\n`, "\r", `\r`)
	return r.Replace(s)
}

func boolLiteral(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// prepareRejectTables clears any reject tables left over from a previous load
// on the same engine, so counts belong to this run alone.
func prepareRejectTables(ctx context.Context, e *engine.Engine) error {
	for _, t := range []string{rejectErrorsTable, rejectScansTable} {
		if err := e.Exec(ctx, "DROP TABLE IF EXISTS "+engine.Ident(t)); err != nil {
			return fmt.Errorf("ingest: clearing reject tables: %w", err)
		}
	}
	return nil
}

// attachRejects matches rejected rows back to the tables they came from.
// DuckDB records rejects per scan and keys them by file path, so the join is
// on the path each table was read from.
func attachRejects(ctx context.Context, e *engine.Engine, tables []*Table) error {
	exists, err := e.TableExists(ctx, rejectErrorsTable)
	if err != nil {
		return err
	}
	if !exists {
		return nil // nothing was rejected
	}

	byPath := make(map[string]*Table, len(tables))
	for _, t := range tables {
		byPath[readPathOf(t)] = t
	}

	countQ := fmt.Sprintf(`
		SELECT s.file_path, count(DISTINCT e.line)
		FROM %s e JOIN %s s USING (scan_id, file_id)
		GROUP BY 1`,
		engine.Ident(rejectErrorsTable), engine.Ident(rejectScansTable))

	counts, err := e.Collect(ctx, countQ, 100_000)
	if err != nil {
		return err
	}
	for _, r := range counts.Rows {
		if t := byPath[fmt.Sprint(r[0])]; t != nil {
			t.RejectCount = toInt64(r[1])
		}
	}

	// A single bad line can raise an error per offending column, so collapse
	// to one sample per line before taking the per-file sample.
	sampleQ := fmt.Sprintf(`
		WITH per_line AS (
			SELECT s.file_path AS file_path, e.line AS line,
			       coalesce(e.column_name, '') AS column_name, e.error_type AS error_type,
			       e.error_message AS error_message, e.csv_line AS csv_line
			FROM %s e JOIN %s s USING (scan_id, file_id)
			QUALIFY row_number() OVER (PARTITION BY s.file_path, e.line
			                           ORDER BY e.column_idx) = 1
		)
		SELECT file_path, line, column_name, error_type, error_message, csv_line
		FROM per_line
		QUALIFY row_number() OVER (PARTITION BY file_path ORDER BY line) <= %d
		ORDER BY file_path, line`,
		engine.Ident(rejectErrorsTable), engine.Ident(rejectScansTable), maxRejectSamples)

	samples, err := e.Collect(ctx, sampleQ, len(tables)*maxRejectSamples+1)
	if err != nil {
		return err
	}
	for _, r := range samples.Rows {
		t := byPath[fmt.Sprint(r[0])]
		if t == nil {
			continue
		}
		t.Rejects = append(t.Rejects, Reject{
			Line:      toInt64(r[1]),
			Column:    fmt.Sprint(r[2]),
			ErrorType: fmt.Sprint(r[3]),
			Message:   fmt.Sprint(r[4]),
			RawLine:   fmt.Sprint(r[5]),
		})
	}

	for _, t := range tables {
		if t.RejectCount > 0 {
			t.Notes = append(t.Notes, source.Note{
				Code: "ingest.rejected_rows",
				Message: fmt.Sprintf("%d rows could not be read as %d columns and were skipped; "+
					"they are absent from every count and total computed downstream",
					t.RejectCount, len(t.Columns)),
			})
		}
	}
	return nil
}

// readPathOf recovers the path a table's data was read from. For CSV that is
// the file itself; for Excel it is the materialized worksheet.
func readPathOf(t *Table) string {
	if t.readPath != "" {
		return t.readPath
	}
	return t.Ref.File.Path
}

// toInt64 normalizes the integer types the DuckDB driver can hand back. The
// reject tables use unsigned counters, so uint64 is the common case here
// rather than the exception.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case uint64:
		return int64(n) //nolint:gosec // line and row counters are far below the overflow point
	case uint32:
		return int64(n)
	case float64:
		return int64(n)
	case nil:
		return 0
	default:
		parsed, err := strconv.ParseInt(fmt.Sprint(n), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	}
}
