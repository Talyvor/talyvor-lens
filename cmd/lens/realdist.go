package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/realdist"
)

// `lens realdist` — how similar are REAL prompts to each other?
//
// Every threshold and doc2query verdict so far rests on a 28-pair synthetic corpus written
// adversarially for the precision work. This measures the same quantity on actual stored traffic.
//
// ⚠ READ-ONLY, AND IT READS NO PROMPT CONTENT. prompt_embeddings holds a hash and a vector, never
// the prompt; token_events.prompt_text is populated only under the `full` logging policy and the
// default is `metadata`. Nothing here needs turning on and no policy statement changes.
func runRealDist() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("realdist: config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("realdist: LENS_DATABASE_URL is required, because this reads the live prompt_embeddings table")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("realdist: connect: %w", err)
	}
	defer pool.Close()

	rows, err := realdist.Measure(ctx, pgxQ{pool})
	if err != nil {
		return err
	}

	// ⚠ REFUSE TO COMPARE ACROSS EMBEDDERS. The synthetic baseline is a property of
	// text-embedding-3-small; quoting it against a deployment on another model would be the same
	// class of error as poolcheck's developer-corpus ceiling.
	label := "synthetic engineering corpus (28 pairs)"
	baseline := realdist.SyntheticEngineeringRephrasings
	if cfg.EmbeddingModel != realdist.SyntheticEmbeddingModel {
		fmt.Printf("⚠ deployment embeds with %q but the synthetic baseline was measured on %q.\n"+
			"  The side-by-side comparison is SUPPRESSED — cosine values are not comparable across\n"+
			"  embedders, and quoting one at the other is how a safety margin gets overstated.\n\n",
			cfg.EmbeddingModel, realdist.SyntheticEmbeddingModel)
		baseline, label = nil, "(suppressed — embedder mismatch)"
	}
	fmt.Print(realdist.Report(rows, baseline, label))
	return nil
}

type pgxQ struct{ p *pgxpool.Pool }

func (q pgxQ) Query(ctx context.Context, sql string, args ...any) (realdist.Rows, error) {
	return q.p.Query(ctx, sql, args...)
}
