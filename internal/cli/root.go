// Package cli wires Veritix's commands together.
//
// The CLI exists for development, scripting, and CI. The primary interface
// for end users is the web UI served by `veritix serve`.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/russellw/veritix/internal/buildinfo"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/telemetry"
	"github.com/russellw/veritix/web"
)

// env carries everything the subcommands need, assembled once in
// PersistentPreRunE so each command does not re-derive it.
type env struct {
	cfg  config.Config
	log  *slog.Logger
	otel *telemetry.Telemetry
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

			// Nothing is exported unless otel.enabled says so, and with it off
			// this installs no provider at all: the OpenTelemetry global stays
			// the no-op it starts as, and every span the pipeline opens costs
			// an interface call. See telemetry.Start.
			e.otel, err = telemetry.Start(cmd.Context(), telemetry.Options{
				Enabled:       cfg.OTel.Enabled,
				Endpoint:      cfg.OTel.Endpoint,
				ServiceName:   cfg.OTel.ServiceName,
				Version:       buildinfo.Version,
				SampleRatio:   cfg.OTel.SampleRatio,
				ExportTimeout: cfg.OTel.ExportTimeout,
			})
			if err != nil {
				return err
			}
			if cfg.OTel.Enabled {
				e.log.Info("exporting OpenTelemetry",
					"endpoint", cfg.OTel.Endpoint, "service", cfg.OTel.ServiceName)
			}
			return nil
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&configPath, "config", "", "path to a config file (default: ./veritix.yaml, then the user config dir)")
	pf.StringVar(&logLevel, "log-level", "info", "log verbosity: debug, info, warn, error")
	pf.StringVar(&logFormat, "log-format", "text", "log format: text or json")

	root.AddCommand(
		newAuditCmd(&e),
		newEvalCmd(&e),
		newServeCmd(&e),
		newMCPCmd(&e),
		newVersionCmd(),
	)

	err := root.ExecuteContext(ctx)

	// Flush what was recorded, on the way out and on every way out. Cobra's
	// PersistentPostRun does not run when a command returns an error, and a
	// failed audit is exactly the run whose telemetry somebody wants.
	//
	// WithoutCancel because by the time a Ctrl-C reaches here ctx is already
	// done; Shutdown carries its own timeout instead, so a collector that has
	// gone away cannot turn an exit into a hang.
	if e.otel != nil {
		if ferr := e.otel.Shutdown(context.WithoutCancel(ctx)); ferr != nil && e.log != nil {
			e.log.Warn("could not flush telemetry", "error", ferr)
		}
	}
	return err
}

func newVersionCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		// Version reporting must work before configuration is loaded, so that
		// a broken config file does not hide which binary is installed.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			// Whether an interface came with this binary is part of "what am I
			// running": plain `go build` produces a working API and a page
			// saying the interface is missing, which is right for a developer
			// and wrong for an image somebody deploys. deploy/Dockerfile
			// asserts on this rather than shipping a blank page.
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(struct {
					Version   string `json:"version"`
					Commit    string `json:"commit"`
					Date      string `json:"date"`
					License   string `json:"license"`
					SourceURL string `json:"source_url"`
					Web       bool   `json:"web"`
				}{
					Version:   buildinfo.Version,
					Commit:    buildinfo.Commit,
					Date:      buildinfo.Date,
					License:   buildinfo.License,
					SourceURL: buildinfo.SourceURL,
					Web:       web.Built(),
				})
			}

			// The license and a pointer to the source are part of what a
			// version report is for: AGPL section 6 wants the source offer to
			// travel with the binary, and this is the one command that answers
			// "what am I running" without a config file or a browser.
			iface := "not built"
			if web.Built() {
				iface = "embedded"
			}
			_, err := fmt.Fprintf(out, "veritix %s\n%s\nSource: %s\nInterface: %s\n",
				buildinfo.Short(), buildinfo.License, buildinfo.SourceURL, iface)
			return err
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON, for a script or a build assertion")
	return cmd
}
