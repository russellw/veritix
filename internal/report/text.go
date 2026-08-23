package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/russellw/veritix/internal/audit"
)

// WriteText renders a run for a terminal.
//
// The layout leads with what is wrong. A profile of a clean dataset is a wall
// of unremarkable numbers, and burying three real problems inside it is how a
// tool gets ignored.
func WriteText(w io.Writer, res *audit.Result, opts Options) error {
	return RenderText(w, Build(res, "", opts), opts)
}

// RenderText renders an already-built document, so that a caller which needs
// the document itself does not build a second one to print.
func RenderText(w io.Writer, doc *Document, opts Options) error {
	p := &printer{w: w}

	p.printf("Dataset: %s\n", doc.Dataset.Root)
	p.printf("  %s\n", summaryLine(doc))
	writeAgentText(p, doc.Agent)
	p.newline()

	writeComparisonText(p, doc.Comparison)
	writeFindingsText(p, doc)
	writeProposalsText(p, doc)

	if len(doc.Skipped) > 0 {
		p.printf("Files not read (%d)\n", len(doc.Skipped))
		for _, s := range doc.Skipped {
			p.printf("  %-28s %s\n", s.File, s.Reason)
		}
		p.newline()
	}

	for _, t := range doc.Tables {
		writeTable(p, t)
	}

	if !opts.IncludeValues {
		p.printf("\n%s\n", doc.Redacted.Note)
	}
	return p.err
}

// summaryLine renders the headline numbers out of the document rather than out
// of the run, so that a report printed from a stored document says exactly
// what one printed straight from an audit says. It is the same line
// audit.Summary.String produces, and TestATextReportFromADocumentMatchesTheRun
// pins that they agree.
func summaryLine(doc *Document) string {
	d := (time.Duration(doc.Run.DurationMS) * time.Millisecond).Round(time.Millisecond)
	out := fmt.Sprintf("%d files, %d tables, %d columns, %d rows in %s",
		doc.Dataset.FileCount, doc.Dataset.TableCount, doc.Dataset.ColumnCount,
		doc.Dataset.RowCount, d)
	if doc.Dataset.UnreadableRows > 0 {
		out += fmt.Sprintf(" (%d rows unreadable)", doc.Dataset.UnreadableRows)
	}
	if len(doc.Skipped) > 0 {
		out += fmt.Sprintf(", %d files skipped", len(doc.Skipped))
	}
	return out
}

// writeAgentText says a model was involved, and under what terms. A reader
// weighing a finding is entitled to know that before they read it, not in an
// appendix afterwards.
func writeAgentText(p *printer, a *AgentInfo) {
	if a == nil {
		return
	}

	p.printf("  investigated by %s (%s): %d steps, %d tool calls, %d findings\n",
		a.Model, a.Provider, a.Steps, a.ToolCalls, a.Findings)

	if a.ValuesSent {
		p.printf("  ! cell values were sent to the model for this run\n")
	} else {
		p.printf("  no cell values were sent to the model; %d were replaced by their shape\n",
			a.ValuesWithheld)
	}
	if a.ContextDocuments > 0 {
		p.printf("  it read %d of your own document(s) from %s; the trace has every byte\n",
			a.ContextDocuments, strings.Join(a.ContextServers, ", "))
	}
	if a.NotReproduced > 0 {
		p.printf("  %d proposed finding(s) did not reproduce and were discarded\n", a.NotReproduced)
	}
	if !a.Complete {
		p.printf("  ! the investigation stopped early (%s), so it may be incomplete\n", a.Stopped)
	}
}

// writeProposalsText renders the proposed rules.
//
// They sit after the findings and before the profile, in the same place in the
// document as in the argument: here is what is wrong, and here is what would
// catch it next time. The section states plainly that nothing has been
// applied, because a list of rules in an audit report reads as a list of rules
// in force unless it says otherwise.
func writeProposalsText(p *printer, doc *Document) {
	if len(doc.Proposals) == 0 {
		return
	}

	p.printf("%d rule(s) proposed for future audits\n", len(doc.Proposals))
	p.printf("  %s\n", wrap("None of these is in force. Each one is a suggestion, "+
		"measured against this data; accept one into your rules file and Veritix "+
		"enforces it on every audit from then on, with no model involved.", 2))

	for _, pr := range doc.Proposals {
		p.printf("\n  %s\n", pr.Description)
		p.printf("    %s on %s   [%s, %s]\n", pr.Expect, pr.Target, pr.Rule, pr.Severity)

		switch pr.ViolationsNow {
		case 0:
			p.printf("    nothing breaks it today\n")
		case 1:
			p.printf("    1 row breaks it today\n")
		default:
			p.printf("    %d rows break it today\n", pr.ViolationsNow)
		}
		if pr.PermittedValueCount > 0 && len(pr.PermittedValues) == 0 {
			p.printf("    %s\n", wrap(fmt.Sprintf(
				"permits the %d values the column holds today; they are not listed here, "+
					"and have to be read before this is accepted", pr.PermittedValueCount), 4))
		}
		if len(pr.PermittedValues) > 0 {
			p.printf("    permits: %s\n", wrap(strings.Join(pr.PermittedValues, ", "), 4))
		}
		if pr.Rationale != "" {
			p.printf("    %s\n", wrap(pr.Rationale, 4))
		}
	}
	p.newline()
}

func writeTable(p *printer, t TableInfo) {
	p.printf("── %s ", t.Source)
	p.printf("%s\n", strings.Repeat("─", maxInt(0, 60-len(t.Source))))
	p.printf("   %d rows, %d columns", t.RowCount, len(t.Columns))
	if t.Reading != nil {
		p.printf("   [%s]", describeReading(*t.Reading))
	}
	p.newline()

	if t.Rejected != nil {
		p.printf("   ! %d rows could not be read and are missing from every total\n",
			t.Rejected.Count)
		for _, s := range t.Rejected.Samples[:minInt(3, len(t.Rejected.Samples))] {
			p.printf("       line %d: %s\n", s.Line, strings.ToLower(s.Reason))
		}
	}
	for _, n := range t.Notes {
		// The rejected-rows block above already says this, at more length.
		if n.Code == "ingest.rejected_rows" {
			continue
		}
		p.printf("   ! %s\n", wrap(n.Message, 7))
	}
	p.newline()

	// The tabwriter writes through the printer, so a failure inside the
	// column table is latched with the rest rather than lost at Flush.
	tw := tabwriter.NewWriter(p, 0, 0, 2, ' ', 0)
	tp := &printer{w: tw}
	tp.printf("   COLUMN\tTYPE\tMISSING\tDISTINCT\tNOTES\n")

	for _, c := range t.Columns {
		tp.printf("   %s\t%s\t%s\t%s\t%s\n",
			c.Name,
			describeType(c),
			describeMissing(c),
			describeDistinct(c),
			strings.Join(columnFlags(c), "; "))
	}
	if err := tw.Flush(); err != nil && p.err == nil {
		p.err = err
	}
	p.newline()
}

func describeReading(r ReadingInfo) string {
	switch r.Format {
	case "delimited":
		parts := []string{"delimiter " + describeDelimiter(r.Delimiter)}
		if r.Encoding != "" && r.Encoding != "utf-8" {
			parts = append(parts, "encoding "+r.Encoding)
		}
		if !r.HasHeader {
			parts = append(parts, "no header")
		}
		return strings.Join(parts, ", ")
	case "excel":
		if r.HeaderRow > 1 {
			return fmt.Sprintf("excel, header on row %d", r.HeaderRow)
		}
		return "excel"
	default:
		return r.Format
	}
}

func describeDelimiter(d string) string {
	switch d {
	case "\t":
		return "tab"
	case ",":
		return "comma"
	case ";":
		return "semicolon"
	case "|":
		return "pipe"
	case "":
		return "none"
	default:
		return fmt.Sprintf("%q", d)
	}
}

func describeType(c ColumnInfo) string {
	t := c.InferredType
	if c.Nonconforming > 0 {
		t += fmt.Sprintf(" (%d bad)", c.Nonconforming)
	} else if c.ClosestType != "" && c.ClosestMatch >= 0.5 {
		t += fmt.Sprintf(" (%.0f%% %s)", c.ClosestMatch*100, c.ClosestType)
	}
	return t
}

func describeMissing(c ColumnInfo) string {
	if c.Rows == 0 || c.Missing == 0 {
		return "-"
	}
	return fmt.Sprintf("%d (%.0f%%)", c.Missing, float64(c.Missing)/float64(c.Rows)*100)
}

func describeDistinct(c ColumnInfo) string {
	if c.Unique {
		return fmt.Sprintf("%d (unique)", c.Distinct)
	}
	return fmt.Sprint(c.Distinct)
}

// columnFlags lists the things about a column a reader should look at. Only
// the irregularities: a column with nothing to say gets a blank cell rather
// than a row of zeroes.
func columnFlags(c ColumnInfo) []string {
	var flags []string

	if n := len(c.Sentinels); n > 0 {
		names := make([]string, 0, n)
		for _, s := range c.Sentinels {
			names = append(names, fmt.Sprintf("%q×%d", s.Value, s.Count))
		}
		sort.Strings(names)
		flags = append(flags, "placeholders "+strings.Join(names, " "))
	}
	if c.LeadingWhitespace+c.TrailingWhitespace > 0 {
		flags = append(flags, fmt.Sprintf("%d values padded with spaces",
			c.LeadingWhitespace+c.TrailingWhitespace))
	}
	if c.Distinct > c.DistinctNormalized {
		flags = append(flags, fmt.Sprintf("%d case/spacing variants",
			c.Distinct-c.DistinctNormalized))
	}
	if c.Temporal != nil {
		if len(c.Temporal.Formats) > 1 {
			flags = append(flags, fmt.Sprintf("%d date formats mixed", len(c.Temporal.Formats)))
		}
		if c.Temporal.Ambiguous > 0 {
			flags = append(flags, fmt.Sprintf("%d ambiguous dates", c.Temporal.Ambiguous))
		}
		if c.Temporal.Implausible > 0 {
			flags = append(flags, fmt.Sprintf("%d implausible dates", c.Temporal.Implausible))
		}
		if c.Temporal.Future > 0 {
			flags = append(flags, fmt.Sprintf("%d dates in the future", c.Temporal.Future))
		}
	}
	if c.Numeric != nil {
		if c.Numeric.Outliers > 0 {
			flags = append(flags, fmt.Sprintf("%d outliers", c.Numeric.Outliers))
		}
		if c.Numeric.Negative > 0 {
			flags = append(flags, fmt.Sprintf("%d negative", c.Numeric.Negative))
		}
	}
	if c.Original != "" {
		flags = append(flags, fmt.Sprintf("renamed from %q", c.Original))
	}
	return flags
}

// wrap indents continuation lines of a long message.
func wrap(s string, indent int) string {
	const width = 72
	if len(s) <= width {
		return s
	}
	var b strings.Builder
	line := 0
	pad := strings.Repeat(" ", indent)
	for _, word := range strings.Fields(s) {
		if line > 0 && line+len(word)+1 > width {
			b.WriteString("\n" + pad)
			line = 0
		} else if line > 0 {
			b.WriteByte(' ')
			line++
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
