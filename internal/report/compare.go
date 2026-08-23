package report

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// A comparison is between two report documents, and deliberately not between
// two sets of rows in the store.
//
// The document is what every entry point already produces and what the store
// already keeps whole, so a diff computed from it cannot disagree with the
// report it sits in — the same argument that has the API serve the stored
// document verbatim rather than rebuilding it. It also means the CLI, which
// has no store at all, compares against a JSON report from a previous run and
// gets the same answer the server would.
//
// The join key is finding.Finding.ID, which is a digest of what a finding is
// about rather than where it landed in a list. That is what makes "the same
// problem" a question with an answer: the id survives a re-run that finds one
// more error alongside it. It is also the limit — a finding is matched by
// rule, table, column and line, so a renamed column is a resolved finding and
// a new one rather than a moved one, and the notes say so when the schema
// moved underneath.

// DeltaStatus is what happened to one finding between two runs.
type DeltaStatus string

const (
	// DeltaNew is a finding this run has and the baseline did not.
	DeltaNew DeltaStatus = "new"
	// DeltaWorsened is the same finding affecting more rows than before.
	DeltaWorsened DeltaStatus = "worsened"
	// DeltaResolved is a finding the baseline had and this run does not.
	DeltaResolved DeltaStatus = "resolved"
	// DeltaImproved is the same finding affecting fewer rows than before.
	DeltaImproved DeltaStatus = "improved"
	// DeltaUnchanged is the same finding affecting the same number of rows.
	DeltaUnchanged DeltaStatus = "unchanged"
)

// rank orders the statuses by how much they want reading.
func (s DeltaStatus) rank() int {
	switch s {
	case DeltaNew:
		return 0
	case DeltaWorsened:
		return 1
	case DeltaResolved:
		return 2
	case DeltaImproved:
		return 3
	default:
		return 4
	}
}

// TableChange is what happened to one table between two runs.
type TableChange string

const (
	// TableAdded is a table this run read and the baseline did not.
	TableAdded TableChange = "added"
	// TableRemoved is a table the baseline read and this run did not.
	TableRemoved TableChange = "removed"
	// TableChanged is a table whose row count or columns moved.
	TableChanged TableChange = "changed"
)

// Baseline names a previous run to compare against.
type Baseline struct {
	// Document is the previous run's report. Required.
	Document *Document
	// RunID identifies it when it came from the store.
	RunID string
	// Source names where it came from when it was loaded from a file.
	Source string
}

// Delta is what changed since a previous audit of the same dataset.
//
// It is part of the report rather than a separate document because the
// question it answers — "is this getting better or worse" — is the one a
// person asks on the second and every subsequent audit, and an answer that
// arrives in a different file from the findings is an answer nobody reads.
type Delta struct {
	Baseline BaselineInfo `json:"baseline"`
	Summary  DeltaSummary `json:"summary"`

	// Findings lists what moved. A finding that is unchanged in both count and
	// severity is counted in the summary and not repeated here: it is already
	// in the document's own findings list, and duplicating every one of them
	// would double the report to say that nothing happened.
	Findings []FindingDelta `json:"findings,omitempty"`

	// Tables lists schema and volume drift: a table that appeared or vanished,
	// a row count that moved, a column that is no longer in the export. No
	// single-run check can see any of it, and an export that quietly lost a
	// third of its rows is a worse problem than anything in the findings.
	Tables []TableDelta `json:"tables,omitempty"`

	// Notes are caveats about the comparison itself, for a reader deciding how
	// much to trust it.
	Notes []string `json:"notes,omitempty"`
}

// BaselineInfo describes the run being compared against.
type BaselineInfo struct {
	RunID     string    `json:"run_id,omitempty"`
	Source    string    `json:"source,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"veritix_version,omitempty"`
	Root      string    `json:"root,omitempty"`
}

// DeltaSummary counts findings by what happened to them.
type DeltaSummary struct {
	New       int `json:"new"`
	Worsened  int `json:"worsened"`
	Resolved  int `json:"resolved"`
	Improved  int `json:"improved"`
	Unchanged int `json:"unchanged"`

	// NewErrors and NewWarnings are what a CI gate acts on: a build should be
	// able to fail on a regression without failing on the debt that was
	// already there when somebody turned Veritix on.
	NewErrors   int `json:"new_errors"`
	NewWarnings int `json:"new_warnings"`
}

// Changed reports whether anything moved at all.
func (s DeltaSummary) Changed() bool {
	return s.New+s.Worsened+s.Resolved+s.Improved > 0
}

// FindingDelta is one finding's history across the two runs.
type FindingDelta struct {
	ID     string      `json:"id"`
	Rule   string      `json:"rule"`
	Status DeltaStatus `json:"status"`

	// Severity is the current one, or the baseline's for a resolved finding.
	// SeverityBefore is set only when it moved, which happens when a rule's
	// severity was edited between the two runs.
	Severity       string `json:"severity"`
	SeverityBefore string `json:"severity_before,omitempty"`

	// Title is the current wording, or the baseline's for a resolved finding.
	Title  string `json:"title"`
	Table  string `json:"table,omitempty"`
	Source string `json:"source,omitempty"`
	Column string `json:"column,omitempty"`

	CountBefore int64 `json:"affected_count_before"`
	CountAfter  int64 `json:"affected_count_after"`
}

// TableDelta is one table's drift across the two runs.
type TableDelta struct {
	Name   string      `json:"name"`
	Source string      `json:"source,omitempty"`
	Change TableChange `json:"change"`

	RowsBefore int64 `json:"row_count_before"`
	RowsAfter  int64 `json:"row_count_after"`

	ColumnsAdded   []string `json:"columns_added,omitempty"`
	ColumnsRemoved []string `json:"columns_removed,omitempty"`
}

// Compare works out what changed between two runs of the same dataset.
//
// Both arguments are required; the caller stamps the baseline's provenance
// onto the result, because only the caller knows whether it came out of the
// store or off the disk.
func Compare(before, after *Document) *Delta {
	d := &Delta{
		Baseline: BaselineInfo{
			StartedAt: before.Run.StartedAt,
			Version:   before.Version,
			Root:      before.Dataset.Root,
		},
	}

	if before.Dataset.Root != after.Dataset.Root && before.Dataset.Root != "" {
		d.Notes = append(d.Notes, fmt.Sprintf(
			"the baseline audited %s and this run audited %s, so this compares two "+
				"different roots", before.Dataset.Root, after.Dataset.Root))
	}
	if before.Schema != after.Schema && before.Schema != "" {
		d.Notes = append(d.Notes, fmt.Sprintf(
			"the baseline was written against report schema %s and this run against %s",
			before.Schema, after.Schema))
	}

	d.Findings = compareFindings(before, after, &d.Summary)
	d.Tables = compareTables(before, after)

	// Schema drift is the one thing that makes the finding comparison itself
	// unreliable, because a finding is identified partly by where it sits: a
	// column that is no longer in the export takes its findings with it, and
	// they show up as resolved. Say so rather than let somebody read a removed
	// column as a fixed defect.
	for _, t := range d.Tables {
		if t.Change == TableRemoved || len(t.ColumnsRemoved) > 0 {
			d.Notes = append(d.Notes, "some tables or columns are no longer in the export, "+
				"so findings about them are reported as resolved: check the table changes below "+
				"before reading that as data that was cleaned up")
			break
		}
	}

	return d
}

func compareFindings(before, after *Document, sum *DeltaSummary) []FindingDelta {
	old := make(map[string]FindingInfo, len(before.Findings))
	for _, f := range before.Findings {
		old[f.ID] = f
	}

	var out []FindingDelta
	for _, f := range after.Findings {
		prev, existed := old[f.ID]
		delete(old, f.ID)

		fd := FindingDelta{
			ID:          f.ID,
			Rule:        f.Rule,
			Severity:    f.Severity,
			Title:       f.Title,
			Table:       f.Table,
			Source:      f.Source,
			Column:      f.Column,
			CountAfter:  f.Count,
			CountBefore: prev.Count,
		}

		switch {
		case !existed:
			fd.Status = DeltaNew
			fd.CountBefore = 0
			sum.New++
			switch f.Severity {
			case "error":
				sum.NewErrors++
			case "warning":
				sum.NewWarnings++
			}
		case f.Count > prev.Count:
			fd.Status = DeltaWorsened
			sum.Worsened++
		case f.Count < prev.Count:
			fd.Status = DeltaImproved
			sum.Improved++
		default:
			fd.Status = DeltaUnchanged
			sum.Unchanged++
		}

		if existed && prev.Severity != f.Severity {
			fd.SeverityBefore = prev.Severity
		}

		// An unchanged finding is already in the document's findings list. It
		// is repeated here only when its severity moved, which is a change a
		// reader would otherwise have no way to see.
		if fd.Status != DeltaUnchanged || fd.SeverityBefore != "" {
			out = append(out, fd)
		}
	}

	// Whatever is left in the baseline was not found this time.
	for _, f := range old {
		sum.Resolved++
		out = append(out, FindingDelta{
			ID:          f.ID,
			Rule:        f.Rule,
			Status:      DeltaResolved,
			Severity:    f.Severity,
			Title:       f.Title,
			Table:       f.Table,
			Source:      f.Source,
			Column:      f.Column,
			CountBefore: f.Count,
		})
	}

	sortFindingDeltas(out)
	return out
}

// sortFindingDeltas puts the deltas in the order somebody reads them: what
// appeared, then what got worse, then what went away, and within each the
// worst severity first. Ties break on rule and location so the order is stable
// between runs, because a report that reshuffles itself is a report nobody can
// diff by eye.
func sortFindingDeltas(ds []FindingDelta) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.Status != b.Status {
			return a.Status.rank() < b.Status.rank()
		}
		if a.Severity != b.Severity {
			return severityRank(a.Severity) < severityRank(b.Severity)
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		return a.Column < b.Column
	})
}

func severityRank(s string) int {
	switch s {
	case "error":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

func compareTables(before, after *Document) []TableDelta {
	old := make(map[string]TableInfo, len(before.Tables))
	for _, t := range before.Tables {
		old[t.Name] = t
	}

	var out []TableDelta
	for _, t := range after.Tables {
		prev, existed := old[t.Name]
		delete(old, t.Name)

		if !existed {
			out = append(out, TableDelta{
				Name: t.Name, Source: t.Source, Change: TableAdded, RowsAfter: t.RowCount,
			})
			continue
		}

		td := TableDelta{
			Name: t.Name, Source: t.Source, Change: TableChanged,
			RowsBefore: prev.RowCount, RowsAfter: t.RowCount,
		}
		td.ColumnsAdded, td.ColumnsRemoved = columnDiff(prev.Columns, t.Columns)
		if td.RowsBefore == td.RowsAfter &&
			len(td.ColumnsAdded) == 0 && len(td.ColumnsRemoved) == 0 {
			continue // nothing moved; not worth a line
		}
		out = append(out, td)
	}

	for _, t := range old {
		out = append(out, TableDelta{
			Name: t.Name, Source: t.Source, Change: TableRemoved, RowsBefore: t.RowCount,
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// columnDiff reports which columns appeared and which vanished, by name.
//
// A column is matched by name and not by position, because a reordered export
// is normal and a renamed one is the thing worth reporting. Both lists are
// sorted so that two runs over unchanged files produce identical reports.
func columnDiff(before, after []ColumnInfo) (added, removed []string) {
	old := make(map[string]bool, len(before))
	for _, c := range before {
		old[c.Name] = true
	}
	for _, c := range after {
		if old[c.Name] {
			delete(old, c.Name)
			continue
		}
		added = append(added, c.Name)
	}
	for name := range old {
		removed = append(removed, name)
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// LoadDocument reads a JSON report written by a previous run.
//
// It is how the CLI supplies a baseline: `veritix audit` has no run store, so
// the previous report is a file the customer kept, which is also what a CI job
// has — the artifact from the last build on the main branch.
func LoadDocument(path string) (*Document, error) {
	body, err := os.ReadFile(path) //nolint:gosec // the path is the operator's choice
	if err != nil {
		return nil, fmt.Errorf("reading the baseline report: %w", err)
	}
	var doc Document
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("reading the baseline report %s: %w", path, err)
	}
	if doc.Schema == "" {
		return nil, fmt.Errorf(
			"%s is not a Veritix JSON report: write one with `veritix audit --format json`", path)
	}
	return &doc, nil
}

// writeComparisonText renders the comparison at the top of a terminal report.
//
// It goes above the findings because on the second and every later audit it is
// the first question: three errors is a number, three errors that were two
// last week is a direction. A run with no baseline prints nothing at all, so a
// first audit reads exactly as it did before.
func writeComparisonText(p *printer, d *Delta) {
	if d == nil {
		return
	}

	when := d.Baseline.StartedAt.Format("2006-01-02 15:04")
	switch {
	case d.Baseline.RunID != "":
		p.printf("Since the previous audit (%s, run %s)\n", when, d.Baseline.RunID)
	case d.Baseline.Source != "":
		p.printf("Since %s (%s)\n", d.Baseline.Source, when)
	default:
		p.printf("Since the previous audit (%s)\n", when)
	}

	s := d.Summary
	if !s.Changed() && len(d.Tables) == 0 {
		p.printf("  nothing changed: the same %d finding(s), unchanged\n\n", s.Unchanged)
		return
	}
	p.printf("  %d new, %d worse, %d resolved, %d improved, %d unchanged\n",
		s.New, s.Worsened, s.Resolved, s.Improved, s.Unchanged)

	for _, n := range d.Notes {
		p.printf("  ! %s\n", wrap(n, 4))
	}

	for _, f := range d.Findings {
		where := f.Source
		if where == "" {
			where = f.Table
		}
		if f.Column != "" {
			where += "." + f.Column
		}
		p.printf("\n  %-9s %s\n", label(f.Status), f.Title)
		p.printf("            at %s   [%s]\n", where, f.Rule)

		switch f.Status {
		case DeltaWorsened, DeltaImproved:
			p.printf("            affected rows %d → %d\n", f.CountBefore, f.CountAfter)
		case DeltaResolved:
			p.printf("            was %s, affecting %d\n", f.Severity, f.CountBefore)
		case DeltaNew, DeltaUnchanged:
		}
		if f.SeverityBefore != "" {
			p.printf("            severity %s → %s\n", f.SeverityBefore, f.Severity)
		}
	}

	writeTableDeltasText(p, d.Tables)
	p.newline()
}

// label renders a status as a column heading a reader can scan down.
func label(s DeltaStatus) string {
	switch s {
	case DeltaWorsened:
		return "WORSE"
	case DeltaImproved:
		return "BETTER"
	default:
		return strings.ToUpper(string(s))
	}
}

// writeTableDeltasText renders volume and schema drift.
//
// It is part of the comparison rather than a section of its own because it is
// the same question — what changed — and because on its own a row count means
// nothing. An export that lost a third of its rows overnight is usually a
// broken extract rather than a business event, and no check that looks at one
// run can possibly say so.
func writeTableDeltasText(p *printer, ts []TableDelta) {
	if len(ts) == 0 {
		return
	}
	p.printf("\n  Tables\n")
	for _, t := range ts {
		name := t.Source
		if name == "" {
			name = t.Name
		}
		switch t.Change {
		case TableAdded:
			p.printf("    + %s is new (%d rows)\n", name, t.RowsAfter)
		case TableRemoved:
			p.printf("    - %s is no longer in the dataset (had %d rows)\n", name, t.RowsBefore)
		case TableChanged:
			if t.RowsBefore != t.RowsAfter {
				p.printf("    %s %d → %d rows (%+d)\n",
					name, t.RowsBefore, t.RowsAfter, t.RowsAfter-t.RowsBefore)
			}
			if len(t.ColumnsRemoved) > 0 {
				p.printf("    %s lost column(s): %s\n", name, strings.Join(t.ColumnsRemoved, ", "))
			}
			if len(t.ColumnsAdded) > 0 {
				p.printf("    %s gained column(s): %s\n", name, strings.Join(t.ColumnsAdded, ", "))
			}
		}
	}
}

// Regressions counts the findings this run introduced or made worse, at or
// above the given severity.
//
// It is the one definition of "a regression", used by the CLI's gate and
// available to anything else that has to decide whether an audit is worse than
// the last one. A worsened finding counts: three orphan references becoming
// three hundred is not the same problem at a different size, it is a problem
// that got away from somebody.
func (d *Delta) Regressions(minSeverity string) int {
	if d == nil {
		return 0
	}
	want := severityRank(minSeverity)
	var n int
	for _, f := range d.Findings {
		if f.Status != DeltaNew && f.Status != DeltaWorsened {
			continue
		}
		if severityRank(f.Severity) <= want {
			n++
		}
	}
	return n
}
