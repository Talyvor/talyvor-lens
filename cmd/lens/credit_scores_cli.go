package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/economy"
)

const creditScoresUsage = `usage:
  lens credit-scores                           every company's open request for a review of its credit score, oldest first
  lens credit-scores close <review-id> <note>  answer it: the owner sees the note with their score`

// creditScoreAdmin is the slice of *economy.DualTokenStore the command needs.
type creditScoreAdmin interface {
	OpenCreditScoreReviews(ctx context.Context) ([]economy.CreditScoreReview, error)
	CloseCreditScoreReview(ctx context.Context, id, by, outcome string) (economy.CreditScoreReview, error)
}

// runCreditScores is `lens credit-scores` (B30.39), run inside the lens container so it reaches the same Postgres
// the server reads.
func runCreditScores(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("credit-scores: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("credit-scores: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return creditScoresCommand(ctx, economy.NewDualTokenStore(nil, pool, nil), args, by, os.Stdout)
}

func creditScoresCommand(ctx context.Context, store creditScoreAdmin, args []string, by string, out io.Writer) error {
	switch {
	case len(args) == 0:
		list, err := store.OpenCreditScoreReviews(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(out, "no credit score reviews waiting")
		}
		for _, r := range list {
			fmt.Fprintf(out, "%s\t%s\t%d %s\tasked by %s at %s\t%q\n", r.ID, r.WorkspaceID, r.Score, r.Band, r.RequestedBy,
				r.RequestedAt.UTC().Format(time.RFC3339), r.Note)
			for _, reason := range r.Reasons {
				fmt.Fprintf(out, "\t%d/%d\t%s\n", reason.Points, reason.MaxPoints, reason.Text)
			}
		}
		return nil
	case args[0] == "close" && len(args) >= 3:
		r, err := store.CloseCreditScoreReview(ctx, args[1], by, strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s closed: %s\n", r.ID, r.Outcome)
		return nil
	}
	return fmt.Errorf("%s", creditScoresUsage)
}
