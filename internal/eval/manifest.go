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
	// Noise are true observations that are not defects, so the scorecard can
	// say so instead of asking a reader to adjudicate the same claim again
	// every run.
	Noise []Noise `yaml:"noise"`
	// Context are the customer's own documents, for a fixture whose defects
	// are not all in the data.
	Context []Context `yaml:"context"`

	// Dir is the directory the manifest was loaded from, so a context
	// document's relative path can be resolved. It is not part of the
	// document.
	Dir string `yaml:"-"`
}

// Context is one document from a customer's own systems — a data dictionary
// page, a warehouse catalog, a ticket.
//
// It is not data and it is not ingested: it lives beside the dataset in a form
// file discovery does not recognize, and it reaches a model only when
// something fetches it. That is the whole point of listing it here. A defect
// that is invisible in the export and obvious once the dictionary is read is
// the case Veritix's deterministic tier cannot reach by construction, and
// until a fixture contained one there was no way to tell an agent that uses
// the customer's context from one that ignores it.
//
// A document states the rule and never the violation. One that named the
// offending row would be handing over the answer, and the fixture would
// measure whether a model can copy an id out of a paragraph.
type Context struct {
	// ID names the document, and is what a target's NeedsContext refers to.
	ID string `yaml:"id"`
	// File is its path relative to the manifest's own directory.
	File string `yaml:"file"`
	// Why says what this document carries and which targets need it.
	Why string `yaml:"why"`
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
	// NeedsContext names the context documents without which this defect is
	// invisible. Empty means the export alone is enough to find it.
	//
	// It is on the defect rather than on the target because it is a claim
	// about the dataset, not about the model: these rows are indistinguishable
	// from correct ones until something outside the export says what the
	// column is for. Splitting the scorecard on it is what turns "context
	// helped" from an impression into a number, and — because a fixture also
	// carries targets that need nothing — lets the same run show whether
	// filling the transcript with documents cost the model the ones it could
	// already find.
	NeedsContext []string `yaml:"needs_context"`
}

// Aided reports whether this defect needs a context document to be visible.
func (d Defect) Aided() bool { return len(d.NeedsContext) > 0 }

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

// Noise is something a model can truthfully report that nobody should act on.
//
// Clean entries police the checks, whose rule names Veritix chose. They cannot
// police an agent claim: the rule slug is model-authored prose, and two runs
// word the same observation two ways -- gpt-oss-120b called the same thing
// inconsistent_status_length once and mixed_status_format the next time. So a
// noise entry is keyed the way a target is keyed, on the engine's number at a
// location, which is the one part of a claim the model does not write.
//
// It labels, and deliberately does not penalize. Scoring a model down for
// noticing something true would be grading its judgment through its wording,
// which is the thing MatchesTarget exists to avoid. A claim that matches a
// noise entry has already failed to match every target, so this can never
// absolve a real hit.
type Noise struct {
	// Where is the location, in the same form as Defect.Where.
	Where string `yaml:"where"`
	// Count is the figure the engine returns for it.
	Count int64 `yaml:"count"`
	// Why explains why this is not a defect.
	Why string `yaml:"why"`
}

// Explains reports whether a claim is this known non-defect.
func (n Noise) Explains(where string, count int64) bool {
	return n.Where == where && n.Count == count
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
	m.Dir = filepath.Dir(path)
	// A context document is resolved here rather than in Validate, which does
	// not touch the filesystem. A named file that is not there scores nothing
	// and says nothing about it, which is the failure Validate exists to
	// refuse everywhere else.
	for _, c := range m.Context {
		if _, err := os.Stat(m.ContextPath(c)); err != nil {
			return nil, fmt.Errorf("manifest: %s: context document %q: %w",
				filepath.Base(path), c.ID, err)
		}
	}
	return &m, nil
}

// ContextPath resolves a context document against the manifest's directory.
func (m *Manifest) ContextPath(c Context) string {
	return filepath.Join(m.Dir, filepath.FromSlash(c.File))
}

// ReadContext returns one context document's text.
//
// This is what a source of the customer's context reads in a test: the
// documents are files here because a fixture has to be committed, and whatever
// serves them in earnest — an MCP server on the customer's own network — hands
// back the same bytes.
func (m *Manifest) ReadContext(id string) (string, error) {
	for _, c := range m.Context {
		if c.ID != id {
			continue
		}
		data, err := os.ReadFile(m.ContextPath(c)) //nolint:gosec // a path out of a manifest the operator chose
		if err != nil {
			return "", fmt.Errorf("manifest: context document %q: %w", id, err)
		}
		return string(data), nil
	}
	return "", fmt.Errorf("manifest: no context document is called %q", id)
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

	docs := make(map[string]bool, len(m.Context))
	for i, c := range m.Context {
		where := fmt.Sprintf("context entry %d", i+1)
		if c.ID != "" {
			where = fmt.Sprintf("context document %q", c.ID)
		}
		if c.ID == "" {
			return fmt.Errorf("%s has no id, which is what a target refers to it by", where)
		}
		if docs[c.ID] {
			return fmt.Errorf("%s is listed more than once", where)
		}
		docs[c.ID] = true
		if c.File == "" {
			return fmt.Errorf("%s does not say which file it is", where)
		}
		if filepath.IsAbs(c.File) || strings.Contains(c.File, "..") {
			// The path is joined to the manifest's directory and read. A
			// fixture is a committed directory, so a document outside it is a
			// mistake rather than a threat -- but one that would read a file
			// nobody meant to publish to a model.
			return fmt.Errorf("%s names %q, which is outside the dataset directory", where, c.File)
		}
		if c.Why == "" {
			return fmt.Errorf("%s does not say what it carries", where)
		}
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
		for _, id := range d.NeedsContext {
			if !docs[id] {
				return fmt.Errorf(
					"%s needs context document %q, which the manifest does not list: "+
						"add it under context:, or the target is marked as needing "+
						"something nothing can supply", where, id)
			}
		}
		if d.Agent == nil && d.Aided() {
			// A deterministic check reads the export and nothing else. A
			// defect claiming both that a check catches it and that it is
			// invisible without a document is a contradiction, and the
			// scorecard would report the check finding it either way.
			return fmt.Errorf(
				"%s says it needs the customer's context but is not an agent target: "+
					"a check reads the export alone, so it cannot be both", where)
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

	for i, n := range m.Noise {
		where := fmt.Sprintf("noise entry %d", i+1)
		if n.Where == "" {
			return fmt.Errorf("%s needs a where", where)
		}
		// A claim measuring nothing does not exist: record_finding refuses a
		// count of zero. An entry keyed on one would match nothing forever.
		if n.Count <= 0 {
			return fmt.Errorf(
				"%s needs the count the engine returns for it, which is what identifies a "+
					"claim whose rule name the model wrote", where)
		}
		if n.Why == "" {
			return fmt.Errorf("%s does not say why that is not a defect", where)
		}
		// A noise entry keyed the same way a target is keyed can be written to
		// describe the target, and then the fixture contains a line saying its
		// own planted defect is nothing to worry about. A target is matched
		// first and so would still be credited, which is worse: the manifest
		// would be wrong and nothing would fail. This is the same collision
		// `equivalent:` produced the first time it was reached for.
		for _, d := range m.Defects {
			if d.Agent != nil && d.Where == n.Where && d.Agent.Measures(n.Count) {
				return fmt.Errorf(
					"%s says %d rows at %s are not a defect, which is exactly what target %s "+
						"measures there", where, n.Count, n.Where, d.ID)
			}
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
