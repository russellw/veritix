package checks

import (
	"context"
	"log/slog"

	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/finding"
	"github.com/russellwallace/veritix/internal/profile"
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
