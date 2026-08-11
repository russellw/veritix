package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// shape renders a value's pattern: digits become 9, letters become X, runs of
// whitespace become a single space, and punctuation is kept.
//
// It mirrors the SQL the profiler uses to derive a column's shapes, and the
// two have to keep agreeing: the model is given a column's shapes as part of
// its profile and then sees shapes again in tool results, and a shape that
// changed on the way would look like a different value.
func shape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			b.WriteByte('9')
		case unicode.IsLetter(r):
			b.WriteByte('X')
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// The patterns below are the identifiers that are recognisable enough to strip
// without knowing anything about the dataset. This is not a PII classifier: a
// column of surnames is personal data and no regex will say so. It is a last
// line under --allow-sample-values, where the operator has already decided
// that values may be sent, and its job is to stop the categories that turn a
// disclosure into a reportable breach.
var (
	emailPattern = regexp.MustCompile(`[\p{L}\p{N}._%+-]+@[\p{L}\p{N}.-]+\.[\p{L}]{2,}`)
	// Twelve or more digits, however they are grouped: card numbers, IBANs,
	// account numbers, and long national identifiers.
	longNumberPattern = regexp.MustCompile(`\b(?:\d[ -]?){12,}\d?\b`)
	// The US social security format, which is too short for the rule above and
	// too distinctive to leave alone.
	ssnPattern = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
)

// mask removes recognisable identifiers from a value that policy allows
// through, and reports whether it changed anything.
func mask(s string) (string, bool) {
	out := emailPattern.ReplaceAllString(s, "[email]")
	out = ssnPattern.ReplaceAllString(out, "[national-id]")
	out = longNumberPattern.ReplaceAllString(out, "[long-number]")
	return out, out != s
}
