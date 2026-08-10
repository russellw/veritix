package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/russellwallace/veritix/internal/audit"
	"github.com/russellwallace/veritix/internal/finding"
)

// FindingInfo is one finding in the JSON report.
type FindingInfo struct {
	// ID addresses this finding in the API. It is derived from what the
	// finding is about, so it survives a re-run that finds more or fewer
	// problems alongside it.
	ID       string `json:"id"`
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Origin   string `json:"origin"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	Remedy   string `json:"remedy,omitempty"`

	Table  string `json:"table,omitempty"`
	Source string `json:"source,omitempty"`
	Column string `json:"column,omitempty"`
	File   string `json:"file,omitempty"`
	Line   int64  `json:"line,omitempty"`

	Count int64   `json:"affected_count,omitempty"`
	Total int64   `json:"total,omitempty"`
	Share float64 `json:"affected_share,omitempty"`

	Expected string `json:"expected,omitempty"`
	Observed string `json:"observed,omitempty"`
	// Query is the statement that reproduces the finding. It is included so a
	// reader can check the claim themselves rather than take it on trust,
	// which is the difference between a report and an assertion.
	Query    string `json:"evidence_query,omitempty"`
	Verified bool   `json:"verified"`
}

// FindingSummary counts findings by severity.
type FindingSummary struct {
	Total    int `json:"total"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
}

func buildFindings(res *audit.Result) ([]FindingInfo, FindingSummary) {
	var summary FindingSummary
	if res.Findings == nil {
		return nil, summary
	}

	all := res.Findings.All()
	out := make([]FindingInfo, 0, len(all))

	for _, f := range all {
		out = append(out, FindingInfo{
			ID:       f.ID(),
			Rule:     f.Rule,
			Severity: f.Severity.String(),
			Origin:   string(f.Origin),
			Title:    f.Title,
			Detail:   f.Detail,
			Remedy:   f.Remedy,
			Table:    f.Location.Table,
			Source:   f.Location.Display,
			Column:   f.Location.Column,
			File:     f.Location.File,
			Line:     f.Location.Line,
			Count:    f.Count,
			Total:    f.Total,
			Share:    round(f.Share()),
			Expected: f.Evidence.Expected,
			Observed: f.Evidence.Observed,
			Query:    f.Evidence.CountQuery,
			Verified: f.Verified,
		})
	}

	counts := res.Findings.Counts()
	summary = FindingSummary{
		Total:    res.Findings.Len(),
		Errors:   counts[finding.Error],
		Warnings: counts[finding.Warning],
		Info:     counts[finding.Info],
	}
	return out, summary
}

// writeFindingsText renders the findings section of a terminal report.
//
// Findings come first and the profile second. A profile of a clean dataset is
// a wall of unremarkable numbers, and burying three real problems inside it is
// how a tool gets ignored.
func writeFindingsText(w io.Writer, doc *Document) {
	if len(doc.Findings) == 0 {
		fmt.Fprintf(w, "No problems found.\n\n")
		return
	}

	s := doc.FindingSummary
	fmt.Fprintf(w, "%d problem(s) found: %d error, %d warning, %d informational\n\n",
		s.Total, s.Errors, s.Warnings, s.Info)

	var current string
	for _, f := range doc.Findings {
		if f.Severity != current {
			current = f.Severity
			fmt.Fprintf(w, "%s\n%s\n", strings.ToUpper(f.Severity), strings.Repeat("─", len(f.Severity)))
		}

		where := f.Source
		if f.Column != "" {
			where += "." + f.Column
		}
		fmt.Fprintf(w, "\n  %s\n", f.Title)
		fmt.Fprintf(w, "    at %s   [%s]\n", where, f.Rule)

		if f.Detail != "" {
			fmt.Fprintf(w, "    %s\n", wrap(f.Detail, 4))
		}
		if f.Remedy != "" {
			fmt.Fprintf(w, "    → %s\n", wrap(f.Remedy, 6))
		}
	}
	fmt.Fprintln(w)
}
