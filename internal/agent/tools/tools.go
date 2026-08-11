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

	"github.com/russellwallace/veritix/internal/agent/llm"
	"github.com/russellwallace/veritix/internal/agent/redact"
	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/profile"
)

// World is what the tools may look at, and where their findings accumulate.
type World struct {
	// Engine runs the measurements. It has been through Lockdown by the time
	// the agent starts, so its SQL cannot reach the filesystem.
	Engine *engine.Engine
	// Profile is what the deterministic pass already measured. Most of what the
	// agent needs is here, which is why most tool calls cost no query at all.
	Profile *profile.Dataset
	// Guard decides what may leave the process.
	Guard *redact.Guard
	// MaxRows caps a query result. Zero uses the engine's own cap.
	MaxRows int
	Log     *slog.Logger

	mu       sync.Mutex
	findings []finding.Finding
	refused  int
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
}

// New assembles the standard tool set.
func New(w *World) *Registry {
	r := &Registry{world: w, byName: make(map[string]*Tool)}
	r.add(listTables())
	r.add(describeTable())
	r.add(profileColumn())
	r.add(runSQL())
	r.add(checkCandidateKey())
	r.add(checkReferentialIntegrity())
	r.add(sampleValues())
	r.add(recordFinding())
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

	start := len(args)
	value, err := tool.invoke(ctx, r.world, args)
	if err != nil {
		r.world.log().Debug("tool call failed", "tool", name, "error", err)
		return Result{Payload: g.SealText("%s", err.Error()), IsError: true}
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
