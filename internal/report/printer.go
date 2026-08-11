package report

import (
	"fmt"
	"io"
)

// printer writes a terminal report and remembers the first write that failed.
//
// A text report is a long sequence of small writes, and checking each one at
// the call site would bury the layout in error handling. Latching the first
// failure instead means WriteText's error return says something true: without
// it the function reported success even when its output had gone nowhere,
// which is exactly what happens when a report is piped into a command that
// exits early.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		// Keep the first error. Later writes to a broken pipe would only
		// replace the cause with a symptom.
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

func (p *printer) newline() { p.printf("\n") }

// Write lets a printer stand in for the io.Writer that tabwriter wants, so
// tabulated sections latch their errors the same way.
func (p *printer) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	var n int
	n, p.err = p.w.Write(b)
	return n, p.err
}
