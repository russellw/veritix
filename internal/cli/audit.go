package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/russellwallace/veritix/internal/audit"
	"github.com/russellwallace/veritix/internal/buildinfo"
	"github.com/russellwallace/veritix/internal/profile"
	"github.com/russellwallace/veritix/internal/report"
)

// auditOptions holds the flags for `veritix audit`. The command is aimed at
// scripting and CI; the web UI drives the same pipeline through the API.
type auditOptions struct {
	format        string
	output        string
	failOn        string
	includeValues bool
	database      string
	topValues     int
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
	f.StringVarP(&opts.format, "format", "f", "text", "report format: text or json")
	f.StringVarP(&opts.output, "output", "o", "-", "write the report here (- for stdout)")
	f.StringVar(&opts.failOn, "fail-on", "", "exit non-zero if any finding reaches this severity: info, warning, error")
	f.BoolVar(&opts.includeValues, "include-values", false,
		"include verbatim cell values in the report")
	f.StringVar(&opts.database, "database", "",
		"keep the loaded dataset in this DuckDB file instead of memory")
	f.IntVar(&opts.topValues, "top-values", 10, "how many frequent values to record per column")

	return cmd
}

func runAudit(cmd *cobra.Command, e *env, opts auditOptions, paths []string) error {
	format := strings.ToLower(opts.format)
	switch format {
	case "text", "json":
	default:
		return fmt.Errorf("unknown format %q: want text or json", opts.format)
	}

	res, err := audit.Run(cmd.Context(), audit.Options{
		Paths:        paths,
		Engine:       e.cfg.Engine,
		DatabasePath: opts.database,
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
		return report.WriteJSON(out, res, buildinfo.Short(), ro)
	default:
		return report.WriteText(out, res, ro)
	}
}

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
