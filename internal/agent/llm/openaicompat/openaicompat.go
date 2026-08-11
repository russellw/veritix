// Package openaicompat drives any endpoint that speaks OpenAI's
// chat-completions dialect: Ollama, vLLM, LM Studio, llama.cpp's server, and
// OpenAI itself.
//
// This is the provider that makes Veritix's premise hold all the way down. A
// customer who will not send their ledger to a software vendor is usually the
// same customer who will not send it to a model vendor, and for them the
// auditor has to work against a model running on their own hardware. That is
// one endpoint shape covering every local runtime worth naming, so it is
// written by hand against the wire format rather than through a vendor SDK: the
// dialect is small, and the servers implementing it disagree about the corners
// in ways an SDK written for the reference implementation would hide.
//
// Two things do not survive the translation, and both are stated rather than
// silently dropped: reasoning blocks come back without the signature the
// Anthropic path replays (this dialect has nowhere to put one), and a server
// that ignores `tool_choice` may answer in prose where Claude would have called
// a tool. The loop copes with both, because a model that says nothing useful is
// a model that records no findings, and a run with no findings is a legitimate
// outcome rather than a failure.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/russellwallace/veritix/internal/agent/llm"
)

// DefaultBaseURL points at a local Ollama, which is the most common way a
// customer runs a model on their own machine.
const DefaultBaseURL = "http://localhost:11434/v1"

const defaultMaxTokens = 8192

// Provider is a client for an OpenAI-compatible endpoint.
type Provider struct {
	http    *http.Client
	baseURL string
	apiKey  string
	model   string
}

// Options configure the provider.
type Options struct {
	// BaseURL is the endpoint root, up to but excluding /chat/completions.
	// Empty picks DefaultBaseURL.
	BaseURL string
	// APIKey is sent as a bearer token when set. Local servers usually want
	// none, and sending an empty one upsets some of them.
	APIKey string
	// Model is the model identifier. Required: there is no sensible default
	// when the endpoint might be serving anything.
	Model string
	// Timeout bounds one request. Zero picks ten minutes, because a local model
	// on CPU is slow rather than broken.
	Timeout time.Duration
}

// New returns a provider.
func New(opts Options) (*Provider, error) {
	if opts.Model == "" {
		return nil, fmt.Errorf("openai-compatible: a model name is required (there is no default for an arbitrary endpoint)")
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Provider{
		http:    &http.Client{Timeout: timeout},
		baseURL: strings.TrimSuffix(base, "/"),
		apiKey:  opts.APIKey,
		model:   opts.Model,
	}, nil
}

// Name identifies the provider.
func (p *Provider) Name() string { return "openai-compatible" }

// Model is the model in use.
func (p *Provider) Model() string { return p.model }

// The wire types. They are private because nothing outside this package should
// be thinking in this dialect.

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	Tools           []chatTool    `json:"tools,omitempty"`
	ToolChoice      string        `json:"tool_choice,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is a pointer so that an assistant turn carrying only tool calls
	// serializes as null rather than "", which some servers reject.
	Content    *string    `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name string `json:"name"`
	// Arguments is JSON encoded as a string, which is this dialect's own
	// oddity: the schema is structured, the call that satisfies it is not.
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatToolFunc `json:"function"`
}

type chatToolFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
			// ReasoningContent is what several servers call a reasoning trace.
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete sends one request.
func (p *Provider) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	body, err := json.Marshal(p.request(req))
	if err != nil {
		return nil, &llm.Error{Provider: p.Name(), Message: "encoding the request: " + err.Error(), Err: err}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &llm.Error{Provider: p.Name(), Message: err.Error(), Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.http.Do(httpReq)
	if err != nil {
		// No response at all: the server is down, slow, or unreachable, and
		// any of those can resolve on their own.
		return nil, &llm.Error{Provider: p.Name(), Message: err.Error(), Retryable: true, Err: err}
	}
	defer resp.Body.Close() //nolint:errcheck // the decode below reports what matters

	// Capped because an endpoint that is not what it claims to be — a proxy
	// error page, say — should not be read into memory in its entirety.
	const maxBody = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, &llm.Error{Provider: p.Name(), Status: resp.StatusCode,
			Message: "reading the response: " + err.Error(), Retryable: true, Err: err}
	}

	var decoded chatResponse
	decodeErr := json.Unmarshal(raw, &decoded)

	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if decodeErr == nil && decoded.Error != nil && decoded.Error.Message != "" {
			msg = decoded.Error.Message
		}
		return nil, &llm.Error{
			Provider:  p.Name(),
			Status:    resp.StatusCode,
			Message:   truncate(msg, 500),
			Retryable: llm.RetryableStatus(resp.StatusCode),
		}
	}
	if decodeErr != nil {
		return nil, &llm.Error{Provider: p.Name(), Status: resp.StatusCode,
			Message: "the endpoint did not return chat-completions JSON: " + decodeErr.Error(),
			Err:     decodeErr}
	}
	if len(decoded.Choices) == 0 {
		return nil, &llm.Error{Provider: p.Name(), Status: resp.StatusCode,
			Message: "the endpoint returned no choices"}
	}

	return p.response(&decoded), nil
}

func (p *Provider) request(req *llm.Request) chatRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	out := chatRequest{
		Model:           p.model,
		MaxTokens:       maxTokens,
		ReasoningEffort: req.Effort,
	}

	if req.System != "" {
		system := req.System
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: &system})
	}

	for _, m := range req.Messages {
		out.Messages = append(out.Messages, translateMessage(m)...)
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, chatTool{
			Type: "function",
			Function: chatToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema(),
			},
		})
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = "auto"
	}

	return out
}

// translateMessage turns one Veritix turn into the one or more messages this
// dialect needs, because a tool result is a message here rather than part of
// one.
func translateMessage(m llm.Message) []chatMessage {
	var out []chatMessage

	// Tool results come first: they answer the previous assistant turn, and
	// this dialect requires each to be its own message keyed by call id.
	for _, p := range m.Parts {
		if p.Kind != llm.PartToolResult {
			continue
		}
		result := p.Result
		out = append(out, chatMessage{
			Role:       "tool",
			ToolCallID: p.ID,
			Content:    &result,
		})
	}

	var (
		text  strings.Builder
		calls []toolCall
	)
	for _, p := range m.Parts {
		switch p.Kind {
		case llm.PartText:
			if p.Text == "" {
				continue
			}
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(p.Text)
		case llm.PartToolUse:
			args := string(p.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, toolCall{
				ID: p.ID, Type: "function",
				Function: toolCallFunc{Name: p.Name, Arguments: args},
			})
		}
		// Thinking parts are dropped: there is nowhere to put them in this
		// dialect, and inventing a place would send the model its own
		// reasoning as if a human had written it.
	}

	if text.Len() == 0 && len(calls) == 0 {
		return out
	}

	msg := chatMessage{Role: string(m.Role)}
	if text.Len() > 0 {
		s := text.String()
		msg.Content = &s
	}
	msg.ToolCalls = calls
	return append(out, msg)
}

func (p *Provider) response(decoded *chatResponse) *llm.Response {
	choice := decoded.Choices[0]

	res := &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant},
		StopReason: stopReason(choice.FinishReason),
		Model:      decoded.Model,
		Usage: llm.Usage{
			Input:     decoded.Usage.PromptTokens,
			Output:    decoded.Usage.CompletionTokens,
			CacheRead: decoded.Usage.PromptDetails.CachedTokens,
			Reasoning: decoded.Usage.CompletionDetails.ReasoningTokens,
		},
	}
	if res.Model == "" {
		res.Model = p.model
	}

	if r := choice.Message.ReasoningContent; r != "" {
		// Carried for the trace, never replayed: it has no signature, and this
		// dialect has no block to replay it into.
		res.Message.Parts = append(res.Message.Parts, llm.Part{Kind: llm.PartThinking, Text: r})
	}
	if c := choice.Message.Content; c != "" {
		res.Message.Parts = append(res.Message.Parts, llm.Part{Kind: llm.PartText, Text: c})
	}
	for _, call := range choice.Message.ToolCalls {
		args := call.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		res.Message.Parts = append(res.Message.Parts, llm.Part{
			Kind:  llm.PartToolUse,
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: json.RawMessage(args),
		})
	}

	// Some servers report "stop" while still returning tool calls. What the
	// message contains is the more reliable signal than what the server says
	// about it.
	if len(choice.Message.ToolCalls) > 0 {
		res.StopReason = llm.StopToolUse
	}

	return res
}

func stopReason(finish string) string {
	switch finish {
	case "tool_calls", "function_call":
		return llm.StopToolUse
	case "length":
		return llm.StopMaxTokens
	case "content_filter":
		return llm.StopRefusal
	default:
		return llm.StopEndTurn
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
