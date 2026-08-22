// Package checks turns a profile into findings.
//
// Each check answers one question about the data and, when the answer is bad,
// produces a finding with the SQL that demonstrates it. The findings are the
// product; the profile is only the evidence they are drawn from.
//
// The wording of a finding matters as much as its detection. "signup_date has
// 2 date formats" is a fact; "some of these dates will be read as the wrong
// day and nothing will error" is what makes somebody act on it.
package checks

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/profile"
)

// columnCheck examines one column.
type columnCheck func(ctx *tableContext, c *profile.Column) []finding.Finding

// tableContext is what a column check needs to know about its surroundings.
type tableContext struct {
	table *profile.Table
	// quoted is the table's quoted SQL identifier, precomputed because every
	// evidence query needs it.
	quoted string
}

func (tc *tableContext) location(c *profile.Column) finding.Location {
	loc := finding.Location{
		Table:   tc.table.Name,
		Display: tc.table.Display,
	}
	if c != nil {
		loc.Column = c.Name
	}
	if tc.table.Ingest != nil {
		loc.File = tc.table.Ingest.Ref.File.Rel
	}
	return loc
}

// col returns the column's quoted identifier.
func col(c *profile.Column) string { return engine.Ident(c.Name) }

// countWhere builds an evidence query counting rows matching a predicate.
func (tc *tableContext) countWhere(predicate string) string {
	return fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", tc.quoted, predicate)
}

// rowsWhere builds the companion query that retrieves the offending rows.
func (tc *tableContext) rowsWhere(predicate string) string {
	return fmt.Sprintf("SELECT * FROM %s WHERE %s LIMIT 100", tc.quoted, predicate)
}

// columnChecks is the full set, run against every column.
var columnChecks = []columnCheck{
	checkEmptyColumn,
	checkMissingValues,
	checkTypeViolations,
	checkNearMissType,
	checkMixedDateFormats,
	checkAmbiguousDates,
	checkImplausibleDates,
	checkFutureDates,
	checkWhitespacePadding,
	checkCaseVariants,
	checkNumericOutliers,
	checkUnexpectedNegatives,
	checkConstantColumn,
	checkRenamedHeader,
}

// checkEmptyColumn reports a column with nothing in it.
func checkEmptyColumn(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Total == 0 || c.Populated() > 0 {
		return nil
	}
	pred := profile.SQLNonBlank(col(c))
	return []finding.Finding{{
		Rule:     "column.empty",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s is entirely empty across all %d rows", c.Name, c.Total),
		Detail: "The column exists in the schema but holds no data at all. Either it was " +
			"never populated by the system that produced this export, or the export " +
			"dropped it. Anything joining or filtering on this column will silently " +
			"match nothing.",
		Remedy:   "Confirm with the source system whether this column should carry data.",
		Location: tc.location(c),
		Count:    c.Total,
		Total:    c.Total,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere("NOT " + pred),
			Expected:   "at least some populated values",
			Observed:   "every value is null, blank, or a placeholder",
		},
	}}
}

// wrongSideOfZero reports whether a magic number is negative in a column where
// nothing real is.
//
// This is the other half of how a person recognizes a placeholder, and it
// catches what frequency does not: -999 among credit limits of 1000 and up is
// obvious on sight however rarely it occurs, and a column with one popular
// legitimate value — a default, a round number — hides it from the frequency
// test entirely.
//
// Sign is the test rather than distance because sign is a boundary business
// data respects: a credit limit, a quantity, a weight, a price is not
// negative, so a negative one is announcing itself. Distance is not available
// to a rule that has to hold at any size — "just past the maximum" is where
// the largest value of a uniform column lives, and 999999 at the top of a
// column of two hundred thousand random numbers is data, not a placeholder.
// The magic numbers that are large and positive are left to standsOut and to
// column.numeric_outliers, which is what a value far above the data trips.
//
// A column that already holds real negatives fails the test, and should: -999
// among credits and debits is not obviously anything.
func wrongSideOfZero(c *profile.Column, s profile.ValueCount) bool {
	n := c.Numeric
	if n == nil || n.Count == 0 || n.MinReal < 0 {
		return false
	}
	v, err := strconv.ParseFloat(s.Value, 64)
	if err != nil {
		return false // not a number after all, so not this test's business
	}
	return v < 0
}

// standsOut reports whether a magic number is more repeated than any real
// value in the column, which is the only thing that distinguishes -999 the
// placeholder from -999 the measurement.
//
// A text placeholder needs no such test: "n/a" is never a quantity. A number
// is, sometimes, and a rule that reported every column containing a 999 would
// report every column of two million numbers. What a person actually reads is
// the frequency: -999 occurring five hundred times in a column where no real
// value occurs more than sixty is not a coincidence, and that holds at any
// size, which a share of the column does not.
//
// TopValues is ordered by frequency and includes the sentinel itself, so a
// sentinel that is not in it is by construction beaten by everything that is.
func standsOut(c *profile.Column, s profile.ValueCount) bool {
	for _, v := range c.TopValues {
		if profile.IsNumericSentinel(strings.ToLower(strings.TrimSpace(v.Value))) {
			continue
		}
		if v.Count >= s.Count {
			// Not more repeated than a real value, and a column of unique
			// values is the case that matters: every order_id occurs once,
			// including the one that happens to be 999999.
			return false
		}
	}
	return true
}

// checkUnprofiled reports a column whose measurements did not run.
//
// This is not a defect in the data; it is the audit declining to make a claim,
// and it has to appear in the report for the same reason rule.never_applied
// does. Silence means either "this column is fine" or "nothing looked at it",
// and the second is dangerous precisely where it happens: on the largest table
// in the dataset, where a per-query timeout runs out and where nobody is going
// to notice by eye that a column has no findings.
//
// It carries no CountQuery. There is no number to reproduce — the measurement
// is what failed — and Set.Verify keeps a finding with no evidence for exactly
// that case.
func checkUnprofiled(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Unprofiled == "" {
		return nil
	}

	detail := "Nothing in this report says anything about this column: the checks that " +
		"would have found placeholders, type violations, duplicates or broken " +
		"references never ran, so their silence is not evidence that it is clean."
	remedy := "Re-run the audit. If it happens again, the reason is on the audit's own " +
		"log, at warning level."
	if c.Unprofiled == profile.UnprofiledTimeout {
		detail += " The measurement ran out of time, which on a table this size " +
			"usually means the per-query timeout is set below what the column costs."
		remedy = "Raise engine.query_timeout (VERITIX_ENGINE_QUERY_TIMEOUT) and re-run. " +
			"A column of twenty million values takes minutes to measure, not seconds."
	}

	return []finding.Finding{{
		Rule:     "column.not_profiled",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s was not measured, so nothing here is a claim about it", c.Name),
		Detail:   detail,
		Remedy:   remedy,
		Location: tc.location(c),
		Total:    c.Total,
	}}
}

// checkMissingValues reports a column with a substantial share of missing
// values, counting placeholders as missing rather than as data.
func checkMissingValues(tc *tableContext, c *profile.Column) []finding.Finding {
	// The placeholders that count are the ones this finding will be held to.
	// Every text placeholder counts; a magic number counts only where it
	// stands out from the column around it. What is counted here is what the
	// evidence query below matches, so the number in the title is the number
	// the engine produces when Set.Verify re-runs it — profile.Column.Missing
	// counts every sentinel, and a finding built on that figure and
	// demonstrated by a query matching a subset of it is corrected to a
	// number its own title contradicts.
	var counted []string
	var textTotal, numericTotal int64
	for _, sv := range c.Sentinels {
		switch {
		case !profile.IsNumericSentinel(sv.Value):
			counted = append(counted, sv.Value)
			textTotal += sv.Count
		case standsOut(c, sv) || wrongSideOfZero(c, sv):
			counted = append(counted, sv.Value)
			numericTotal += sv.Count
		}
	}
	placeholders := textTotal + numericTotal

	missing := c.Nulls + c.Blanks + placeholders
	if c.Total == 0 || missing == 0 || missing == c.Total {
		return nil // wholly empty is handled by checkEmptyColumn
	}
	share := float64(missing) / float64(c.Total)

	severity := finding.Info
	switch {
	case share >= 0.5:
		severity = finding.Warning
	case share < 0.05 && placeholders == 0:
		// A few gaps in a large column is not news. A placeholder is,
		// however few there are: "n/a" is not a rate, it is a value that
		// defeats every null check downstream, and thirteen of them in two
		// million rows do it exactly as thoroughly as two in nine. Anything
		// proportional here goes blind on the datasets the product exists to
		// audit — which is where nobody is going to notice by eye.
		return nil
	}

	// The placeholder half of the count is the part people miss, so lead with
	// it when there is one.
	var sentinelNote string
	if placeholders > 0 {
		names := make([]string, 0, len(counted))
		for _, v := range counted {
			names = append(names, fmt.Sprintf("%q", v))
		}
		sentinelNote = fmt.Sprintf(
			" Of those, %d are placeholders (%s) rather than nulls, so a check for "+
				"null values would report this column as complete.",
			placeholders, strings.Join(names, ", "))
	}
	if numericTotal > 0 {
		sentinelNote += fmt.Sprintf(
			" %d of the placeholders are numbers, which survive a numeric cast and are "+
				"then averaged, summed and charted as if they were measurements.",
			numericTotal)
	}

	pred := "NOT " + profile.SQLNonBlank(col(c))
	if len(counted) > 0 {
		quoted := make([]string, len(counted))
		for i, v := range counted {
			quoted[i] = engine.Literal(v)
		}
		pred += fmt.Sprintf(" OR lower(trim(%s)) IN (%s)", col(c), strings.Join(quoted, ", "))
	}

	return []finding.Finding{{
		Rule:     "column.missing_values",
		Severity: severity,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%s is missing a value in %d of %d rows (%.1f%%)",
			c.Name, missing, c.Total, share*100),
		Detail: fmt.Sprintf("%d rows have no usable value for this column.%s",
			missing, sentinelNote),
		Remedy: "Decide whether the gaps are legitimate. If they are not, they will " +
			"quietly reduce the denominator of every average and rate computed on this column.",
		Location: tc.location(c),
		Count:    missing,
		Total:    c.Total,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere("(" + pred + ")"),
			RowQuery:   tc.rowsWhere("(" + pred + ")"),
			Expected:   "a value in every row",
			Observed:   fmt.Sprintf("%d rows without one", missing),
		},
	}}
}

// checkTypeViolations reports values that do not fit their column's type and
// are not a recognized way of writing "missing".
func checkTypeViolations(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Inferred.Nonconforming == 0 {
		return nil
	}
	match := profile.SQLMatchesKind(c.Inferred.Kind, col(c))
	if match == "" {
		return nil
	}

	pred := fmt.Sprintf("%s AND NOT %s AND NOT %s",
		profile.SQLNonBlank(col(c)), match, profile.SQLIsSentinel(col(c)))

	return []finding.Finding{{
		Rule:     "column.type_violation",
		Severity: finding.Error,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%s holds %d value(s) that are not %s",
			c.Name, c.Inferred.Nonconforming, describeKind(c.Inferred.Kind)),
		Detail: fmt.Sprintf(
			"%.0f%% of the values in this column are %s, so it is a %s column. The "+
				"remaining %d cannot be read as one, and are not a recognized way of "+
				"writing \"missing\" either. An import that types this column will turn "+
				"each of them into a null without reporting anything.",
			c.Inferred.Conformance*100, describeKind(c.Inferred.Kind),
			describeKind(c.Inferred.Kind), c.Inferred.Nonconforming),
		Remedy:   "Correct the offending values at source, or agree that they mean \"missing\".",
		Location: tc.location(c),
		Count:    c.Inferred.Nonconforming,
		Total:    c.Total,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere(pred),
			RowQuery:   tc.rowsWhere(pred),
			Expected:   fmt.Sprintf("every populated value to be %s", describeKind(c.Inferred.Kind)),
			Observed:   fmt.Sprintf("%d that are not", c.Inferred.Nonconforming),
		},
	}}
}

// checkNearMissType reports a text column that is mostly one type. It is the
// signature of a column whose meaning is numeric or temporal but whose
// contents have been contaminated.
func checkNearMissType(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Inferred.Kind != profile.KindText || c.Inferred.BestConformance < 0.6 {
		return nil
	}
	match := profile.SQLMatchesKind(c.Inferred.BestCandidate, col(c))
	if match == "" {
		return nil
	}

	pred := fmt.Sprintf("%s AND NOT %s AND NOT %s",
		profile.SQLNonBlank(col(c)), match, profile.SQLIsSentinel(col(c)))
	offending := int64(float64(c.Populated()) * (1 - c.Inferred.BestConformance))

	return []finding.Finding{{
		Rule:     "column.mostly_typed",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%s is %.0f%% %s but cannot be treated as one",
			c.Name, c.Inferred.BestConformance*100, describeKind(c.Inferred.BestCandidate)),
		Detail: fmt.Sprintf(
			"Most of this column is %s, which suggests it is meant to be. Enough values "+
				"are not that it has to be handled as text, so it cannot be summed, "+
				"averaged, sorted, or compared as %s anywhere downstream.",
			describeKind(c.Inferred.BestCandidate), describeKind(c.Inferred.BestCandidate)),
		Remedy:   "Clean the minority of values that do not fit, so the column can carry its real type.",
		Location: tc.location(c),
		Count:    offending,
		Total:    c.Total,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere(pred),
			RowQuery:   tc.rowsWhere(pred),
			Expected:   fmt.Sprintf("all values %s", describeKind(c.Inferred.BestCandidate)),
			Observed:   "a minority that are not",
		},
	}}
}

// checkMixedDateFormats reports a column written in more than one date format.
func checkMixedDateFormats(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Temporal == nil || len(c.Temporal.Formats) < 2 {
		return nil
	}

	// A format is a genuine second format when it reads values the leading one
	// cannot. Counting each format over the whole column double-counts —
	// "05/06/2019" is read day-first and month-first alike — and the share of
	// the column a format accounts for does not separate that artifact from a
	// real minority format. It only makes the artifact small, and small is
	// what a real second format looks like in a file with two million rows:
	// two thousand dates written the other way round is 0.1% of the column and
	// every one of them is read as the wrong day.
	var significant []profile.FormatCount
	var leading profile.FormatCount
	for _, f := range c.Temporal.Formats {
		if f.Count > leading.Count {
			leading = f
		}
	}
	for _, f := range c.Temporal.Formats {
		if f.Format == leading.Format || f.Exclusive > 0 {
			significant = append(significant, f)
		}
	}
	if len(significant) < 2 {
		return nil
	}

	descriptions := make([]string, 0, len(significant))
	for _, f := range significant {
		descriptions = append(descriptions, fmt.Sprintf("%s ×%d", f.Example, f.Count))
	}

	// The minority is the count that matters: those are the rows a
	// single-format reader will get wrong or drop. It is the values the
	// leading format cannot read, not the column minus its largest format,
	// because the formats overlap and subtracting one from the total counts
	// the overlap on both sides.
	var minority int64
	for _, f := range significant {
		minority += f.Exclusive
	}

	iso := profile.SQLMatchesKind(profile.KindDate, col(c))
	pred := fmt.Sprintf("%s AND TRY_CAST(%s AS DATE) IS NULL AND %s",
		profile.SQLNonBlank(col(c)), col(c), iso)

	return []finding.Finding{{
		Rule:     "column.mixed_date_formats",
		Severity: finding.Error,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s mixes %d date formats", c.Name, len(significant)),
		Detail: fmt.Sprintf(
			"The dates in this column are written as %s. Any tool that reads the column "+
				"with a single format will either fail on the others or, worse, parse "+
				"them into the wrong dates. Sorting and date filtering on this column "+
				"are unreliable until it is consistent.",
			strings.Join(descriptions, " and ")),
		Remedy:   "Normalize the column to one format, ideally ISO 8601 (YYYY-MM-DD).",
		Location: tc.location(c),
		Count:    minority,
		Total:    c.Temporal.Count,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere(pred),
			RowQuery:   tc.rowsWhere(pred),
			Expected:   "one consistent date format",
			Observed:   strings.Join(descriptions, ", "),
		},
	}}
}

// checkAmbiguousDates reports dates that are valid under both day-first and
// month-first readings while meaning different days.
func checkAmbiguousDates(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Temporal == nil || c.Temporal.Ambiguous == 0 {
		return nil
	}
	pred := profile.SQLNonBlank(col(c)) + " AND " + profile.SQLAmbiguousDate(col(c))

	return []finding.Finding{{
		Rule:     "column.ambiguous_dates",
		Severity: finding.Error,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%s has %d dates that could be read two different ways",
			c.Name, c.Temporal.Ambiguous),
		Detail: "These values parse as valid dates under both DD/MM/YYYY and MM/DD/YYYY, " +
			"and mean a different day under each. Nothing in the value itself says which " +
			"was intended, so whichever convention a reader assumes, some of these dates " +
			"will be wrong — and no error will be raised when they are.",
		Remedy: "Rewrite the column in ISO 8601 (YYYY-MM-DD), which cannot be read two ways. " +
			"Establish which convention the source system used before converting.",
		Location: tc.location(c),
		Count:    c.Temporal.Ambiguous,
		Total:    c.Temporal.Count,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere(pred),
			RowQuery:   tc.rowsWhere(pred),
			Expected:   "dates with an unambiguous reading",
			Observed:   fmt.Sprintf("%d that are ambiguous", c.Temporal.Ambiguous),
		},
	}}
}

// checkImplausibleDates reports dates outside any sane business range.
func checkImplausibleDates(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Temporal == nil || c.Temporal.Implausible == 0 {
		return nil
	}
	return []finding.Finding{{
		Rule:     "column.implausible_dates",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%s has %d date(s) outside a plausible range",
			c.Name, c.Temporal.Implausible),
		Detail: "Dates before 1900 or more than a century ahead are almost always a " +
			"placeholder that escaped, an epoch-zero default, or a parsing accident. " +
			"They drag date ranges and any minimum or maximum computed on this column.",
		Remedy:   "Treat these as missing rather than as real dates.",
		Location: tc.location(c),
		Count:    c.Temporal.Implausible,
		Total:    c.Temporal.Count,
		Evidence: finding.Evidence{
			Expected: "dates within a plausible business range",
			Observed: fmt.Sprintf("%d outside it (earliest %s)", c.Temporal.Implausible, c.Temporal.Min),
		},
	}}
}

// checkFutureDates reports dates after the audit ran.
func checkFutureDates(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Temporal == nil || c.Temporal.Future == 0 {
		return nil
	}
	// A column named for something scheduled is expected to hold future dates.
	if looksScheduled(c.Name) {
		return nil
	}
	return []finding.Finding{{
		Rule:     "column.future_dates",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s has %d date(s) in the future", c.Name, c.Temporal.Future),
		Detail: "This column records something that has happened, so a date after today " +
			"suggests a typo in the year, a timezone error, or a test record.",
		Remedy:   "Check the offending rows against the source system.",
		Location: tc.location(c),
		Count:    c.Temporal.Future,
		Total:    c.Temporal.Count,
		Evidence: finding.Evidence{
			Expected: "dates at or before today",
			Observed: fmt.Sprintf("%d later than today (latest %s)", c.Temporal.Future, c.Temporal.Max),
		},
	}}
}

// checkWhitespacePadding reports values with leading or trailing spaces.
func checkWhitespacePadding(tc *tableContext, c *profile.Column) []finding.Finding {
	n := c.LeadingWhitespace + c.TrailingWhitespace
	if n == 0 {
		return nil
	}
	pred := fmt.Sprintf("%s <> trim(%s)", col(c), col(c))

	return []finding.Finding{{
		Rule:     "column.whitespace_padding",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s has %d value(s) padded with spaces", c.Name, n),
		Detail: "Surrounding whitespace is invisible on screen but not to a comparison. " +
			`" ACME" and "ACME" are different values to a join, a GROUP BY, and a ` +
			"lookup, so padded rows split into their own group or fail to match at all.",
		Remedy:   "Trim the column at source, or on the way in.",
		Location: tc.location(c),
		Count:    n,
		Total:    c.Total,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere(pred),
			RowQuery:   tc.rowsWhere(pred),
			Expected:   "values with no surrounding whitespace",
			Observed:   fmt.Sprintf("%d padded values", n),
		},
	}}
}

// checkCaseVariants reports values that differ only by case or spacing.
func checkCaseVariants(tc *tableContext, c *profile.Column) []finding.Finding {
	extra := c.Distinct - c.DistinctNormalized
	if extra <= 0 {
		return nil
	}
	// A free-text column will differ in case constantly and meaninglessly;
	// this only matters for columns behaving like a category.
	if c.DistinctNormalized > 100 || float64(c.DistinctNormalized) > float64(c.Populated())*0.5 {
		return nil
	}

	q := fmt.Sprintf(
		"SELECT count(*) FROM (SELECT lower(trim(%[1]s)) AS v FROM %[2]s WHERE %[3]s "+
			"GROUP BY 1 HAVING count(DISTINCT %[1]s) > 1)",
		col(c), tc.quoted, profile.SQLNonBlank(col(c)))

	return []finding.Finding{{
		Rule:     "column.case_variants",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%s writes the same value in %d inconsistent way(s)",
			c.Name, extra),
		Detail: fmt.Sprintf(
			"The column has %d distinct values, but only %d once case and surrounding "+
				"spaces are ignored. Grouping or filtering on this column will split "+
				"what is really one category across several, so counts per category "+
				"will be wrong without anything looking wrong.",
			c.Distinct, c.DistinctNormalized),
		Remedy:   "Normalize the column to a single casing, and constrain it at source if it is a category.",
		Location: tc.location(c),
		Count:    extra,
		Total:    c.Distinct,
		Evidence: finding.Evidence{
			CountQuery: q,
			Expected:   fmt.Sprintf("%d distinct values", c.DistinctNormalized),
			Observed:   fmt.Sprintf("%d, differing only by case or spacing", c.Distinct),
		},
	}}
}

// checkNumericOutliers reports values far from the rest of the distribution.
func checkNumericOutliers(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Numeric == nil || c.Numeric.Outliers == 0 {
		return nil
	}
	n := c.Numeric
	pred := fmt.Sprintf("TRY_CAST(%s AS DOUBLE) IS NOT NULL AND abs(TRY_CAST(%s AS DOUBLE) - %v) > %v * %v",
		col(c), col(c), n.Mean, profile.OutlierSigma, n.StdDev)

	return []finding.Finding{{
		Rule:     "column.numeric_outliers",
		Severity: finding.Info,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%s has %d value(s) more than %.0f standard deviations from the mean",
			c.Name, n.Outliers, profile.OutlierSigma),
		Detail: fmt.Sprintf(
			"Values range from %g to %g with a median of %g. A handful sit far outside "+
				"that spread, which is the usual signature of a units mix-up (pence "+
				"against pounds), a misplaced decimal point, or a placeholder that was "+
				"never meant to be read as a quantity.",
			n.Min, n.Max, n.Median),
		Remedy:   "Check the extreme values against the source before including them in any total.",
		Location: tc.location(c),
		Count:    n.Outliers,
		Total:    n.Count,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere(pred),
			RowQuery:   tc.rowsWhere(pred),
			Expected:   fmt.Sprintf("values within %.0f standard deviations of %g", profile.OutlierSigma, n.Mean),
			Observed:   fmt.Sprintf("%d beyond it, up to %g", n.Outliers, n.Max),
		},
	}}
}

// nonNegativeNames are column names whose meaning rules out a negative value.
var nonNegativeNames = []string{
	"quantity", "qty", "count", "age", "price", "amount", "total", "revenue",
	"weight", "height", "length", "duration", "size", "stock", "units",
}

// checkUnexpectedNegatives reports negative values in a column whose name says
// it should not have any.
func checkUnexpectedNegatives(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Numeric == nil || c.Numeric.Negative == 0 {
		return nil
	}
	name := strings.ToLower(c.Name)
	// Anything explicitly signed is expected to go negative.
	for _, ok := range []string{"balance", "delta", "change", "adjustment", "diff", "net", "margin", "profit"} {
		if strings.Contains(name, ok) {
			return nil
		}
	}
	var matched string
	for _, n := range nonNegativeNames {
		if strings.Contains(name, n) {
			matched = n
			break
		}
	}
	if matched == "" {
		return nil
	}

	pred := fmt.Sprintf("TRY_CAST(%s AS DOUBLE) < 0", col(c))
	return []finding.Finding{{
		Rule:     "column.unexpected_negative",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s has %d negative value(s)", c.Name, c.Numeric.Negative),
		Detail: fmt.Sprintf(
			"The name suggests a %s, which cannot sensibly be negative. These rows are "+
				"most often refunds or reversals recorded in the same column as the "+
				"original, corrections that were never reconciled, or a sign convention "+
				"that differs between source systems. Any total over this column silently "+
				"nets them off.", matched),
		Remedy:   "Confirm whether negatives are meaningful here; if they are reversals, record them distinguishably.",
		Location: tc.location(c),
		Count:    c.Numeric.Negative,
		Total:    c.Numeric.Count,
		Evidence: finding.Evidence{
			CountQuery: tc.countWhere(pred),
			RowQuery:   tc.rowsWhere(pred),
			Expected:   "no negative values",
			Observed:   fmt.Sprintf("%d negative, minimum %g", c.Numeric.Negative, c.Numeric.Min),
		},
	}}
}

// checkConstantColumn reports a column holding the same value throughout.
func checkConstantColumn(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Distinct != 1 || c.Total < 5 || c.Populated() == 0 {
		return nil
	}
	return []finding.Finding{{
		Rule:     "column.constant",
		Severity: finding.Info,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("%s has the same value in all %d rows", c.Name, c.Total),
		Detail: "A column that never varies carries no information. It is often a filter " +
			"that was applied when the export was produced, which matters because the " +
			"file is then a subset of the data rather than the whole of it.",
		Remedy:   "Check whether this export was filtered, and whether the filter was intended.",
		Location: tc.location(c),
		Count:    c.Total,
		Total:    c.Total,
		Evidence: finding.Evidence{
			Expected: "more than one distinct value",
			Observed: "a single value throughout",
		},
	}}
}

// checkRenamedHeader reports a column the loader had to rename.
func checkRenamedHeader(tc *tableContext, c *profile.Column) []finding.Finding {
	if c.Original == "" || c.Original == c.Name {
		return nil
	}
	return []finding.Finding{{
		Rule:     "column.duplicate_header",
		Severity: finding.Warning,
		Origin:   finding.OriginCheck,
		Title:    fmt.Sprintf("the header name %q is used by more than one column", c.Original),
		Detail: fmt.Sprintf(
			"Two columns in this file share the name %q, so Veritix read the second as "+
				"%q. Every other tool will do something different: some take the first, "+
				"some the last, some fail. Which of the two a downstream report is "+
				"actually using is unpredictable.",
			c.Original, c.Name),
		Remedy:   "Give the columns distinct names at source.",
		Location: tc.location(c),
		Count:    1,
		Total:    1,
		Evidence: finding.Evidence{
			Expected: "unique column names",
			Observed: fmt.Sprintf("%q appears more than once", c.Original),
		},
	}}
}

func describeKind(k profile.Kind) string {
	switch k {
	case profile.KindInteger:
		return "whole numbers"
	case profile.KindDecimal:
		return "numbers"
	case profile.KindDate:
		return "dates"
	case profile.KindTimestamp:
		return "timestamps"
	case profile.KindBoolean:
		return "true/false values"
	case profile.KindEmpty:
		return "empty"
	default:
		return "text"
	}
}

func looksScheduled(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"due", "expiry", "expires", "expiration", "renewal",
		"scheduled", "planned", "target", "deadline", "valid_to", "valid_until", "end_date"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}
