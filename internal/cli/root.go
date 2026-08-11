// Package cli wires Veritix's commands together.
//
// The CLI exists for development, scripting, and CI. The primary interface
// for end users is the web UI served by `veritix serve`.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/russellwallace/veritix/internal/buildinfo"
	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/telemetry"
)

// env carries everything the subcommands need, assembled once in
// PersistentPreRunE so each command does not re-derive it.
type env struct {
	cfg config.Config
	log *slog.Logger
}

// Execute builds the command tree and runs it.
func Execute(ctx context.Context) error {
	var (
		e          env
		configPath string
		logLevel   string
		logFormat  string
	)

	root := &cobra.Command{
		Use:   "veritix",
		Short: "Audit datasets for integrity problems",
		Long: "Veritix audits CSV files, Excel workbooks, and SQL databases:\n" +
			"it profiles them, verifies their integrity, and reports\n" +
			"inconsistencies and likely problems.\n\n" +
			"Everything runs on your own machine or your own cloud. Dataset\n" +
			"contents are never sent anywhere you have not configured.",
		Version:       buildinfo.Short(),
		SilenceUsage:  true,
		SilenceErrors: false,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			// Flags win over file and environment.
			if cmd.Flags().Changed("log-level") {
				cfg.Log.Level = logLevel
			}
			if cmd.Flags().Changed("log-format") {
				cfg.Log.Format = logFormat
			}
			if err := cfg.Validate(); err != nil {
				return err
			}

			e.cfg = cfg
			// Diagnostics go to stderr so that stdout stays a clean channel
			// for machine-readable report output.
			e.log = telemetry.NewLogger(os.Stderr, cfg.Log.Level, cfg.Log.Format)
			return nil
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&configPath, "config", "", "path to a config file (default: ./veritix.yaml, then the user config dir)")
	pf.StringVar(&logLevel, "log-level", "info", "log verbosity: debug, info, warn, error")
	pf.StringVar(&logFormat, "log-format", "text", "log format: text or json")

	root.AddCommand(
		newAuditCmd(&e),
		newServeCmd(&e),
		newVersionCmd(),
	)

	return root.ExecuteContext(ctx)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		// Version reporting must work before configuration is loaded, so that
		// a broken config file does not hide which binary is installed.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			// The license and a pointer to the source are part of what a
			// version report is for: AGPL section 6 wants the source offer to
			// travel with the binary, and this is the one command that answers
			// "what am I running" without a config file or a browser.
			_, err := fmt.Fprintf(out, "veritix %s\n%s\nSource: %s\n",
				buildinfo.Short(), buildinfo.License, buildinfo.SourceURL)
			return err
		},
	}
}
