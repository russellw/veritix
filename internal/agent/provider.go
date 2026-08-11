package agent

import (
	"fmt"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/agent/llm/anthropic"
	"github.com/russellw/veritix/internal/agent/llm/openaicompat"
	"github.com/russellw/veritix/internal/agent/redact"
	"github.com/russellw/veritix/internal/config"
)

// Configure builds the agent's options from the operator's configuration,
// returning nil when no model is configured.
//
// A nil result is the normal case rather than a failure. Veritix without a
// model is the complete deterministic auditor that M2 shipped, and that is what
// `provider: none` means: no key to obtain, nothing leaving the machine, and a
// report that says exactly what it checked. The agent is what a customer turns
// on afterwards.
func Configure(cfg config.LLM) (*Options, error) {
	if cfg.Provider == "" || cfg.Provider == config.ProviderNone {
		return nil, nil
	}

	provider, err := newProvider(cfg)
	if err != nil {
		return nil, err
	}

	return &Options{
		Provider: provider,
		Policy: redact.Policy{
			AllowValues: cfg.AllowSampleValues,
		},
		MaxSteps:       cfg.MaxSteps,
		TokenBudget:    cfg.TokenBudget,
		RequestTimeout: cfg.RequestTimeout,
	}, nil
}

func newProvider(cfg config.LLM) (llm.Provider, error) {
	switch cfg.Provider {
	case config.ProviderAnthropic:
		// The key may legitimately be empty: the SDK also reads
		// ANTHROPIC_API_KEY and the operator's stored credentials, and
		// refusing here would break a machine that is already logged in.
		return anthropic.New(anthropic.Options{
			APIKey:  cfg.APIKey,
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model,
		}), nil

	case config.ProviderOpenAICompatible:
		p, err := openaicompat.New(openaicompat.Options{
			BaseURL: cfg.BaseURL,
			APIKey:  cfg.APIKey,
			Model:   cfg.Model,
			Timeout: cfg.RequestTimeout,
		})
		if err != nil {
			return nil, err
		}
		return p, nil

	default:
		return nil, fmt.Errorf("agent: unknown llm provider %q", cfg.Provider)
	}
}
