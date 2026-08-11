// Command veritix audits datasets for integrity problems.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/russellwallace/veritix/internal/cli"
)

func main() {
	// os.Exit skips deferred calls, so the exit code is worked out in run and
	// spent here, where there is nothing left to unwind. Otherwise the signal
	// handler installed below would never be released on the failure paths —
	// harmless at process exit, and exactly the kind of thing that stops being
	// harmless once main grows a second thing to clean up.
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.Execute(ctx); err != nil {
		// Cobra has already reported the error; a canceled context is a
		// deliberate Ctrl-C rather than a failure worth shouting about.
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	return 0
}
