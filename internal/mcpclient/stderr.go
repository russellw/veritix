package mcpclient

import (
	"fmt"
	"strings"
	"sync"
)

// stderrTail is how much of a context server's standard error Veritix keeps.
//
// A context server is somebody else's program, started as a subprocess, and
// when one refuses to speak the protocol what it printed on the way down is
// the only account of why: "connecting: EOF" names the symptom and stops
// there, which leaves an operator asking why their data dictionary never
// appears with nothing to go on. The tail is bounded because a server that
// logs a line per request would otherwise be held for the whole audit, and it
// keeps the end rather than the beginning because the last thing a process
// said before it died is the one that explains it.
//
// It reaches the trace and the log, which is where the operator is. It does
// not reach the model: Connection is a field of the trace's context section,
// and nothing assembles it into the brief.
const stderrTail = 2 << 10

// tail is an io.Writer keeping the last stderrTail bytes written to it.
//
// The SDK writes to it from the goroutine draining the subprocess while attach
// reads it on the way out, so it is guarded.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTail {
		t.buf = t.buf[len(t.buf)-stderrTail:]
	}
	return len(p), nil
}

// explain folds whatever the server printed into err.
//
// The nil receiver is the configured-transport case, where there is no
// subprocess and so nothing to have printed.
func (t *tail) explain(err error) error {
	if t == nil || err == nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Collapsed to one line: this becomes a JSON field in the trace and a
	// slog attribute, and neither is a place for somebody else's stack trace
	// laid out over forty lines.
	said := strings.Join(strings.Fields(string(t.buf)), " ")
	if said == "" {
		return err
	}
	return fmt.Errorf("%w; it printed: %s", err, said)
}
