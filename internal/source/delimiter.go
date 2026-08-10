package source

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Delimiter detection is done here rather than left entirely to DuckDB.
//
// A statistical sniffer picks the delimiter that yields the most consistent
// row width, which is the right rule for a clean file and the wrong one for a
// broken export. Given a comma-separated file where a few rows have a stray
// comma inside an unquoted field, comma looks inconsistent while a character
// that appears nowhere looks perfectly consistent at one column per row. The
// sniffer then reports a single-column file, every column check downstream
// becomes vacuous, and the audit passes a file that is badly broken.
//
// Veritix therefore scores the candidates itself, prefers a delimiter that
// actually splits the file, and reports disagreement rather than hiding it.

// candidateDelimiters are the separators worth considering, in preference
// order for breaking ties.
var candidateDelimiters = []string{",", ";", "\t", "|"}

// delimiterScore describes how well one candidate explains a file.
type delimiterScore struct {
	delim string
	// fields is the most common number of fields per line.
	fields int
	// consistency is the share of lines that have exactly that many fields.
	consistency float64
	// lines is how many lines were scored.
	lines int
}

// scanRows is how many lines are examined when scoring delimiters. Enough to
// be representative, few enough to stay cheap on a large export.
const scanRows = 200

// scoreDelimiters parses the head of a file with each candidate delimiter.
func scoreDelimiters(head []byte, enc Encoding) []delimiterScore {
	scores := make([]delimiterScore, 0, len(candidateDelimiters))

	for _, d := range candidateDelimiters {
		counts := fieldCounts(head, enc, rune(d[0]))
		if len(counts) == 0 {
			continue
		}

		modal, occurrences := mode(counts)
		scores = append(scores, delimiterScore{
			delim:       d,
			fields:      modal,
			consistency: float64(occurrences) / float64(len(counts)),
			lines:       len(counts),
		})
	}
	return scores
}

// fieldCounts returns the number of fields on each of the first scanRows lines.
func fieldCounts(head []byte, enc Encoding, delim rune) []int {
	r := csv.NewReader(enc.decoder(bytes.NewReader(head)))
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.ReuseRecord = true

	var counts []int
	for len(counts) < scanRows {
		rec, err := r.Read()
		if err != nil {
			// A parse failure mid-file is normal for the wrong delimiter and
			// for a truncated final line; score what was read.
			if errors.Is(err, io.EOF) || len(counts) > 0 {
				break
			}
			return nil
		}
		counts = append(counts, len(rec))
	}
	return counts
}

func mode(xs []int) (value, occurrences int) {
	freq := make(map[int]int, len(xs))
	for _, x := range xs {
		freq[x]++
	}
	// Iterate deterministically so ties resolve the same way every run.
	keys := make([]int, 0, len(freq))
	for k := range freq {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	for _, k := range keys {
		if freq[k] > occurrences || (freq[k] == occurrences && k > value) {
			value, occurrences = k, freq[k]
		}
	}
	return value, occurrences
}

// chooseDelimiter picks the delimiter to read a file with, given DuckDB's
// suggestion, the file extension, and Veritix's own scoring.
//
// The rule that matters: a delimiter that splits the file into several columns
// beats one that does not, even if the single-column reading looks more
// consistent. A one-column CSV is almost always a detection failure rather
// than a real file.
func chooseDelimiter(head []byte, enc Encoding, sniffed, extHint string) (string, []Note) {
	var notes []Note

	scores := scoreDelimiters(head, enc)
	byDelim := make(map[string]delimiterScore, len(scores))
	for _, s := range scores {
		byDelim[s.delim] = s
	}

	// Rank by field count first, then by consistency: splitting the file at
	// all matters more than splitting it tidily.
	best := ""
	for _, d := range candidateDelimiters {
		s, ok := byDelim[d]
		if !ok || s.fields < 2 {
			continue
		}
		b, seen := byDelim[best]
		switch {
		case !seen:
			best = d
		case s.fields > b.fields:
			best = d
		case s.fields == b.fields && s.consistency > b.consistency:
			best = d
		}
	}

	// An extension that names its delimiter outranks statistics, provided that
	// delimiter actually splits the file.
	if extHint != "" {
		if s, ok := byDelim[extHint]; ok && s.fields >= 2 {
			if best != "" && best != extHint {
				notes = append(notes, Note{
					Code: "csv.delimiter_ambiguous",
					Message: fmt.Sprintf("the file extension implies %s as the delimiter, "+
						"but %s also parses; reading it as %s",
						describeDelim(extHint), describeDelim(best), describeDelim(extHint)),
				})
			}
			best = extHint
		}
	}

	if best == "" {
		// Nothing splits the file. Keep DuckDB's answer, which at least
		// produces a readable single-column table.
		if sniffed != "" {
			return sniffed, notes
		}
		return ",", notes
	}

	if sniffed != "" && sniffed != best {
		s := byDelim[sniffed]
		notes = append(notes, Note{
			Code: "csv.delimiter_disagreement",
			Message: fmt.Sprintf(
				"automatic detection chose %s, which yields %d column(s); reading the file as "+
					"%s instead, which yields %d. Rows with an inconsistent number of fields "+
					"can defeat delimiter detection, so check the file for stray unquoted "+
					"separators inside values",
				describeDelim(sniffed), s.fields, describeDelim(best), byDelim[best].fields),
		})
	}

	if s := byDelim[best]; s.consistency < 0.95 {
		notes = append(notes, Note{
			Code: "csv.inconsistent_width",
			Message: fmt.Sprintf(
				"only %.0f%% of the first %d lines have the expected %d fields; the rest have a "+
					"different number, which usually means an unquoted separator inside a value",
				s.consistency*100, s.lines, s.fields),
		})
	}

	return best, notes
}

func describeDelim(d string) string {
	switch d {
	case "\t":
		return "tab"
	case ",":
		return "comma"
	case ";":
		return "semicolon"
	case "|":
		return "pipe"
	case "":
		return "none"
	default:
		return strings.TrimSpace(fmt.Sprintf("%q", d))
	}
}
