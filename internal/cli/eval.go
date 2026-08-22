package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/eval"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/rules"
)

// evalOptions holds the flags for `veritix eval`.
type evalOptions struct {
	manifest  string
	rulesPath string
	runs      int
	format    string
	output    string
	minRecall float64

	llmProvider    string
	llmModel       string
	llmBaseURL     string
	llmEffort      string
	llmMaxSteps    int
	llmTokenBudget int
}

func newEvalCmd(e *env) *cobra.Command {
	var opts evalOptions

	cmd := &cobra.Command{
		Use:   "eval [path...]",
		Short: "Score an audit against a dataset whose defects are already known",
		Long: "Eval audits a dataset that comes with a manifest of its own defects\n" +
			"and reports what was found and what was missed.\n\n" +
			"Without a model this measures the deterministic checks, which is a\n" +
			"thing CI can do on every commit. With one it measures the model, and\n" +
			"that needs --runs: an agent takes a different path every time, so a\n" +
			"single audit says almost nothing about what the next one will find.\n\n" +
			"Two numbers come out and they are not the same number. Mean recall is\n" +
			"what one audit finds. Coverage is what the runs find between them. A\n" +
			"model scoring half and half is finding some defects and missing\n" +
			"others; one scoring half and all is finding a different one each time.\n\n" +
			"--rules is how the other half is measured: load the rules accepted\n" +
			"from an earlier run's proposals and the scorecard reports which of the\n" +
			"agent's targets the deterministic pass now catches on its own.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEval(cmd, e, opts, args)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.rulesPath, "rules", "",
		"apply these rules before scoring, to measure what accepting a proposal bought")
	f.StringVar(&opts.manifest, "manifest", "",
		"the defect manifest to score against (default: "+eval.FileName+" in the dataset directory)")
	f.IntVar(&opts.runs, "runs", 1, "how many times to audit the dataset")
	f.StringVarP(&opts.format, "format", "f", "text", "scorecard format: text or json")
	f.StringVarP(&opts.output, "output", "o", "-", "write the scorecard here (- for stdout)")
	f.Float64Var(&opts.minRecall, "min-recall", -1,
		"exit non-zero if mean recall falls below this fraction, e.g. 0.5")

	f.StringVar(&opts.llmProvider, "llm", "",
		"evaluate this provider's model: none, anthropic, or openai-compatible")
	f.StringVar(&opts.llmModel, "llm-model", "", "the model to evaluate")
	f.StringVar(&opts.llmBaseURL, "llm-base-url", "",
		"the model endpoint, for a local Ollama, vLLM, or LM Studio")
	f.StringVar(&opts.llmEffort, "llm-effort", "", "how much deliberation to ask the model for")
	f.IntVar(&opts.llmMaxSteps, "llm-max-steps", 0, "cap the agent's tool-calling loop")
	f.IntVar(&opts.llmTokenBudget, "llm-token-budget", 0, "stop a run after this many tokens")

	return cmd
}

func runEval(cmd *cobra.Command, e *env, opts evalOptions, paths []string) error {
	format := strings.ToLower(opts.format)
	switch format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown format %q: want text or json", opts.format)
	}

	manifestPath := opts.manifest
	if manifestPath == "" {
		manifestPath = paths[0]
	}
	manifest, err := eval.Load(manifestPath)
	if err != nil {
		return err
	}

	cfg := e.cfg.LLM
	if cmd.Flags().Changed("llm") {
		cfg.Provider = opts.llmProvider
	}
	if cmd.Flags().Changed("llm-model") {
		cfg.Model = opts.llmModel
	}
	if cmd.Flags().Changed("llm-base-url") {
		cfg.BaseURL = opts.llmBaseURL
	}
	if cmd.Flags().Changed("llm-effort") {
		cfg.Effort = opts.llmEffort
	}
	if cmd.Flags().Changed("llm-max-steps") {
		cfg.MaxSteps = opts.llmMaxSteps
	}
	if cmd.Flags().Changed("llm-token-budget") {
		cfg.TokenBudget = opts.llmTokenBudget
	}

	// An eval never lifts the egress policy, whatever the configuration says.
	// A score obtained by showing the model cell values is not a score for the
	// product anybody ships, and a scorecard that quietly meant something else
	// would be worse than none.
	cfg.AllowSampleValues = false

	agentOpts, err := agent.Configure(cfg)
	if err != nil {
		return err
	}
	if agentOpts != nil {
		agentOpts.MaxRows = e.cfg.Engine.MaxResultRows
	}

	// Where the scorecard goes is settled before the runs start. An hour of a
	// local model's time is the expensive way to find out the output path was
	// wrong.
	out, closeOut, err := openOutput(cmd, opts.output)
	if err != nil {
		return err
	}
	defer closeOut()

	var ruleFile *rules.File
	if opts.rulesPath != "" {
		if ruleFile, err = rules.Load(opts.rulesPath); err != nil {
			return err
		}
	}

	score, err := eval.Run(cmd.Context(), eval.Options{
		Paths:    paths,
		Manifest: manifest,
		Engine:   e.cfg.Engine,
		Profile:  profile.Options{TopValues: 10},
		Rules:    ruleFile,
		Agent:    agentOpts,
		Runs:     opts.runs,
	}, e.log)
	if err != nil {
		return err
	}

	if format == "json" {
		err = eval.WriteJSON(out, score)
	} else {
		err = eval.WriteText(out, score)
	}
	if err != nil {
		return err
	}

	return evalGate(score, opts, agentOpts != nil)
}

// evalGate decides the exit code, after the scorecard has been written.
//
// A missed planted defect or a check firing on clean data fails without being
// asked to, because both are unambiguous: the manifest says what the checks
// have to do and they did not do it. The model's score is not treated the same
// way, and --min-recall is opt-in, because a model is nondeterministic and a
// build that fails on a bad afternoon teaches people to ignore it.
func evalGate(score eval.Score, opts evalOptions, haveModel bool) error {
	if !score.Checks.Complete() {
		return &exitError{msg: fmt.Sprintf(
			"the deterministic checks missed %d planted defect(s) and fired on %d clean location(s)",
			len(score.Checks.Missed), len(score.Checks.FalsePositives))}
	}
	if score.ChecksUnstable {
		return &exitError{msg: "the deterministic checks did not agree with themselves across runs"}
	}
	if opts.minRecall < 0 {
		return nil
	}
	if !haveModel {
		return fmt.Errorf("--min-recall needs a model to measure: pass --llm, or set llm.provider")
	}
	if score.MeanRecall() < opts.minRecall {
		return &exitError{msg: fmt.Sprintf("mean recall %.2f is below %.2f",
			score.MeanRecall(), opts.minRecall)}
	}
	return nil
}
