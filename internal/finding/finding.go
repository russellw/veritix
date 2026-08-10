// Package finding is Veritix's report currency: one problem found in a
// dataset, with the evidence that proves it.
//
// Every finding carries a re-runnable query. That is the whole design: a
// deterministic check and an agent-authored observation produce the same kind
// of object, and both can be verified by running their evidence again. A
// finding whose evidence no longer reproduces is dropped rather than reported,
// which is what makes an agentic auditor trustworthy instead of merely
// plausible.
package finding

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/russellwallace/veritix/internal/engine"
)

// Severity is how much a finding matters.
type Severity int

const (
	// Info is worth knowing but not necessarily wrong.
	Info Severity = iota
	// Warning is likely wrong, or right but fragile.
	Warning
	// Error is data that cannot be correct.
	Error
)

// String renders a severity for reports.
func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// ParseSeverity reads a severity name, for the --fail-on flag and rule files.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return Info, nil
	case "warn", "warning":
		return Warning, nil
	case "error":
		return Error, nil
	default:
		return Info, fmt.Errorf("unknown severity %q: want info, warning, or error", s)
	}
}

// MarshalText renders a severity in JSON.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText parses a severity from JSON or YAML.
func (s *Severity) UnmarshalText(b []byte) error {
	v, err := ParseSeverity(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// Origin records what produced a finding, so a reader can weigh it.
type Origin string

const (
	// OriginCheck is one of Veritix's built-in deterministic checks.
	OriginCheck Origin = "check"
	// OriginRule is a rule the customer wrote.
	OriginRule Origin = "rule"
	// OriginAgent is a model-proposed finding. It is still backed by
	// evidence, and that evidence is re-run before it is reported.
	OriginAgent Origin = "agent"
)

// Location says where in the dataset a finding sits.
type Location struct {
	// Table is the SQL name of the table.
	Table string
	// Display is the human-readable origin, e.g. "sales.xlsx#Q1".
	Display string
	// Column is the column, when the finding is about one.
	Column string
	// File is the path relative to the dataset root.
	File string
	// Line is a line number in the source file, where one applies.
	Line int64
}

// String renders a location compactly.
func (l Location) String() string {
	where := l.Display
	if where == "" {
		where = l.Table
	}
	if l.Column != "" {
		where += "." + l.Column
	}
	if l.Line > 0 {
		where += fmt.Sprintf(":%d", l.Line)
	}
	return where
}

// Evidence is the proof behind a finding.
//
// CountQuery must return exactly one row and one integer column: the number of
// affected rows or values. It is what Verify re-runs. RowQuery, when set,
// returns the offending rows themselves; it is never run automatically,
// because its results are raw customer data.
type Evidence struct {
	// CountQuery re-measures the finding. Required.
	CountQuery string
	// RowQuery retrieves the affected rows for a human to inspect.
	RowQuery string
	// Expected describes what should have been true, in words.
	Expected string
	// Observed describes what was actually found, in words.
	Observed string
}

// Finding is one problem, with its evidence.
type Finding struct {
	// Rule identifies the check, e.g. "column.mixed_date_formats". Stable
	// across releases so that findings can be suppressed and tracked.
	Rule     string
	Severity Severity
	Origin   Origin

	// Title is one specific line: what is wrong, with numbers.
	Title string
	// Detail explains why it matters, in terms of what will go wrong
	// downstream rather than in terms of the check that fired.
	Detail string
	// Remedy is what to do about it.
	Remedy string

	Location Location

	// Count is how many rows or values are affected, and Total is how many
	// there were. Both feed the report and Verify.
	Count int64
	Total int64

	Evidence Evidence

	// Verified records that the evidence was re-run and reproduced.
	Verified bool
}

// Share is the affected proportion, for reports.
func (f Finding) Share() float64 {
	if f.Total == 0 {
		return 0
	}
	return float64(f.Count) / float64(f.Total)
}

// key identifies a finding for de-duplication. Two checks noticing the same
// problem about the same column should produce one entry in the report.
func (f Finding) key() string {
	return strings.Join([]string{
		f.Rule, f.Location.Table, f.Location.Column, fmt.Sprint(f.Location.Line),
	}, "\x00")
}

// Set accumulates findings and keeps them ordered and unique.
type Set struct {
	items []Finding
	seen  map[string]int
}

// NewSet returns an empty set.
func NewSet() *Set {
	return &Set{seen: make(map[string]int)}
}

// Add records a finding, keeping the more severe of any duplicate pair.
func (s *Set) Add(f Finding) {
	if s.seen == nil {
		s.seen = make(map[string]int)
	}
	k := f.key()
	if i, dup := s.seen[k]; dup {
		if f.Severity > s.items[i].Severity {
			s.items[i] = f
		}
		return
	}
	s.seen[k] = len(s.items)
	s.items = append(s.items, f)
}

// AddAll records several findings.
func (s *Set) AddAll(fs []Finding) {
	for _, f := range fs {
		s.Add(f)
	}
}

// All returns the findings, most severe first, then by location so that two
// runs over the same data produce the same report.
func (s *Set) All() []Finding {
	out := make([]Finding, len(s.items))
	copy(out, s.items)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.Location.Display != b.Location.Display {
			return a.Location.Display < b.Location.Display
		}
		if a.Location.Column != b.Location.Column {
			return a.Location.Column < b.Location.Column
		}
		return a.Rule < b.Rule
	})
	return out
}

// Len is how many findings the set holds.
func (s *Set) Len() int { return len(s.items) }

// Counts totals the findings by severity.
func (s *Set) Counts() map[Severity]int {
	m := make(map[Severity]int, 3)
	for _, f := range s.items {
		m[f.Severity]++
	}
	return m
}

// Max returns the highest severity present, and whether there were any
// findings at all.
func (s *Set) Max() (Severity, bool) {
	if len(s.items) == 0 {
		return Info, false
	}
	worst := Info
	for _, f := range s.items {
		if f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst, true
}

// Verify re-runs every finding's count query and marks those that reproduce.
//
// Findings that no longer reproduce are removed. This is what lets Veritix
// accept observations from a language model without accepting its arithmetic:
// the model chooses what to look at, but a number only reaches the report if
// the engine produces it.
func (s *Set) Verify(ctx context.Context, e *engine.Engine) (dropped []Finding, err error) {
	kept := make([]Finding, 0, len(s.items))

	for _, f := range s.items {
		if f.Evidence.CountQuery == "" {
			// Nothing to check against. Structural observations made while
			// reading a file have no SQL behind them by nature.
			kept = append(kept, f)
			continue
		}

		var got int64
		if err := e.ScanOne(ctx, f.Evidence.CountQuery, []any{&got}); err != nil {
			f.Verified = false
			dropped = append(dropped, f)
			continue
		}
		if got != f.Count {
			// The claim and the engine disagree. Trust the engine.
			f.Count = got
			if got == 0 {
				dropped = append(dropped, f)
				continue
			}
		}
		f.Verified = true
		kept = append(kept, f)
	}

	s.items = kept
	s.reindex()
	return dropped, nil
}

func (s *Set) reindex() {
	s.seen = make(map[string]int, len(s.items))
	for i, f := range s.items {
		s.seen[f.key()] = i
	}
}
