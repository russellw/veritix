// Package mcp exposes Veritix over the Model Context Protocol, so that an
// assistant — Claude Code, Claude Desktop, anything speaking MCP — can audit a
// dataset and read what was found.
//
// It is a third door onto the same building, not a third building. Every audit
// started here goes through [runs.Execute] and therefore [audit.Run], is
// recorded in the same SQLite store, and produces the same report.Document the
// JSON report writes and the web interface displays. An audit run from an
// assistant and the same audit run from the browser are the same run, in the
// same history, with the same findings.
//
// # What a caller may decide, and what it may not
//
// The client of an MCP server is somebody else's model, in a context Veritix
// neither controls nor records. So the division of authority is deliberate:
// **the caller chooses what to audit, and the operator chooses what Veritix
// may disclose.** Whether reports may carry verbatim cell values, and whether
// Veritix's own agentic pass runs, are [Options] set when the operator
// configures this server — not tool parameters a model can set for itself.
// Lifting an egress policy is a decision a person takes.
//
// For the same reason the per-finding rows endpoint has no tool here. Over
// HTTP it is one person clicking one finding in a page they opened; an
// automated caller could walk every finding of every run, which is a different
// thing wearing the same name. The exception in internal/api is deliberate and
// stays there.
package mcp

import (
	"context"
	"errors"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/rules"
	"github.com/russellw/veritix/internal/store"
)

// Options configures the server.
type Options struct {
	// Store is the run history, shared with `veritix serve` when both are
	// pointed at the same data directory. Required.
	Store *store.Store
	// Config carries the engine settings a run needs and the model settings
	// the agentic pass needs.
	Config config.Config
	// Version is reported to the client and recorded on every run.
	Version string
	// Log receives diagnostics. On a stdio transport this must not be stdout:
	// stdout is the protocol.
	Log *slog.Logger
	// Agent runs Veritix's own model-driven pass on every audit this server
	// performs. It is the operator's decision, not the caller's, because it
	// spends the operator's tokens and sends dataset metadata to the operator's
	// provider.
	Agent bool
	// IncludeValues permits verbatim cell values in the reports this server
	// returns. Off by default, like every other entry point.
	IncludeValues bool
	// Rules is a YAML file of the customer's own expectations, applied to
	// every audit this server performs. Loaded once at startup so that a typo
	// fails the server the operator is watching rather than an audit somebody
	// else asked for.
	Rules *rules.File
	// TopValues is how many frequent values to record per column.
	TopValues int
}

// Server is Veritix's MCP surface.
type Server struct {
	opts Options
	log  *slog.Logger
	srv  *sdk.Server
}

// New builds the server and registers its tools.
//
// Unlike the HTTP server this does not close out runs left behind by a
// previous process. An MCP server is normally a subprocess of an assistant and
// may be one of several talking to the same store, possibly alongside a
// `veritix serve` with runs genuinely in flight; marking those interrupted
// would be one process declaring another one's work dead.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("the MCP server needs a run store")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.TopValues == 0 {
		opts.TopValues = 10
	}

	// Configured once, here, so that a broken provider is an error the
	// operator sees at startup instead of one that surfaces inside the first
	// audit an assistant asks for.
	if opts.Agent {
		if _, err := agentOptions(opts); err != nil {
			return nil, err
		}
	}

	s := &Server{opts: opts, log: log}
	s.srv = sdk.NewServer(&sdk.Implementation{
		Name:    "veritix",
		Title:   "Veritix data auditor",
		Version: opts.Version,
	}, &sdk.ServerOptions{
		Instructions: instructions,
	})
	s.addTools()
	return s, nil
}

// instructions tell the calling assistant what this server is for. They are
// the MCP equivalent of the agent's system prompt, and carry the same two
// facts a caller needs before it reads anything back: findings are measured,
// not asserted, and cell values do not come out of here.
const instructions = `Veritix audits a dataset — a directory of CSV exports, or an Excel workbook —
and reports integrity problems: broken references between files, columns whose
contents do not match their declared type, duplicate keys, inconsistent date
formats, and the rest.

Start with audit_dataset, giving it a path on this machine. It ingests the
files, profiles them, runs every check, and returns what it found. Auditing a
large dataset takes a while; the call returns when the audit is complete.

Every finding carries the SQL that demonstrates it and has been re-executed
before being reported, so a finding is a measurement rather than a claim.

Reports contain no verbatim cell values unless this server's operator has
turned that on. A column is described by the shape its contents take — a
column of identifiers is reported as XXX-999999 — which is enough to reason
about and useless for anything else. Ask the operator, not this server, if you
need the values themselves.`

// Run serves the protocol over the transport until the client disconnects.
func (s *Server) Run(ctx context.Context, t sdk.Transport) error {
	return s.srv.Run(ctx, t)
}

// Connect attaches one transport without blocking, which is what the tests
// use with an in-memory pair.
func (s *Server) Connect(ctx context.Context, t sdk.Transport) (*sdk.ServerSession, error) {
	return s.srv.Connect(ctx, t, nil)
}

func readOnly(t *sdk.Tool) *sdk.Tool {
	t.Annotations = &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	return t
}

func (s *Server) addTools() {
	sdk.AddTool(s.srv, readOnly(&sdk.Tool{
		Name:        "list_datasets",
		Title:       "List datasets",
		Description: "List the datasets this Veritix instance knows about.",
	}), s.listDatasets)

	sdk.AddTool(s.srv, &sdk.Tool{
		Name:  "register_dataset",
		Title: "Register a dataset",
		Description: "Register a directory or file on this machine as a dataset, without auditing it. " +
			"Registering the same path twice returns the dataset that already exists. " +
			"audit_dataset accepts a path directly, so this is only needed to name a dataset in advance.",
		Annotations: &sdk.ToolAnnotations{IdempotentHint: true},
	}, s.registerDataset)

	sdk.AddTool(s.srv, &sdk.Tool{
		Name:  "audit_dataset",
		Title: "Audit a dataset",
		Description: "Audit a dataset and return what was found. Give either a path on this machine " +
			"or the id of a dataset already registered. A directory is audited as one dataset: the " +
			"relationships between its files are checked, not only the files themselves. " +
			"The audit is recorded in this instance's history and can be read back later by its run id.",
		// Not read-only: it writes a run to the history and leaves a database
		// behind, and it is the expensive call on this server.
		Annotations: &sdk.ToolAnnotations{IdempotentHint: false},
	}, s.auditDataset)

	sdk.AddTool(s.srv, readOnly(&sdk.Tool{
		Name:        "list_runs",
		Title:       "List runs",
		Description: "List past audits, most recent first, optionally for one dataset.",
	}), s.listRuns)

	sdk.AddTool(s.srv, readOnly(&sdk.Tool{
		Name:        "get_run",
		Title:       "Get a run",
		Description: "Read one audit's status and finding counts.",
	}), s.getRun)

	sdk.AddTool(s.srv, readOnly(&sdk.Tool{
		Name:  "list_findings",
		Title: "List findings",
		Description: "List what an audit found, most severe first, optionally filtered by severity. " +
			"Each finding carries the SQL that demonstrates it.",
	}), s.listFindings)

	sdk.AddTool(s.srv, readOnly(&sdk.Tool{
		Name:  "get_report",
		Title: "Get a report",
		Description: "Read an audit's full report: the findings, and the profile of every table and " +
			"column behind them. This is the same document the JSON report and the web interface show.",
	}), s.getReport)
}
