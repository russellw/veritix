package rules

import (
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RenderProposals writes proposals as a rules file: the same document a
// customer loads with --rules, ready to read and edit.
//
// This is the one place a proposal's permitted values are written out, and
// deliberately so. A report is a file that gets emailed and pasted into
// tickets, so it carries the shape of a proposed rule and the count of what it
// permits; this is a rules file, whose entire purpose is to be loaded back
// into Veritix, and a one_of rule without its values is not a rule at all. It
// is written when somebody asks for it, on their own machine, like every other
// path to a verbatim value.
//
// Nothing rendered here is in force. The header says so, because a file of
// rules that looks authoritative and is not is worse than no file: the point
// of the review step is that a person decides, and they cannot decide what
// they think is already decided.
func RenderProposals(w io.Writer, ps []Proposal, header string) error {
	doc := struct {
		Version int       `yaml:"version"`
		Rules   yaml.Node `yaml:"rules"`
	}{Version: 1}
	doc.Rules.Kind = yaml.SequenceNode

	for _, p := range ps {
		var n yaml.Node
		if err := n.Encode(p.Rule); err != nil {
			return fmt.Errorf("rules: rendering proposal %s: %w", p.ID(), err)
		}
		n.HeadComment = proposalComment(p)
		doc.Rules.Content = append(doc.Rules.Content, &n)
	}

	if header != "" {
		for _, line := range strings.Split(strings.TrimRight(header, "\n"), "\n") {
			if _, err := fmt.Fprintln(w, strings.TrimRight("# "+line, " ")); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("rules: rendering proposals: %w", err)
	}
	return enc.Close()
}

// ProposalHeader is the preamble RenderProposals takes, naming what was
// audited and when.
func ProposalHeader(root string, when time.Time) string {
	return wrap(fmt.Sprintf(
		"Rules proposed by Veritix while auditing %s on %s. None of them is in "+
			"force: nothing here runs until you move it into the rules file you "+
			"load with --rules, and everything here is a suggestion made by a "+
			"language model and measured by Veritix against your data.",
		root, when.Format(time.RFC3339)), 72)
}

// proposalComment is what the reviewer reads above the rule.
//
// The values note is the important line. A permitted set materialized from the
// data is a description of what the column happens to hold, mistakes included:
// on the fixture that is Active, active, ACTIVE and the typo Actve. Accepting
// it unread would bless the typo forever, which is the failure this whole
// review step exists to prevent, so the file says so where the values are.
func proposalComment(p Proposal) string {
	var lines []string

	target := p.Rule.Table
	if p.Display != "" {
		target = p.Display
	}
	if p.Rule.Column != "" {
		target += "." + p.Rule.Column
	}
	lines = append(lines, fmt.Sprintf("%s — %s (%s)", p.Rule.ID, target, p.Rule.Expect))

	if p.Rationale != "" {
		lines = append(lines, wrap("Proposed because: "+p.Rationale, 72))
	}

	switch p.ViolationsNow {
	case 0:
		lines = append(lines, "Nothing in this dataset breaks it today.")
	case 1:
		lines = append(lines, "1 row breaks it today.")
	default:
		lines = append(lines, fmt.Sprintf("%d rows break it today.", p.ViolationsNow))
	}

	if n := len(p.Rule.Values); n > 0 {
		lines = append(lines, wrap(fmt.Sprintf(
			"The %d permitted values below are what the column held when this was "+
				"audited, not a vocabulary anybody chose. Strike out anything that is "+
				"a mistake rather than a category before you accept this.", n), 72))
	}

	return strings.Join(lines, "\n")
}

// wrap breaks text at width, on spaces. Comments in a file somebody reads in a
// terminal have to fit in it.
func wrap(s string, width int) string {
	var out strings.Builder
	line := 0
	for i, word := range strings.Fields(s) {
		switch {
		case i == 0:
			out.WriteString(word)
			line = len(word)
		case line+1+len(word) > width:
			out.WriteString("\n" + word)
			line = len(word)
		default:
			out.WriteString(" " + word)
			line += 1 + len(word)
		}
	}
	return out.String()
}
