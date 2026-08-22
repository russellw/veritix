package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/agent/redact"
	"github.com/russellw/veritix/internal/mcpclient"
)

// Trace is the record of what the agent did.
//
// It is written to the run store and served to the interface, and it is a
// product feature rather than a debugging aid. A customer who has just been
// told that a model examined their sales ledger is entitled to see exactly what
// it was sent and exactly what it sent back, and that is what a trace is: every
// tool call's arguments as the model wrote them, and every result as the exact
// bytes the egress guard released. Nothing is summarized, because a summary of
// what left the process is not evidence about what left the process.
type Trace struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`

	Steps []Step `json:"steps"`

	// Usage is what the run cost in tokens, across every call.
	Usage llm.Usage `json:"usage"`
	// Redaction is what the guard withheld.
	Redaction redact.Stats `json:"redaction"`
	// ValuesAllowed records whether this run was permitted to send cell values.
	// It belongs in the trace because it is the single most important thing
	// about a run for anybody reviewing one later.
	ValuesAllowed bool `json:"values_allowed"`
	// Context records the customer's own documents this run could reach, and
	// every request Veritix made to reach them. Nil when no context server was
	// configured, which is the default.
	Context *ContextTrace `json:"context,omitempty"`

	// Findings is how many the agent recorded, and Refused how many it proposed
	// that the engine measured at zero. The second number is worth keeping: it
	// is how often the model asserted something the data did not support.
	Findings int `json:"findings"`
	Refused  int `json:"not_reproduced"`
	// Proposals is how many rules the agent proposed. They are not findings
	// and are not applied: somebody accepts them, and from then on the
	// deterministic pass does that part of the job.
	Proposals int `json:"proposals,omitempty"`

	MaxSteps    int `json:"max_steps"`
	TokenBudget int `json:"token_budget,omitempty"`

	Stopped Stopped `json:"stopped"`
	// Error is set when the run ended because the provider failed.
	Error string `json:"error,omitempty"`

	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"-"`
}

// MarshalJSON renders a trace, with its duration suffixed so that a reader
// cannot mistake the unit.
func (t Trace) MarshalJSON() ([]byte, error) {
	type alias Trace
	return json.Marshal(struct {
		alias
		DurationMS int64 `json:"duration_ms"`
	}{alias(t), t.Duration.Milliseconds()})
}

// UnmarshalJSON reads back a stored trace.
func (t *Trace) UnmarshalJSON(b []byte) error {
	type alias Trace
	var wire struct {
		alias
		DurationMS int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*t = Trace(wire.alias)
	t.Duration = time.Duration(wire.DurationMS) * time.Millisecond
	return nil
}

// ContextTrace is the outbound half of the egress record: what Veritix asked
// the customer's own MCP servers for, and what it got.
//
// The trace has always answered "what was the model sent". Once Veritix can
// fetch documents it has to answer a second question — what did Veritix send,
// and to whom — because a context server is the first thing since the model
// itself that anything leaves the process toward. Every entry in Requests is a
// listing or a read of a URI that came out of a listing, which is what makes
// "no text the model wrote leaves the process" something a customer can check
// by reading rather than by believing. The documents that came back are in the
// steps, verbatim, like every other tool result.
type ContextTrace struct {
	// Servers is what each configured server contributed, including the ones
	// that contributed nothing and why.
	Servers []mcpclient.Connection `json:"servers"`
	// Documents is the catalog the model was offered. It carries no URIs, for
	// the same reason the model is not shown any.
	Documents []mcpclient.Document `json:"documents,omitempty"`
	// Requests is every call Veritix made, in order.
	Requests []mcpclient.Request `json:"requests,omitempty"`
	// Read and Bytes are how many documents the model actually read and how
	// much of them was admitted.
	Read  int `json:"documents_read"`
	Bytes int `json:"bytes_admitted"`
}

// contextTrace snapshots a library, or returns nil when there is none.
func contextTrace(lib *mcpclient.Library) *ContextTrace {
	if lib == nil {
		return nil
	}
	read, bytes := lib.Stats()
	return &ContextTrace{
		Servers:   lib.Connections(),
		Documents: lib.Catalog(),
		Requests:  lib.Requests(),
		Read:      read,
		Bytes:     bytes,
	}
}

// Step is one turn: what the model said, and what it asked to be run.
type Step struct {
	N int `json:"step"`
	// Thinking is the model's reasoning, when the provider returns a summary of
	// it. It is often empty, because most providers omit it by default.
	Thinking string `json:"thinking,omitempty"`
	// Text is what the model said in prose.
	Text string `json:"text,omitempty"`
	// Calls are the tools it invoked.
	Calls []TraceCall `json:"calls,omitempty"`
	// Correction is what Veritix sent back when this step's message was a tool
	// call written as prose. It is recorded because the trace is the answer to
	// "what was the model sent", and this is the one thing sent to a model that
	// is neither the brief nor a tool result.
	Correction string `json:"correction,omitempty"`

	StopReason string        `json:"stop_reason,omitempty"`
	Usage      llm.Usage     `json:"usage"`
	Duration   time.Duration `json:"-"`
}

// MarshalJSON renders a step, with its duration suffixed so the unit is not in
// doubt.
func (s Step) MarshalJSON() ([]byte, error) {
	type alias Step
	return json.Marshal(struct {
		alias
		DurationMS int64 `json:"duration_ms"`
	}{alias(s), s.Duration.Milliseconds()})
}

// UnmarshalJSON reads back a stored step.
func (s *Step) UnmarshalJSON(b []byte) error {
	type alias Step
	var wire struct {
		alias
		DurationMS int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*s = Step(wire.alias)
	s.Duration = time.Duration(wire.DurationMS) * time.Millisecond
	return nil
}

// TraceCall is one tool call and its result, verbatim on both sides.
type TraceCall struct {
	Tool string `json:"tool"`
	// Arguments are the model's own words.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Result is exactly what was sent back, as released by the egress guard.
	Result string `json:"result,omitempty"`
	// IsError marks a call the model got wrong, or a finding that did not
	// reproduce.
	IsError    bool  `json:"is_error,omitempty"`
	DurationMS int64 `json:"duration_ms"`
}

// Stopped says why a run ended.
type Stopped string

const (
	// StoppedModelFinished is the ordinary ending: the model had nothing more
	// to do.
	StoppedModelFinished Stopped = "finished"
	// StoppedStepBudget means the loop hit its iteration cap. The findings so
	// far are still valid; the investigation was not necessarily complete.
	StoppedStepBudget Stopped = "step_budget"
	// StoppedTokenBudget means the run hit its token cap.
	StoppedTokenBudget Stopped = "token_budget"
	// StoppedProviderError means the model could not be reached.
	StoppedProviderError Stopped = "provider_error"
	// StoppedRefused means the provider declined the request.
	StoppedRefused Stopped = "refused"
	// StoppedCanceled means somebody stopped the run.
	StoppedCanceled Stopped = "canceled"
)

// Complete reports whether the agent finished its own investigation rather than
// being cut short. A report should say so when it did not.
func (s Stopped) Complete() bool { return s == StoppedModelFinished }

// Summary renders a trace as one line, for logs and for the report header.
func (t *Trace) Summary() string {
	if t == nil {
		return ""
	}
	steps := len(t.Steps)
	var calls int
	for _, s := range t.Steps {
		calls += len(s.Calls)
	}
	return formatSummary(t.Model, steps, calls, t.Findings, t.Proposals, t.Usage.Total(), t.Stopped)
}

func formatSummary(model string, steps, calls, findings, proposals, tokens int, stopped Stopped) string {
	out := fmt.Sprintf("%s: %d steps, %d tool calls, %d findings, %d tokens",
		model, steps, calls, findings, tokens)
	if proposals > 0 {
		out += fmt.Sprintf(", %d rules proposed", proposals)
	}
	if !stopped.Complete() {
		out += " (" + string(stopped) + ")"
	}
	return out
}
