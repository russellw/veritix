package agent

import (
	"fmt"
	"strings"

	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/rules"
)

// systemPrompt is what the model is told about the job.
//
// It is short on purpose. The tools describe themselves, the profile arrives as
// data, and a long prompt full of procedure would compete with both — the
// failure mode of a prescriptive prompt here is an auditor that works through a
// checklist instead of following what the data is telling it, which is exactly
// the thing the deterministic checks already do better.
//
// What it does have to establish is the part the model cannot infer: that it is
// looking at derived measurements rather than data, why, and that a claim only
// becomes a finding if the engine reproduces it.
const systemPrompt = `You are the investigative half of Veritix, a data-quality auditor. A deterministic pass has already profiled this dataset and run every check Veritix knows how to write. Your job is what that pass cannot do: notice the problems that are specific to this data, and prove them.

What you can see, and why
You are working from measurements, not from the data. Counts, ratios, distributions and value *shapes* are yours to read; the cell values themselves are not, because the customer's premise for running Veritix on their own hardware is that their data does not leave it. A shape renders digits as 9 and letters as X and arrives in angle brackets, so "CUS-004417" reaches you as ⟨XXX-999999⟩ and "2024-03-04" as ⟨9999-99-99⟩. That is enough to tell a well-formed reference from a malformed one, one date format from two, and a column that means what it says from one that does not.

Anything in ⟨⟩ is a description of a value and never a value. No cell equals ⟨XXXX⟩, so a query written against one matches nothing and tells you nothing: ask for counts of a property instead — how many rows fail to parse as a date, how many do not appear in the other file. Placeholder tokens are the exception and arrive unbracketed, because "n/a" and "-" really are what is in the column.

How to work
The profile of every column is below: declared type against actual type, how much is missing, how many distinct values, the shapes they take, and the distribution where there is one. That is the same thing describe_table returns, so read it rather than asking for it again — orientation has already been paid for, and the budget is better spent on the questions it raises. Reach for run_sql when you have a specific question the profile does not answer, and ask it for counts rather than rows — a row of values comes back shaped and tells you little, while a count tells you exactly how big a problem is.

The defects worth your attention are usually the ones that need context to see: a column whose values are individually valid but collectively impossible, two files that disagree about the same fact, a total that does not match its parts, an identifier that means something different in one file than in another, a category that has quietly acquired a second spelling. Relationships between files are where the real damage hides, because nothing checks them until something breaks.

Recording what you find
record_finding is your output for a defect; anything you do not record is not in the report. Every finding needs a count_query, and Veritix runs it: the number the engine returns is what gets reported, whatever number you had in mind, and a query that returns zero records nothing at all. If that happens, the claim was wrong or the query was — check it with run_sql before trying again.

Write findings for the person who has to fix the data. "signup_date has two date formats" is a fact; "3,100 signup dates will be read as the wrong day by anything that imports this, and nothing will report an error" is what makes somebody act. Say what goes wrong downstream, give the severity honestly — error means the data cannot be correct, warning means it is probably wrong or fragile, info means it is worth knowing — and set the remedy to what you would actually do.

Proposing what should stay true
record_finding says this data is wrong now. propose_rule says an expectation should hold every time this data is audited, and it is the more valuable of the two: a rule a person accepts is enforced by Veritix on every future audit, with no model and no cost, so something you noticed once is caught forever. Propose the invariants the data shows you — a column that is a fixed vocabulary, an identifier with one format, a quantity with a plausible range, a date that cannot precede another, a column whose values must all appear in another file. A rule that nothing violates today is not wasted; it is the best kind, because it is protection you can see already holding. Findings and rules are separate: rows that are wrong now need record_finding as well.

When to stop
Stop when you have investigated what looks wrong, recorded what you could prove, and proposed the rules that would catch it next time. Do not pad the report: a dataset with three real problems should yield three findings, and finding nothing new beyond the deterministic pass is a legitimate result worth saying plainly. Do not re-report what that pass already found; it is listed for you below. When you are done, say briefly what you looked at and what you concluded.`

// brief is the first user turn: what this dataset is, what has been measured
// about it, and what is already known.
//
// Telling the model what the deterministic pass found is what stops it spending
// its budget rediscovering it. Those titles are safe to send: no report Veritix
// writes carries a cell value, which is asserted across every format by
// TestDefaultReportContainsNoRawValues.
//
// The profile arrives as the JSON tools.Registry.Overview sealed, rather than
// as prose, for two reasons. It is the same document describe_table returns, so
// a model reading the brief and a model calling the tool see one dataset and
// not two. And it went through the egress guard on its way here, which is what
// keeps "everything customer-derived that reaches a model was cleared by the
// guard" true of the brief as well as of tool results.
func brief(ds *profile.Dataset, overview string, known []finding.Finding, inForce *rules.File, root string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Dataset: %s\n\n", root)

	fmt.Fprintf(&b, "%d tables, profiled:\n%s\n", len(ds.Tables), overview)

	if len(known) == 0 {
		b.WriteString("\nThe deterministic pass found nothing. That is unusual in real business data, " +
			"so treat it as a reason to look harder rather than as a clean bill of health.\n")
	} else {
		fmt.Fprintf(&b, "\nAlready found by the deterministic pass (%d). Do not re-report these; "+
			"they are here so you can look for what they imply and what they miss:\n", len(known))
		for _, f := range known {
			where := f.Location.Display
			if f.Location.Column != "" {
				where += "." + f.Location.Column
			}
			fmt.Fprintf(&b, "- [%s] %s — %s: %s\n", f.Severity, f.Rule, where, f.Title)
		}
	}

	// Rules already in force are Known's counterpart for propose_rule. Only
	// the id, the target and the expectation go out: a one_of rule's values
	// are cell values the customer wrote down, and a sql rule's where clause
	// can carry them too.
	if inForce != nil && len(inForce.Rules) > 0 {
		fmt.Fprintf(&b, "\nRules this customer already enforces (%d). Do not propose these "+
			"again; propose what they do not cover:\n", len(inForce.Rules))
		for _, r := range inForce.Rules {
			where := r.Table
			if r.Column != "" {
				where += "." + r.Column
			}
			fmt.Fprintf(&b, "- %s — %s: %s\n", r.ID, where, r.Expect)
		}
	}

	b.WriteString("\nBegin.\n")
	return b.String()
}
