package eval

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/profile"
)

// Options controls an evaluation.
type Options struct {
	// Paths are the files and directories to audit as one dataset.
	Paths []string
	// Manifest is the ground truth to score against.
	Manifest *Manifest
	// Engine configures the DuckDB instance and its limits.
	Engine config.Engine
	// Profile controls the depth of profiling.
	Profile profile.Options
	// Agent is the model under evaluation. Nil scores the deterministic
	// auditor alone, which is a useful thing to measure and the only thing CI
	// can measure without a model.
	Agent *agent.Options
	// Runs is how many times to audit the dataset. One is enough for the
	// deterministic pass and is not enough for a model.
	Runs int
}

// Run audits a dataset repeatedly and scores each pass against the manifest.
//
// Repetition is the whole point when a model is configured. One run of an agent
// is an anecdote: the same model on the same data takes a different path every
// time, and a defect it found once it may not find again. Repeating the audit
// is the only instrument that distinguishes a model that finds half the defects
// from one that finds a different half each time.
//
// A run that fails is recorded and the evaluation continues. Losing four
// completed runs because the fifth timed out would be the worst possible way to
// spend an hour of a local model's time.
func Run(ctx context.Context, opts Options, log *slog.Logger) (Score, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Manifest == nil {
		return Score{}, fmt.Errorf("eval: no manifest to score against")
	}
	runs := opts.Runs
	if runs < 1 {
		runs = 1
	}
	if opts.Agent == nil && runs > 1 {
		// Nothing here is random without a model, so the second run would
		// measure the same thing at the same cost as the first.
		log.Info("no model is configured; one run measures everything a repeat would")
		runs = 1
	}

	scores := make([]RunScore, 0, runs)
	for i := range runs {
		if err := ctx.Err(); err != nil {
			return withModel(Aggregate(opts.Manifest, scores), opts), err
		}
		started := time.Now()
		log.Info("eval run starting", "run", i+1, "of", runs)

		score := evalOnce(ctx, opts, log)
		scores = append(scores, score)

		if score.Err != "" {
			log.Warn("eval run failed", "run", i+1, "error", score.Err)
		} else {
			log.Info("eval run complete", "run", i+1,
				"detected", len(score.Detected), "missed", len(score.Missed),
				"duration", time.Since(started).Round(time.Second))
		}
	}
	return withModel(Aggregate(opts.Manifest, scores), opts), nil
}

// withModel names the model under evaluation from the configuration rather
// than from a trace.
//
// An eval where every run failed to reach the provider has no trace to read the
// name out of, and a scorecard that then reported "no model configured" would
// describe the wrong failure entirely.
func withModel(s Score, opts Options) Score {
	if opts.Agent == nil || opts.Agent.Provider == nil {
		return s
	}
	s.Provider = opts.Agent.Provider.Name()
	s.Model = opts.Agent.Provider.Model()
	return s
}

// evalOnce is one audit, scored. It drives audit.Run rather than assembling the
// pipeline itself, so what is being scored is the auditor a customer runs and
// not an arrangement that exists only in the eval.
func evalOnce(ctx context.Context, opts Options, log *slog.Logger) RunScore {
	res, err := audit.Run(ctx, audit.Options{
		Paths:   opts.Paths,
		Engine:  opts.Engine,
		Profile: opts.Profile,
		Agent:   opts.Agent,
	}, log)
	if err != nil {
		return RunScore{Err: err.Error()}
	}
	defer res.Close() //nolint:errcheck // the findings are already scored by then

	score := ScoreRun(opts.Manifest, res.Findings.All())
	score.Trace = res.Trace
	return score
}
