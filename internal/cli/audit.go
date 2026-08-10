package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/russellwallace/veritix/internal/audit"
	"github.com/russellwallace/veritix/internal/buildinfo"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/profile"
	"github.com/russellwallace/veritix/internal/report"
	"github.com/russellwallace/veritix/internal/rules"
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

	return cmd
}

func runAudit(cmd *cobra.Command, e *env, opts auditOptions, paths []string) error {
	format := strings.ToLower(opts.format)
	switch format {
	case "text", "json", "sarif", "html":
	default:
		return fmt.Errorf("unknown format %q: want text, json, sarif, or html", opts.format)
	}

	var ruleFile *rules.File
	if opts.rulesPath != "" {
		var err error
		if ruleFile, err = rules.Load(opts.rulesPath); err != nil {
			return err
		}
	}

	res, err := audit.Run(cmd.Context(), audit.Options{
		Paths:        paths,
		Engine:       e.cfg.Engine,
		DatabasePath: opts.database,
		Rules:        ruleFile,
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

	return failOn(res, opts.failOn)
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
