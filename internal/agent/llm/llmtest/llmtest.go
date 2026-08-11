// Package llmtest provides a scripted model, so that the agent loop can be
// tested without a model.
//
// Every test of the loop, the tools, and the egress guard runs against this:
// the point of those tests is that Veritix behaves correctly given what a model
// said, and a real model would make them slow, expensive, and non-deterministic
// while testing somebody else's software. It also records every outbound
// request, which is what lets a test assert that no cell value was in any of
// them.
package llmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/russellw/veritix/internal/agent/llm"
)

// Turn is one scripted reply.
type Turn struct {
	// Text is prose the model "says".
	Text string
	// Calls are tools it asks for.
	Calls []Call
	// Err, when set, is returned instead of a reply.
	Err error
	// Usage is what the turn claims to have cost.
	Usage llm.Usage
}

// Call is one scripted tool call.
type Call struct {
	ID    string
	Name  string
	Input map[string]any
}

// Provider replays scripted turns and records what it was sent.
type Provider struct {
	mu       sync.Mutex
	turns    []Turn
	next     int
	requests []llm.Request
	// Reply, when set, produces a turn from the conversation so far, which is
	// how a test writes a model that reacts rather than one that recites.
	Reply func(req *llm.Request) Turn
}

// New returns a provider that replays these turns in order. When the script
// runs out it ends the conversation, so a loop under test always terminates.
func New(turns ...Turn) *Provider { return &Provider{turns: turns} }

// Name identifies the provider.
func (p *Provider) Name() string { return "scripted" }

// Model is the model in use.
func (p *Provider) Model() string { return "scripted-model" }

// Complete records the request and returns the next scripted turn.
func (p *Provider) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests = append(p.requests, cloneRequest(req))

	var turn Turn
	switch {
	case p.Reply != nil:
		turn = p.Reply(req)
	case p.next < len(p.turns):
		turn = p.turns[p.next]
		p.next++
	default:
		turn = Turn{Text: "Nothing further."}
	}

	if turn.Err != nil {
		return nil, turn.Err
	}

	res := &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant},
		StopReason: llm.StopEndTurn,
		Usage:      turn.Usage,
		Model:      p.Model(),
	}
	if turn.Text != "" {
		res.Message.Parts = append(res.Message.Parts, llm.Part{Kind: llm.PartText, Text: turn.Text})
	}
	for i, c := range turn.Calls {
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call-%d-%d", p.next, i)
		}
		input, err := json.Marshal(c.Input)
		if err != nil {
			return nil, fmt.Errorf("llmtest: encoding scripted arguments for %s: %w", c.Name, err)
		}
		res.Message.Parts = append(res.Message.Parts, llm.Part{
			Kind: llm.PartToolUse, ID: id, Name: c.Name, Input: input,
		})
		res.StopReason = llm.StopToolUse
	}

	return res, nil
}

// Requests returns every request the provider was sent.
func (p *Provider) Requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]llm.Request, len(p.requests))
	copy(out, p.requests)
	return out
}

// Calls reports how many times the model was called.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// Outbound returns everything that was sent to the model as one string: the
// system prompt, every message, and every tool description and schema.
//
// It is what an egress test scans. Assembling it here rather than in each test
// means a test cannot accidentally check only the part that happens to be
// clean.
func Outbound(reqs []llm.Request) string {
	var b []byte
	for _, r := range reqs {
		b = append(b, r.System...)
		b = append(b, '\n')
		for _, t := range r.Tools {
			b = append(b, t.Name...)
			b = append(b, ' ')
			b = append(b, t.Description...)
			b = append(b, '\n')
			if schema, err := json.Marshal(t.Schema()); err == nil {
				b = append(b, schema...)
				b = append(b, '\n')
			}
		}
		for _, m := range r.Messages {
			for _, p := range m.Parts {
				b = append(b, p.Text...)
				b = append(b, '\n')
				b = append(b, p.Result...)
				b = append(b, '\n')
				b = append(b, p.Name...)
				b = append(b, '\n')
				b = append(b, p.Input...)
				b = append(b, '\n')
			}
		}
	}
	return string(b)
}

// cloneRequest copies a request so that a later mutation by the caller cannot
// rewrite what a test believes was sent.
func cloneRequest(req *llm.Request) llm.Request {
	out := *req
	out.Messages = make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		out.Messages[i] = llm.Message{Role: m.Role, Parts: append([]llm.Part(nil), m.Parts...)}
	}
	out.Tools = append([]llm.Tool(nil), req.Tools...)
	return out
}
