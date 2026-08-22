package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/buildinfo"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/report"
	"github.com/russellw/veritix/internal/rules"
)

// auditOptions holds the flags for `veritix audit`. The command is aimed at
// scripting and CI; the web UI drives the same pipeline through the API.
type auditOptions struct {
	format        string
	output        string
	failOn        string
	includeValues bool
	database      string
	rulesPath     string
	topValues     int
	traceOut      string
	proposeOut    string

	// The LLM flags override configuration for this run. They are here rather
	// than on the root command because the agent is a property of an audit,
	// not of the process.
	llmProvider       string
	llmModel          string
	llmBaseURL        string
	llmEffort         string
	llmMaxSteps       int
	allowSampleValues bool
}

func newAuditCmd(e *env) *cobra.Command {
	var opts auditOptions

	cmd := &cobra.Command{
		Use:   "audit [path...]",
		Short: "Audit a dataset and report its problems",
		Long: "Audit reads the given files and directories as a single dataset,\n" +
			"profiles every column, checks integrity within and across files,\n" +
			"and writes a report.\n\n" +
			"A directory is treated as one dataset rather than a pile of\n" +
			"unrelated files, so keys and joins that span files are checked.\n\n" +
			"Verbatim cell values are left out of reports unless you ask for\n" +
			"them with --include-values.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAudit(cmd, e, opts, args)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&opts.format, "format", "f", "text", "report format: text, json, sarif, or html")
	f.StringVarP(&opts.output, "output", "o", "-", "write the report here (- for stdout)")
	f.StringVar(&opts.failOn, "fail-on", "",
		"exit non-zero if any finding reaches this severity: info, warning, error")
	f.BoolVar(&opts.includeValues, "include-values", false,
		"include verbatim cell values in the report")
	f.StringVar(&opts.database, "database", "",
		"keep the loaded dataset in this DuckDB file instead of memory")
	f.IntVar(&opts.topValues, "top-values", 10, "how many frequent values to record per column")
	f.StringVar(&opts.rulesPath, "rules", "", "a YAML file of your own expectations to enforce")

	f.StringVar(&opts.llmProvider, "llm", "",
		"run the agentic auditor with this provider: none, anthropic, or openai-compatible")
	f.StringVar(&opts.llmModel, "llm-model", "", "the model to use")
	f.StringVar(&opts.llmBaseURL, "llm-base-url", "",
		"the model endpoint, for a local Ollama, vLLM, or LM Studio")
	f.StringVar(&opts.llmEffort, "llm-effort", "",
		"how much deliberation to ask the model for; \"none\" turns off a hybrid "+
			"reasoning model's thinking, which is what makes one usable on a CPU")
	f.IntVar(&opts.llmMaxSteps, "llm-max-steps", 0, "cap the agent's tool-calling loop")
	f.BoolVar(&opts.allowSampleValues, "allow-sample-values", false,
		"permit the model to see cell values, masked; off by default, and the report says which was used")
	f.StringVar(&opts.traceOut, "trace-out", "",
		"write the agent's trace here as JSON: every payload sent and received (- for stdout)")
	f.StringVar(&opts.proposeOut, "propose-rules-out", "",
		"write the rules the agent proposed here as YAML, to review and load with --rules (- for stdout)")

	return cmd
}

func runAudit(cmd *cobra.Command, e *env, opts auditOptions, paths []string) error {
	format := strings.ToLower(opts.format)
	switch format {
	case "text", "json", "sarif", "html":
	default:
		return fmt.Errorf("unknown format %q: want text, json, sarif, or html", opts.format)
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
	if cmd.Flags().Changed("allow-sample-values") {
		cfg.AllowSampleValues = opts.allowSampleValues
	}

	agentOpts, err := agent.Configure(cfg)
	if err != nil {
		return err
	}
	if agentOpts != nil {
		agentOpts.UseEngineLimits(e.cfg.Engine)
	}

	// Both of these are settled before the audit runs, because an audit is
	// minutes of work and finding out afterwards that its trace had nowhere to
	// go is the expensive way to learn it.
	traceOut, closeTrace, err := openTrace(cmd, opts, agentOpts != nil)
	if err != nil {
		return err
	}
	defer closeTrace()

	proposeOut, closePropose, err := openProposals(cmd, opts, agentOpts != nil)
	if err != nil {
		return err
	}
	defer closePropose()

	var ruleFile *rules.File
	if opts.rulesPath != "" {
		if ruleFile, err = rules.Load(opts.rulesPath); err != nil {
			return err
		}
	}

	res, err := audit.Run(cmd.Context(), audit.Options{
		Paths:        paths,
		Engine:       e.cfg.Engine,
		DatabasePath: opts.database,
		Rules:        ruleFile,
		Agent:        agentOpts,
		Profile: profile.Options{
			TopValues: opts.topValues,
		},
	}, e.log)
	if err != nil {
		return err
	}
	defer res.Close() //nolint:errcheck // the report is already written by then

	out, closeOut, err := openOutput(cmd, opts.output)
	if err != nil {
		return err
	}
	defer closeOut()

	ro := report.Options{IncludeValues: opts.includeValues, Indent: true}
	switch format {
	case "json":
		err = report.WriteJSON(out, res, buildinfo.Short(), ro)
	case "sarif":
		err = report.WriteSARIF(out, res, buildinfo.Short(), ro)
	case "html":
		err = report.WriteHTML(out, res, buildinfo.Short(), ro)
	default:
		err = report.WriteText(out, res, ro)
	}
	if err != nil {
		return err
	}

	if traceOut != nil {
		if err := writeTrace(traceOut, res, e); err != nil {
			return err
		}
	}

	if proposeOut != nil {
		if err := writeProposals(proposeOut, res, e); err != nil {
			return err
		}
	}

	return failOn(res, opts.failOn)
}

// openTrace resolves --trace-out, before the audit rather than after it.
//
// Asking for a trace with no model configured is refused rather than quietly
// producing nothing: the flag exists to answer "what did the model see", and
// silence in reply to that question is the one answer that could be
// misread as "nothing".
func openTrace(cmd *cobra.Command, opts auditOptions, haveModel bool) (io.Writer, func(), error) {
	if opts.traceOut == "" {
		return nil, func() {}, nil
	}
	if !haveModel {
		return nil, nil, fmt.Errorf(
			"--trace-out needs a model to trace: pass --llm, or set llm.provider in the configuration")
	}
	if opts.traceOut == "-" {
		if opts.output == "" || opts.output == "-" {
			return nil, nil, fmt.Errorf(
				"--trace-out - and --output - would interleave two documents on stdout: send one of them to a file")
		}
		return cmd.OutOrStdout(), func() {}, nil
	}
	f, err := os.Create(opts.traceOut) //nolint:gosec // the path is the operator's choice
	if err != nil {
		return nil, nil, fmt.Errorf("creating trace file: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

// openProposals resolves --propose-rules-out, before the audit for the same
// reason --trace-out is resolved before it.
func openProposals(cmd *cobra.Command, opts auditOptions, haveModel bool) (io.Writer, func(), error) {
	if opts.proposeOut == "" {
		return nil, func() {}, nil
	}
	if !haveModel {
		return nil, nil, fmt.Errorf(
			"--propose-rules-out needs a model to propose the rules: pass --llm, or set " +
				"llm.provider in the configuration")
	}
	if opts.proposeOut == "-" {
		if opts.output == "" || opts.output == "-" || opts.traceOut == "-" {
			return nil, nil, fmt.Errorf(
				"--propose-rules-out - would interleave with another document on stdout: " +
					"send one of them to a file")
		}
		return cmd.OutOrStdout(), func() {}, nil
	}
	f, err := os.Create(opts.proposeOut) //nolint:gosec // the path is the operator's choice
	if err != nil {
		return nil, nil, fmt.Errorf("creating rules file: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

// writeProposals saves what the agent suggested as a rules file.
//
// It is written even when the agent proposed nothing, because the file was
// asked for and an empty one that says so is easier to act on than a path that
// silently does not exist. Nothing here is in force: the header says so, and
// the customer moves what they accept into the file they load with --rules.
func writeProposals(w io.Writer, res *audit.Result, e *env) error {
	header := rules.ProposalHeader(res.Dataset.Root, res.StartedAt)
	if len(res.Proposals) == 0 {
		e.log.Info("the agent proposed no rules")
		header += "\n\nThe agent proposed no rules in this run."
	}
	if err := rules.RenderProposals(w, res.Proposals, header); err != nil {
		return err
	}
	e.log.Info("wrote proposed rules", "count", len(res.Proposals))
	return nil
}

// writeTrace saves the record of what the model was sent and what it sent back.
//
// It goes out before --fail-on decides the exit code, for the same reason the
// report does: the run that fails a build is the one somebody most needs to be
// able to read afterwards.
func writeTrace(w io.Writer, res *audit.Result, e *env) error {
	if res.Trace == nil {
		// A configured model that produced no trace means the run ended before
		// the agent started — a load failure, or a cancellation. Saying so beats
		// leaving a file containing the word "null".
		e.log.Warn("no trace was written: the run ended before the agent started")
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res.Trace); err != nil {
		return fmt.Errorf("writing the trace: %w", err)
	}
	return nil
}

// failOn implements the CI gate. The report is always written first: a
// pipeline that fails the build still needs to be able to show why.
func failOn(res *audit.Result, threshold string) error {
	if threshold == "" || res.Findings == nil {
		return nil
	}
	want, err := finding.ParseSeverity(threshold)
	if err != nil {
		return err
	}

	counts := res.Findings.Counts()
	var atOrAbove int
	for sev, n := range counts {
		if sev >= want {
			atOrAbove += n
		}
	}
	if atOrAbove == 0 {
		return nil
	}
	return &exitError{
		msg: fmt.Sprintf("%d finding(s) at or above %s", atOrAbove, want),
	}
}

// exitError reports a policy failure rather than a malfunction. Cobra has
// already printed the report, so the usage banner would be noise.
type exitError struct{ msg string }

func (e *exitError) Error() string { return e.msg }

// openOutput resolves the --output flag to a writer.
func openOutput(cmd *cobra.Command, path string) (io.Writer, func(), error) {
	if path == "" || path == "-" {
		return cmd.OutOrStdout(), func() {}, nil
	}
	f, err := os.Create(path) //nolint:gosec // the path is the operator's choice
	if err != nil {
		return nil, nil, fmt.Errorf("creating report file: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}
