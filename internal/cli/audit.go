package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// auditOptions holds the flags for `veritix audit`. The command is aimed at
// scripting and CI; the web UI drives the same pipeline through the API.
type auditOptions struct {
	format string
	output string
	failOn string
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
			"unrelated files, so keys and joins that span files are checked.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAudit(cmd, e, opts, args)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&opts.format, "format", "f", "text", "report format: text, json, sarif, html")
	f.StringVarP(&opts.output, "output", "o", "-", "write the report here (- for stdout)")
	f.StringVar(&opts.failOn, "fail-on", "", "exit non-zero if any finding reaches this severity: info, warning, error")

	return cmd
}

func runAudit(_ *cobra.Command, _ *env, _ auditOptions, _ []string) error {
	return errors.New("audit: the ingest and profiling pipeline is not wired up yet")
}
