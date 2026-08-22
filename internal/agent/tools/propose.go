package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/rules"
)

// proposeRule is the agent's second output, and it asserts something
// record_finding cannot.
//
// record_finding says this data is wrong now. propose_rule says this
// expectation should hold in future — and that is how a defect the model found
// on one run gets found on every run, by the deterministic pass, with no model
// and no cost. The eval measures the gap this closes: on dirty-logistics
// gpt-oss-120b scored 42% mean recall against 75% coverage, and the whole of
// that gap is defects found on one run of three. A rule accepted from the run
// that found one converts it to every run, permanently.
//
// The discipline is record_finding's, with one inversion that matters. Veritix
// compiles the proposal into a real rules.File, materializes anything the
// model could not see, and puts it through the real rules.Evaluate against the
// data in front of it: the model supplies the expectation, the engine supplies
// the number, and a stated violation count that disagrees with the measured
// one records nothing. What is *not* ported is the zero rule. A count query
// returning zero refuses a finding, because a problem that does not reproduce
// is not a problem; a proposed rule that nothing violates today is the best
// kind of rule there is. What has to be refused instead is the true analogue:
// a rule that applies to nothing, which would sit in a customer's rules file
// forever looking like protection.
//
// The result carries no cell values, and must not start to. A rule's permitted
// set is materialized from the data by design — see rules.Materialize — so it
// is exactly the kind of thing that would leak if it were echoed back as
// confirmation. The model is told how many values were filled in; the person
// reviewing the proposal is the one shown what they are.
// TestAProposalsValuesNeverReachTheModel pins that.
func proposeRule() *Tool {
	names := make([]string, 0, len(rules.Expectations()))
	for _, e := range rules.Expectations() {
		names = append(names, string(e))
	}

	return &Tool{
		Definition: llm.Tool{
			Name: "propose_rule",
			Description: "Propose a rule: an expectation that should hold on this data every " +
				"time it is audited, not just today. A person reviews it and, once accepted, " +
				"Veritix enforces it on every future audit with no model involved — this is how " +
				"something you noticed once gets caught forever. Findings and rules are " +
				"different assertions: use record_finding for rows that are wrong now, and this " +
				"for the expectation they broke. A rule that nothing violates today is not " +
				"wasted, it is the best kind, so propose the invariants you can see holding. " +
				"State violations_now, the number of rows you expect to break the rule today: " +
				"Veritix compiles the rule and runs it, and a disagreement proposes nothing. A " +
				"rule that matches no column is refused, because a rule that cannot fire " +
				"protects nothing. one_of takes no value list — you have not been shown the " +
				"values, so Veritix fills the permitted set with what the column holds today " +
				"and the reviewer reads it.",
			Properties: map[string]any{
				"rule": str("a short stable slug for the rule, e.g. status_domain or " +
					"weight_within_plausible_range"),
				"description": str("what the rule is for, in one line, for the person deciding " +
					"whether to accept it and for the report when it fires"),
				"rationale": str("why this data needs it: what you saw that makes the expectation " +
					"worth enforcing"),
				"table":  str("the table the rule applies to"),
				"column": str("the column it applies to; omit only for expect: sql"),
				"expect": map[string]any{
					"type": "string",
					"enum": names,
					"description": "the assertion: not_null, unique, positive, non_negative, " +
						"one_of (a fixed vocabulary, filled in for you), matches (a regular " +
						"expression, which you can derive from the shapes you were shown), range " +
						"(min and/or max), not_future, references (values must exist in another " +
						"column), or sql (a WHERE clause selecting rows that are wrong)",
				},
				"pattern":    str("for matches: a regular expression the whole value must match"),
				"min":        map[string]any{"type": "number", "description": "for range: the smallest permitted value"},
				"max":        map[string]any{"type": "number", "description": "for range: the largest permitted value"},
				"references": str("for references: the column every value must exist in, written as table.column"),
				"where": str("for sql: a WHERE clause that selects the rows that are wrong, e.g. " +
					"delivered_at < dispatched_at. This is the expectation that catches a " +
					"contradiction between two columns."),
				"ignore_case":   map[string]any{"type": "boolean", "description": "compare text ignoring case and surrounding spaces; usually what a person means"},
				"allow_missing": map[string]any{"type": "boolean", "description": "exempt absent values, so the rule constrains what is there without also requiring it to be there"},
				"severity": map[string]any{
					"type":        "string",
					"enum":        []string{"info", "warning", "error"},
					"description": "how a future violation should be reported; the reviewer confirms it",
				},
				"violations_now": integer("how many rows you expect to break this rule today. " +
					"Zero is a good answer for an invariant that already holds. It is checked " +
					"against what the rule actually measures."),
			},
			Required: []string{"rule", "description", "table", "expect", "violations_now"},
		},

		invoke: func(ctx context.Context, w *World, args json.RawMessage) (any, error) {
			var in struct {
				Rule         string   `json:"rule"`
				Description  string   `json:"description"`
				Rationale    string   `json:"rationale"`
				Table        string   `json:"table"`
				Column       string   `json:"column"`
				Expect       string   `json:"expect"`
				Pattern      string   `json:"pattern"`
				Min          *float64 `json:"min"`
				Max          *float64 `json:"max"`
				References   string   `json:"references"`
				Where        string   `json:"where"`
				IgnoreCase   bool     `json:"ignore_case"`
				AllowMissing bool     `json:"allow_missing"`
				Severity     string   `json:"severity"`
				Claimed      *int64   `json:"violations_now"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}

			slug := slugify(in.Rule)
			if slug == "" {
				return nil, errors.New("give the rule a slug, e.g. status_domain")
			}
			if strings.TrimSpace(in.Description) == "" {
				return nil, errors.New("give the rule a description: it is what the person " +
					"deciding whether to accept it reads first")
			}
			if in.Claimed == nil {
				return nil, errors.New("violations_now is required: state how many rows you " +
					"expect to break the rule today, so a disagreement with the engine is " +
					"caught rather than accepted. Zero is a legitimate answer")
			}
			expect, err := rules.ParseExpectation(in.Expect)
			if err != nil {
				return nil, fmt.Errorf("%w; it must be one of: %s", err, strings.Join(names, ", "))
			}

			// The rule is written against the profile's own names, never the
			// model's: a rule is stored and re-run later, so an invented
			// identifier reaching SQL here would reach it again on every
			// future audit.
			t, err := w.table(in.Table)
			if err != nil {
				return nil, err
			}
			r := rules.Rule{
				ID:           slug,
				Description:  strings.TrimSpace(in.Description),
				Table:        t.Name,
				Expect:       expect,
				Pattern:      in.Pattern,
				Min:          in.Min,
				Max:          in.Max,
				Where:        in.Where,
				IgnoreCase:   in.IgnoreCase,
				AllowMissing: in.AllowMissing,
			}
			if in.Column != "" {
				c, err := w.column(t, in.Column)
				if err != nil {
					return nil, err
				}
				r.Column = c.Name
			}
			if in.Severity != "" {
				severity, err := finding.ParseSeverity(in.Severity)
				if err != nil {
					return nil, err
				}
				r.Severity = &severity
			}
			if expect == rules.ExpectOneOf {
				// The one rule kind whose body is literally a list of cell
				// values, and therefore the one the model cannot write.
				r.ValuesFrom = rules.ValuesFromCurrent
			}
			if in.References != "" {
				resolved, err := w.reference(in.References)
				if err != nil {
					return nil, err
				}
				r.References = resolved
			}

			file := &rules.File{Version: 1, Rules: []rules.Rule{r}}
			if err := file.Validate(); err != nil {
				return nil, fmt.Errorf("that rule was not accepted: %w", err)
			}

			// Protection that already exists is not worth a step, and a model
			// re-proposing what a customer accepted last month is the same
			// failure as one re-reporting what the deterministic pass found.
			if id := w.Rules.Covering(&r, t.Display); id != "" {
				return nil, fmt.Errorf(
					"nothing was proposed: a rule already in force covers this — %q, on the same "+
						"target with the same expectation. Look for what it does not cover", id)
			}

			// A WHERE clause is model-authored SQL that will be re-run on
			// every future audit, so it goes through the same parse as any
			// other statement the model writes: one read-only SELECT or
			// nothing.
			if expect == rules.ExpectSQL {
				probe := fmt.Sprintf("SELECT count(*) FROM %s WHERE (%s)", engine.Ident(t.Name), in.Where)
				if _, err := w.Engine.AnalyzeSelect(ctx, probe); err != nil {
					return nil, fmt.Errorf("the where clause was refused: %v",
						w.Guard.EngineError(err, probe))
				}
			}

			proposal := rules.Proposal{Rule: r, Rationale: strings.TrimSpace(in.Rationale), Display: t.Display}
			if existing := w.proposalMade(proposal.ID()); existing != "" {
				return nil, fmt.Errorf(
					"nothing was proposed: you have already proposed this rule in this run, as %q",
					existing)
			}

			// Fill in what the model was not allowed to see, then run the rule
			// as rules.Evaluate would run it on any future audit. Both steps
			// are the customer's own code paths, not a lenient rehearsal of
			// them.
			if err := rules.Materialize(ctx, w.Engine, w.Profile, file); err != nil {
				return nil, fmt.Errorf("the rule could not be completed from the data: %w", err)
			}
			found, err := rules.Evaluate(ctx, w.Engine, w.Profile, file, w.log())
			if err != nil {
				return nil, fmt.Errorf("the rule could not be evaluated: %v",
					w.Guard.EngineError(err, in.Where))
			}

			var violations int64
			for _, f := range found {
				switch f.Rule {
				case "rule.never_applied":
					// The real analogue of a finding that does not reproduce.
					return nil, fmt.Errorf(
						"nothing was proposed: the rule matched no column in this dataset, so it " +
							"could never fire. Check the table and column names against " +
							"describe_table")
				case "rule.invalid":
					return nil, fmt.Errorf("the rule could not be evaluated: %v",
						w.Guard.EngineError(errors.New(f.Detail), in.Where))
				case "rule." + slug:
					violations += f.Count
				}
			}

			// The claim and the measurement have to agree, for the reason
			// record_finding gives: the description is prose the model wrote
			// and usually carries the figure, so a silent correction leaves a
			// rule whose own account of itself is wrong.
			if *in.Claimed != violations {
				w.mu.Lock()
				w.refusedProposals++
				w.mu.Unlock()
				w.log().Info("declined a proposal whose claim did not match the measurement",
					"rule", slug, "table", t.Name, "claimed", *in.Claimed, "measured", violations)
				return nil, fmt.Errorf(
					"nothing was proposed: you said %d rows break this rule today, but it breaks "+
						"on %d. If %d is right, propose it again with that figure; if not, the "+
						"expectation is not the one you meant", *in.Claimed, violations, violations)
			}

			proposal.Rule = file.Rules[0] // materialized
			proposal.ViolationsNow = violations

			w.mu.Lock()
			w.proposals = append(w.proposals, proposal)
			total := len(w.proposals)
			w.mu.Unlock()

			w.log().Info("agent proposed a rule",
				"rule", slug, "expect", string(expect), "table", t.Name,
				"column", r.Column, "violations_now", violations)

			return struct {
				Proposed        bool   `json:"proposed"`
				ProposalID      string `json:"proposal_id"`
				Rule            string `json:"rule"`
				Target          string `json:"target"`
				Expect          string `json:"expect"`
				ViolationsNow   int64  `json:"violations_now"`
				PermittedValues int    `json:"permitted_values,omitempty"`
				ProposalsSoFar  int    `json:"proposals_so_far"`
				Note            string `json:"note"`
			}{
				Proposed:        true,
				ProposalID:      proposal.ID(),
				Rule:            slug,
				Target:          target(t.Display, r.Column),
				Expect:          string(expect),
				ViolationsNow:   violations,
				PermittedValues: len(proposal.Rule.Values),
				ProposalsSoFar:  total,
				Note:            proposalNote(proposal),
			}, nil
		},
	}
}

// proposalNote says what was done and what was not, because the two are easy
// to conflate: a proposal changes nothing about this report, and violations it
// counted today are not findings unless the model records them as such.
func proposalNote(p rules.Proposal) string {
	var b strings.Builder
	b.WriteString("proposed, not applied: a person accepts it before it runs on anything. ")
	if n := len(p.Rule.Values); n > 0 {
		fmt.Fprintf(&b, "The permitted set was filled in with the %d values the column holds "+
			"today, which you are not shown and the reviewer is. ", n)
	}
	if p.ViolationsNow == 0 {
		b.WriteString("Nothing breaks it today, which is what makes it worth accepting.")
	} else {
		fmt.Fprintf(&b, "%d rows break it today; that is not in the report — if those rows are "+
			"a defect worth reporting now, record_finding is what reports them.", p.ViolationsNow)
	}
	return b.String()
}

// target renders a rule's subject for the model, from the profile's names.
func target(display, column string) string {
	if column == "" {
		return display
	}
	return display + "." + column
}

// reference resolves the "table.column" a references rule points at, through
// the profile, so that neither half of it is a name the model invented.
//
// It splits on the last dot rather than the first: a display name carries one
// ("customers.csv"), and a model that has been reading display names all run
// will write the one it was shown.
func (w *World) reference(s string) (string, error) {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return "", fmt.Errorf("references must be written as table.column; %q is not", s)
	}
	t, err := w.table(s[:i])
	if err != nil {
		return "", err
	}
	c, err := w.column(t, s[i+1:])
	if err != nil {
		return "", err
	}
	// The rule is stored with SQL names, which contain no dot, so the file's
	// own first-dot split reads it back the way it was written.
	return t.Name + "." + c.Name, nil
}
