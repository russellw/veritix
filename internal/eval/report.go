package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/russellw/veritix/internal/agent"
)

// WriteText renders a scorecard for a terminal.
//
// It leads with the two numbers that are easy to conflate. A single figure for
// "how good is this model at auditing" would be the wrong shape of answer:
// what an operator is choosing between is a model that reliably finds the same
// two defects and one that finds two of five at random, and those can share a
// mean.
func WriteText(w io.Writer, s Score) error {
	p := &printer{w: w}

	p.printf("Dataset: %s\n", s.Dataset)
	writeChecksText(p, s)
	writeAgentText(p, s)
	writeRunsText(p, s)
	return p.err
}

func writeChecksText(p *printer, s Score) {
	c := s.Checks
	p.printf("\nDeterministic checks\n")
	p.printf("  %d of %d planted defects found, %d false positives\n",
		len(c.Found), len(c.Found)+len(c.Missed), len(c.FalsePositives))

	for _, d := range c.Missed {
		p.printf("  MISSED  %-28s %s should have caught it\n", d.ID, d.CaughtBy)
		p.printf("          %s\n", wrap(d.Why, 10))
	}
	for _, f := range c.FalsePositives {
		p.printf("  FIRED   %-28s %s is clean here\n", f.Rule, f.Where)
		p.printf("          %s\n", wrap(f.Why, 10))
	}
	if s.ChecksUnstable {
		p.printf("  ! the deterministic pass did not agree with itself across runs\n")
	}
	if len(c.Uncovered) > 0 {
		p.printf("  %d defect(s) no check proposes; those are the agent's to find\n",
			len(c.Uncovered))
	}
}

func writeAgentText(p *printer, s Score) {
	if len(s.Targets) == 0 {
		return
	}

	if s.Model == "" {
		p.printf("\nAgent  none configured, so nothing below was in reach of this run\n")
	} else {
		p.printf("\nAgent  %s via %s, %s\n", s.Model, s.Provider, plural(len(s.Runs), "run"))
		p.printf("  mean recall  %5.0f%%   what one audit finds\n", 100*s.MeanRecall())
		p.printf("  coverage     %5.0f%%   what %s find between them\n",
			100*s.Coverage(), plural(len(s.Runs), "run"))
	}

	for _, t := range s.Targets {
		// With no model the hit count would read as two failed attempts rather
		// than as two defects nothing tried to find.
		hits := "   -  "
		if s.Model != "" {
			hits = fmt.Sprintf("%2d/%-2d ", t.Hits, t.Runs)
		}
		p.printf("  %s %-28s %s\n", hits, t.Defect.ID, t.Defect.Where)
		p.printf("         %s\n", wrap(t.Defect.Why, 9))
	}

	var restated, novel []Claim
	for _, c := range s.UnclassifiedClaims() {
		if c.Covers != "" {
			restated = append(restated, c)
		} else {
			novel = append(novel, c)
		}
	}

	if len(restated) > 0 {
		// Not a mistake, and not the job either. The check tools tell the model
		// when a defect is already covered, so this line is how well that lands.
		p.printf("\n  Also recorded, where a check had already found it:\n")
		for _, c := range restated {
			p.printf("    %-32s %-28s %s\n", c.Rule, c.Where, c.Covers)
		}
	}
	if len(novel) > 0 {
		// Measured and verified, so true; simply not on the list. Only a person
		// can say whether that is a defect the manifest is missing or a model
		// calling something trivial a problem.
		p.printf("\n  Recorded and not on the manifest, measured by the engine:\n")
		for _, c := range novel {
			p.printf("    %-32s %-28s %d row(s)\n", c.Rule, c.Where, c.Count)
		}
	}
}

func writeRunsText(p *printer, s Score) {
	// Per-run detail is about the model. With no model configured every run is
	// the same run, and the deterministic score above has already said so.
	if len(s.Runs) == 0 || (s.Model == "" && !s.anyRunFailed()) {
		return
	}
	p.printf("\nRuns\n")
	for i, r := range s.Runs {
		if r.Err != "" {
			p.printf("  %2d  failed: %s\n", i+1, r.Err)
			continue
		}
		p.printf("  %2d  %s\n", i+1, describeRun(r))
	}
}

func describeRun(r RunScore) string {
	out := fmt.Sprintf("%d of %d found", len(r.Detected), len(r.Detected)+len(r.Missed))
	t := r.Trace
	if t == nil {
		return out + ", no model"
	}
	out += fmt.Sprintf(", %s, %s, %d recorded",
		plural(len(t.Steps), "step"), plural(toolCalls(t), "tool call"), t.Findings)
	if t.Refused > 0 {
		out += fmt.Sprintf(", %d refused", t.Refused)
	}
	out += fmt.Sprintf(", %d tokens, %s", t.Usage.Total(), t.Duration.Round(time.Second))
	if !t.Stopped.Complete() {
		out += " (" + string(t.Stopped) + ")"
	}
	if t.Error != "" {
		out += ": " + t.Error
	}
	return out
}

func toolCalls(t *agent.Trace) int {
	var n int
	for _, s := range t.Steps {
		n += len(s.Calls)
	}
	return n
}

// WriteJSON renders a scorecard for a machine: a run recorded over months is
// how a model choice gets defended later, and that wants a stable shape rather
// than a terminal layout somebody has to parse back.
func WriteJSON(w io.Writer, s Score) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(document(s))
}

// The JSON shape is written out as its own types rather than tagged onto the
// scoring types, so that the file format is decided in one place and does not
// change every time a field is added for the terminal's benefit.
type doc struct {
	Dataset  string    `json:"dataset"`
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	Checks   docChecks `json:"checks"`
	Agent    docAgent  `json:"agent"`
	Runs     []docRun  `json:"runs"`
}

type docChecks struct {
	Found          int        `json:"found"`
	Total          int        `json:"total"`
	Missed         []string   `json:"missed"`
	FalsePositives []docClaim `json:"false_positives"`
	Uncovered      int        `json:"uncovered"`
	Unstable       bool       `json:"unstable,omitempty"`
}

type docAgent struct {
	MeanRecall float64     `json:"mean_recall"`
	Coverage   float64     `json:"coverage"`
	Targets    []docTarget `json:"targets"`
}

type docTarget struct {
	ID    string  `json:"id"`
	Where string  `json:"where"`
	Why   string  `json:"why"`
	Hits  int     `json:"hits"`
	Runs  int     `json:"runs"`
	Rate  float64 `json:"rate"`
}

type docClaim struct {
	Rule      string `json:"rule"`
	Where     string `json:"where"`
	Count     int64  `json:"count,omitempty"`
	CoveredBy string `json:"covered_by,omitempty"`
}

type docRun struct {
	Detected     []string     `json:"detected"`
	Missed       []string     `json:"missed"`
	Unclassified []docClaim   `json:"unclassified,omitempty"`
	Recall       float64      `json:"recall"`
	Trace        *agent.Trace `json:"trace,omitempty"`
	Error        string       `json:"error,omitempty"`
}

func document(s Score) doc {
	d := doc{Dataset: s.Dataset, Provider: s.Provider, Model: s.Model}

	d.Checks = docChecks{
		Found:     len(s.Checks.Found),
		Total:     len(s.Checks.Found) + len(s.Checks.Missed),
		Uncovered: len(s.Checks.Uncovered),
		Unstable:  s.ChecksUnstable,
		Missed:    make([]string, 0, len(s.Checks.Missed)),
	}
	for _, m := range s.Checks.Missed {
		d.Checks.Missed = append(d.Checks.Missed, m.ID)
	}
	d.Checks.FalsePositives = make([]docClaim, 0, len(s.Checks.FalsePositives))
	for _, f := range s.Checks.FalsePositives {
		d.Checks.FalsePositives = append(d.Checks.FalsePositives,
			docClaim{Rule: f.Rule, Where: f.Where})
	}

	d.Agent = docAgent{
		MeanRecall: s.MeanRecall(),
		Coverage:   s.Coverage(),
		Targets:    make([]docTarget, 0, len(s.Targets)),
	}
	for _, t := range s.Targets {
		d.Agent.Targets = append(d.Agent.Targets, docTarget{
			ID: t.Defect.ID, Where: t.Defect.Where, Why: t.Defect.Why,
			Hits: t.Hits, Runs: t.Runs, Rate: t.Rate(),
		})
	}

	d.Runs = make([]docRun, 0, len(s.Runs))
	for _, r := range s.Runs {
		run := docRun{
			Detected: r.Detected, Missed: r.Missed,
			Recall: r.Recall(), Trace: r.Trace, Error: r.Err,
		}
		for _, c := range r.Unclassified {
			run.Unclassified = append(run.Unclassified,
				docClaim{Rule: c.Rule, Where: c.Where, Count: c.Count, CoveredBy: c.Covers})
		}
		d.Runs = append(d.Runs, run)
	}
	return d
}

// printer keeps the first write error rather than checking every call.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

// plural renders a count with its noun, because "1 runs" in a scorecard reads
// like a bug in the scorecard.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// wrap reflows explanatory prose under a hanging indent, so that a defect's
// reason stays readable in a terminal instead of running off the edge.
func wrap(s string, indent int) string {
	const width = 72
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, w := range words {
		if i > 0 && line+1+len(w) > width-indent {
			b.WriteString("\n" + strings.Repeat(" ", indent))
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}
