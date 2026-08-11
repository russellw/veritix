package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/russellw/veritix/internal/store"
)

// handleRunEvents streams a run's progress as server-sent events.
//
// An audit takes seconds to minutes, so the browser needs to see it moving.
// The stream ends with one terminal event carrying the finished run, and that
// event is read from the store rather than remembered in memory — so a client
// that subscribes after the run has already finished gets the outcome
// immediately instead of waiting on a stream that will never speak.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")

	run, err := s.store.Run(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err, "could not read the run")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "this server cannot stream events")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer would defeat the point of streaming at all.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	active := s.runs.get(id)
	if active == nil {
		// Either it finished, or it belongs to a process that is gone; either
		// way the store is the authority on how it ended.
		s.sendDone(w, flusher, run)
		return
	}

	backlog, updates := active.subscribe()
	for _, ev := range backlog {
		if !s.sendEvent(w, flusher, ev) {
			return
		}
	}
	if updates == nil {
		s.sendFinalStatus(r.Context(), w, flusher, id)
		return
	}
	defer active.unsubscribe(updates)

	for {
		select {
		case <-r.Context().Done():
			// The client went away. The run keeps going: it was started by a
			// request that has already returned, and closing a browser tab is
			// not a decision to abandon an audit.
			return

		case <-s.stopping:
			// The server is shutting down. Say so rather than drop the
			// connection, so the browser shows a stopped server instead of a
			// spinner that never resolves.
			s.sendFinalStatus(r.Context(), w, flusher, id)
			return

		case ev, open := <-updates:
			if !open {
				s.sendFinalStatus(r.Context(), w, flusher, id)
				return
			}
			if !s.sendEvent(w, flusher, ev) {
				return
			}
		}
	}
}

func (s *Server) sendFinalStatus(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, id string) {
	run, err := s.store.Run(ctx, id)
	if err != nil {
		s.log.Error("could not read the finished run", "run", id, "error", err)
		return
	}
	s.sendDone(w, flusher, run)
}

func (s *Server) sendDone(w http.ResponseWriter, flusher http.Flusher, run *store.Run) {
	s.sendEvent(w, flusher, Event{
		Type:    eventDone,
		Time:    time.Now(),
		Message: string(run.Status),
		Run:     toRunJSON(run),
	})
}

// sendEvent writes one event and reports whether the stream is still usable.
func (s *Server) sendEvent(w http.ResponseWriter, flusher http.Flusher, ev Event) bool {
	body, err := json.Marshal(ev)
	if err != nil {
		s.log.Error("could not encode a progress event", "error", err)
		return true
	}

	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, body); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
