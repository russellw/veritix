// Package agent is Veritix's investigative auditor: a tool-calling loop that
// explores a dataset and proposes findings.
//
// The bet the package rests on is stated once here and enforced everywhere
// else. A language model is good at noticing that something looks wrong and
// bad at being trusted about how wrong it is. So the model chooses what to
// look at and writes the explanation, and every number it reports comes out of
// the engine: record_finding runs the model's own evidence query and keeps
// what that returns, and finding.Set.Verify runs it again before anything is
// reported. A finding that does not reproduce is dropped rather than printed.
//
// That is what separates this from a plausible-sounding summary, and it is why
// the agent is additive: it runs after the deterministic checks, over the same
// engine, and its findings go into the same set to be verified alongside them.
// Turning it off loses investigation and loses nothing else.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/russellwallace/veritix/internal/agent/llm"
	"github.com/russellwallace/veritix/internal/agent/redact"
	"github.com/russellwallace/veritix/internal/agent/tools"
	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/profile"
)

// Options configure a run of the agent.
type Options struct {
	// Provider is the model. Required.
	Provider llm.Provider
	// Policy is the egress policy for this run.
	Policy redact.Policy
	// MaxSteps bounds the loop: one step is one model call plus the tool calls
	// it asks for. Zero picks a default.
	MaxSteps int
	// TokenBudget stops the run once this many tokens have been spent. Zero
	// means no cap, which is only reasonable when MaxSteps is doing the work.
	TokenBudget int
	// MaxTokens bounds one model response.
	MaxTokens int
	// Effort asks the model for more or less deliberation.
	Effort string
	// MaxRows caps the rows a tool query returns.
	MaxRows int
	// RequestTimeout bounds a single model call. Zero means the provider's own.
	RequestTimeout time.Duration
}

const (
	defaultMaxSteps = 40
	// maxRetries is how many times a call is retried when the provider says the
	// failure was about the moment rather than the request.
	maxRetries = 3
)

// Result is what a run of the agent produced.
type Result struct {
	// Findings are the agent's proposals, each already measured once by the
	// engine. They still have to survive Verify.
	Findings []finding.Finding
	// Trace is the record of how they were arrived at.
	Trace *Trace
}

// Input is the dataset the agent works on.
type Input struct {
	Engine  *engine.Engine
	Profile *profile.Dataset
	// Known is what the deterministic pass already found, so the agent does not
	// spend its budget rediscovering it.
	Known []finding.Finding
	// Root is the dataset's root path, for the brief.
	Root string
}

// Run drives the loop until the model stops, the budget runs out, or the
// context is cancelled.
//
// It returns an error only when the run could not happen at all — no provider,
// a model that cannot be reached. A model that misbehaves is not an error: it
// produces a run with few findings or none, which is a result the report can
// state honestly.
func Run(ctx context.Context, in Input, opts Options, log *slog.Logger) (*Result, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Provider == nil {
		return nil, errors.New("agent: no model provider is configured")
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = defaultMaxSteps
	}

	guard := redact.New(opts.Policy)
	world := &tools.World{
		Engine:  in.Engine,
		Profile: in.Profile,
		Guard:   guard,
		MaxRows: opts.MaxRows,
		Log:     log,
	}
	registry := tools.New(world)

	trace := &Trace{
		Provider:      opts.Provider.Name(),
		Model:         opts.Provider.Model(),
		StartedAt:     time.Now(),
		MaxSteps:      opts.MaxSteps,
		TokenBudget:   opts.TokenBudget,
		ValuesAllowed: opts.Policy.AllowValues,
	}

	req := &llm.Request{
		System:    systemPrompt,
		Tools:     registry.Definitions(),
		MaxTokens: opts.MaxTokens,
		Effort:    opts.Effort,
		Messages: []llm.Message{{
			Role:  llm.RoleUser,
			Parts: []llm.Part{{Kind: llm.PartText, Text: brief(in.Profile, in.Known, in.Root)}},
		}},
	}

	log.Info("agent starting",
		"provider", trace.Provider, "model", trace.Model,
		"max_steps", opts.MaxSteps, "values_allowed", opts.Policy.AllowValues)

	for step := 1; ; step++ {
		if err := ctx.Err(); err != nil {
			trace.Stopped = StoppedCancelled
			break
		}
		if step > opts.MaxSteps {
			trace.Stopped = StoppedStepBudget
			log.Warn("the agent reached its step budget", "steps", opts.MaxSteps)
			break
		}
		if opts.TokenBudget > 0 && trace.Usage.Total() >= opts.TokenBudget {
			trace.Stopped = StoppedTokenBudget
			log.Warn("the agent reached its token budget",
				"tokens", trace.Usage.Total(), "budget", opts.TokenBudget)
			break
		}

		stepStarted := time.Now()
		res, err := complete(ctx, opts, req, log)
		if err != nil {
			// The conversation cannot continue, but whatever was recorded
			// before this point is still evidence-backed and still counts.
			trace.Stopped = StoppedProviderError
			trace.Error = err.Error()
			log.Error("the agent's model call failed", "step", step, "error", err)
			break
		}

		trace.Usage.Add(res.Usage)
		entry := Step{
			N:          step,
			Text:       res.Message.Text(),
			StopReason: res.StopReason,
			Usage:      res.Usage,
		}
		for _, p := range res.Message.Parts {
			if p.Kind == llm.PartThinking && p.Text != "" {
				entry.Thinking = p.Text
			}
		}

		req.Messages = append(req.Messages, res.Message)

		calls := res.Message.ToolCalls()
		if len(calls) == 0 {
			// Nothing more to do: the model has said its piece.
			entry.Duration = time.Since(stepStarted)
			trace.Steps = append(trace.Steps, entry)
			if trace.Stopped == "" {
				trace.Stopped = StoppedModelFinished
			}
			if res.StopReason == llm.StopRefusal {
				trace.Stopped = StoppedRefused
				log.Warn("the model declined the request")
			}
			break
		}

		results := make([]llm.Part, 0, len(calls))
		for _, call := range calls {
			started := time.Now()
			out := registry.Invoke(ctx, call.Name, call.Input)
			took := time.Since(started)

			entry.Calls = append(entry.Calls, TraceCall{
				Tool: call.Name,
				// The arguments are the model's own words, and the result is
				// the exact bytes that went back to it. Together they are the
				// record of everything that crossed the boundary.
				Arguments:  call.Input,
				Result:     out.Payload.String(),
				IsError:    out.IsError,
				DurationMS: took.Milliseconds(),
			})

			results = append(results, llm.Part{
				Kind:    llm.PartToolResult,
				ID:      call.ID,
				Result:  out.Payload.String(),
				IsError: out.IsError,
			})
		}

		entry.Duration = time.Since(stepStarted)
		trace.Steps = append(trace.Steps, entry)
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser, Parts: results})
	}

	findings := world.Findings()

	trace.Duration = time.Since(trace.StartedAt)
	trace.Findings = len(findings)
	trace.Refused = world.Refused()
	trace.Redaction = guard.Stats()
	if trace.Stopped == "" {
		trace.Stopped = StoppedModelFinished
	}

	log.Info("agent complete",
		"steps", len(trace.Steps),
		"findings", trace.Findings,
		"not_reproduced", trace.Refused,
		"tokens", trace.Usage.Total(),
		"stopped", string(trace.Stopped),
		"duration", trace.Duration.Round(time.Millisecond))

	return &Result{Findings: findings, Trace: trace}, nil
}

// complete makes one model call, retrying the failures the provider says are
// about the moment rather than the request — but never its own expired
// deadline, which is about neither.
func complete(ctx context.Context, opts Options, req *llm.Request, log *slog.Logger) (*llm.Response, error) {
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		callCtx := ctx
		var cancel context.CancelFunc
		if opts.RequestTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, opts.RequestTimeout)
		}

		res, err := opts.Provider.Complete(callCtx, req)
		// Read this before cancel(), which would set Err on a context whose
		// deadline had not actually fired.
		expired := errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return res, nil
		}
		lastErr = err

		// A deadline Veritix imposed on itself is not a transient condition. It
		// reaches the provider as a dead connection and comes back marked
		// retryable, because from down there it is indistinguishable from one —
		// but retrying asks the identical question of the same endpoint with the
		// same deadline, so a model that was too slow once is too slow three
		// times, for three times as long. A local model on a CPU hits this as a
		// matter of course: what it needs is a longer timeout, not another go.
		if expired {
			return nil, fmt.Errorf("no reply within llm.request_timeout (%s); "+
				"raise it if the model is slow rather than stuck: %w", opts.RequestTimeout, err)
		}

		var provErr *llm.Error
		if !errors.As(err, &provErr) || !provErr.Retryable || ctx.Err() != nil {
			return nil, err
		}
		if attempt == maxRetries {
			break
		}

		// Plain exponential backoff. There is no jitter because there is one
		// client here, not a fleet of them synchronising on a shared outage.
		wait := time.Duration(1<<attempt) * time.Second
		log.Warn("retrying the model call", "attempt", attempt, "wait", wait, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}

	return nil, fmt.Errorf("after %d attempts: %w", maxRetries, lastErr)
}
