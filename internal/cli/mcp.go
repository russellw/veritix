package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/russellw/veritix/internal/buildinfo"
	"github.com/russellw/veritix/internal/mcp"
	"github.com/russellw/veritix/internal/rules"
	"github.com/russellw/veritix/internal/store"
)

func newMCPCmd(e *env) *cobra.Command {
	var (
		dataDir       string
		rulesPath     string
		useAgent      bool
		includeValues bool
		topValues     int
	)

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve Veritix over the Model Context Protocol",
		Long: "The mcp command serves Veritix's audits to an assistant — Claude\n" +
			"Code, Claude Desktop, anything speaking MCP — over stdin and\n" +
			"stdout.\n\n" +
			"It is normally launched by the assistant rather than by hand. Add\n" +
			"it to the client's MCP configuration as the command to run.\n\n" +
			"Audits started this way are recorded in the same history as the\n" +
			"ones started from the web interface, so they share a data\n" +
			"directory and both are visible in either place.\n\n" +
			"What the assistant may ask for is bounded here, not there:\n" +
			"reports withhold verbatim cell values unless --include-values is\n" +
			"passed, and Veritix's own agentic pass runs only with --agent.\n" +
			"Those are the operator's decisions, so the calling model cannot\n" +
			"make them for itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("data-dir") {
				e.cfg.Server.DataDir = dataDir
			}
			return runMCP(cmd.Context(), e, mcpFlags{
				rulesPath:     rulesPath,
				agent:         useAgent,
				includeValues: includeValues,
				topValues:     topValues,
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&dataDir, "data-dir", "", "directory for the run store and the databases runs leave behind")
	f.StringVar(&rulesPath, "rules", "", "a YAML file of your own expectations, applied to every audit")
	f.BoolVar(&useAgent, "agent", false,
		"run the model-driven pass on every audit; needs llm.provider configured")
	f.BoolVar(&includeValues, "include-values", false,
		"allow verbatim cell values in what this server returns (off by default)")
	f.IntVar(&topValues, "top-values", 10, "how many frequent values to record per column")

	return cmd
}

// mcpFlags are the operator's decisions about what this server may do, kept
// together because none of them is the calling model's to make.
type mcpFlags struct {
	rulesPath     string
	agent         bool
	includeValues bool
	topValues     int
}

func runMCP(ctx context.Context, e *env, f mcpFlags) error {
	// Rules are read now rather than per audit, so that a typo is an error the
	// operator sees when they start the server instead of one that surfaces
	// inside somebody else's tool call.
	var ruleFile *rules.File
	if f.rulesPath != "" {
		var err error
		if ruleFile, err = rules.Load(f.rulesPath); err != nil {
			return err
		}
	}

	// The same directory `veritix serve` uses, and for the same reason: an
	// audit run from an assistant belongs in the same history as one run from
	// the browser. 0700 because all of it is customer data.
	dir := e.cfg.Server.DataDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create the data directory %s: %w", dir, err)
	}

	st, err := store.Open(filepath.Join(dir, "veritix.db"))
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // the serve error below is the one worth reporting

	srv, err := mcp.New(mcp.Options{
		Store:         st,
		Config:        e.cfg,
		Version:       buildinfo.Short(),
		Log:           e.log,
		Agent:         f.agent,
		IncludeValues: f.includeValues,
		Rules:         ruleFile,
		TopValues:     f.topValues,
	})
	if err != nil {
		return err
	}

	// Diagnostics are already on stderr, which matters more here than
	// anywhere else in the program: stdout is the protocol, and one stray
	// line on it corrupts the session rather than merely looking untidy.
	e.log.Info("veritix is serving MCP on stdio",
		"data_dir", dir, "agent", f.agent, "values", f.includeValues)

	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// Run returns when the client disconnects, which for a subprocess is the
	// assistant shutting it down: a clean close is how this process
	// ordinarily ends, and it returns no error. An error here means the pipe
	// went away with work still in flight — the client was killed rather than
	// closed — and it is worth saying which of the two happened, because the
	// operator is reading a log they went looking for.
	if err := srv.Run(sigCtx, &sdk.StdioTransport{}); err != nil {
		return fmt.Errorf("the MCP session ended abnormally: %w", err)
	}
	e.log.Info("the MCP client disconnected")
	return nil
}
