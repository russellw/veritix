// Package anthropic drives Claude through the official SDK.
//
// It is the default provider and the one the agent is tuned against. The
// translation here is deliberately dull: everything interesting about how
// Veritix uses a model lives in the loop, the tools, and the egress guard, so
// that swapping the provider changes what answers and nothing else.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/russellw/veritix/internal/agent/llm"
)

// DefaultModel is what a run uses unless the operator names another.
const DefaultModel = "claude-opus-5"

// defaultMaxTokens leaves the model room to think and to answer. Thinking and
// response text share this budget, so a figure sized around the answer alone
// truncates the answer.
const defaultMaxTokens = 16000

// Provider is a Claude client.
type Provider struct {
	client sdk.Client
	model  string
}

// Options configure the provider.
type Options struct {
	// APIKey authenticates. Empty falls back to the SDK's own resolution,
	// which reads ANTHROPIC_API_KEY and the operator's stored credentials.
	APIKey string
	// BaseURL overrides the endpoint, for a proxy or a gateway.
	BaseURL string
	// Model is the model identifier. Empty picks DefaultModel.
	Model string
}

// New returns a provider.
func New(opts Options) *Provider {
	var client []option.RequestOption
	if opts.APIKey != "" {
		client = append(client, option.WithAPIKey(opts.APIKey))
	}
	if opts.BaseURL != "" {
		client = append(client, option.WithBaseURL(opts.BaseURL))
	}

	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	return &Provider{client: sdk.NewClient(client...), model: model}
}

// Name identifies the provider.
func (p *Provider) Name() string { return "anthropic" }

// Model is the model in use.
func (p *Provider) Model() string { return p.model }

// Complete sends one request.
func (p *Provider) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	params, err := p.params(req)
	if err != nil {
		return nil, err
	}

	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, translateError(err)
	}
	return translateResponse(msg), nil
}

func (p *Provider) params(req *llm.Request) (sdk.MessageNewParams, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	params := sdk.MessageNewParams{
		Model:     p.model,
		MaxTokens: int64(maxTokens),
	}

	// The system prompt is the same on every turn of a run and sits at the
	// front of the prefix, so a cache breakpoint here is read back by every
	// subsequent call. Tools render before it and are cached with it. That
	// covers a fixed few thousand tokens; markConversationPrefix below covers
	// the transcript, which is the part that grows.
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{
			Text:         req.System,
			CacheControl: sdk.NewCacheControlEphemeralParam(),
		}}
	}

	if req.Effort != "" {
		params.OutputConfig = sdk.OutputConfigParam{
			Effort: sdk.OutputConfigEffort(req.Effort),
		}
	}

	for _, t := range req.Tools {
		tool := sdk.ToolParam{
			Name:        t.Name,
			Description: sdk.String(t.Description),
			InputSchema: sdk.ToolInputSchemaParam{
				Properties: t.Properties,
				Required:   t.Required,
			},
		}
		params.Tools = append(params.Tools, sdk.ToolUnionParam{OfTool: &tool})
	}

	for _, m := range req.Messages {
		blocks, err := translateParts(m.Parts)
		if err != nil {
			return params, err
		}
		if len(blocks) == 0 {
			continue
		}
		switch m.Role {
		case llm.RoleAssistant:
			params.Messages = append(params.Messages, sdk.NewAssistantMessage(blocks...))
		default:
			params.Messages = append(params.Messages, sdk.NewUserMessage(blocks...))
		}
	}

	markConversationPrefix(params.Messages)

	return params, nil
}

// markConversationPrefix puts cache breakpoints at the end of the conversation.
//
// The system breakpoint above only covers the prompt and the tools, which are
// a fixed few thousand tokens. What actually grows is the transcript: a step
// re-sends every earlier tool call and result, so without this the same bytes
// are billed at full input price on every call and the cost of a run is
// quadratic in its length. An 18-step audit measured 257k full-price input
// tokens against 66k of cache reads, the latter being nothing but the system
// prefix read back once per step.
//
// Two breakpoints, not one, because a breakpoint finds an earlier entry only by
// walking back at most 20 content blocks, and a step appends both an assistant
// message and the user message carrying its tool results. Marking each halves
// the distance the next request has to reach back, so the hit survives a step
// that fires many tools at once. Three in total is inside the limit of four.
func markConversationPrefix(messages []sdk.MessageParam) {
	marked := 0
	for i := len(messages) - 1; i >= 0 && marked < 2; i-- {
		if markLastBlock(messages[i].Content) {
			marked++
		}
	}
}

// markLastBlock sets a breakpoint on the last block of a message that can carry
// one, reporting whether it found any. Thinking blocks cannot, so a message
// ending in one is walked past rather than counted as marked.
func markLastBlock(blocks []sdk.ContentBlockParamUnion) bool {
	for i := len(blocks) - 1; i >= 0; i-- {
		// The union holds a pointer to the block, so this writes through to
		// the block the request will marshal.
		if cc := blocks[i].GetCacheControl(); cc != nil {
			*cc = sdk.NewCacheControlEphemeralParam()
			return true
		}
	}
	return false
}

func translateParts(parts []llm.Part) ([]sdk.ContentBlockParamUnion, error) {
	out := make([]sdk.ContentBlockParamUnion, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case llm.PartText:
			if p.Text == "" {
				continue
			}
			out = append(out, sdk.NewTextBlock(p.Text))

		case llm.PartThinking:
			// Replayed exactly as received, signature and all: a modified
			// thinking block is a 400, and an omitted one breaks the ordering
			// the provider checks.
			out = append(out, sdk.NewThinkingBlock(p.Signature, p.Text))

		case llm.PartRedactedThinking:
			out = append(out, sdk.NewRedactedThinkingBlock(p.Data))

		case llm.PartToolUse:
			var input any
			if len(p.Input) > 0 {
				if err := json.Unmarshal(p.Input, &input); err != nil {
					return nil, fmt.Errorf("anthropic: replaying tool call %s: %w", p.Name, err)
				}
			} else {
				input = map[string]any{}
			}
			out = append(out, sdk.NewToolUseBlock(p.ID, input, p.Name))

		case llm.PartToolResult:
			out = append(out, sdk.NewToolResultBlock(p.ID, p.Result, p.IsError))
		}
	}
	return out, nil
}

func translateResponse(msg *sdk.Message) *llm.Response {
	res := &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant},
		StopReason: string(msg.StopReason),
		Model:      msg.Model,
		Usage: llm.Usage{
			Input:      int(msg.Usage.InputTokens),
			Output:     int(msg.Usage.OutputTokens),
			CacheRead:  int(msg.Usage.CacheReadInputTokens),
			CacheWrite: int(msg.Usage.CacheCreationInputTokens),
		},
	}

	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			res.Message.Parts = append(res.Message.Parts, llm.Part{
				Kind: llm.PartText, Text: b.Text,
			})
		case sdk.ThinkingBlock:
			res.Message.Parts = append(res.Message.Parts, llm.Part{
				Kind: llm.PartThinking, Text: b.Thinking, Signature: b.Signature,
			})
		case sdk.RedactedThinkingBlock:
			res.Message.Parts = append(res.Message.Parts, llm.Part{
				Kind: llm.PartRedactedThinking, Data: b.Data,
			})
		case sdk.ToolUseBlock:
			res.Message.Parts = append(res.Message.Parts, llm.Part{
				Kind:  llm.PartToolUse,
				ID:    b.ID,
				Name:  b.Name,
				Input: json.RawMessage(b.JSON.Input.Raw()),
			})
		}
	}

	return res
}

// translateError normalizes an SDK error, keeping whether a retry could help.
func translateError(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return &llm.Error{
			Provider:  "anthropic",
			Status:    apiErr.StatusCode,
			Message:   apiErr.Error(),
			Retryable: llm.RetryableStatus(apiErr.StatusCode),
			Err:       err,
		}
	}
	// No HTTP response at all: a transport failure, which is worth retrying.
	return &llm.Error{
		Provider:  "anthropic",
		Message:   err.Error(),
		Retryable: true,
		Err:       err,
	}
}
