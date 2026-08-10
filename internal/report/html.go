package report

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"strings"

	"github.com/russellwallace/veritix/internal/audit"
)

//go:embed templates/report.html.tmpl
var templateFS embed.FS

// htmlTemplate is parsed once at startup. A template that fails to parse is a
// programming error, not a runtime condition, so it panics rather than
// deferring the failure to the moment somebody asks for a report.
var htmlTemplate = template.Must(
	template.New("report.html.tmpl").
		Funcs(template.FuncMap{"columnNotes": columnNotes}).
		ParseFS(templateFS, "templates/report.html.tmpl"))

// WriteHTML renders a run as a single self-contained page.
func WriteHTML(w io.Writer, res *audit.Result, version string, opts Options) error {
	doc := Build(res, version, opts)
	if err := htmlTemplate.Execute(w, doc); err != nil {
		return fmt.Errorf("report: rendering HTML: %w", err)
	}
	return nil
}

// columnNotes summarises a column's irregularities for the table view. It
// reuses the terminal report's flags so the two never drift apart.
func columnNotes(c ColumnInfo) string {
	flags := columnFlags(c)
	if len(flags) == 0 {
		return ""
	}
	const maxFlags = 3
	if len(flags) > maxFlags {
		extra := len(flags) - maxFlags
		flags = append(flags[:maxFlags:maxFlags], fmt.Sprintf("and %d more", extra))
	}
	return strings.Join(flags, "; ")
}
