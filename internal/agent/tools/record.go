package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/russellwallace/veritix/internal/agent/llm"
	"github.com/russellwallace/veritix/internal/finding"
)

// recordFinding is the agent's only output.
//
// This is the mechanism the whole design turns on. The model does not report a
// number; it reports a claim and the query that would demonstrate it, Veritix
// runs the query, and what the engine measures is what gets recorded. A model
// that says "312 orders reference a customer who does not exist" and supplies a
// query returning 4 produces a finding about 4 rows. A model that supplies a
// query returning nothing produces no finding at all, and is told so, which is
// the fastest way for it to learn that guessing does not work here.
//
// Everything recorded goes on to be re-verified with the deterministic
// findings by finding.Set.Verify before any of it is reported, so this is the
// first of two independent measurements, not the only one.
func recordFinding() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "record_finding",
			Description: "Record a problem you have found. This is the only way to produce " +
				"output: anything you do not record here is not in the report. You must supply " +
				"a count_query — a SELECT returning one number, the count of affected rows — " +
				"and Veritix will run it. You must also state affected_count, the number you " +
				"believe it will return: if the two disagree, nothing is recorded and you are " +
				"told the real figure, and a query returning zero records nothing either. Run " +
				"the query with run_sql first and you will always agree with it. Write the " +
				"title and detail for the person who has to fix the data: say what will go " +
				"wrong downstream, not which check noticed it, and make sure any number in the " +
				"title is the one the query returns.",
			Properties: map[string]any{
				"rule": str("a short stable slug for this kind of problem, e.g. orphaned_reference " +
					"or mixed_currency_units; the same problem found again should use the same slug"),
				"severity": map[string]any{
					"type": "string",
					"enum": []string{"info", "warning", "error"},
					"description": "error for data that cannot be correct, warning for data that is " +
						"probably wrong or fragile, info for something worth knowing",
				},
				"table":  str("the table the problem is in"),
				"column": str("the column, when the problem is about one"),
				"title":  str("one specific line naming the problem, with numbers"),
				"detail": str("why it matters: what will go wrong downstream if it is not fixed"),
				"remedy": str("what to do about it"),
				"count_query": str("a SELECT returning exactly one row and one integer: the number " +
					"of affected rows. This is re-run before the report is written."),
				"affected_count": integer("how many rows you expect count_query to return. It is " +
					"checked against what it actually returns, and a disagreement records nothing."),
				"row_query": str("optional: a SELECT returning the offending rows themselves, so a " +
					"person can inspect them. It is never run automatically and never shown to you."),
				"expected": str("what should have been true, in words"),
				"observed": str("what was actually found, in words"),
			},
			Required: []string{"rule", "severity", "table", "title", "detail",
				"count_query", "affected_count"},
		},

		invoke: func(ctx context.Context, w *World, args json.RawMessage) (any, error) {
			var in struct {
				Rule       string `json:"rule"`
				Severity   string `json:"severity"`
				Table      string `json:"table"`
				Column     string `json:"column"`
				Title      string `json:"title"`
				Detail     string `json:"detail"`
				Remedy     string `json:"remedy"`
				CountQuery string `json:"count_query"`
				Claimed    *int64 `json:"affected_count"`
				RowQuery   string `json:"row_query"`
				Expected   string `json:"expected"`
				Observed   string `json:"observed"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}

			slug := slugify(in.Rule)
			if slug == "" {
				return nil, fmt.Errorf("give the finding a rule slug, e.g. orphaned_reference")
			}
			if strings.TrimSpace(in.Title) == "" {
				return nil, fmt.Errorf("give the finding a title")
			}
			severity, err := finding.ParseSeverity(in.Severity)
			if err != nil {
				return nil, err
			}

			t, err := w.table(in.Table)
			if err != nil {
				return nil, err
			}
			location := finding.Location{Table: t.Name, Display: t.Display}
			if in.Column != "" {
				c, err := w.column(t, in.Column)
				if err != nil {
					return nil, err
				}
				location.Column = c.Name
			}
			if t.Ingest != nil {
				location.File = t.Ingest.Ref.File.Rel
			}

			// The count query goes through the same parse as any other
			// model-authored SQL: it will be re-run at report time, and by
			// something that trusts it, so it has to be one read-only SELECT.
			analysis, err := w.Engine.AnalyzeSelect(ctx, in.CountQuery)
			if err != nil {
				return nil, fmt.Errorf("the count_query was refused: %v", w.Guard.EngineError(err))
			}
			if len(analysis.Aggregate) != 1 {
				return nil, fmt.Errorf(
					"the count_query returns %d columns; it must return exactly one, the number of affected rows",
					len(analysis.Aggregate))
			}

			count, err := w.scanCount(ctx, in.CountQuery)
			if err != nil {
				return nil, fmt.Errorf("the count_query did not run: %w", err)
			}

			if count == 0 {
				w.mu.Lock()
				w.refused++
				w.mu.Unlock()
				w.log().Info("declined a finding that did not reproduce",
					"rule", "agent."+slug, "table", t.Name, "column", location.Column)
				return nil, fmt.Errorf(
					"nothing was recorded: the count_query returned 0, so this problem does not "+
						"reproduce. Either the query is wrong, or the data is fine here. Check the "+
						"query against %s with run_sql before recording it again", t.Name)
			}

			// The claim and the measurement have to agree.
			//
			// Correcting the number silently is not enough, and the reason is
			// the title: it is model-authored prose, it usually contains the
			// figure, and it is the most prominent thing a reader sees. A
			// finding headed "400 orders have a negative amount" above a count
			// of 1 is worse than no finding, because it looks like Veritix
			// vouched for the 400. Requiring the model to state the number
			// separately turns a discrepancy that would have hidden in prose
			// into one the engine can see — and the refusal hands back the real
			// figure, so the retry says the right thing in every place at once.
			if in.Claimed == nil {
				return nil, fmt.Errorf(
					"affected_count is required: state how many rows you expect count_query to " +
						"return, so that a disagreement with the engine is caught rather than " +
						"printed")
			}
			if *in.Claimed != count {
				w.mu.Lock()
				w.refused++
				w.mu.Unlock()
				w.log().Info("declined a finding whose claim did not match the measurement",
					"rule", "agent."+slug, "table", t.Name,
					"claimed", *in.Claimed, "measured", count)
				return nil, fmt.Errorf(
					"nothing was recorded: you said %d rows, but the count_query returned %d. "+
						"If %d is right, record it again with that figure and with a title that "+
						"says %d; if not, fix the query", *in.Claimed, count, count, count)
			}

			if in.RowQuery != "" {
				// A row query is served to a person on request, so it has to be
				// safe to run later, when nothing is watching.
				if _, err := w.Engine.AnalyzeSelect(ctx, in.RowQuery); err != nil {
					return nil, fmt.Errorf("the row_query was refused: %v", w.Guard.EngineError(err))
				}
			}

			f := finding.Finding{
				Rule:     "agent." + slug,
				Severity: severity,
				Origin:   finding.OriginAgent,
				Title:    in.Title,
				Detail:   in.Detail,
				Remedy:   in.Remedy,
				Location: location,
				Count:    count,
				Total:    t.RowCount,
				Evidence: finding.Evidence{
					CountQuery: in.CountQuery,
					RowQuery:   in.RowQuery,
					Expected:   in.Expected,
					Observed:   in.Observed,
				},
			}

			w.mu.Lock()
			w.findings = append(w.findings, f)
			total := len(w.findings)
			w.mu.Unlock()

			w.log().Info("agent recorded a finding",
				"rule", f.Rule, "severity", severity.String(),
				"table", t.Name, "column", location.Column, "affected", count)

			return struct {
				Recorded      bool   `json:"recorded"`
				Rule          string `json:"rule"`
				MeasuredCount int64  `json:"measured_count"`
				OutOf         int64  `json:"out_of_rows"`
				FindingsSoFar int    `json:"findings_so_far"`
				Note          string `json:"note"`
			}{
				Recorded:      true,
				Rule:          f.Rule,
				MeasuredCount: count,
				OutOf:         t.RowCount,
				FindingsSoFar: total,
				Note: "measured_count is what the engine returned; it is the number that will " +
					"appear in the report, and the query will be run once more before it does",
			}, nil
		},
	}
}

// slugify reduces a model-supplied rule name to a stable identifier.
//
// The result becomes part of finding.Rule, which is hashed into the id that
// appears in URLs and in the audit trail, so it has to be predictable and
// contain nothing that came out of a data file.
func slugify(s string) string {
	var b strings.Builder
	lastUnderscore := true // suppresses a leading underscore
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastUnderscore = false
		case !lastUnderscore:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	const maxLen = 60
	if len(out) > maxLen {
		out = strings.Trim(out[:maxLen], "_")
	}
	return out
}
