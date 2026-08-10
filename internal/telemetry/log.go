// Package telemetry sets up diagnostic output. It will grow to cover
// OpenTelemetry tracing and metrics; for now it owns structured logging.
package telemetry

import (
	"io"
	"log/slog"
	"strings"
)

// NewLogger builds a slog.Logger from the configured level and format.
// Unrecognised values fall back to info/text rather than failing, because
// config.Validate has already rejected genuinely invalid input and a logger
// that refuses to exist is worse than a logger with the wrong verbosity.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
