package checks

import (
	"context"
	"log/slog"

	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/profile"
)

// Run applies every built-in check to a profiled dataset.
func Run(ctx context.Context, e *engine.Engine, ds *profile.Dataset, log *slog.Logger) (*finding.Set, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	set := finding.NewSet()

	for _, t := range ds.Tables {
		tc := &tableContext{table: t, quoted: engine.Ident(t.Name)}

		set.AddAll(checkStructure(tc))
		set.AddAll(checkEmptyTable(tc))
		set.AddAll(checkUnreadableRows(tc))
		set.AddAll(checkNoCandidateKey(tc))

		dupes, err := checkDuplicateRows(ctx, e, tc)
		if err != nil {
			return nil, err
		}
		set.AddAll(dupes)

		for _, c := range t.Columns {
			if f := checkUnprofiled(tc, c); f != nil {
				// And nothing else. Every other column check reads
				// measurements this column does not have, and a zero count
				// reads exactly like a clean column — which is how an audit
				// comes to report a table it never looked at as healthy.
				set.AddAll(f)
				continue
			}
			for _, check := range columnChecks {
				set.AddAll(check(tc, c))
			}
		}
	}

	related, err := relate(ctx, e, ds)
	if err != nil {
		return nil, err
	}
	set.AddAll(related)

	counts := set.Counts()
	log.Info("checks complete",
		"findings", set.Len(),
		"errors", counts[finding.Error],
		"warnings", counts[finding.Warning],
		"info", counts[finding.Info])

	return set, nil
}
