package rules

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/profile"
)

// Evaluate applies every rule to the dataset.
//
// A rule that matches no table or column is itself reported. Silence from a
// rule is ambiguous — it means either "your data is fine" or "this rule never
// ran" — and the second is dangerous, because the customer is relying on it.
func Evaluate(ctx context.Context, e *engine.Engine, ds *profile.Dataset, f *File, log *slog.Logger) ([]finding.Finding, error) {
	if f == nil || len(f.Rules) == 0 {
		return nil, nil
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	var out []finding.Finding

	for i := range f.Rules {
		r := &f.Rules[i]
		applied := 0

		for _, t := range ds.Tables {
			if !matchGlob(r.Table, t.Display) && !matchGlob(r.Table, t.Name) {
				continue
			}

			if r.Expect == ExpectSQL {
				applied++
				found, err := evaluateOne(ctx, e, r, t, nil)
				if err != nil {
					return nil, err
				}
				out = append(out, found...)
				continue
			}

			for _, c := range t.Columns {
				if !matchGlob(r.Column, c.Name) && !matchGlob(r.Column, c.Original) {
					continue
				}
				applied++
				found, err := evaluateOne(ctx, e, r, t, c)
				if err != nil {
					return nil, err
				}
				out = append(out, found...)
			}
		}

		if applied == 0 {
			out = append(out, finding.Finding{
				Rule:     "rule.never_applied",
				Severity: finding.Warning,
				Origin:   finding.OriginRule,
				Title:    fmt.Sprintf("rule %q matched nothing in this dataset", r.ID),
				Detail: fmt.Sprintf(
					"The rule targets table %q, column %q, but no table or column of that "+
						"name is present. The rule did not run, so it protected nothing. "+
						"This is usually a rename upstream or a typo in the rule file.",
					r.Table, r.Column),
				Remedy:   "Correct the rule's target, or remove the rule if the data it guarded is gone.",
				Location: finding.Location{Display: ruleTarget(r)},
			})
			continue
		}
		log.Debug("applied rule", "id", r.ID, "targets", applied)
	}
	return out, nil
}

// evaluateOne runs a rule against one table, or one column of one table.
func evaluateOne(ctx context.Context, e *engine.Engine, r *Rule, t *profile.Table, c *profile.Column) ([]finding.Finding, error) {
	tbl := engine.Ident(t.Name)

	var colq string
	if c != nil {
		colq = engine.Ident(c.Name)
	}

	predicate, expected, err := violationPredicate(r, tbl, colq)
	if err != nil {
		return nil, err
	}

	// Missing values are exempted where asked, so that "one of these four
	// values" does not silently also mean "and never absent". A placeholder
	// such as "N/A" counts as missing here: a user who writes allow_missing
	// means "where there is no value", not "where the cell is empty but not
	// where somebody typed N/A into it".
	if c != nil && r.AllowMissing {
		predicate = fmt.Sprintf("(%s) AND NOT (%s) AND (%s)",
			profile.SQLNonBlank(colq), profile.SQLIsSentinel(colq), predicate)
	}

	countQ := fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", tbl, predicate)

	var violations int64
	if err := e.ScanOne(ctx, countQ, []any{&violations}); err != nil {
		// A rule that cannot be executed is a defect in the rule, and the
		// customer needs to know rather than have it pass quietly.
		return []finding.Finding{{
			Rule:     "rule.invalid",
			Severity: finding.Warning,
			Origin:   finding.OriginRule,
			Title:    fmt.Sprintf("rule %q could not be evaluated", r.ID),
			Detail:   fmt.Sprintf("The rule produced SQL the engine rejected: %v", err),
			Remedy:   "Correct the rule definition.",
			Location: location(t, c),
		}}, nil
	}

	if violations == 0 {
		return nil, nil
	}

	// An omitted severity means error: the customer wrote the rule because
	// breaking it matters.
	severity := finding.Error
	if r.Severity != nil {
		severity = *r.Severity
	}

	title := r.Message
	if title == "" {
		title = fmt.Sprintf("%d row(s) break rule %q", violations, r.ID)
	}
	detail := r.Description
	if detail == "" {
		detail = fmt.Sprintf("The rule requires %s.", expected)
	} else {
		if !strings.HasSuffix(detail, ".") {
			detail += "."
		}
		detail = fmt.Sprintf("%s The rule requires %s.", detail, expected)
	}
	remedy := r.Remedy
	if remedy == "" {
		remedy = "Correct the offending rows, or amend the rule if the expectation has changed."
	}

	return []finding.Finding{{
		Rule:     "rule." + r.ID,
		Severity: severity,
		Origin:   finding.OriginRule,
		Title:    title,
		Detail:   detail,
		Remedy:   remedy,
		Location: location(t, c),
		Count:    violations,
		Total:    t.RowCount,
		Evidence: finding.Evidence{
			CountQuery: countQ,
			RowQuery:   fmt.Sprintf("SELECT * FROM %s WHERE %s LIMIT 100", tbl, predicate),
			Expected:   expected,
			Observed:   fmt.Sprintf("%d row(s) that do not", violations),
		},
	}}, nil
}

// ruleTarget describes what a rule was aimed at, for a rule that matched
// nothing and therefore has no real location to report.
func ruleTarget(r *Rule) string {
	if r.Column == "" {
		return r.Table
	}
	return r.Table + "." + r.Column
}

func location(t *profile.Table, c *profile.Column) finding.Location {
	loc := finding.Location{Table: t.Name, Display: t.Display}
	if c != nil {
		loc.Column = c.Name
	}
	if t.Ingest != nil {
		loc.File = t.Ingest.Ref.File.Rel
	}
	return loc
}

// violationPredicate builds the SQL that selects rows breaking a rule, and a
// plain-English statement of what was required.
//
// It selects violations rather than compliance deliberately: a NULL in SQL
// makes a positive assertion neither true nor false, so "NOT (amount > 0)"
// and "amount <= 0" disagree on empty cells. Writing the violation directly
// keeps that decision explicit instead of accidental.
func violationPredicate(r *Rule, tbl, col string) (predicate, expected string, err error) {
	switch r.Expect {
	case ExpectNotNull:
		return fmt.Sprintf("NOT %s", profile.SQLNonBlank(col)),
			"a value in every row", nil

	case ExpectUnique:
		return fmt.Sprintf(
			"%[1]s AND %[2]s IN (SELECT %[2]s FROM %[3]s WHERE %[1]s GROUP BY 1 HAVING count(*) > 1)",
			profile.SQLNonBlank(col), col, tbl), "every value to be distinct", nil

	case ExpectPositive:
		return fmt.Sprintf("%s AND coalesce(TRY_CAST(%s AS DOUBLE), 0) <= 0",
				profile.SQLNonBlank(col), col),
			"values greater than zero", nil

	case ExpectNonNegative:
		return fmt.Sprintf("%s AND coalesce(TRY_CAST(%s AS DOUBLE), 0) < 0",
				profile.SQLNonBlank(col), col),
			"values of zero or more", nil

	case ExpectOneOf:
		lhs, values := comparableList(r, col)
		return fmt.Sprintf("%s AND %s NOT IN (%s)",
				profile.SQLNonBlank(col), lhs, strings.Join(values, ", ")),
			fmt.Sprintf("one of: %s", strings.Join(r.Values, ", ")), nil

	case ExpectMatches:
		return fmt.Sprintf("%s AND NOT regexp_full_match(trim(%s), %s)",
				profile.SQLNonBlank(col), col, engine.Literal(r.Pattern)),
			fmt.Sprintf("values matching %s", r.Pattern), nil

	case ExpectRange:
		var conds []string
		var desc []string
		if r.Min != nil {
			conds = append(conds, fmt.Sprintf("TRY_CAST(%s AS DOUBLE) < %v", col, *r.Min))
			desc = append(desc, fmt.Sprintf("at least %v", *r.Min))
		}
		if r.Max != nil {
			conds = append(conds, fmt.Sprintf("TRY_CAST(%s AS DOUBLE) > %v", col, *r.Max))
			desc = append(desc, fmt.Sprintf("at most %v", *r.Max))
		}
		return fmt.Sprintf("%s AND (%s)", profile.SQLNonBlank(col), strings.Join(conds, " OR ")),
			"values " + strings.Join(desc, " and "), nil

	case ExpectNotFuture:
		return fmt.Sprintf("%s AND TRY_CAST(%s AS TIMESTAMP) > now()",
				profile.SQLNonBlank(col), col),
			"dates no later than today", nil

	case ExpectReferences:
		table, column, ok := strings.Cut(r.References, ".")
		if !ok {
			return "", "", fmt.Errorf("rule %q: references must be written as table.column", r.ID)
		}
		lhs, rhs := col, engine.Ident(column)
		if r.IgnoreCase {
			lhs = fmt.Sprintf("lower(trim(%s))", col)
			rhs = fmt.Sprintf("lower(trim(%s))", rhs)
		}
		return fmt.Sprintf("%s AND %s NOT IN (SELECT %s FROM %s)",
				profile.SQLNonBlank(col), lhs, rhs, engine.Ident(table)),
			fmt.Sprintf("every value to exist in %s", r.References), nil

	case ExpectSQL:
		// The clause is written by whoever owns the rules file, which is the
		// same person who can already read the data. It is bounded by the
		// engine's query timeout and result caps like any other statement.
		return "(" + r.Where + ")",
			fmt.Sprintf("no rows matching: %s", r.Where), nil

	default:
		return "", "", fmt.Errorf("rule %q: unknown expectation %q", r.ID, r.Expect)
	}
}

// comparableList renders the left-hand side and the permitted values for
// one_of, honouring ignore_case.
func comparableList(r *Rule, col string) (lhs string, values []string) {
	lhs = col
	values = make([]string, len(r.Values))

	if r.IgnoreCase {
		lhs = fmt.Sprintf("lower(trim(%s))", col)
		for i, v := range r.Values {
			values[i] = engine.Literal(strings.ToLower(strings.TrimSpace(v)))
		}
		return lhs, values
	}
	for i, v := range r.Values {
		values[i] = engine.Literal(v)
	}
	return lhs, values
}
