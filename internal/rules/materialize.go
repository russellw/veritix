package rules

import (
	"context"
	"fmt"
	"strings"

	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/profile"
)

// The permitted set is bounded so that what a person is asked to accept is
// something they can actually read. A vocabulary of four statuses is an
// expectation; four thousand distinct values is a copy of the column, and
// accepting it would protect nothing while looking like protection. The same
// argument bounds one value's length: a rule enumerates a vocabulary, not free
// text.
const (
	// MaxMaterializedValues is the largest permitted set values_from will fill in.
	MaxMaterializedValues   = 50
	maxMaterializedValueLen = 120
)

// Materialize resolves every rule that reads its values from the data, in
// place.
//
// It exists because expect: one_of is the most valuable rule kind and the one
// a model cannot write. Its body is literally a list of cell values, and the
// egress guard never shows a model one — under the default policy even
// sample_values comes back as shapes. The answer is not to relax the guard for
// rule proposal. The model proposes the shape of the expectation ("status is
// drawn from a fixed vocabulary") and the engine fills in the contents here,
// in the customer's own process, from the customer's own data. The concrete
// list is what a person reviews before the rule is accepted.
//
// The values this writes are cell values. They belong in the rules file and on
// the accept screen, and they must never travel back to the model.
//
// A resolved rule is an ordinary one_of rule and says so: values_from is
// cleared, which also makes materializing twice a no-op rather than a second
// reading of data that may have changed underneath.
func Materialize(ctx context.Context, e *engine.Engine, ds *profile.Dataset, f *File) error {
	if f == nil {
		return nil
	}
	for i := range f.Rules {
		r := &f.Rules[i]
		if r.ValuesFrom == "" {
			continue
		}
		if r.ValuesFrom != ValuesFromCurrent {
			return fmt.Errorf("rules: rule %q reads its values from %q; the only source is %q",
				r.ID, r.ValuesFrom, ValuesFromCurrent)
		}
		values, err := currentValues(ctx, e, ds, r)
		if err != nil {
			return err
		}
		r.Values = values
		r.ValuesFrom = ""
	}
	return nil
}

// currentValues reads the distinct values a rule's target holds today.
func currentValues(ctx context.Context, e *engine.Engine, ds *profile.Dataset, r *Rule) ([]string, error) {
	// A rule targets columns by glob and may reach more than one, so the
	// candidates are unioned and reduced once. Reducing them in SQL rather
	// than in Go is what makes ignore_case mean the same thing here as it
	// does when the rule is evaluated: two spellings the engine considers
	// equal must not both end up in the list.
	var parts []string
	for _, t := range ds.Tables {
		if !matchGlob(r.Table, t.Display) && !matchGlob(r.Table, t.Name) {
			continue
		}
		for _, c := range t.Columns {
			if !matchGlob(r.Column, c.Name) && !matchGlob(r.Column, c.Original) {
				continue
			}
			col := engine.Ident(c.Name)
			parts = append(parts, fmt.Sprintf("SELECT %s AS v FROM %s WHERE %s",
				col, engine.Ident(t.Name), profile.SQLNonBlank(col)))
		}
	}
	if len(parts) == 0 {
		// The rule would be reported as rule.never_applied if it were
		// evaluated, but a proposal is refused before it gets that far:
		// there is nothing to fill the list in from.
		return nil, fmt.Errorf("rules: rule %q reads its values from %s, which selects no column in this dataset",
			r.ID, ruleTarget(r))
	}

	// Blanks are excluded above: one_of never fires on an absent value, so a
	// blank in the list would permit nothing that is not already permitted.
	source := "(" + strings.Join(parts, " UNION ALL ") + ")"
	key := "v"
	if r.IgnoreCase {
		key = "lower(trim(v))"
	}

	query := fmt.Sprintf("SELECT min(v) FROM %s GROUP BY %s ORDER BY 1 LIMIT %d",
		source, key, MaxMaterializedValues+1)
	rs, err := e.Collect(ctx, query, MaxMaterializedValues+1)
	if err != nil {
		return nil, fmt.Errorf("rules: rule %q: %w", r.ID, err)
	}

	if len(rs.Rows) > MaxMaterializedValues {
		var distinct int64
		countQ := fmt.Sprintf("SELECT count(DISTINCT %s) FROM %s", key, source)
		if err := e.ScanOne(ctx, countQ, []any{&distinct}); err != nil {
			return nil, fmt.Errorf("rules: rule %q: %w", r.ID, err)
		}
		return nil, fmt.Errorf(
			"rules: rule %q reads its values from %s, which holds %d distinct values; "+
				"a permitted set is a vocabulary somebody can review, so it stops at %d",
			r.ID, ruleTarget(r), distinct, MaxMaterializedValues)
	}

	values := make([]string, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		v, ok := cellText(row[0])
		if !ok {
			continue
		}
		if len(v) > maxMaterializedValueLen {
			return nil, fmt.Errorf(
				"rules: rule %q reads its values from %s, which holds values of %d characters; "+
					"a permitted set enumerates a vocabulary, not free text",
				r.ID, ruleTarget(r), len(v))
		}
		values = append(values, v)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("rules: rule %q reads its values from %s, which holds no value at all",
			r.ID, ruleTarget(r))
	}
	return values, nil
}

// cellText reads one VARCHAR cell. Every column is loaded as text, but the
// driver is free to hand back either spelling of it.
func cellText(cell any) (string, bool) {
	switch v := cell.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		return "", false
	}
}
