package api

import (
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/store"
)

// handleFindingRows serves the rows a finding is about.
//
// This is the one endpoint in Veritix that returns raw customer data. It
// exists because showing somebody the three bad rows is the most useful thing
// an auditing tool can do, and everything about how it is built is meant to
// keep it the only one: the rows are never folded into a list response, never
// included in a report unless --include-values was passed, and never logged.
//
// It reads the DuckDB file the run left behind, opened read-only, rather than
// re-reading the customer's files. That is both faster and safer: the rows
// shown are the rows the finding was computed from, not whatever the source
// file says today.
func (s *Server) handleFindingRows(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runId")

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "limit must be a number between 1 and 1000")
			return
		}
		limit = n
	}

	run, err := s.store.Run(r.Context(), runID)
	if err != nil {
		s.writeStoreError(w, err, "could not read the run")
		return
	}

	f, err := s.store.Finding(r.Context(), runID, r.PathValue("findingId"))
	if err != nil {
		s.writeStoreError(w, err, "could not read the finding")
		return
	}
	if f.RowQuery == "" {
		writeError(w, http.StatusConflict,
			"finding %s has no rows to show: it is an observation about how the file is "+
				"structured rather than about particular rows", f.ID)
		return
	}

	rs, err := s.queryRows(r, run, f, limit)
	if err != nil {
		// The query text is not returned: it embeds column names and
		// predicates, and an error page is the wrong place for either.
		s.log.Error("could not fetch the finding's rows",
			"run", runID, "finding", f.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not fetch the rows for this finding")
		return
	}

	// Cells arrive as whatever DuckDB scanned them into. Everything was
	// ingested as VARCHAR, so this is a formality, but a value that is not a
	// string still has to render as one rather than break the response.
	rows := make([][]*string, 0, len(rs.Rows))
	for _, cells := range rs.Rows {
		out := make([]*string, len(cells))
		for i, c := range cells {
			if c == nil {
				continue
			}
			v := fmt.Sprint(c)
			out[i] = &v
		}
		rows = append(rows, out)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"finding_id": f.ID,
		"title":      f.Title,
		"columns":    rs.Columns,
		"rows":       rows,
		"truncated":  rs.Truncated,
	})
}

func (s *Server) queryRows(
	r *http.Request, run *store.Run, f *store.Finding, limit int,
) (*engine.ResultSet, error) {
	if run.DatabasePath == "" {
		return nil, fmt.Errorf("run %s kept no database", run.ID)
	}
	// The path was written by runs.DatabasePath from the data directory and the
	// run's generated id, never by anything a request supplied.
	if _, err := os.Stat(run.DatabasePath); err != nil { //nolint:gosec // server-generated path

		return nil, fmt.Errorf("run %s: %w", run.ID, err)
	}

	// Read-only is enforced by DuckDB rather than by inspecting the query
	// text. The row query came out of a check here, but the same endpoint will
	// serve agent-authored findings at M4, and the guarantee has to hold for
	// those without anything else changing.
	e, err := engine.OpenReadOnly(r.Context(), run.DatabasePath, s.cfg.Engine, s.log)
	if err != nil {
		return nil, err
	}
	defer e.Close() //nolint:errcheck // read-only handle; nothing to flush

	return e.Collect(r.Context(), f.RowQuery, limit)
}
