package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeManifest states what was planted, in the format internal/eval reads.
//
// The counts come from the generator's own tallies rather than from the rates
// it was aiming for, because `veritix eval` re-runs every agent target's query
// and a target whose count is wrong is a target nothing can ever match.
func writeManifest(dir string, customers, orders counts) error {
	var b strings.Builder

	b.WriteString(`version: 1
dataset: big-synthetic
description: >-
  Ground truth for the generated scale dataset. Written by
  scripts/gen-dataset, which is the only thing that should edit it: the counts
  below are what the generator actually planted, and hand-editing one makes it
  disagree with the data it describes.

  The defects are the same kinds the small fixtures carry. What differs is the
  number of rows they are hidden in, which is the point: a check that finds a
  placeholder among nine customers has not been shown to find one among two
  million.

defects:
`)

	defect := func(id, where, why, caughtBy string) {
		fmt.Fprintf(&b, "  - id: %s\n    where: %s\n    why: >-\n%s    caught_by: %s\n\n",
			id, where, fold(why), caughtBy)
	}
	// A defect planted at a rate of one row in half a million is not planted
	// at all in a dataset generated at a small scale. Listing it anyway would
	// have the eval report a miss for a defect that is not there, which is the
	// manifest lying rather than the checks failing.
	planted := func(n int, id, where, why, caughtBy string) {
		if n > 0 {
			defect(id, where, why, caughtBy)
		}
	}

	planted(customers.duplicateRows, "customers.duplicate_rows", "customers.csv",
		fmt.Sprintf("%d rows are written twice, identical in every column", customers.duplicateRows),
		"table.duplicate_rows")
	planted(customers.duplicateKeys, "customers.duplicate_key", "customers.csv.customer_id",
		fmt.Sprintf("%d ids from the head of the file are repeated at its tail", customers.duplicateKeys),
		"key.duplicate_values")
	planted(customers.paddedNames, "customers.whitespace_padding", "customers.csv.name",
		fmt.Sprintf("%d names are padded with spaces", customers.paddedNames),
		"column.whitespace_padding")
	planted(customers.altDateFormat, "customers.mixed_date_formats", "customers.csv.signup_date",
		fmt.Sprintf("%d signup dates are written DD/MM/YYYY among ISO ones", customers.altDateFormat),
		"column.mixed_date_formats")
	planted(customers.notADate, "customers.date_type_violation", "customers.csv.signup_date",
		fmt.Sprintf("%d signup dates are the literal 'not a date'", customers.notADate),
		"column.type_violation")
	planted(customers.lowercaseStatus, "customers.case_variants", "customers.csv.status",
		fmt.Sprintf("%d statuses are lowercased copies of a value spelled differently elsewhere", customers.lowercaseStatus),
		"column.case_variants")
	planted(customers.placeholders, "customers.missing_value_placeholders", "customers.csv.region",
		fmt.Sprintf("%d regions are the placeholders 'N/A' or '-'", customers.placeholders),
		"column.missing_values")
	planted(customers.numericSentinel, "customers.numeric_sentinel", "customers.csv.credit_limit",
		fmt.Sprintf("%d credit limits are -999, which survives a numeric cast and is then averaged", customers.numericSentinel),
		"column.missing_values")
	defect("customers.empty_column", "customers.csv.legacy_flag",
		"legacy_flag is written on every row and filled on none",
		"column.empty")

	planted(orders.orphanParents, "orders.customer_orphan", "orders.csv.customer_id",
		fmt.Sprintf("%d orders point at CUS-99999999, which is not in customers.csv", orders.orphanParents),
		"reference.orphan_values")
	planted(orders.implausible, "orders.implausible_date", "orders.csv.order_date",
		fmt.Sprintf("%d order dates are the placeholder 1900-01-01", orders.implausible),
		"column.implausible_dates")
	planted(orders.future, "orders.future_date", "orders.csv.ship_date",
		fmt.Sprintf("%d ship dates are in 2031", orders.future),
		"column.future_dates")
	planted(orders.negative, "orders.negative_amount", "orders.csv.amount",
		fmt.Sprintf("%d amounts are negative", orders.negative),
		"column.unexpected_negative")
	planted(orders.raggedRows, "orders.unreadable_rows", "orders.csv",
		fmt.Sprintf("%d rows carry a stray comma and are wider than the header", orders.raggedRows),
		"table.unreadable_rows")

	defect("metrics.constant_column", fmt.Sprintf("metrics.csv.m%03d", metricCols/2),
		"the column holds 0 on every row, so it measures nothing",
		"column.constant")
	defect("metrics.empty_column", fmt.Sprintf("metrics.csv.m%03d", metricCols),
		"the column is written on every row and filled on none",
		"column.empty")

	planted(customers.orphanRegions, "customers.region_orphans", "customers.csv.region",
		fmt.Sprintf("%d customer rows carry a region that is not in regions.csv: the "+
			"placeholders 'N/A' and '-', and R99, which is not a placeholder and is the "+
			"one nobody notices", customers.orphanRegions),
		"reference.orphan_values")

	// Agent territory. Both dates are valid and plausible, and neither column
	// is wrong on its own, so nothing deterministic proposes comparing them.
	if orders.beforeOrder > 0 {
		fmt.Fprintf(&b, `  - id: orders.shipped_before_ordered
    where: orders.csv.ship_date
    why: >-
      %d order rows have a ship_date five days before their order_date. Every
      value in both columns is a valid, plausible date, and no check compares
      one column against another, so this is invisible to the deterministic
      pass however many rows it reads.
    caught_by: none
    agent:
      count: %d
      query: >-
        SELECT count(*) FROM orders_csv
        WHERE TRY_CAST(ship_date AS DATE) < TRY_CAST(order_date AS DATE)

`, orders.beforeOrder, orders.beforeOrder)
	}

	b.WriteString(`# Places the data is deliberately clean. A check that fires on everything is
# useless, and at this size a check that fires on one row in a million is the
# same thing wearing a disguise.
clean:
`)
	clean := func(rule, where, why string) {
		fmt.Fprintf(&b, "  - rule: %s\n    where: %s\n    why: >-\n%s\n", rule, where, fold(why))
	}
	clean("column.type_violation", "orders.csv.order_id", "every order_id is a whole number")
	clean("column.whitespace_padding", "customers.csv.email", "the emails are not padded")
	clean("column.empty", "customers.csv.name", "every customer has a name")
	clean("column.case_variants", "customers.csv.customer_id", "the ids differ by more than case")
	clean("table.empty", "orders.csv", "orders.csv has rows")
	clean("column.missing_values", "orders.csv.qty", "every quantity is a real number between 1 and 9")
	clean("reference.orphan_values", "orders.csv.sku", "every sku is in products.csv")
	clean("column.constant", "products.csv.unit_price", "unit prices vary")

	path := filepath.Join(dir, "veritix-manifest.yaml")
	return os.WriteFile(path, []byte(b.String()), 0o644) //nolint:gosec // a fixture manifest, meant to be read
}

// fold wraps prose as an indented YAML folded scalar.
//
// The text is written by the generator and describes real data, so it contains
// colons, quotes and placeholder values like 'N/A'. A plain scalar holding any
// of those is either a parse error or, worse, parses into something other than
// what was written.
func fold(text string) string {
	const indent = "      "
	var b strings.Builder
	line := indent
	for _, word := range strings.Fields(text) {
		if len(line) > len(indent) && len(line)+1+len(word) > 74 {
			b.WriteString(line + "\n")
			line = indent
		}
		if len(line) > len(indent) {
			line += " "
		}
		line += word
	}
	if len(line) > len(indent) {
		b.WriteString(line + "\n")
	}
	return b.String()
}
