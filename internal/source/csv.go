package source

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"github.com/russellwallace/veritix/internal/engine"
)

// Encoding is a character encoding Veritix can read.
type Encoding string

const (
	EncodingUTF8    Encoding = "utf-8"
	EncodingUTF16   Encoding = "utf-16"
	EncodingLatin1  Encoding = "latin-1"
	EncodingUnknown Encoding = ""
)

// CSVDialect is everything needed to read one delimited file, plus what was
// noticed about it along the way.
type CSVDialect struct {
	Delimiter string
	Quote     string
	Escape    string
	NewLine   string
	Comment   string
	SkipRows  int
	HasHeader bool
	Encoding  Encoding

	// HeaderNames are the column names exactly as they appear in the file,
	// before any de-duplication. DuckDB silently renames a repeated header to
	// keep its schema valid, which would hide a real defect, so Veritix reads
	// the header line itself as well.
	HeaderNames []string

	// SniffedTypes is DuckDB's guess at each column's type. Veritix loads all
	// columns as text regardless; this is retained so the profiler can report
	// where a naive import would have guessed wrong, silently coerced a value,
	// or turned a bad value into a null.
	SniffedTypes []SniffedColumn

	// Notes are observations about the file itself, promoted to findings later.
	Notes []Note
}

// SniffedColumn is one column of DuckDB's guessed schema.
type SniffedColumn struct {
	Name string
	Type string
}

// Note is an observation made while reading a file.
type Note struct {
	Code    string
	Message string
}

// headSize is how much of a file is read for encoding detection. Large enough
// to catch a non-ASCII byte that only appears once the data starts, small
// enough to stay cheap on a directory of large exports.
const headSize = 256 << 10

// SniffCSV determines how to read a delimited file.
func SniffCSV(ctx context.Context, e *engine.Engine, f File) (CSVDialect, error) {
	d := CSVDialect{}

	head, err := readHead(f.Path, headSize)
	if err != nil {
		return d, fmt.Errorf("source: reading %s: %w", f.Rel, err)
	}

	d.Encoding, d.Notes = detectEncoding(head)

	// First pass: let DuckDB propose a dialect.
	if err := e.ScanOne(ctx, sniffQuery(f.Path, d.Encoding, ""), []any{
		&d.Delimiter, &d.Quote, &d.Escape, &d.NewLine, &d.Comment,
		&d.SkipRows, &d.HasHeader,
	}); err != nil {
		return d, fmt.Errorf("source: sniffing %s: %w", f.Rel, err)
	}
	d.normalise()

	// Second pass: check that proposal against Veritix's own scoring, and
	// re-sniff with the chosen delimiter if they disagree. Getting this wrong
	// is not a cosmetic problem — a file read as one column passes every
	// column-level check vacuously.
	extHint := csvExtensions[strings.ToLower(extOf(f.Path))]
	chosen, delimNotes := chooseDelimiter(head, d.Encoding, d.Delimiter, extHint)
	d.Notes = append(d.Notes, delimNotes...)

	if chosen != d.Delimiter {
		retry := d
		err := e.ScanOne(ctx, sniffQuery(f.Path, d.Encoding, chosen), []any{
			&retry.Delimiter, &retry.Quote, &retry.Escape, &retry.NewLine, &retry.Comment,
			&retry.SkipRows, &retry.HasHeader,
		})
		if err == nil {
			retry.normalise()
			d.Quote, d.Escape, d.NewLine = retry.Quote, retry.Escape, retry.NewLine
			d.Comment, d.SkipRows, d.HasHeader = retry.Comment, retry.SkipRows, retry.HasHeader
		} else {
			// A file too irregular to describe is still a file Veritix has to
			// audit; fall back to conventional defaults and say so.
			d.Quote, d.Escape = `"`, `"`
			d.HasHeader = true
			d.Notes = append(d.Notes, Note{
				Code: "csv.dialect_undetectable",
				Message: "the file is irregular enough that its dialect could not be determined " +
					"automatically; it was read as standard comma-style quoting with a header row",
			})
		}
		d.Delimiter = chosen
	}

	cols, err := sniffColumns(ctx, e, f.Path, d.Encoding, d.Delimiter)
	if err != nil {
		// Falling back to the header line keeps a badly-formed file readable.
		cols = nil
	}
	d.SniffedTypes = cols

	if d.HasHeader {
		names, err := readHeaderLine(head, d)
		if err == nil {
			d.HeaderNames = names
			d.Notes = append(d.Notes, inspectHeader(names)...)

			// The header line is the file's own statement of its width, and it
			// outranks a count inferred from the data. A single over-long row
			// caused by a stray separator will otherwise widen the whole
			// schema, and then every properly-formed row is rejected for being
			// too short — turning one bad row into a whole bad file.
			switch {
			case len(d.SniffedTypes) == 0:
				d.SniffedTypes = columnsFromHeader(names)
			case len(d.SniffedTypes) != len(names):
				d.Notes = append(d.Notes, Note{
					Code: "csv.width_disagreement",
					Message: fmt.Sprintf(
						"the header declares %d columns but the data implies %d; reading the file "+
							"with %d columns and reporting the rows that do not fit",
						len(names), len(d.SniffedTypes), len(names)),
				})
				d.SniffedTypes = columnsFromHeader(names)
			}
		}
	} else {
		d.Notes = append(d.Notes, Note{
			Code: "csv.no_header",
			Message: "no header row was detected; columns are identified by position, " +
				"so a column added or reordered upstream will go unnoticed",
		})
	}

	return d, nil
}

// duckdbEmptySentinel is what sniff_csv reports for a dialect option that is
// not in use. It is the literal text "(empty)", not an empty string, and
// passing it back to read_csv is rejected as an over-long quote character.
const duckdbEmptySentinel = "(empty)"

// normalise converts DuckDB's sentinel values into the empty strings the rest
// of the package expects.
func (d *CSVDialect) normalise() {
	for _, p := range []*string{&d.Delimiter, &d.Quote, &d.Escape, &d.NewLine, &d.Comment} {
		if *p == duckdbEmptySentinel {
			*p = ""
		}
	}
}

// sniffArgs renders the optional arguments shared by every sniff_csv call.
// The path and options are literals because DuckDB table functions do not take
// bound parameters.
func sniffArgs(enc Encoding, delim string) string {
	// Veritix's whole subject matter is files that do not comply with the CSV
	// standard. Strict sniffing refuses to describe them at all, so tolerance
	// is switched on: the goal here is to learn the dialect, and the
	// non-compliant rows are captured properly by the reject tables at load
	// time rather than being wished away.
	args := ", ignore_errors=true, null_padding=true"
	if enc != EncodingUTF8 && enc != EncodingUnknown {
		args += ", encoding=" + engine.Literal(string(enc))
	}
	if delim != "" {
		args += ", delim=" + engine.Literal(delim)
	}
	return args
}

// sniffQuery builds the sniff_csv call.
func sniffQuery(path string, enc Encoding, delim string) string {
	return "SELECT Delimiter, Quote, Escape, NewLineDelimiter, Comment, SkipRows, HasHeader " +
		"FROM sniff_csv(" + engine.Literal(path) + sniffArgs(enc, delim) + ")"
}

// sniffColumns pulls the guessed schema out of sniff_csv's Columns field,
// which is a list of structs rather than a scalar.
func sniffColumns(ctx context.Context, e *engine.Engine, path string, enc Encoding, delim string) ([]SniffedColumn, error) {
	q := "SELECT unnest(Columns).name AS name, unnest(Columns).type AS type FROM sniff_csv(" +
		engine.Literal(path) + sniffArgs(enc, delim) + ")"

	rs, err := e.Collect(ctx, q, 10_000)
	if err != nil {
		return nil, fmt.Errorf("source: reading sniffed schema: %w", err)
	}
	cols := make([]SniffedColumn, 0, len(rs.Rows))
	for _, r := range rs.Rows {
		cols = append(cols, SniffedColumn{
			Name: asString(r[0]),
			Type: asString(r[1]),
		})
	}
	return cols, nil
}

// columnsFromHeader builds a schema straight from the header line, applying
// the same de-duplication a database import would, for files DuckDB could not
// describe.
func columnsFromHeader(names []string) []SniffedColumn {
	used := make(map[string]int, len(names))
	cols := make([]SniffedColumn, 0, len(names))

	for i, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			name = fmt.Sprintf("column_%d", i+1)
		}
		if n, taken := used[name]; taken {
			used[name] = n + 1
			name = fmt.Sprintf("%s_%d", name, n)
		} else {
			used[name] = 1
		}
		cols = append(cols, SniffedColumn{Name: name, Type: "VARCHAR"})
	}
	return cols
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	case nil:
		return ""
	default:
		return fmt.Sprint(s)
	}
}

func extOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i:]
	}
	return ""
}

func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from dataset discovery
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:read], nil
}

// Byte-order marks, longest first so that UTF-8's three bytes are not mistaken
// for the start of a two-byte UTF-16 mark.
var boms = []struct {
	prefix []byte
	enc    Encoding
	label  string
}{
	{[]byte{0xEF, 0xBB, 0xBF}, EncodingUTF8, "UTF-8"},
	{[]byte{0xFF, 0xFE}, EncodingUTF16, "UTF-16 little-endian"},
	{[]byte{0xFE, 0xFF}, EncodingUTF16, "UTF-16 big-endian"},
}

// detectEncoding works out how to decode a file's bytes.
//
// Encoding matters more than it looks. A Latin-1 export read as UTF-8 turns
// every accented name into replacement characters, and a de-duplication check
// then reports two customers as distinct when they are the same person.
func detectEncoding(head []byte) (Encoding, []Note) {
	var notes []Note

	for _, b := range boms {
		if bytes.HasPrefix(head, b.prefix) {
			notes = append(notes, Note{
				Code:    "csv.bom",
				Message: b.label + " byte-order mark present; it will be stripped rather than treated as data",
			})
			return b.enc, notes
		}
	}

	if utf8.Valid(head) {
		return EncodingUTF8, notes
	}

	// A file that is not valid UTF-8 is most often a Windows-1252 or Latin-1
	// export. Latin-1 cannot fail to decode, so this is a fallback rather than
	// a detection, and it is worth telling the user about.
	notes = append(notes, Note{
		Code: "csv.encoding_not_utf8",
		Message: "file is not valid UTF-8; reading it as Latin-1. Accented and " +
			"non-Latin characters may be wrong, which can make identical values " +
			"look distinct",
	})
	return EncodingLatin1, notes
}

// decoder returns a reader that converts the file's bytes to UTF-8.
func (e Encoding) decoder(r io.Reader) io.Reader {
	switch e {
	case EncodingUTF16:
		// The BOM decides the byte order; the declared order is only a default.
		dec := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()
		return transform.NewReader(r, dec)
	case EncodingLatin1:
		return transform.NewReader(r, charmap.ISO8859_1.NewDecoder())
	default:
		// Strip a UTF-8 BOM so it does not end up glued to the first header name.
		return transform.NewReader(r, unicode.UTF8BOM.NewDecoder())
	}
}

// readHeaderLine parses the header exactly as written, without the
// de-duplication a database import would apply.
func readHeaderLine(head []byte, d CSVDialect) ([]string, error) {
	r := csv.NewReader(d.Encoding.decoder(bytes.NewReader(head)))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = false
	if d.Delimiter != "" {
		delim, _ := utf8.DecodeRuneInString(d.Delimiter)
		if delim != utf8.RuneError {
			r.Comma = delim
		}
	}

	for i := 0; i <= d.SkipRows; i++ {
		rec, err := r.Read()
		if err != nil {
			return nil, err
		}
		if i == d.SkipRows {
			return rec, nil
		}
	}
	return nil, io.EOF
}

// inspectHeader reports header problems that a database import would quietly
// paper over. Each of these has a habit of turning into a wrong answer much
// further downstream.
func inspectHeader(names []string) []Note {
	var notes []Note

	seen := make(map[string][]int, len(names))
	for i, n := range names {
		key := strings.ToLower(strings.TrimSpace(n))
		seen[key] = append(seen[key], i)

		switch {
		case strings.TrimSpace(n) == "":
			notes = append(notes, Note{
				Code:    "csv.header_blank",
				Message: fmt.Sprintf("column %d has no name", i+1),
			})
		case n != strings.TrimSpace(n):
			notes = append(notes, Note{
				Code: "csv.header_whitespace",
				Message: fmt.Sprintf("column %d is named %q, with leading or trailing whitespace "+
					"that will not match a lookup on the trimmed name", i+1, n),
			})
		}
	}

	for key, positions := range seen {
		if len(positions) > 1 && key != "" {
			notes = append(notes, Note{
				Code: "csv.header_duplicate",
				Message: fmt.Sprintf("the name %q is used by %d columns (positions %s); "+
					"an import will silently rename all but the first",
					key, len(positions), joinInts(positions)),
			})
		}
	}
	return notes
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprint(x + 1)
	}
	return strings.Join(parts, ", ")
}
