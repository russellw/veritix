package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Proposal is a rule somebody suggested and nobody has accepted yet.
//
// It is deliberately not a finding, and the two are not checkable the same
// way. A finding asserts that this data is wrong now, so one whose count query
// returns zero is refused. A proposal asserts that an expectation should hold
// in future, so one with no violations today is the best kind there is:
// "status is drawn from these four values" is worth having precisely because
// it holds now and should keep holding. Filing proposals among the findings
// would also report the same rows twice, once as the defect and once as the
// rule that would have caught it.
//
// Nothing here is applied. An accepted rule raises errors on future data and
// can fail a CI gate, which is not a thing a model gets to do unattended.
type Proposal struct {
	// Rule is the expectation, resolved: the profile's own table and column
	// names, and any values already materialized from the data.
	Rule Rule `json:"rule"`
	// Rationale is the argument for it, for the person deciding whether to
	// accept. It is prose from whoever proposed the rule and carries no
	// authority of its own.
	Rationale string `json:"rationale,omitempty"`
	// ViolationsNow is what the rule measured against the data in front of it
	// when it was proposed.
	ViolationsNow int64 `json:"violations_now"`
	// Display is the source a person recognizes — "sales.xlsx#Q1" — since
	// Rule.Table carries the SQL name the rule has to be written against.
	Display string `json:"display,omitempty"`
}

// key identifies a proposal by what it asserts.
//
// The model's slug, description and rationale are all wording, and two runs
// word the same expectation two ways; scoring or de-duplicating on them would
// measure vocabulary. The materialized values are left out for a different
// reason: they come from data that changes between runs, and "one_of on
// status" proposed in March and again in June is one proposal to review, not
// two, even where the column has since acquired a fifth spelling.
func (p Proposal) key() string {
	r := p.Rule
	parts := []string{
		r.Table, r.Column, string(r.Expect),
		r.Pattern, r.References, normalizeSpace(r.Where),
	}
	if r.Min != nil {
		parts = append(parts, fmt.Sprint(*r.Min))
	}
	if r.Max != nil {
		parts = append(parts, fmt.Sprint(*r.Max))
	}
	return strings.Join(parts, "\x00")
}

// ID is a stable, URL-safe handle for a proposal, digested for the same reason
// [finding.Finding.ID] is: the key contains customer column names and an id
// ends up in URLs and access logs.
func (p Proposal) ID() string {
	sum := sha256.Sum256([]byte(p.key()))
	return hex.EncodeToString(sum[:8])
}

// Covering names a rule in this file that already asserts what r asserts, or
// "" if none does.
//
// A model that re-proposes protection the customer already has is the same
// failure as one that re-reports what the deterministic pass already found,
// and the answer is the same: say so where the model is looking. display is
// the target's human-readable name, because a rule a person wrote targets
// "customers.csv" while a resolved rule targets the SQL name.
func (f *File) Covering(r *Rule, display string) string {
	if f == nil || r == nil {
		return ""
	}
	for i := range f.Rules {
		in := &f.Rules[i]
		if in.Expect != r.Expect {
			continue
		}
		if !matchGlob(in.Table, r.Table) && !matchGlob(in.Table, display) {
			continue
		}
		// Two sql rules on one table are two different expectations unless
		// they say the same thing; every other expectation is identified by
		// what it applies to.
		if r.Expect == ExpectSQL {
			if normalizeSpace(in.Where) != normalizeSpace(r.Where) {
				continue
			}
		} else if !matchGlob(in.Column, r.Column) {
			continue
		}
		return in.ID
	}
	return ""
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
