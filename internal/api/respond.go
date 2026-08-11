package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/russellw/veritix/internal/store"
)

// errorBody is the one error shape the API returns, so a client has a single
// thing to parse whatever went wrong.
type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		// Encoding our own response failed, so the response is already lost.
		// Say so in the one place a client will look rather than sending a
		// half-written body with a 200 on it.
		http.Error(w, `{"error":"could not encode the response"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, errorBody{Error: fmt.Sprintf(format, args...)})
}

// writeStoreError maps a store failure onto a status code. A missing id is a
// 404 whatever the caller asked for; anything else is ours, not theirs.
func (s *Server) writeStoreError(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "%s", err)
		return
	}
	s.log.Error(what, "error", err)
	writeError(w, http.StatusInternalServerError, "%s", what)
}

// decodeJSON reads a request body into v, refusing unknown fields.
//
// A rejected field is how a caller finds out that `include_value` is not
// `include_values` before shipping a report that quietly contains raw data,
// rather than after.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("could not read the request body: %w", err)
	}
	return nil
}
