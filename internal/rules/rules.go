// Package rules lets a customer state what their data is supposed to look
// like, in a file they own.
//
// The built-in checks find problems that are wrong in any dataset. A rule
// expresses something only the customer knows: that an order amount is never
// negative, that status is one of four values, that every invoice must
// reference a known account. Those expectations are the ones that catch the
// defects that actually cost money, and they cannot be inferred from the data
// — a column full of wrong-but-plausible values looks exactly like a column
// full of right ones.
//
// Rules are also the destination for the agent's proposals: the model suggests
// a rule, a human reads it, and once accepted it becomes a deterministic check
// that runs on every future audit without the model. That is the only way a
// defect the model found on one run gets found on every run — see Materialize
// for the part the model cannot write itself.
package rules

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/russellw/veritix/internal/finding"
)

// File is a rules document.
type File struct {
	// Version is the document format version. Only 1 exists.
	Version int `yaml:"version"`
	// Rules are the expectations to enforce.
	Rules []Rule `yaml:"rules"`
}

// Expectation is what a rule asserts about the data.
type Expectation string

const (
	// ExpectNotNull requires every row to have a value.
	ExpectNotNull Expectation = "not_null"
	// ExpectUnique requires no value to repeat.
	ExpectUnique Expectation = "unique"
	// ExpectPositive requires values greater than zero.
	ExpectPositive Expectation = "positive"
	// ExpectNonNegative requires values of zero or more.
	ExpectNonNegative Expectation = "non_negative"
	// ExpectOneOf restricts a column to a fixed set of values.
	ExpectOneOf Expectation = "one_of"
	// ExpectMatches requires values to match a regular expression.
	ExpectMatches Expectation = "matches"
	// ExpectRange bounds a numeric column.
	ExpectRange Expectation = "range"
	// ExpectNotFuture forbids dates after the audit runs.
	ExpectNotFuture Expectation = "not_future"
	// ExpectReferences requires every value to exist in another column.
	ExpectReferences Expectation = "references"
	// ExpectSQL treats rows matching a WHERE clause as violations.
	ExpectSQL Expectation = "sql"
)

// Expectations lists every assertion a rule can make, in the order a caller
// offering them as a choice should offer them.
func Expectations() []Expectation {
	return []Expectation{
		ExpectNotNull, ExpectUnique, ExpectPositive, ExpectNonNegative,
		ExpectOneOf, ExpectMatches, ExpectRange, ExpectNotFuture,
		ExpectReferences, ExpectSQL,
	}
}

// ParseExpectation reads an expectation by name.
func ParseExpectation(s string) (Expectation, error) {
	for _, e := range Expectations() {
		if string(e) == strings.TrimSpace(strings.ToLower(s)) {
			return e, nil
		}
	}
	return "", fmt.Errorf("no expectation called %q", s)
}

// ValuesSource names where a one_of rule's permitted values come from when the
// rule does not list them itself.
type ValuesSource string

// ValuesFromCurrent fills the permitted set with the distinct values the
// column holds when the rule is materialized.
const ValuesFromCurrent ValuesSource = "current"

// Rule is one expectation about the data.
type Rule struct {
	// ID names the rule in reports. Required and unique within a file.
	ID string `yaml:"id"`
	// Description explains what the rule is for, in the report.
	Description string `yaml:"description,omitempty"`
	// Severity defaults to error when omitted: a rule the customer wrote
	// states an expectation they hold, not a suggestion. It is a pointer so
	// that an omitted severity is distinguishable from an explicit "info".
	Severity *finding.Severity `yaml:"severity,omitempty"`

	// Table selects tables by name or display path. A "*" matches any run of
	// characters, so "*.csv" applies a rule to every CSV in the dataset.
	Table string `yaml:"table"`
	// Column selects a column, with the same globbing.
	Column string `yaml:"column,omitempty"`

	// Expect is the assertion.
	Expect Expectation `yaml:"expect"`

	// Values enumerates the permitted values for one_of.
	Values []string `yaml:"values,omitempty"`
	// ValuesFrom asks Veritix to fill Values in from the data instead of
	// listing them, which is how a rule proposed by a model gets a value
	// list: the model is never shown a cell value, so it can propose the
	// shape of the expectation but not its contents. Materialize resolves
	// it; an accepted rule carries the concrete list.
	ValuesFrom ValuesSource `yaml:"values_from,omitempty"`
	// Pattern is the regular expression for matches.
	Pattern string `yaml:"pattern,omitempty"`
	// Min and Max bound a range. Either may be omitted.
	Min *float64 `yaml:"min,omitempty"`
	Max *float64 `yaml:"max,omitempty"`
	// References names the table and column that must contain every value,
	// written as "table.column".
	References string `yaml:"references,omitempty"`
	// Where is the violation predicate for expect: sql.
	Where string `yaml:"where,omitempty"`

	// IgnoreCase compares text without regard to case or surrounding spaces.
	// Usually what a human means, and rarely what SQL does by default.
	IgnoreCase bool `yaml:"ignore_case,omitempty"`
	// AllowMissing exempts null and blank values from the rule, so that
	// "must be one of these four values" does not also mean "and must be
	// present". Completeness is a separate expectation.
	AllowMissing bool `yaml:"allow_missing,omitempty"`

	// Message overrides the generated finding title.
	Message string `yaml:"message,omitempty"`
	// Remedy overrides the generated advice.
	Remedy string `yaml:"remedy,omitempty"`
}

// Load reads a rules file.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's choice
	if err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}

	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("rules: parsing %s: %w", filepath.Base(path), err)
	}
	if f.Version == 0 {
		f.Version = 1
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("rules: %s declares version %d; this build understands version 1",
			filepath.Base(path), f.Version)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("rules: %s: %w", filepath.Base(path), err)
	}
	// values_from is an instruction to Veritix, not an expectation about the
	// data, and it is carried out once — when a proposed rule is accepted.
	// Left in a file on disk it would mean "whatever is there today is
	// permitted", which is a rule that cannot fire: the same disease as a
	// rule that matches nothing, wearing a different coat.
	for i := range f.Rules {
		if f.Rules[i].ValuesFrom != "" {
			return nil, fmt.Errorf(
				"rules: %s: rule %q reads its values from the data, which is resolved when a "+
					"proposed rule is accepted; an accepted rule lists the values it permits",
				filepath.Base(path), f.Rules[i].ID)
		}
	}
	return &f, nil
}

// Validate reports rules that cannot be applied.
//
// It is strict on purpose. A rule with a typo in its column name would
// otherwise match nothing and pass silently, and a rule that silently never
// runs is worse than no rule at all: the customer believes it is protecting
// them.
func (f *File) Validate() error {
	seen := make(map[string]bool, len(f.Rules))

	for i := range f.Rules {
		r := &f.Rules[i]
		where := fmt.Sprintf("rule %d", i+1)
		if r.ID != "" {
			where = fmt.Sprintf("rule %q", r.ID)
		}

		if r.ID == "" {
			return fmt.Errorf("%s has no id", where)
		}
		if seen[r.ID] {
			return fmt.Errorf("%s is defined more than once", where)
		}
		seen[r.ID] = true

		if r.Table == "" {
			return fmt.Errorf("%s does not say which table it applies to", where)
		}
		if r.Expect == "" {
			return fmt.Errorf("%s does not say what to expect", where)
		}

		// Every message from here on has to say what to change, not only what
		// is wrong. A person is reading their own file and can see the shape
		// of it; a model is handed this back as a tool error with nothing else
		// to go on, and gpt-oss-120b spent two consecutive steps re-sending an
		// identical one_of proposal because "names none" told it there was a
		// problem and not what the fix was.
		needsColumn := r.Expect != ExpectSQL
		if needsColumn && r.Column == "" {
			return fmt.Errorf("%s expects %s, which applies to a column: set column to the "+
				"column it constrains, or use expect: sql with a where clause for a rule "+
				"about a whole row", where, r.Expect)
		}

		if r.ValuesFrom != "" {
			if r.ValuesFrom != ValuesFromCurrent {
				return fmt.Errorf("%s reads its values from %q; the only source is %q",
					where, r.ValuesFrom, ValuesFromCurrent)
			}
			if r.Expect != ExpectOneOf {
				return fmt.Errorf("%s sets values_from, which fills in the list for one_of, but expects %s",
					where, r.Expect)
			}
		}

		switch r.Expect {
		case ExpectNotNull, ExpectUnique, ExpectPositive, ExpectNonNegative, ExpectNotFuture:
			// No further configuration.
		case ExpectOneOf:
			switch {
			case len(r.Values) > 0 && r.ValuesFrom != "":
				return fmt.Errorf("%s both lists values and reads them from the data; it can do one or the other",
					where)
			case len(r.Values) == 0 && r.ValuesFrom == "":
				return fmt.Errorf("%s expects one_of but lists no values", where)
			}
		case ExpectMatches:
			if r.Pattern == "" {
				return fmt.Errorf("%s expects matches but gives no pattern", where)
			}
		case ExpectRange:
			if r.Min == nil && r.Max == nil {
				return fmt.Errorf("%s expects range but sets neither min nor max", where)
			}
			if r.Min != nil && r.Max != nil && *r.Min > *r.Max {
				return fmt.Errorf("%s has min greater than max", where)
			}
		case ExpectReferences:
			if !strings.Contains(r.References, ".") {
				return fmt.Errorf("%s expects references but %q is not written as table.column",
					where, r.References)
			}
		case ExpectSQL:
			if r.Where == "" {
				return fmt.Errorf("%s expects sql but gives no where clause", where)
			}
		default:
			return fmt.Errorf("%s uses unknown expectation %q", where, r.Expect)
		}
	}
	return nil
}

// matchGlob reports whether a name matches a pattern, where "*" stands for any
// run of characters. Comparison ignores case, because a rule file is written
// by hand and nobody should have to remember whether a sheet was called Q1
// or q1.
func matchGlob(pattern, name string) bool {
	pattern = strings.ToLower(pattern)
	name = strings.ToLower(name)
	if pattern == name {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}

	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		switch {
		case i == 0:
			if !strings.HasPrefix(name, part) {
				return false
			}
			pos = len(part)
		case i == len(parts)-1:
			if !strings.HasSuffix(name[pos:], part) {
				return false
			}
		default:
			idx := strings.Index(name[pos:], part)
			if idx < 0 {
				return false
			}
			pos += idx + len(part)
		}
	}
	return true
}
