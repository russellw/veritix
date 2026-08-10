package api

import (
	"crypto/subtle"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// requireAuth enforces the bearer token, when one is configured.
//
// No token means no check, which is safe only because the server refuses to
// bind a non-loopback address without one — that refusal is in the serve
// command, and it is what makes this branch acceptable.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := s.cfg.Server.AuthToken
		if want == "" {
			next.ServeHTTP(w, r)
			return
		}

		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Constant-time so that a wrong token cannot be refined a byte at a
		// time by measuring how long the refusal took.
		if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="veritix"`)
			writeError(w, http.StatusUnauthorized, "this server requires a bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers what a handler sent, so the request log can report
// it. It carries Flush because the SSE handler needs to push each event out
// rather than wait for a buffer to fill.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (rec *statusRecorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		// The path is logged; the query string and body are not. A log is a
		// file that outlives the request and gets shipped to whoever collects
		// logs, so anything written here has left the process for good.
		//
		// The body is the one that matters today: an upload's body is the
		// customer's file contents, and copying those into a log would be an
		// egress path that bypasses --include-values entirely and that nobody
		// would think to audit. The query string carries nothing sensitive
		// yet — the rows endpoint's is just ?limit=50 — and is dropped
		// defensively, because a query string is where filter values end up
		// as an API grows and a URL is the most-copied part of a request.
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"took", time.Since(start),
		)
	})
}

// recoverPanics turns a panic into a 500 rather than a dropped connection.
//
// A panic in one handler must not take down a server that is in the middle of
// a long audit for somebody else.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("handler panicked",
					"method", r.Method, "path", r.URL.Path,
					"panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
