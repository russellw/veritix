package profile

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/ingest"
)

// sentinelValues are strings that stand in for "no value" without being null.
// Compared case-insensitively after trimming.
//
// These matter because every one of them silently defeats a null check. A
// report that says "no missing regions" is wrong if 300 rows say "N/A".
var sentinelValues = []string{
	"n/a", "n.a.", "na", "null", "nil", "none", "-", "--", "?", "??",
	"unknown", "unspecified", "tbd", "tbc", "not applicable", "not available",
	"#n/a", "missing", ".",
}

// numericSentinels are magic numbers conventionally used to mean "missing".
// They are far more dangerous than a text sentinel because they survive a
// numeric cast and are then averaged, summed, and charted as if they were real.
var numericSentinels = []string{
	"-1", "-99", "-999", "-9999", "999", "9999", "99999", "999999", "9999999",
	"0000-00-00",
}

// dateFormats are tried in order against every column that might hold dates.
// The pairs of day-first and month-first patterns are what make ambiguity
// detectable.
var dateFormats = []struct {
	format string
	label  string
}{
	{"%Y-%m-%d", "ISO 8601 (YYYY-MM-DD)"},
	{"%d/%m/%Y", "day first (DD/MM/YYYY)"},
	{"%m/%d/%Y", "month first (MM/DD/YYYY)"},
	{"%Y/%m/%d", "year first with slashes (YYYY/MM/DD)"},
	{"%d-%m-%Y", "day first with dashes (DD-MM-YYYY)"},
	{"%m-%d-%Y", "month first with dashes (MM-DD-YYYY)"},
	{"%d.%m.%Y", "day first with dots (DD.MM.YYYY)"},
	{"%Y%m%d", "compact (YYYYMMDD)"},
	{"%d %b %Y", "day with short month name"},
	{"%b %d, %Y", "short month name first"},
	{"%d/%m/%y", "day first, two-digit year"},
	{"%m/%d/%y", "month first, two-digit year"},
}

// quotedTextSentinels renders the textual placeholder list as a SQL IN clause.
//
// Only the textual ones. The numeric placeholders (-999, 999999) are excluded
// here because they cast cleanly to numbers: treating them as non-data would
// wrongly exclude a column that genuinely contains -1.
func quotedTextSentinels() string {
	quoted := make([]string, len(sentinelValues))
	for i, s := range sentinelValues {
		quoted[i] = engine.Literal(s)
	}
	return strings.Join(quoted, ", ")
}

// nonBlank is the predicate for a value that carries content. It is repeated
// throughout the profiling SQL, so it lives in one place.
func nonBlank(col string) string {
	return fmt.Sprintf("(%s IS NOT NULL AND trim(%s) <> '')", col, col)
}

// The exported wrappers below let the checks package build evidence queries
// from the same expressions the profiler measured with. Two definitions of
// "is this a valid date" would eventually disagree, and then a finding's
// evidence would contradict the profile it came from.

// SQLNonBlank is the predicate for a value that carries content.
func SQLNonBlank(quotedCol string) string { return nonBlank(quotedCol) }

// SQLTextSentinelList renders the textual placeholder values as a SQL list.
func SQLTextSentinelList() string { return quotedTextSentinels() }

// SQLIsNumericSentinel is the predicate for one of the magic numbers, as
// distinct from a textual placeholder. It is the SQL half of
// IsNumericSentinel, and the two read the same list for the reason
// profile exports its predicates at all: two definitions of "is this a
// placeholder" would eventually disagree, and then a finding would contradict
// the profile it came from.
func SQLIsNumericSentinel(quotedCol string) string {
	quoted := make([]string, len(numericSentinels))
	for i, s := range numericSentinels {
		quoted[i] = engine.Literal(s)
	}
	return fmt.Sprintf("lower(trim(%s)) IN (%s)", quotedCol, strings.Join(quoted, ", "))
}

// IsNumericSentinel reports whether a placeholder is one of the magic numbers
// rather than a piece of text.
//
// The two are different kinds of evidence and the checks have to tell them
// apart. "n/a" is never a measurement: whatever share of the column it takes,
// it means the value is absent and a null check will say the column is
// complete. -1 and 999 are magic numbers *conventionally* used that way and
// are also, sometimes, real numbers — so they are evidence only when they
// stand out from the column around them.
//
// The value is compared as readSentinels stores it: lowercased and trimmed.
func IsNumericSentinel(v string) bool {
	for _, s := range numericSentinels {
		if v == s {
			return true
		}
	}
	return false
}

// SQLIsSentinel is the predicate for a recognized "missing" placeholder.
func SQLIsSentinel(quotedCol string) string {
	return fmt.Sprintf("lower(trim(%s)) IN (%s)", quotedCol, quotedTextSentinels())
}

// SQLMatchesKind is the predicate for a value conforming to a type. It returns
// an empty string for kinds that have no meaningful test.
func SQLMatchesKind(kind Kind, quotedCol string) string {
	switch kind {
	case KindInteger:
		return integerExpr(quotedCol)
	case KindDecimal:
		return fmt.Sprintf("TRY_CAST(%s AS DOUBLE) IS NOT NULL", quotedCol)
	case KindBoolean:
		return fmt.Sprintf("TRY_CAST(%s AS BOOLEAN) IS NOT NULL", quotedCol)
	case KindDate:
		return anyDateExpr(quotedCol)
	case KindTimestamp:
		return fmt.Sprintf("TRY_CAST(%s AS TIMESTAMP) IS NOT NULL", quotedCol)
	default:
		return ""
	}
}

// SQLAmbiguousDate is the predicate for a value that reads as a valid but
// different date under day-first and month-first conventions.
func SQLAmbiguousDate(quotedCol string) string {
	return fmt.Sprintf(
		`try_strptime(trim(%[1]s), '%%d/%%m/%%Y') IS NOT NULL `+
			`AND try_strptime(trim(%[1]s), '%%m/%%d/%%Y') IS NOT NULL `+
			`AND try_strptime(trim(%[1]s), '%%d/%%m/%%Y') <> try_strptime(trim(%[1]s), '%%m/%%d/%%Y')`,
		quotedCol)
}

// integerExpr tests whether a value is written as a whole number.
//
// A cast is not good enough here: DuckDB accepts TRY_CAST('89.99' AS BIGINT)
// and truncates it, so a column of prices would be reported as integers and
// every fractional penny in it would go unremarked. The written form is what
// matters, so match on that.
func integerExpr(col string) string {
	return fmt.Sprintf(`regexp_full_match(trim(%s), '^[+-]?[0-9]{1,18}$')`, col)
}

// inferenceDateFormats are the patterns tried when deciding whether a column
// holds dates. It is a subset of dateFormats: this expression runs against
// every value of every column, so it covers the common cases cheaply, and the
// full enumeration runs later only for columns that turn out to be temporal.
var inferenceDateFormats = []string{
	"%d/%m/%Y", "%m/%d/%Y", "%d-%m-%Y", "%d.%m.%Y", "%Y/%m/%d", "%Y%m%d",
}

// anyDateExpr tests whether a value parses as a date in any common format.
//
// Restricting this to ISO would misclassify every European export as text and
// lose the fact that the column was ever meant to hold dates — along with the
// range, ordering, and ambiguity checks that follow from knowing it does.
func anyDateExpr(col string) string {
	parts := make([]string, 0, len(inferenceDateFormats)+1)
	parts = append(parts, fmt.Sprintf("TRY_CAST(%s AS DATE) IS NOT NULL", col))
	for _, f := range inferenceDateFormats {
		parts = append(parts, fmt.Sprintf("try_strptime(trim(%s), %s) IS NOT NULL",
			col, engine.Literal(f)))
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// profileColumn measures one column.
func profileColumn(ctx context.Context, e *engine.Engine, t *Table, lc ingest.Column, opts Options) (*Column, error) {
	c := &Column{
		Name:         lc.Name,
		Original:     lc.Original,
		Ordinal:      lc.Ordinal,
		DeclaredType: lc.SniffedType,
		Total:        t.RowCount,
	}

	if t.RowCount == 0 {
		c.Inferred = Inference{Kind: KindEmpty, Candidates: map[Kind]int64{}}
		return c, nil
	}

	if err := c.readBasics(ctx, e, t.Name); err != nil {
		return nil, err
	}
	if err := c.readSentinels(ctx, e, t.Name); err != nil {
		return nil, err
	}
	if err := c.readShapes(ctx, e, t.Name, opts.Shapes); err != nil {
		return nil, err
	}
	if err := c.readTopValues(ctx, e, t.Name, opts.TopValues); err != nil {
		return nil, err
	}

	// Follow-up measurements only make sense where the values support them.
	if c.Numeric != nil && c.Numeric.Count > 0 {
		if err := c.readNumericDetail(ctx, e, t.Name); err != nil {
			return nil, err
		}
	}
	if c.Temporal != nil && c.Temporal.Count > 0 {
		if err := c.readTemporalDetail(ctx, e, t.Name); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// readBasics runs the single pass that produces most of the profile: counts,
// lengths, whitespace, and the type-probe tallies that drive inference.
func (c *Column) readBasics(ctx context.Context, e *engine.Engine, table string) error {
	col := engine.Ident(c.Name)
	nb := nonBlank(col)

	// Every type probe is a TRY_CAST, so a value that does not fit produces
	// NULL rather than an error. Counting the successes gives the share of the
	// column that genuinely is each type.
	q := fmt.Sprintf(`
		SELECT
			count(*)                                                       AS total,
			count(%[1]s)                                                   AS non_null,
			sum(CASE WHEN %[1]s IS NOT NULL AND trim(%[1]s) = '' THEN 1 ELSE 0 END) AS blanks,
			count(DISTINCT %[1]s)                                          AS distinct_all,
			count(DISTINCT lower(trim(%[1]s)))                             AS distinct_norm,
			coalesce(min(length(%[1]s)), 0)                                AS min_len,
			coalesce(max(length(%[1]s)), 0)                                AS max_len,
			coalesce(avg(length(%[1]s)), 0)                                AS avg_len,
			sum(CASE WHEN %[1]s <> ltrim(%[1]s) THEN 1 ELSE 0 END)         AS lead_ws,
			sum(CASE WHEN %[1]s <> rtrim(%[1]s) THEN 1 ELSE 0 END)         AS trail_ws,
			min(%[1]s)                                                     AS min_val,
			max(%[1]s)                                                     AS max_val,
			sum(CASE WHEN %[2]s THEN 1 ELSE 0 END)                         AS non_blank,
			sum(CASE WHEN %[2]s AND lower(trim(%[1]s)) IN (%[4]s) THEN 1 ELSE 0 END) AS sentinel_hits,
			sum(CASE WHEN %[2]s AND %[5]s THEN 1 ELSE 0 END)               AS as_int,
			sum(CASE WHEN %[2]s AND TRY_CAST(%[1]s AS DOUBLE)    IS NOT NULL THEN 1 ELSE 0 END) AS as_dec,
			sum(CASE WHEN %[2]s AND TRY_CAST(%[1]s AS BOOLEAN)   IS NOT NULL THEN 1 ELSE 0 END) AS as_bool,
			sum(CASE WHEN %[2]s AND %[6]s THEN 1 ELSE 0 END)               AS as_date,
			sum(CASE WHEN %[2]s AND TRY_CAST(%[1]s AS TIMESTAMP) IS NOT NULL THEN 1 ELSE 0 END) AS as_ts
		FROM %[3]s`, col, nb, engine.Ident(table), quotedTextSentinels(),
		integerExpr(col), anyDateExpr(col))

	var (
		total, nonNull, blanks, distinctAll, distinctNorm int64
		minLen, maxLen                                    int64
		avgLen                                            float64
		leadWS, trailWS                                   int64
		minVal, maxVal                                    sql.NullString
		nonBlankCount, sentinelHits                       int64
		asInt, asDec, asBool, asDate, asTS                int64
	)

	err := e.ScanOne(ctx, q, []any{
		&total, &nonNull, &blanks, &distinctAll, &distinctNorm,
		&minLen, &maxLen, &avgLen, &leadWS, &trailWS,
		&minVal, &maxVal, &nonBlankCount, &sentinelHits,
		&asInt, &asDec, &asBool, &asDate, &asTS,
	})
	if err != nil {
		return err
	}

	c.Total = total
	c.Nulls = total - nonNull
	c.Blanks = blanks
	c.Distinct = distinctAll
	c.DistinctNormalized = distinctNorm
	c.MinLength, c.MaxLength, c.AvgLength = minLen, maxLen, avgLen
	c.LeadingWhitespace, c.TrailingWhitespace = leadWS, trailWS
	c.MinValue, c.MaxValue = minVal.String, maxVal.String

	// A textual placeholder such as "N/A" is a missing value, not a type
	// violation. Counting it against the column's type would report a numeric
	// column with a few gaps as a text column, which loses the fact that it
	// was ever meant to be numeric — and with it every numeric check.
	c.Inferred = infer(nonBlankCount-sentinelHits, map[Kind]int64{
		KindInteger:   asInt,
		KindDecimal:   asDec,
		KindBoolean:   asBool,
		KindDate:      asDate,
		KindTimestamp: asTS,
	})

	if asDec > 0 {
		c.Numeric = &NumericStats{Count: asDec}
	}
	if asDate > 0 || asTS > 0 {
		c.Temporal = &TemporalStats{Count: max64(asDate, asTS)}
	}
	return nil
}

// infer decides a column's type from the probe tallies.
//
// Order matters. Integer is tested before decimal because every integer also
// casts to a double, and date before timestamp for the same reason. Boolean is
// tested last among the scalars: DuckDB accepts "0" and "1" as booleans, so an
// integer column would otherwise be misreported as a flag.
func infer(dataValues int64, candidates map[Kind]int64) Inference {
	inf := Inference{Kind: KindText, Candidates: candidates}
	if dataValues == 0 {
		inf.Kind = KindEmpty
		return inf
	}

	// Always allow at least one exception. A proportional threshold alone
	// would say a ten-row column with one bad date is a text column, which is
	// both wrong and the opposite of useful: the whole point is to report the
	// one bad value, and that requires still calling the column a date column.
	allowed := int64(float64(dataValues) * (1 - conformanceThreshold))
	if allowed < 1 {
		allowed = 1
	}

	order := []Kind{KindInteger, KindDecimal, KindDate, KindTimestamp, KindBoolean}
	for _, k := range order {
		matched := candidates[k]
		if conf := float64(matched) / float64(dataValues); conf > inf.BestConformance {
			inf.BestCandidate, inf.BestConformance = k, conf
		}
		if inf.Kind != KindText {
			continue // keep scanning only to complete the best-candidate figures
		}
		if dataValues-matched <= allowed {
			inf.Kind = k
			inf.Conformance = float64(matched) / float64(dataValues)
			inf.Nonconforming = dataValues - matched
		}
	}

	if inf.Kind == KindText {
		// A text column conforms to being text, but the near-miss is the
		// interesting part: "82% of these values are dates" is what turns an
		// unremarkable text column into a finding worth reading.
		inf.Conformance = 1
	}
	return inf
}

// readSentinels counts the placeholder values that mean "missing" without
// being null.
func (c *Column) readSentinels(ctx context.Context, e *engine.Engine, table string) error {
	col := engine.Ident(c.Name)

	all := make([]string, 0, len(sentinelValues)+len(numericSentinels))
	all = append(all, sentinelValues...)
	all = append(all, numericSentinels...)

	quoted := make([]string, len(all))
	for i, s := range all {
		quoted[i] = engine.Literal(s)
	}

	q := fmt.Sprintf(`
		SELECT lower(trim(%[1]s)) AS v, count(*) AS n
		FROM %[2]s
		WHERE lower(trim(%[1]s)) IN (%[3]s)
		GROUP BY 1 ORDER BY n DESC, v`,
		col, engine.Ident(table), strings.Join(quoted, ", "))

	rs, err := e.Collect(ctx, q, len(all))
	if err != nil {
		return err
	}

	populated := c.Total - c.Nulls
	for _, r := range rs.Rows {
		vc := ValueCount{Value: asText(r[0]), Count: asInt64(r[1])}
		if populated > 0 {
			vc.Share = float64(vc.Count) / float64(populated)
		}
		c.Sentinels = append(c.Sentinels, vc)
	}
	return nil
}

// readShapes derives the column's value patterns.
//
// Digits become 9 and letters become X, character for character, so
// "CUS-004417" becomes "XXX-999999". The result describes a column's format
// precisely while disclosing none of its contents, which is what allows the
// agent to reason about a column it is not permitted to read.
func (c *Column) readShapes(ctx context.Context, e *engine.Engine, table string, limit int) error {
	col := engine.Ident(c.Name)

	shape := fmt.Sprintf(
		`regexp_replace(regexp_replace(regexp_replace(%s, '[0-9]', '9', 'g'), '[A-Za-z]', 'X', 'g'), '\s', ' ', 'g')`,
		col)

	q := fmt.Sprintf(`
		SELECT %[1]s AS shape, count(*) AS n
		FROM %[2]s
		WHERE %[3]s
		GROUP BY 1 ORDER BY n DESC, shape
		LIMIT %[4]d`,
		shape, engine.Ident(table), nonBlank(col), limit)

	rs, err := e.Collect(ctx, q, limit)
	if err != nil {
		return err
	}

	populated := c.Total - c.Nulls
	for _, r := range rs.Rows {
		vc := ValueCount{Value: asText(r[0]), Count: asInt64(r[1])}
		if populated > 0 {
			vc.Share = float64(vc.Count) / float64(populated)
		}
		// A shape longer than this is free text, and reporting it verbatim
		// would be both useless and a disclosure risk.
		const maxShapeLen = 60
		if len(vc.Value) > maxShapeLen {
			vc.Value = vc.Value[:maxShapeLen] + "…"
		}
		c.Shapes = append(c.Shapes, vc)
	}
	return nil
}

// readTopValues collects the most frequent values.
//
// RAW DATA: the values retained here are verbatim cell contents.
func (c *Column) readTopValues(ctx context.Context, e *engine.Engine, table string, limit int) error {
	col := engine.Ident(c.Name)

	q := fmt.Sprintf(`
		SELECT %[1]s AS v, count(*) AS n
		FROM %[2]s
		WHERE %[3]s
		GROUP BY 1 ORDER BY n DESC, v
		LIMIT %[4]d`,
		col, engine.Ident(table), nonBlank(col), limit)

	rs, err := e.Collect(ctx, q, limit)
	if err != nil {
		return err
	}

	populated := c.Total - c.Nulls
	for _, r := range rs.Rows {
		vc := ValueCount{Value: asText(r[0]), Count: asInt64(r[1])}
		if populated > 0 {
			vc.Share = float64(vc.Count) / float64(populated)
		}
		c.TopValues = append(c.TopValues, vc)
	}
	return nil
}

// readNumericDetail measures the distribution of a column's numeric values.
func (c *Column) readNumericDetail(ctx context.Context, e *engine.Engine, table string) error {
	col := engine.Ident(c.Name)

	// The subquery casts once; the outer aggregates then work on real numbers.
	q := fmt.Sprintf(`
		WITH v AS (
			SELECT TRY_CAST(%[1]s AS DOUBLE) AS x, NOT (%[4]s) AS measured
			FROM %[2]s WHERE %[3]s
		)
		SELECT
			count(x), coalesce(min(x), 0), coalesce(max(x), 0),
			coalesce(avg(x), 0), coalesce(stddev_samp(x), 0),
			coalesce(quantile_cont(x, 0.25), 0),
			coalesce(quantile_cont(x, 0.50), 0),
			coalesce(quantile_cont(x, 0.75), 0),
			sum(CASE WHEN x < 0 THEN 1 ELSE 0 END),
			sum(CASE WHEN x = 0 THEN 1 ELSE 0 END),
			coalesce(min(CASE WHEN measured THEN x END), 0),
			coalesce(max(CASE WHEN measured THEN x END), 0)
		FROM v WHERE x IS NOT NULL`,
		col, engine.Ident(table), nonBlank(col), SQLIsNumericSentinel(col))

	n := c.Numeric
	if err := e.ScanOne(ctx, q, []any{
		&n.Count, &n.Min, &n.Max, &n.Mean, &n.StdDev,
		&n.P25, &n.Median, &n.P75, &n.Negative, &n.Zero,
		&n.MinReal, &n.MaxReal,
	}); err != nil {
		return err
	}

	// Outliers are only meaningful once there is a spread to measure against.
	if n.StdDev > 0 && n.Count > 2 {
		outQ := fmt.Sprintf(`
			WITH v AS (
				SELECT TRY_CAST(%[1]s AS DOUBLE) AS x FROM %[2]s WHERE %[3]s
			)
			SELECT count(*) FROM v
			WHERE x IS NOT NULL AND abs(x - %[4]v) > %[5]v * %[6]v`,
			col, engine.Ident(table), nonBlank(col), n.Mean, OutlierSigma, n.StdDev)
		if err := e.ScanOne(ctx, outQ, []any{&n.Outliers}); err != nil {
			return err
		}
	}
	return nil
}

// readTemporalDetail works out which date formats a column uses and whether
// any of its values are dangerously ambiguous.
func (c *Column) readTemporalDetail(ctx context.Context, e *engine.Engine, table string) error {
	col := engine.Ident(c.Name)
	tbl := engine.Ident(table)
	nb := nonBlank(col)

	// One pass counting how many values each candidate format parses.
	var probes []string
	for i, f := range dateFormats {
		probes = append(probes, fmt.Sprintf(
			"sum(CASE WHEN try_strptime(trim(%s), %s) IS NOT NULL THEN 1 ELSE 0 END) AS f%d",
			col, engine.Literal(f.format), i))
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s", strings.Join(probes, ", "), tbl, nb)

	counts := make([]int64, len(dateFormats))
	dest := make([]any, len(dateFormats))
	for i := range counts {
		dest[i] = &counts[i]
	}
	if err := e.ScanOne(ctx, q, dest); err != nil {
		return err
	}

	tp := c.Temporal
	for i, f := range dateFormats {
		if counts[i] > 0 {
			tp.Formats = append(tp.Formats, FormatCount{
				Format: f.format, Example: f.label, Count: counts[i],
			})
		}
	}
	if err := c.readExclusiveFormats(ctx, e, table); err != nil {
		return err
	}

	// A value that parses under both day-first and month-first readings, and
	// means a different date under each, cannot be resolved from the value
	// alone. Whichever reading a downstream tool picks, some of these dates
	// will be wrong, and nothing will report an error.
	ambQ := fmt.Sprintf(`
		SELECT count(*) FROM %[1]s
		WHERE %[2]s
		  AND try_strptime(trim(%[3]s), '%%d/%%m/%%Y') IS NOT NULL
		  AND try_strptime(trim(%[3]s), '%%m/%%d/%%Y') IS NOT NULL
		  AND try_strptime(trim(%[3]s), '%%d/%%m/%%Y') <> try_strptime(trim(%[3]s), '%%m/%%d/%%Y')`,
		tbl, nb, col)
	if err := e.ScanOne(ctx, ambQ, []any{&tp.Ambiguous}); err != nil {
		return err
	}

	// Range checks against the best-matching format.
	best := ""
	var bestCount int64
	for i, f := range dateFormats {
		if counts[i] > bestCount {
			best, bestCount = f.format, counts[i]
		}
	}
	if best == "" {
		return nil
	}

	parsed := fmt.Sprintf("try_strptime(trim(%s), %s)", col, engine.Literal(best))
	rangeQ := fmt.Sprintf(`
		SELECT
			coalesce(cast(min(%[1]s) AS VARCHAR), ''),
			coalesce(cast(max(%[1]s) AS VARCHAR), ''),
			sum(CASE WHEN %[1]s > now() THEN 1 ELSE 0 END),
			sum(CASE WHEN %[1]s < TIMESTAMP '%[4]s' OR %[1]s > now() + INTERVAL 100 YEAR
			         THEN 1 ELSE 0 END)
		FROM %[2]s WHERE %[3]s AND %[1]s IS NOT NULL`,
		parsed, tbl, nb, implausibleBefore)

	return e.ScanOne(ctx, rangeQ, []any{&tp.Min, &tp.Max, &tp.Future, &tp.Implausible})
}

// readExclusiveFormats measures, for each format the column matched, how many
// values it parses that the leading format cannot.
//
// Counting each format over the whole column double-counts: every value a
// day-first pattern reads with a day of 12 or less is read by the month-first
// pattern too, so a column written in one format reports two. The share of the
// column each format accounts for does not separate those cases — it only
// makes the artifact small, which is indistinguishable from a real second
// format that is rare. Rare is what a real second format looks like in a file
// with two million rows, and it is the case worth reporting: those are the
// rows a single-format reader silently gets wrong.
func (c *Column) readExclusiveFormats(ctx context.Context, e *engine.Engine, table string) error {
	tp := c.Temporal
	if len(tp.Formats) < 2 {
		return nil // nothing to be exclusive of
	}

	best := 0
	for i, f := range tp.Formats {
		if f.Count > tp.Formats[best].Count {
			best = i
		}
	}

	col := engine.Ident(c.Name)
	probes := make([]string, 0, len(tp.Formats))
	for _, f := range tp.Formats {
		// The leading format's test comes first so that DuckDB's short
		// circuit does the work: on a column that is 99.9% one format, the
		// second probe is evaluated only on the handful of rows the leading
		// format could not read, and the whole extra pass costs about one
		// strptime per row rather than two per format per row.
		probes = append(probes, fmt.Sprintf(
			"sum(CASE WHEN try_strptime(trim(%[1]s), %[3]s) IS NULL "+
				"AND try_strptime(trim(%[1]s), %[2]s) IS NOT NULL THEN 1 ELSE 0 END)",
			col, engine.Literal(f.Format), engine.Literal(tp.Formats[best].Format)))
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s",
		strings.Join(probes, ", "), engine.Ident(table), nonBlank(col))

	exclusive := make([]int64, len(tp.Formats))
	dest := make([]any, len(tp.Formats))
	for i := range exclusive {
		dest[i] = &exclusive[i]
	}
	if err := e.ScanOne(ctx, q, dest); err != nil {
		return err
	}
	for i := range tp.Formats {
		tp.Formats[i].Exclusive = exclusive[i]
	}
	return nil
}

// implausibleBefore is the date before which a business record is almost
// certainly a placeholder or a parsing accident rather than a real event.
// Epoch-zero and "1900-01-01" defaults are the usual culprits.
const implausibleBefore = "1900-01-02"

func asText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	case nil:
		return ""
	case time.Time:
		return s.Format(time.RFC3339)
	default:
		return fmt.Sprint(s)
	}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case uint64:
		return int64(n) //nolint:gosec // counts are bounded by row counts
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
