// Package api is Veritix's HTTP interface: the REST and SSE surface the web
// UI is built on, and the same surface a script or a CI job can drive.
//
// It does not reimplement any part of the audit. Every run goes through
// audit.Run, and every report served here is the document report.Build
// produces, so the web interface and the JSON report cannot disagree about
// what was found.
//
// The contract is internal/api/openapi.yaml, served at
// /api/v1/openapi.yaml. Settle a change there before changing a handler.
package api

import (
	"context"
	_ "embed"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"

	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/store"
)

//go:embed openapi.yaml
var openAPISpec []byte

// Options configures a server.
type Options struct {
	// Store is the run history. Required.
	Store *store.Store
	// Config carries the server and engine settings a run needs.
	Config config.Config
	// Version is reported by /health and recorded on every run.
	Version string
	// Log receives diagnostics. Request logs go here, run progress goes to
	// the run's event stream as well.
	Log *slog.Logger
	// Web is the built web interface, normally web.FS(). It is injected rather
	// than imported so that this package's tests can drive the API without a
	// front-end build, and can serve a stub one when they are testing how it is
	// served. A nil Web serves the JSON 404 that predates the interface.
	Web fs.FS
}

// Server holds the API's state: the store, the settings a run needs, and the
// registry of runs currently executing.
type Server struct {
	store   *store.Store
	cfg     config.Config
	version string
	log     *slog.Logger
	web     fs.FS
	runs    *runner
	// stopping is closed by Close. Event streams watch it: they stay open by
	// design, so without a signal they would hold a graceful shutdown open
	// until its timeout expired.
	stopping  chan struct{}
	closeOnce sync.Once
}

// New builds a server. The caller owns the store and closes it.
func New(ctx context.Context, opts Options) (*Server, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	s := &Server{
		store:    opts.Store,
		cfg:      opts.Config,
		version:  opts.Version,
		log:      log,
		web:      opts.Web,
		stopping: make(chan struct{}),
	}
	s.runs = newRunner(s)

	// A run executes in the memory of the process that started it, so any run
	// still marked in-flight belongs to a process that is gone. Closing them
	// out at startup keeps the history honest and stops an events stream from
	// waiting on something nothing is working on.
	if n, err := opts.Store.MarkInterrupted(ctx); err != nil {
		return nil, err
	} else if n > 0 {
		log.Warn("closed out runs interrupted by a previous shutdown", "runs", n)
	}

	return s, nil
}

// Handler returns the routed, wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: a container probe should not need a credential, and a
	// client needs to be able to read the contract to know how to send one.
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/openapi.yaml", s.handleOpenAPI)

	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/v1/capabilities", s.handleCapabilities)
	authed.HandleFunc("GET /api/v1/datasets", s.handleListDatasets)
	authed.HandleFunc("POST /api/v1/datasets", s.handleCreateDataset)
	authed.HandleFunc("GET /api/v1/datasets/{datasetId}", s.handleGetDataset)
	authed.HandleFunc("DELETE /api/v1/datasets/{datasetId}", s.handleDeleteDataset)

	authed.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	authed.HandleFunc("POST /api/v1/runs", s.handleCreateRun)
	authed.HandleFunc("GET /api/v1/runs/{runId}", s.handleGetRun)
	authed.HandleFunc("POST /api/v1/runs/{runId}/cancel", s.handleCancelRun)
	authed.HandleFunc("GET /api/v1/runs/{runId}/report", s.handleGetReport)
	authed.HandleFunc("GET /api/v1/runs/{runId}/report.html", s.handleGetReportHTML)
	authed.HandleFunc("GET /api/v1/runs/{runId}/trace", s.handleGetTrace)
	authed.HandleFunc("GET /api/v1/runs/{runId}/events", s.handleRunEvents)
	authed.HandleFunc("GET /api/v1/runs/{runId}/findings/{findingId}/rows", s.handleFindingRows)

	// An unmatched path under /api/v1 stops here rather than falling through to
	// the web interface, and gets the same JSON error shape as everything else.
	// Without this it reached ServeMux's own plain-text 404, so a client that
	// mistyped an endpoint got back something it could not parse.
	authed.HandleFunc("/api/v1/", s.handleNotFound)

	mux.Handle("/api/v1/", s.requireAuth(authed))

	// Everything that is not the API is the web interface, including paths that
	// exist only as client-side routes. A server built without one falls back
	// to a JSON 404, so a script driving the API still has one error shape to
	// parse whatever this binary was built with.
	if s.web != nil {
		mux.Handle("/", s.spaHandler(s.web))
	} else {
		mux.HandleFunc("/", s.handleNotFound)
	}

	return s.recoverPanics(s.logRequests(mux))
}

// Close ends the event streams and stops any run still executing, waiting for
// each to unwind so that a shutdown does not leave a DuckDB handle open on a
// half-written file. It is safe to call more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.stopping)
		s.runs.shutdown()
	})
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// source_url rides along on the one unauthenticated endpoint because the
	// interface shows it before a token has been accepted. AGPL section 13
	// asks a modified network-served build to offer its source to the people
	// using it, and someone staring at the token gate is one of them.
	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "ok",
		"version":    s.version,
		"source_url": s.cfg.Server.SourceURL,
	})
}

// handleCapabilities tells the interface what this server can do, so that it
// offers the agentic audit only where a model is actually configured rather
// than presenting a control that fails when it is used.
//
// It is authenticated, unlike health. Whether a machine has a model wired up,
// and which one, is the operator's business and not something to publish to
// anybody who can reach the port. The API key is of course never included.
func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	llm := s.cfg.LLM
	configured := llm.Provider != "" && llm.Provider != config.ProviderNone

	agentInfo := map[string]any{"available": configured}
	if configured {
		agentInfo["provider"] = llm.Provider
		agentInfo["model"] = llm.Model
		// Whether the operator has already lifted the egress policy in the
		// server's own configuration, which the interface has to say plainly
		// rather than showing an unticked box that is a lie.
		agentInfo["values_allowed_by_default"] = llm.AllowSampleValues
	}

	writeJSON(w, http.StatusOK, map[string]any{"agent": agentInfo})
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(openAPISpec)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "no such endpoint: %s %s", r.Method, r.URL.Path)
}
