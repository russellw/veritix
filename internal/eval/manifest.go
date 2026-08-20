// Package eval scores an audit against a dataset whose defects are already
// known.
//
// A dataset with a manifest is a dataset somebody has already worked out the
// answers for: every problem planted in it, and every place it is deliberately
// clean. That turns two questions that were previously matters of opinion into
// numbers. For the deterministic checks it answers "did this change break
// anything", which the test suite has always asked. For the agent it answers
// the question the test suite cannot: a model is nondeterministic, two runs of
// the same model on the same data find different things, and "it found a
// defect" is not the same claim as "it found the defects". Scoring a defect set
// across repeated runs is the only way to tell those apart.
//
// The manifest lives beside the data it describes, as one file, because a
// second list of the same defects would eventually disagree with the first —
// and then a passing test would mean nothing.
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the manifest's conventional name inside a dataset directory.
const FileName = "veritix-manifest.yaml"

// Manifest is the ground truth for one dataset.
type Manifest struct {
	// Version is the document format version. Only 1 exists.
	Version int `yaml:"version"`
	// Dataset names the fixture, for the scorecard's header.
	Dataset string `yaml:"dataset"`
	// Description says what the dataset is for.
	Description string `yaml:"description"`
	// Defects are the problems planted in it.
	Defects []Defect `yaml:"defects"`
	// Clean are places a check must stay quiet. A check that fires on
	// everything is useless, and only this half of the manifest catches one.
	Clean []Clean `yaml:"clean"`
}

// Defect is one problem placed on purpose.
type Defect struct {
	// ID names the defect. It appears in the scorecard and is stable.
	ID string `yaml:"id"`
	// Where is the location, as "<display>" or "<display>.<column>".
	Where string `yaml:"where"`
	// Why explains the defect to a person reading a failure.
	Why string `yaml:"why"`
	// CaughtBy is the deterministic rule that must find it, or "none" when no
	// check proposes it.
	CaughtBy string `yaml:"caught_by"`
	// Agent is set when a model is expected to find this one.
	Agent *AgentTarget `yaml:"agent"`
}

// AgentTarget describes what a model has to produce to be credited with a
// defect.
//
// The count is the load-bearing field, and it is deliberately the engine's
// number rather than anything the model wrote. A model's own rule slug and
// title are prose: two runs name the same defect two ways, and scoring against
// prose would measure vocabulary. What the engine measured from the model's own
// count_query is not prose, so a finding at the right location measuring the
// right number is the same claim however it was worded.
type AgentTarget struct {
	// Count is what a correct measurement of this defect returns.
	Count int64 `yaml:"count"`
	// Equivalent are other true measurements of the same defect.
	//
	// Two orphaned rows sharing one bad code are two rows and one value, and a
	// model that counts distinct offenders rather than affected rows has found
	// the same defect. Where a target admits more than one true figure the
	// manifest has to say so: the alternative is a correct model scoring zero,
	// which would show up as the model's failure rather than the fixture's.
	//
	// This is a concession and it should stay a small one. A target whose
	// figure genuinely depends on how the question is asked is a target that
	// cannot be credited reliably, and the fixture is usually the thing to fix.
	Equivalent []int64 `yaml:"equivalent"`
	// Query is a SELECT returning Count, so the manifest's own claim can be
	// re-run rather than believed.
	//
	// It is not shown to the model and it is not how a finding is credited —
	// the model has to write its own query and the engine has to run that one.
	// It is here because a target with a wrong count is a target nothing can
	// ever match, and the eval would report a model missing it forever without
	// anything saying why. Evidence that re-runs is the rule everywhere else in
	// Veritix, and a manifest making claims about a dataset is not the place to
	// make it an exception.
	Query string `yaml:"query"`
}

// Clean is a place the dataset is deliberately correct.
type Clean struct {
	// Rule is the check that must not fire here.
	Rule string `yaml:"rule"`
	// Where is the location, in the same form as Defect.Where.
	Where string `yaml:"where"`
	// Why explains why this location is clean.
	Why string `yaml:"why"`
}

// Measures reports whether a figure is a true measurement of this defect.
func (a *AgentTarget) Measures(n int64) bool {
	if n == a.Count {
		return true
	}
	for _, alt := range a.Equivalent {
		if n == alt {
			return true
		}
	}
	return false
}

// Deterministic reports whether a check is supposed to catch this defect.
func (d Defect) Deterministic() bool {
	return d.CaughtBy != "" && d.CaughtBy != "none"
}

// Load reads the manifest at a path. If the path is a directory, the
// conventional file name inside it is used.
func Load(path string) (*Manifest, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, FileName)
	}

	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's choice
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}

	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: parsing %s: %w", filepath.Base(path), err)
	}
	if m.Version == 0 {
		m.Version = 1
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("manifest: %s declares version %d; this build understands version 1",
			filepath.Base(path), m.Version)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("manifest: %s: %w", filepath.Base(path), err)
	}
	return &m, nil
}

// Validate refuses a manifest that could not score anything.
//
// It is strict for the same reason rules.Validate is: a manifest entry with a
// typo scores nothing and says nothing about it, and a silent zero is
// indistinguishable from a passing score.
func (m *Manifest) Validate() error {
	if len(m.Defects) == 0 {
		return fmt.Errorf("no defects are listed; a manifest with nothing in it scores everything as perfect")
	}

	seen := make(map[string]bool, len(m.Defects))
	for i, d := range m.Defects {
		where := fmt.Sprintf("defect %d", i+1)
		if d.ID != "" {
			where = fmt.Sprintf("defect %q", d.ID)
		}
		if d.ID == "" {
			return fmt.Errorf("%s has no id", where)
		}
		if seen[d.ID] {
			return fmt.Errorf("%s is listed more than once", where)
		}
		seen[d.ID] = true
		if d.Where == "" {
			return fmt.Errorf("%s does not say where it is", where)
		}
		if d.Why == "" {
			return fmt.Errorf("%s does not say why it is a defect", where)
		}
		if d.CaughtBy == "" {
			return fmt.Errorf(
				"%s does not say what catches it: name a rule, or \"none\" if no check proposes it",
				where)
		}
		if d.Agent != nil {
			if d.Agent.Count <= 0 {
				return fmt.Errorf(
					"%s is an agent target with no count: state what a correct measurement "+
						"returns, because a model is credited on the engine's figure rather "+
						"than on its own wording", where)
			}
			for _, alt := range d.Agent.Equivalent {
				if alt <= 0 || alt == d.Agent.Count {
					return fmt.Errorf(
						"%s lists %d as an equivalent measurement, which is either not a "+
							"count or is the count it already states", where, alt)
				}
			}
			if strings.TrimSpace(d.Agent.Query) == "" {
				return fmt.Errorf(
					"%s is an agent target with no query: give the SELECT that returns %d, so "+
						"the count can be re-run rather than believed",
					where, d.Agent.Count)
			}
		}
	}

	for i, c := range m.Clean {
		where := fmt.Sprintf("clean entry %d", i+1)
		if c.Rule == "" || c.Where == "" {
			return fmt.Errorf("%s needs both a rule and a where", where)
		}
		if c.Why == "" {
			return fmt.Errorf("%s does not say why that location is clean", where)
		}
	}
	return nil
}

// AgentTargets is the subset of defects a model is expected to find.
func (m *Manifest) AgentTargets() []Defect {
	var out []Defect
	for _, d := range m.Defects {
		if d.Agent != nil {
			out = append(out, d)
		}
	}
	return out
}

// Name is what to call the dataset in a scorecard.
func (m *Manifest) Name() string {
	if m.Dataset != "" {
		return m.Dataset
	}
	return "dataset"
}
