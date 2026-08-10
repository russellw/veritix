package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/profile"
)

// Relationships between files are where the interesting defects live.
//
// Each file in an export usually looks fine on its own: the orders file has
// orders, the customers file has customers. What breaks is the join between
// them — an order referring to a customer who is not in the export, a code
// spelled one way in one file and another way elsewhere. Nothing inside a
// single file can detect either, which is why Veritix treats a directory as
// one dataset rather than a pile of separate ones.

// keyRef identifies a column that could be the target of a reference.
type keyRef struct {
	table  *profile.Table
	column *profile.Column
	// uniqueness is the share of rows whose value is distinct. A genuine key
	// is 1.0; a key with duplicates is still the intended key, and the
	// duplicates are themselves a finding.
	uniqueness float64
}

// candidate is a proposed reference from one column to another.
type candidate struct {
	child  keyRef
	parent keyRef
	// nameMatch reports that the columns are named as a reference, which is
	// much stronger evidence than value overlap alone.
	nameMatch bool
}

// minParentUniqueness is how nearly-unique a column must be to be treated as
// the target of a reference. Below this it is an attribute, not a key.
const minParentUniqueness = 0.9

// minContainment is the share of child values that must exist in the parent
// before a relationship is inferred from values alone.
const minContainment = 0.85

// relate infers relationships across the dataset and reports where they fail.
func relate(ctx context.Context, e *engine.Engine, ds *profile.Dataset) ([]finding.Finding, error) {
	parents := candidateKeys(ds)
	if len(parents) == 0 {
		return nil, nil
	}

	var out []finding.Finding

	// A duplicated key is worth reporting on its own: everything that joins to
	// it will multiply rows.
	out = append(out, duplicateKeyFindings(parents)...)

	for _, cand := range proposeReferences(ds, parents) {
		f, err := evaluateReference(ctx, e, cand)
		if err != nil {
			return nil, err
		}
		out = append(out, f...)
	}

	shared, err := sharedDomainFindings(ctx, e, ds)
	if err != nil {
		return nil, err
	}
	return append(out, shared...), nil
}

// candidateKeys finds the columns that could identify a row.
func candidateKeys(ds *profile.Dataset) []keyRef {
	var keys []keyRef
	for _, t := range ds.Tables {
		if t.RowCount < 2 {
			continue
		}
		for _, c := range t.Columns {
			populated := c.Total - c.Nulls - c.Blanks
			if populated == 0 || c.Inferred.Kind == profile.KindEmpty {
				continue
			}
			u := float64(c.Distinct) / float64(populated)

			// A column named as its table's identifier is that table's key
			// even when it contains a duplicate. Requiring near-perfect
			// uniqueness would disqualify exactly the tables worth checking:
			// the duplicate is a defect in the key, not evidence that the
			// column was never a key, and disqualifying it would hide both
			// that defect and every broken reference pointing at it.
			named := namesOwnTable(t.Name, c.Name)
			switch {
			case u >= minParentUniqueness:
			case named && u >= 0.5:
			default:
				continue
			}
			// A column that is unique only because every row differs (free
			// text, timestamps) is not a key anyone references.
			if c.Inferred.Kind == profile.KindDecimal && !looksLikeIdentifier(c.Name) {
				continue
			}
			keys = append(keys, keyRef{table: t, column: c, uniqueness: u})
		}
	}
	return keys
}

// duplicateKeyFindings reports a key column that repeats a value.
func duplicateKeyFindings(keys []keyRef) []finding.Finding {
	var out []finding.Finding
	for _, k := range keys {
		if k.uniqueness >= 1.0 {
			continue
		}
		if !looksLikeIdentifier(k.column.Name) {
			continue
		}

		quoted := engine.Ident(k.table.Name)
		colq := engine.Ident(k.column.Name)
		q := fmt.Sprintf(
			"SELECT coalesce(sum(n - 1), 0) FROM (SELECT count(*) AS n FROM %s WHERE %s GROUP BY %s HAVING count(*) > 1)",
			quoted, profile.SQLNonBlank(colq), colq)

		populated := k.column.Total - k.column.Nulls - k.column.Blanks
		surplus := populated - k.column.Distinct

		out = append(out, finding.Finding{
			Rule:     "key.duplicate_values",
			Severity: finding.Error,
			Origin:   finding.OriginCheck,
			Title: fmt.Sprintf("%s repeats %d value(s) despite looking like an identifier",
				k.column.Name, surplus),
			Detail: fmt.Sprintf(
				"%s is distinct in %.0f%% of rows, which is what an identifier looks like, "+
					"but not all of them. Anything joining to this column will match more "+
					"rows than it should and multiply the results, and any correction "+
					"applied \"to that record\" will hit several.",
				k.column.Name, k.uniqueness*100),
			Remedy: "Establish which of the repeated records is correct, or find the column that " +
				"distinguishes them.",
			Location: finding.Location{
				Table:   k.table.Name,
				Display: k.table.Display,
				Column:  k.column.Name,
			},
			Count: surplus,
			Total: populated,
			Evidence: finding.Evidence{
				CountQuery: q,
				RowQuery: fmt.Sprintf(
					"SELECT %s, count(*) AS occurrences FROM %s GROUP BY 1 HAVING count(*) > 1 LIMIT 100",
					colq, quoted),
				Expected: "a distinct value in every row",
				Observed: fmt.Sprintf("%d surplus repeats", surplus),
			},
		})
	}
	return out
}

// proposeReferences pairs child columns with the keys they might refer to.
//
// The pairing is deliberately conservative. Comparing every column against
// every key is quadratic and produces nonsense pairs; naming conventions carry
// most of the real signal, so they are used to prune before any SQL runs.
func proposeReferences(ds *profile.Dataset, parents []keyRef) []candidate {
	var out []candidate

	for _, t := range ds.Tables {
		for _, c := range t.Columns {
			if c.Populated() == 0 {
				continue
			}
			// A column that names its own table is that table's identifier, not
			// a reference to somewhere else. Without this, orders.order_id
			// gets matched against every other order_id in the dataset and
			// each difference in membership is reported as a broken reference,
			// when the two files are simply peers holding different rows.
			if namesOwnTable(t.Name, c.Name) {
				continue
			}
			for _, p := range parents {
				if p.table.Name == t.Name {
					continue // a column does not refer to its own table
				}
				if !compatible(c, p.column) {
					continue
				}
				named := namesSuggestReference(t.Name, c.Name, p.table.Name, p.column.Name)
				if !named && !shapesMatch(c, p.column) {
					continue
				}
				out = append(out, candidate{
					child:     keyRef{table: t, column: c},
					parent:    p,
					nameMatch: named,
				})
			}
		}
	}

	// Where several keys are plausible, prefer a name match, so that
	// orders.customer_id is tested against customers.customer_id rather than
	// against every other identifier in the dataset.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].nameMatch != out[j].nameMatch {
			return out[i].nameMatch
		}
		return out[i].parent.uniqueness > out[j].parent.uniqueness
	})

	seen := make(map[string]bool, len(out))
	deduped := out[:0]
	for _, c := range out {
		k := c.child.table.Name + "\x00" + c.child.column.Name
		if seen[k] {
			continue
		}
		seen[k] = true
		deduped = append(deduped, c)
	}
	return deduped
}

// compatible reports whether two columns could hold the same kind of value.
func compatible(child, parent *profile.Column) bool {
	if child.Inferred.Kind != parent.Inferred.Kind {
		return false
	}
	switch child.Inferred.Kind {
	case profile.KindEmpty, profile.KindBoolean:
		return false
	}
	return true
}

// shapesMatch reports whether two columns share a dominant value format. It is
// what allows a reference to be found when the columns are not named alike.
func shapesMatch(child, parent *profile.Column) bool {
	if len(child.Shapes) == 0 || len(parent.Shapes) == 0 {
		return false
	}
	// Only trust a shape that actually dominates its column.
	if child.Shapes[0].Share < 0.8 || parent.Shapes[0].Share < 0.8 {
		return false
	}
	return child.Shapes[0].Value == parent.Shapes[0].Value
}

// namesSuggestReference applies the usual naming conventions for a foreign key.
func namesSuggestReference(childTable, childCol, parentTable, parentCol string) bool {
	cc := strings.ToLower(childCol)
	pc := strings.ToLower(parentCol)
	pt := strings.ToLower(parentTable)

	// Identical column names across two tables, e.g. orders.customer_id and
	// customers.customer_id.
	if cc == pc {
		return true
	}
	// The child column names the parent table, e.g. orders.customer_id
	// against customers.id.
	base := strings.TrimSuffix(strings.TrimSuffix(cc, "_id"), "id")
	base = strings.Trim(base, "_")
	if base != "" && (pt == base || pt == base+"s" || strings.HasPrefix(pt, base)) {
		return pc == "id" || pc == base+"_id" || pc == cc
	}
	return false
}

// namesOwnTable reports whether a column is named as the identifier of the
// table it sits in, e.g. order_id inside orders.csv.
func namesOwnTable(tableName, columnName string) bool {
	base := strings.Trim(strings.TrimSuffix(strings.ToLower(columnName), "_id"), "_")
	if base == "" || base == strings.ToLower(columnName) {
		// Not an "_id" column at all, or nothing left once the suffix is gone.
		if strings.ToLower(columnName) != "id" {
			return false
		}
		return true // a bare "id" always identifies its own table
	}

	t := strings.ToLower(tableName)
	for _, form := range []string{base, base + "s", base + "es"} {
		if t == form || strings.HasPrefix(t, form+"_") {
			return true
		}
	}
	return false
}

// looksLikeIdentifier reports whether a column's name suggests it identifies
// something rather than describing it.
func looksLikeIdentifier(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"_id", "id", "code", "key", "ref", "number", "no", "sku", "uuid"} {
		if n == s || strings.HasSuffix(n, "_"+s) || strings.HasSuffix(n, s) {
			return true
		}
	}
	return false
}

// evaluateReference measures how well a proposed reference holds, and reports
// the rows that break it.
func evaluateReference(ctx context.Context, e *engine.Engine, c candidate) ([]finding.Finding, error) {
	childT := engine.Ident(c.child.table.Name)
	childC := engine.Ident(c.child.column.Name)
	parentT := engine.Ident(c.parent.table.Name)
	parentC := engine.Ident(c.parent.column.Name)

	// Compare on the normalised value so that a reference is not reported as
	// broken purely because of casing or padding — that is a separate finding
	// with a separate remedy.
	orphanPred := fmt.Sprintf(
		`%[1]s AND lower(trim(%[2]s)) NOT IN (SELECT lower(trim(%[3]s)) FROM %[4]s WHERE %[5]s)`,
		profile.SQLNonBlank(childC), childC, parentC, parentT, profile.SQLNonBlank(parentC))

	countQ := fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", childT, orphanPred)

	var orphans int64
	if err := e.ScanOne(ctx, countQ, []any{&orphans}); err != nil {
		return nil, err
	}

	populated := c.child.column.Populated()
	if populated == 0 {
		return nil, nil
	}
	containment := 1 - float64(orphans)/float64(populated)

	// Without a naming convention to back it, weak overlap means the two
	// columns are simply unrelated rather than that the reference is broken.
	if !c.nameMatch && containment < minContainment {
		return nil, nil
	}
	if orphans == 0 {
		return nil, nil // the relationship holds
	}
	// Even with matching names, near-total mismatch means the columns are not
	// really the same thing, and reporting every row as an orphan is noise.
	if containment < 0.2 {
		return nil, nil
	}

	childRef := c.child.table.Display + "." + c.child.column.Name
	parentRef := c.parent.table.Display + "." + c.parent.column.Name

	return []finding.Finding{{
		Rule:     "reference.orphan_values",
		Severity: finding.Error,
		Origin:   finding.OriginCheck,
		Title: fmt.Sprintf("%d value(s) in %s have no matching row in %s",
			orphans, childRef, parentRef),
		Detail: fmt.Sprintf(
			"%.0f%% of the values in %s appear in %s, so the two are evidently the same "+
				"identifier. The remaining %d do not match anything. Every one of those "+
				"rows will vanish from an inner join and appear as a blank in an outer "+
				"one, so any figure grouped by %s silently omits them.",
			containment*100, childRef, parentRef, orphans, parentRef),
		Remedy: "Establish whether the referenced records were omitted from this export or " +
			"deleted at source. Until then, reconcile totals against the child file, not the join.",
		Location: finding.Location{
			Table:   c.child.table.Name,
			Display: c.child.table.Display,
			Column:  c.child.column.Name,
		},
		Count: orphans,
		Total: populated,
		Evidence: finding.Evidence{
			CountQuery: countQ,
			RowQuery:   fmt.Sprintf("SELECT * FROM %s WHERE %s LIMIT 100", childT, orphanPred),
			Expected:   fmt.Sprintf("every value present in %s", parentRef),
			Observed:   fmt.Sprintf("%d of %d absent", orphans, populated),
		},
	}}, nil
}

// sharedDomainFindings reports a column that appears in several files with
// inconsistent contents.
//
// The same code list maintained in two places drifts, and a value spelled
// "EMEA" in one file and "emea" in another produces two rows in a report that
// should have one.
func sharedDomainFindings(ctx context.Context, e *engine.Engine, ds *profile.Dataset) ([]finding.Finding, error) {
	type occurrence struct {
		table  *profile.Table
		column *profile.Column
	}
	byName := make(map[string][]occurrence)

	for _, t := range ds.Tables {
		for _, c := range t.Columns {
			// Only categorical columns: a small, repeated set of values.
			if c.Populated() == 0 || c.DistinctNormalised > 50 {
				continue
			}
			// Identifiers are handled by the reference check, which can say
			// something far more useful about them than "these two lists
			// differ".
			if looksLikeIdentifier(c.Name) {
				continue
			}
			if int64(c.DistinctNormalised) >= c.Populated() {
				continue // every value differs; not a category
			}
			byName[strings.ToLower(c.Name)] = append(byName[strings.ToLower(c.Name)],
				occurrence{table: t, column: c})
		}
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []finding.Finding
	for _, name := range names {
		occs := byName[name]
		if len(occs) < 2 {
			continue
		}
		// Compare the first occurrence against each of the others.
		base := occs[0]
		for _, other := range occs[1:] {
			q := fmt.Sprintf(
				`SELECT count(*) FROM (
					SELECT DISTINCT lower(trim(%[1]s)) AS v FROM %[2]s WHERE %[3]s
					EXCEPT
					SELECT DISTINCT lower(trim(%[4]s)) AS v FROM %[5]s WHERE %[6]s
				)`,
				engine.Ident(other.column.Name), engine.Ident(other.table.Name),
				profile.SQLNonBlank(engine.Ident(other.column.Name)),
				engine.Ident(base.column.Name), engine.Ident(base.table.Name),
				profile.SQLNonBlank(engine.Ident(base.column.Name)))

			var extra int64
			if err := e.ScanOne(ctx, q, []any{&extra}); err != nil {
				return nil, err
			}
			if extra == 0 {
				continue
			}

			out = append(out, finding.Finding{
				Rule:     "domain.inconsistent_values",
				Severity: finding.Warning,
				Origin:   finding.OriginCheck,
				Title: fmt.Sprintf("%s in %s has %d value(s) not found in %s",
					other.column.Name, other.table.Display, extra, base.table.Display),
				Detail: fmt.Sprintf(
					"Both files have a %s column holding a small set of repeated values, so "+
						"they are evidently the same category. They do not agree on what "+
						"that set is. Either one file has values the other is missing, or "+
						"the same category is spelled differently in each — both of which "+
						"split a report's rows in ways nobody notices.",
					other.column.Name),
				Remedy: "Reconcile the two lists, and hold the category in one place if it is " +
					"meant to be a shared reference.",
				Location: finding.Location{
					Table:   other.table.Name,
					Display: other.table.Display,
					Column:  other.column.Name,
				},
				Count: extra,
				Total: int64(other.column.DistinctNormalised),
				Evidence: finding.Evidence{
					CountQuery: q,
					Expected:   fmt.Sprintf("the same value set as %s", base.table.Display),
					Observed:   fmt.Sprintf("%d additional value(s)", extra),
				},
			})
		}
	}
	return out, nil
}
