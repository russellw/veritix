package eval

import (
	"sort"
	"strings"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/finding"
)

// ChecksScore is how the deterministic pass did against the manifest.
type ChecksScore struct {
	// Found are the ids of defects their nominated rule caught.
	Found []string
	// Missed are the defects a rule was supposed to catch and did not.
	Missed []Defect
	// FalsePositives are clean locations where a check fired anyway.
	FalsePositives []Clean
	// Uncovered are the defects no check proposes, listed rather than
	// counted: they are not failures of the deterministic pass, they are the
	// reason the agentic tier exists.
	Uncovered []Defect
}

// Complete reports whether the checks did everything the manifest asks of them.
func (s ChecksScore) Complete() bool {
	return len(s.Missed) == 0 && len(s.FalsePositives) == 0
}

// ScoreChecks measures a finding set against the deterministic half of a
// manifest.
func ScoreChecks(m *Manifest, findings []finding.Finding) ChecksScore {
	var s ChecksScore
	for _, d := range m.Defects {
		if !d.Deterministic() {
			s.Uncovered = append(s.Uncovered, d)
			continue
		}
		if hasRuleAt(findings, d.CaughtBy, d.Where) {
			s.Found = append(s.Found, d.ID)
		} else {
			s.Missed = append(s.Missed, d)
		}
	}
	for _, c := range m.Clean {
		if hasRuleAt(findings, c.Rule, c.Where) {
			s.FalsePositives = append(s.FalsePositives, c)
		}
	}
	return s
}

// Claim is an agent finding the manifest does not account for.
//
// It is reported, not penalized. Every agent finding has already been measured
// by the engine and re-verified by finding.Set.Verify, so an unclassified claim
// is a true statement about the data that nobody thought to plant — which is
// either a defect the manifest should gain, or the model finding something
// trivially true and calling it a problem. Only a person can tell those apart,
// so the scorecard shows them and declines to grade them.
type Claim struct {
	Rule  string
	Where string
	Count int64
}

// RunScore is one audit scored against the manifest.
type RunScore struct {
	// Detected and Missed are agent-target defect ids.
	Detected []string
	Missed   []string
	// Unclassified are agent findings that matched no target.
	Unclassified []Claim
	// Checks is how the deterministic pass did on the same run.
	Checks ChecksScore
	// Trace is what the agent run cost, when one ran.
	Trace *agent.Trace
	// Err is set when the run itself failed.
	Err string
}

// Recall is the fraction of this manifest's agent targets this run found.
func (r RunScore) Recall() float64 {
	total := len(r.Detected) + len(r.Missed)
	if total == 0 {
		return 0
	}
	return float64(len(r.Detected)) / float64(total)
}

// ScoreRun credits a run's agent findings against the manifest's targets.
//
// A finding is credited when it sits at the target's location and the engine
// measured the target's count. Neither half is enough on its own: a location
// match alone would credit any observation about the column, and a count match
// alone would credit a coincidence elsewhere in the dataset.
func ScoreRun(m *Manifest, findings []finding.Finding) RunScore {
	var r RunScore
	r.Checks = ScoreChecks(m, findings)

	claimed := make(map[int]bool) // indices of findings credited to a target

	for _, d := range m.AgentTargets() {
		hit := -1
		for i, f := range findings {
			if f.Origin != finding.OriginAgent || claimed[i] {
				continue
			}
			if MatchesTarget(f, d) {
				hit = i
				break
			}
		}
		if hit >= 0 {
			claimed[hit] = true
			r.Detected = append(r.Detected, d.ID)
		} else {
			r.Missed = append(r.Missed, d.ID)
		}
	}

	for i, f := range findings {
		if f.Origin != finding.OriginAgent || claimed[i] {
			continue
		}
		r.Unclassified = append(r.Unclassified, Claim{
			Rule:  f.Rule,
			Where: locationOf(f),
			Count: f.Count,
		})
	}
	return r
}

// TargetScore is how one defect fared across repeated runs.
type TargetScore struct {
	Defect Defect
	// Hits is how many runs found it, out of Runs.
	Hits int
	Runs int
}

// Rate is the fraction of runs that found this defect.
func (t TargetScore) Rate() float64 {
	if t.Runs == 0 {
		return 0
	}
	return float64(t.Hits) / float64(t.Runs)
}

// Score aggregates repeated runs over one dataset.
//
// Two numbers matter and they are not the same number. MeanRecall is what a
// single audit can be expected to find. Coverage is what the model finds given
// enough attempts. When they diverge — half the defects per run, all of them
// across runs — the model is picking a different one each time rather than
// finding some and missing others, and no single run is evidence of either.
type Score struct {
	Dataset  string
	Provider string
	Model    string
	Runs     []RunScore
	Targets  []TargetScore
}

// MeanRecall is the average fraction of targets found per run.
func (s Score) MeanRecall() float64 {
	if len(s.Runs) == 0 {
		return 0
	}
	var sum float64
	for _, r := range s.Runs {
		sum += r.Recall()
	}
	return sum / float64(len(s.Runs))
}

// Coverage is the fraction of targets found by at least one run.
func (s Score) Coverage() float64 {
	if len(s.Targets) == 0 {
		return 0
	}
	var hit int
	for _, t := range s.Targets {
		if t.Hits > 0 {
			hit++
		}
	}
	return float64(hit) / float64(len(s.Targets))
}

// Aggregate rolls a set of run scores into a scorecard.
func Aggregate(m *Manifest, runs []RunScore) Score {
	s := Score{Dataset: m.Name(), Runs: runs}

	counted := 0
	for _, r := range runs {
		if r.Err == "" {
			counted++
		}
		if r.Trace != nil && s.Model == "" {
			s.Provider, s.Model = r.Trace.Provider, r.Trace.Model
		}
	}

	for _, d := range m.AgentTargets() {
		t := TargetScore{Defect: d, Runs: counted}
		for _, r := range runs {
			if r.Err != "" {
				continue
			}
			for _, id := range r.Detected {
				if id == d.ID {
					t.Hits++
					break
				}
			}
		}
		s.Targets = append(s.Targets, t)
	}
	return s
}

// UnclassifiedClaims collects every claim across runs, most frequent first.
func (s Score) UnclassifiedClaims() []Claim {
	seen := make(map[Claim]int)
	for _, r := range s.Runs {
		for _, c := range r.Unclassified {
			seen[c]++
		}
	}
	out := make([]Claim, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if seen[out[i]] != seen[out[j]] {
			return seen[out[i]] > seen[out[j]]
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

// MatchesTarget reports whether a finding measures a manifest target,
// regardless of what produced it.
//
// This is the whole of what "found it" means, in one place, because two
// callers ask the question for opposite reasons: the scorer asks whether to
// credit a model, and the check suite asks whether a deterministic rule has
// started covering a defect the model is still being credited for. A target
// caught by both would quietly inflate every score after it.
func MatchesTarget(f finding.Finding, d Defect) bool {
	return d.Agent != nil && covers(f, d.Where) && f.Count == d.Agent.Count
}

// locationOf renders a finding's location the way a manifest writes one.
func locationOf(f finding.Finding) string {
	where := f.Location.Display
	if where == "" {
		where = f.Location.Table
	}
	if f.Location.Column != "" {
		where += "." + f.Location.Column
	}
	return where
}

// covers reports whether a finding is about a manifest location.
//
// An exact match is the ordinary case. The one concession is a finding that
// names no column: record_finding makes the column optional, and a model that
// reports an orphaned reference against the table rather than the column has
// still found the defect. The concession runs one way only — a finding that
// names a column must name the right one.
func covers(f finding.Finding, where string) bool {
	if locationOf(f) == where {
		return true
	}
	if f.Location.Column != "" {
		return false
	}
	table := f.Location.Display
	if table == "" {
		table = f.Location.Table
	}
	return table != "" && strings.HasPrefix(where, table+".")
}

// hasRuleAt reports whether a finding with this rule exists at this location.
func hasRuleAt(findings []finding.Finding, rule, where string) bool {
	for _, f := range findings {
		if f.Rule == rule && locationOf(f) == where {
			return true
		}
	}
	return false
}
