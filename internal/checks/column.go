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
	"strings"

	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/profile"
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

// checkMissingValues reports a column with a substantial share of missing
// values, counting placeholders as missing rather than as data.
func checkMissingValues(tc *tableContext, c *profile.Column) []finding.Finding {
	missing := c.Missing()
	if c.Total == 0 || missing == 0 || missing == c.Total {
		return nil // wholly empty is handled by checkEmptyColumn
	}
	share := float64(missing) / float64(c.Total)

	severity := finding.Info
	switch {
	case share >= 0.5:
		severity = finding.Warning
	case share < 0.05:
		return nil // a few gaps in a large column is not news
	}

	// The placeholder half of the count is the part people miss, so lead with
	// it when there is one.
	var sentinelNote string
	var sentinelTotal int64
	for _, s := range c.Sentinels {
		sentinelTotal += s.Count
	}
	if sentinelTotal > 0 {
		names := make([]string, 0, len(c.Sentinels))
		for _, s := range c.Sentinels {
			names = append(names, fmt.Sprintf("%q", s.Value))
		}
		sentinelNote = fmt.Sprintf(
			" Of those, %d are placeholder text (%s) rather than nulls, so a check for "+
				"null values would report this column as complete.",
			sentinelTotal, strings.Join(names, ", "))
	}

	pred := fmt.Sprintf("NOT %s OR %s", profile.SQLNonBlank(col(c)), profile.SQLIsSentinel(col(c)))
	return []finding.Finding{{
		Rule:     "column.missing_values",
		Severity: severity,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%s is missing a value in %d of %d rows (%.0f%%)",
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
// are not a recognised way of writing "missing".
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
				"remaining %d cannot be read as one, and are not a recognised way of "+
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

	// Formats that each account for a trivial number of values are usually one
	// format being matched by two patterns rather than a genuine mixture.
	var significant []profile.FormatCount
	for _, f := range c.Temporal.Formats {
		if float64(f.Count)/float64(max64(c.Temporal.Count, 1)) >= 0.02 {
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

	// The minority format is the count that matters: those are the rows a
	// single-format reader will get wrong or drop.
	var largest int64
	for _, f := range significant {
		if f.Count > largest {
			largest = f.Count
		}
	}
	minority := c.Temporal.Count - largest

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
		Remedy:   "Normalise the column to one format, ideally ISO 8601 (YYYY-MM-DD).",
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
	extra := c.Distinct - c.DistinctNormalised
	if extra <= 0 {
		return nil
	}
	// A free-text column will differ in case constantly and meaninglessly;
	// this only matters for columns behaving like a category.
	if c.DistinctNormalised > 100 || float64(c.DistinctNormalised) > float64(c.Populated())*0.5 {
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
			c.Distinct, c.DistinctNormalised),
		Remedy:   "Normalise the column to a single casing, and constrain it at source if it is a category.",
		Location: tc.location(c),
		Count:    extra,
		Total:    c.Distinct,
		Evidence: finding.Evidence{
			CountQuery: q,
			Expected:   fmt.Sprintf("%d distinct values", c.DistinctNormalised),
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

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
