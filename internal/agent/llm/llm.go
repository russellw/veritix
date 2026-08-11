// Package llm is Veritix's model-provider abstraction: the shape of a
// tool-calling conversation, expressed so that the agent loop does not know
// which vendor is answering.
//
// The abstraction exists for a commercial reason rather than a tidiness one.
// Veritix runs on the customer's own hardware, and a customer who will not send
// their sales ledger to a vendor is often the same customer who will not send
// it to a model API either. Such a customer runs Ollama or vLLM on the box next
// to it, and the auditor has to work the same way against both. So the loop,
// the tools, and the egress guard are written once, and a provider is a thin
// translation to somebody's wire format.
//
// The types here are deliberately close to Anthropic's Messages API, because
// that is the surface with the richest tool-calling semantics; the
// OpenAI-compatible provider translates down to what its endpoint supports and
// says what it dropped.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Role is who a message is from. There are only two: system instructions are a
// field on the request, and tool results are carried as parts of a user turn,
// which is how both providers model them underneath.
type Role string

const (
	// RoleUser is Veritix speaking: the task, and the results of tool calls.
	RoleUser Role = "user"
	// RoleAssistant is the model speaking.
	RoleAssistant Role = "assistant"
)

// PartKind is what a piece of a message contains.
type PartKind string

const (
	// PartText is prose.
	PartText PartKind = "text"
	// PartThinking is the model's reasoning. It is carried so that it can be
	// replayed to the same model unchanged on the next turn, which is what the
	// provider requires; it is not otherwise interpreted.
	PartThinking PartKind = "thinking"
	// PartRedactedThinking is reasoning the provider encrypted. It is opaque
	// and exists only to be handed back.
	PartRedactedThinking PartKind = "redacted_thinking"
	// PartToolUse is the model asking for a tool to be run.
	PartToolUse PartKind = "tool_use"
	// PartToolResult is Veritix answering a PartToolUse.
	PartToolResult PartKind = "tool_result"
)

// Part is one piece of a message. Which fields are meaningful depends on Kind.
type Part struct {
	Kind PartKind

	// Text carries prose for PartText and reasoning for PartThinking.
	Text string
	// Signature authenticates a thinking block. It must be replayed exactly as
	// received or the provider rejects the turn.
	Signature string
	// Data is the opaque payload of a PartRedactedThinking.
	Data string

	// ID is the tool call's identifier: assigned by the model on a PartToolUse
	// and echoed by Veritix on the matching PartToolResult.
	ID string
	// Name is the tool being called.
	Name string
	// Input is the model's arguments, as JSON.
	Input json.RawMessage

	// Result is the tool's output. It comes from the egress guard, which is the
	// only thing that can produce one.
	Result string
	// IsError marks a tool result as a failure, so the model can correct itself
	// rather than treating an error message as data.
	IsError bool
}

// Message is one conversational turn.
type Message struct {
	Role  Role
	Parts []Part
}

// Text returns the message's prose, joined.
func (m Message) Text() string {
	var out string
	for _, p := range m.Parts {
		if p.Kind == PartText {
			if out != "" {
				out += "\n"
			}
			out += p.Text
		}
	}
	return out
}

// ToolCalls returns the tool calls the message asks for.
func (m Message) ToolCalls() []Part {
	var out []Part
	for _, p := range m.Parts {
		if p.Kind == PartToolUse {
			out = append(out, p)
		}
	}
	return out
}

// Tool is a capability offered to the model.
//
// The schema is given as properties and required names rather than as a raw
// JSON Schema document, because both providers want it structured and neither
// wants the same envelope around it.
type Tool struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

// Schema renders the tool's JSON Schema, for providers that want one document.
func (t Tool) Schema() map[string]any {
	props := t.Properties
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(t.Required) > 0 {
		s["required"] = t.Required
	}
	return s
}

// Request is one call to a model.
type Request struct {
	// System is the instruction block. It is stable across a run's turns, which
	// is what lets a provider cache it.
	System string
	// Messages is the conversation so far.
	Messages []Message
	// Tools are the capabilities offered.
	Tools []Tool
	// MaxTokens bounds one response. Zero picks the provider's default.
	MaxTokens int
	// Effort asks for more or less deliberation: one of low, medium, high,
	// xhigh, max. Empty leaves the provider's default alone. Providers that
	// have no equivalent ignore it.
	Effort string
}

// Usage is what a call cost, in tokens.
type Usage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_tokens,omitempty"`
	CacheWrite int `json:"cache_write_tokens,omitempty"`
	Reasoning  int `json:"reasoning_tokens,omitempty"`
}

// Total is every token the call was billed for.
func (u Usage) Total() int { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// Add accumulates another call's usage.
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Reasoning += o.Reasoning
}

// Stop reasons, normalised across providers.
const (
	// StopEndTurn means the model finished speaking.
	StopEndTurn = "end_turn"
	// StopToolUse means the model is waiting for tool results.
	StopToolUse = "tool_use"
	// StopMaxTokens means the response was cut off.
	StopMaxTokens = "max_tokens"
	// StopRefusal means the provider declined the request.
	StopRefusal = "refusal"
)

// Response is one model reply.
type Response struct {
	Message    Message
	StopReason string
	Usage      Usage
	// Model is what actually answered, which is not always what was asked for.
	Model string
}

// Provider is a model that can hold a tool-calling conversation.
type Provider interface {
	// Name identifies the provider in traces and logs.
	Name() string
	// Model is the model identifier in use.
	Model() string
	// Complete sends one request and returns one reply.
	Complete(ctx context.Context, req *Request) (*Response, error)
}

// Error is a provider failure, carrying whether retrying could help.
type Error struct {
	Provider string
	Status   int
	Message  string
	// Retryable marks a failure that is about the moment rather than the
	// request: rate limits, overloads, and transport errors.
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: %d: %s", e.Provider, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Provider, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// RetryableStatus reports whether an HTTP status is worth retrying. 429 is a
// rate limit, 529 is Anthropic's overload signal, and 5xx is the server having
// a bad time; everything else is the request's own fault and will fail again.
func RetryableStatus(status int) bool {
	return status == 429 || status >= 500
}
