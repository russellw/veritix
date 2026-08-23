// Package tools is the surface the model is allowed to touch.
//
// Everything the agent can do to a dataset is one of these, and each returns
// evidence rather than prose: a measurement, and where it applies the SQL that
// produced it. The model chooses what to look at; the engine decides what is
// true.
//
// Two rules hold across every tool here, and both are structural rather than
// advisory:
//
//   - No identifier from the model reaches SQL. A table or column name is
//     looked up in the profile first and the *profile's* name is what gets
//     quoted, so a model that invents a name gets "no such table" instead of a
//     query. That is a second line behind [engine.Ident], not a replacement
//     for it.
//   - No result reaches the model except through [redact.Guard.Seal]. A tool
//     returns an ordinary Go value and the registry seals it; a value carrying
//     content the guard has not cleared fails to seal, at the point where it
//     would have been sent.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/agent/redact"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/mcpclient"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/rules"
)

// World is what the tools may look at, and where their findings accumulate.
type World struct {
	// Engine runs the measurements. It has been through Lockdown by the time
	// the agent starts, so its SQL cannot reach the filesystem.
	Engine *engine.Engine
	// Profile is what the deterministic pass already measured. Most of what the
	// agent needs is here, which is why most tool calls cost no query at all.
	Profile *profile.Dataset
	// Known is what that pass already reported. The brief lists it so the agent
	// does not go rediscovering it; the tools consult it so that a check landing
	// on new ground can say so at the moment it lands. See [World.knownAt].
	Known []finding.Finding
	// Rules are the customer's rules already in force for this run. They are
	// Known's counterpart for propose_rule: protection that already exists is
	// not worth a step, and a model cannot tell without being told. Only their
	// ids and targets are ever sent — a rule's permitted values are cell
	// values, and its where clause can be.
	Rules *rules.File
	// Context is the customer's own documents, reachable over MCP. Nil when no
	// context server is configured, which is the default and which is what
	// keeps the tool surface — and therefore the cached prompt prefix —
	// exactly what it was before M5b for everybody who has not turned this on.
	Context *mcpclient.Library
	// Guard decides what may leave the process.
	Guard *redact.Guard
	// MaxRows caps a query result. Zero uses the engine's own cap.
	MaxRows int
	// QueryTimeout bounds one tool call, which is where a bound on
	// model-written SQL belongs: the engine's own limit has to be sized for
	// Veritix's measurements over the whole dataset, and a model's statement
	// is the one piece of SQL in the process that nobody reviewed. Zero
	// leaves the engine's limit in place.
	QueryTimeout time.Duration
	Log          *slog.Logger

	mu               sync.Mutex
	findings         []finding.Finding
	proposals        []rules.Proposal
	refused          int
	refusedProposals int
}

// Findings returns what the agent recorded, in the order it recorded them.
func (w *World) Findings() []finding.Finding {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]finding.Finding(nil), w.findings...)
}

// Refused is how many proposed findings the engine measured at zero and
// therefore declined to record. It is worth reporting: it is the count of
// times the model asserted something the data did not support.
func (w *World) Refused() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refused
}

// Proposals returns the rules the agent proposed, in the order it proposed
// them. None of them has been applied to anything.
func (w *World) Proposals() []rules.Proposal {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]rules.Proposal(nil), w.proposals...)
}

// RefusedProposals is how many proposals the engine measured differently from
// the model's claim and therefore declined. It counts alongside Refused for
// the same reason: it is how often the model asserted something the data did
// not bear out.
func (w *World) RefusedProposals() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refusedProposals
}

// proposalMade reports the slug a proposal of this identity was already made
// under during this run, or "".
func (w *World) proposalMade(id string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.proposals {
		if p.ID() == id {
			return p.Rule.ID
		}
	}
	return ""
}

func (w *World) log() *slog.Logger {
	if w.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return w.Log
}

// table finds a profiled table by SQL name or by the display name a person
// would recognize. Returning the profile's own record is what keeps
// model-supplied text out of SQL.
func (w *World) table(name string) (*profile.Table, error) {
	for _, t := range w.Profile.Tables {
		if t.Name == name || t.Display == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("no table called %q; call list_tables to see what there is", name)
}

func (w *World) column(t *profile.Table, name string) (*profile.Column, error) {
	for _, c := range t.Columns {
		if c.Name == name || c.Original == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("table %q has no column called %q; call describe_table to see its columns", t.Name, name)
}

// knownAt reports which deterministic rule, if any, already covers a defect of
// one of these kinds at this location. It matches on the profile's own names,
// which is what a finding's Location carries.
func (w *World) knownAt(table, column string, rules ...string) string {
	for _, f := range w.Known {
		if f.Location.Table != table || f.Location.Column != column {
			continue
		}
		for _, r := range rules {
			if f.Rule == r {
				return r
			}
		}
	}
	return ""
}

// verdict is the sentence a check tool adds to its own result when it has
// measured a defect: whether this is new ground or ground the deterministic
// pass already covered.
//
// It exists because a measurement the model has to interpret unaided is a
// measurement it can walk away from, and one did. Against dirty-retail a 4B
// model was handed two unresolved references that no deterministic finding
// reports, said nothing, and spent its remaining eleven steps elsewhere. The
// system prompt covers all of this and had been true for twelve steps by then;
// a tool result is read at the moment the evidence is in front of the model,
// which is the same reason record_finding corrects an inflated count where the
// count is made rather than in the prompt.
//
// It is a nudge and not an instruction. What to record is still the model's
// judgment, the engine still decides the number, and Set.Verify still has the
// last word on whether it reaches the report.
func (w *World) verdict(what, table, column string, rules ...string) string {
	if rule := w.knownAt(table, column, rules...); rule != "" {
		return fmt.Sprintf("the deterministic pass already reports this as %s, so do not "+
			"re-report it; what it implies elsewhere may still be worth following", rule)
	}
	return fmt.Sprintf("%s, and no deterministic finding covers this. If it is a real "+
		"problem, record it now with record_finding, passing evidence_query as the "+
		"count_query — nothing you leave unrecorded reaches the report.", what)
}

// Tool is one capability.
type Tool struct {
	// Definition is what the model is told about it.
	Definition llm.Tool
	// invoke runs it. The value it returns is sealed by the registry; the error
	// it returns is shown to the model so it can correct itself.
	invoke func(ctx context.Context, w *World, args json.RawMessage) (any, error)
}

// Registry is the set of tools offered to a model.
type Registry struct {
	world  *World
	order  []*Tool
	byName map[string]*Tool
	// failed counts how many times each exact call has been refused, so a
	// repeat can be told apart from a first attempt. See noteRepeat.
	failed map[string]int
}

// New assembles the standard tool set.
func New(w *World) *Registry {
	r := &Registry{world: w, byName: make(map[string]*Tool), failed: make(map[string]int)}
	r.add(listTables())
	r.add(describeTable())
	r.add(profileColumn())
	r.add(runSQL())
	r.add(checkCandidateKey())
	r.add(checkReferentialIntegrity())
	r.add(sampleValues())
	// The context tools go before the outputs and after the measurements,
	// because that is the order the work happens in: read what a column is
	// for, measure whether the data agrees, record what it does not.
	if w.Context != nil {
		r.add(listContext())
		r.add(readContext())
	}
	r.add(recordFinding())
	r.add(proposeRule())
	return r
}

func (r *Registry) add(t *Tool) {
	r.order = append(r.order, t)
	r.byName[t.Definition.Name] = t
}

// Definitions describes the tools to a provider, in a stable order so that the
// prompt prefix — and therefore the provider's cache of it — does not change
// between calls.
func (r *Registry) Definitions() []llm.Tool {
	out := make([]llm.Tool, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, t.Definition)
	}
	return out
}

// World returns the world the tools operate on.
func (r *Registry) World() *World { return r.world }

// Result is one tool call's outcome, cleared for egress.
type Result struct {
	// Payload is what the model will be shown.
	Payload redact.Sealed
	// IsError marks a failure, so the model treats it as something to fix
	// rather than as data.
	IsError bool
}

// Invoke runs a tool by name and seals its result.
//
// Every failure — unknown tool, malformed arguments, a rejected query, a
// finding that did not reproduce — comes back as a sealed error rather than as
// a Go error, because all of them are things the model should see and correct.
// A run does not end because the model made a mistake; it ends when the model
// stops or the budget does.
func (r *Registry) Invoke(ctx context.Context, name string, args json.RawMessage) Result {
	g := r.world.Guard

	tool, ok := r.byName[name]
	if !ok {
		return Result{Payload: g.SealText("there is no tool called %q", name), IsError: true}
	}

	if r.world.QueryTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.world.QueryTimeout)
		defer cancel()
	}

	start := len(args)
	value, err := tool.invoke(ctx, r.world, args)
	if err != nil {
		r.world.log().Debug("tool call failed", "tool", name, "error", err)
		msg := err.Error()
		if n := r.noteRepeat(name, args); n > 1 {
			msg += "\n\n" + repeatNote(name, n)
		}
		return Result{Payload: g.SealText("%s", msg), IsError: true}
	}

	sealed, err := g.Seal(value)
	if err != nil {
		// The guard refused to serialize the result. That is a bug in the tool
		// rather than a mistake by the model, so it is logged loudly and the
		// model is told nothing about the data.
		r.world.log().Error("a tool result could not be sealed for egress",
			"tool", name, "error", err)
		return Result{
			Payload: g.SealText("%s could not return a result", name),
			IsError: true,
		}
	}

	r.world.log().Debug("tool call",
		"tool", name, "args_bytes", start, "result_bytes", sealed.Len())
	return Result{Payload: sealed}
}

// noteRepeat records that a call failed and returns how many times this exact
// call has now failed.
//
// The arguments are canonicalized before hashing so that the same call reworded
// by the serializer — a different key order, different spacing, a key added
// with nothing in it — is still the same call. Anything that will not parse is
// compared as written, which is the safe direction: two unparseable calls that
// differ only in whitespace are still two attempts at the same thing.
func (r *Registry) noteRepeat(name string, args json.RawMessage) int {
	key := name + "\x00" + canonical(args)
	r.failed[key]++
	return r.failed[key]
}

func canonical(args json.RawMessage) string {
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return string(args)
	}
	// Go sorts map keys, so this is stable for any object the model sends.
	out, err := json.Marshal(prune(v))
	if err != nil {
		return string(args)
	}
	return string(out)
}

// prune drops object keys whose value cannot change how the call is read:
// null, "", [] and {}. Every tool decodes its arguments into a plain struct,
// so a key carrying one of those yields exactly the struct an absent key
// yields — {"where": ""} is the same propose_rule as one with no where at all,
// and is refused for the same reason.
//
// It is there because a model will find this gap on its own. gpt-oss-120b
// re-sent a refused propose_rule on dirty-meters with "where": "" added and
// nothing else changed: a different key, so a different hash, so no repeat
// note — the exact livelock noteRepeat exists to break, at about eight minutes
// a step.
//
// Deliberately not 0, and not false: those are values the model asserted
// rather than keys it left out, and record_finding's affected_count and
// propose_rule's violations_now are pointers precisely so that a claimed zero
// is not an absent claim. Nor is a whitespace-only string empty — propose_rule
// puts where straight into a probe, so "" and " " are refused by different
// things. Array elements are pruned within but never removed: ["region", ""]
// names two columns, one of them blank, which is not the call that names one.
func prune(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			if e = prune(e); !emptyValue(e) {
				out[k] = e
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = prune(e)
		}
		return out
	}
	return v
}

func emptyValue(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// repeatNote is what a model is told when it sends a call that has already been
// refused, unchanged.
//
// It exists because one will. gpt-oss-120b spent four consecutive steps of a
// dirty-logistics run re-sending an identical propose_rule with no column on
// it, at five minutes a step, against a budget of twenty-four. The refusal it
// got each time was correct and identical, and nothing in it distinguished
// "you got this wrong" from "you got this wrong in exactly the same way again",
// which is the distinction that would have made it change something.
//
// So: say the attempt was identical, say the refusal stands, and say that
// moving on is a legitimate answer. That last clause is the same one
// writtenCallCorrection carries, for the same reason — a correction that reads
// as "you were supposed to succeed at this" is how a model burns a budget
// rather than spending it.
func repeatNote(name string, n int) string {
	return fmt.Sprintf("This is attempt %d at that exact %s call: the arguments are "+
		"unchanged from the one that was just refused, so the refusal is unchanged too. "+
		"Sending it again will fail again. Change what the message above asks you to "+
		"change, or leave this and do something else — moving on is a legitimate answer, "+
		"and an audit does not have to contain every rule that could be proposed.", n, name)
}

// decode reads a tool's arguments, reporting a malformed call in terms the
// model can act on.
func decode(args json.RawMessage, into any) error {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, into); err != nil {
		return fmt.Errorf("those arguments could not be read: %w", err)
	}
	return nil
}

// str is a JSON Schema string property.
func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// integer is a JSON Schema integer property.
func integer(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// stringList is a JSON Schema array-of-strings property.
func stringList(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": description,
	}
}

// countedText is a value and how often it occurs. The value has been through
// the guard; the count has not, because a count is a count.
type countedText struct {
	Value redact.Text `json:"value"`
	Count int64       `json:"count"`
	Share float64     `json:"share,omitempty"`
}

func round2(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}

// quoteColumns renders a list of profiled columns for SQL.
func quoteColumns(cols []*profile.Column) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return engine.Idents(names)
}
