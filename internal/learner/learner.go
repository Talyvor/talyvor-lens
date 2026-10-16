package learner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

const (
	streamName      = "LENS_EVENTS"
	streamSubject   = "lens.events.>"
	eventSubject    = "lens.events.token"
	analyseInterval = time.Hour
)

// pgxDB is the subset of *pgxpool.Pool that Learner needs.
// Defined so tests can inject a pgxmock pool.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Learner struct {
	nc   *nats.Conn
	js   nats.JetStreamContext
	pool pgxDB
}

type TokenEvent struct {
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	Prompt       string    `json:"prompt"`
	Response     string    `json:"response"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Cached       bool      `json:"cached"`
	Compressed   bool      `json:"compressed"`
	SavingsPct   float64   `json:"savings_pct"`
	PIIDetected  bool      `json:"pii_detected,omitempty"`
	Team         string    `json:"team,omitempty"`
	Feature      string    `json:"feature,omitempty"`
	UserID       string    `json:"user_id,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
}

type PatternInsight struct {
	PromptPattern  string `json:"prompt_pattern"`
	HitCount       int    `json:"hit_count"`
	AvgTokensSaved int    `json:"avg_tokens_saved"`
	Recommendation string `json:"recommendation"`
}

// New constructs a Learner backed by the given NATS connection and Postgres
// pool. JetStream is initialised and the LENS_EVENTS stream is created if
// missing. If JetStream isn't available, the Learner still works as a
// fire-and-forget publisher (messages just won't be persisted).
func New(nc *nats.Conn, pool *pgxpool.Pool) *Learner {
	l, err := newLearner(nc, pool)
	if err != nil {
		slog.Warn("learner: jetstream init failed; continuing without persistence",
			slog.String("err", err.Error()))
		return &Learner{nc: nc, pool: pool}
	}
	return l
}

func newLearner(nc *nats.Conn, pool pgxDB) (*Learner, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream context: %w", err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:      streamName,
		Subjects:  []string{streamSubject},
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		MaxAge:    30 * 24 * time.Hour,
		MaxBytes:  1 << 30, // 1 GiB
	}); err != nil && !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		// Stream already existing under a different config is a non-fatal
		// warning — we still get a usable JS context.
		slog.Warn("learner: AddStream returned non-fatal error",
			slog.String("err", err.Error()))
	}
	return &Learner{nc: nc, js: js, pool: pool}, nil
}

// Record publishes a token event to NATS in fire-and-forget mode.
// It never returns an error — learner failures must never break the hot path.
func (l *Learner) Record(_ context.Context, event TokenEvent) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("learner: marshal token event failed",
			slog.String("err", err.Error()))
		return nil
	}
	if l.nc == nil {
		return nil
	}
	// nc.Publish is non-blocking — it queues to the flush goroutine. The
	// JetStream stream attached to lens.events.> still captures the message
	// without us paying for a synchronous ack.
	if err := l.nc.Publish(eventSubject, data); err != nil {
		slog.Warn("learner: publish failed",
			slog.String("subject", eventSubject),
			slog.String("err", err.Error()))
	}
	return nil
}

// ⚠ "UNCACHED" IS serve_source, NOT THE `cached` BOOLEAN. This filter used to read
// `AND cached = false`, and because nothing has ever written that column it was TRUE for every
// row — an inert predicate that filtered nothing. The consequence is not a crash but a wrong
// recommendation: prompts ALREADY being served from cache were returned as pre-warming
// candidates, and their hit_count inflates the estimated_monthly_savings_usd that
// get_model_recommendations derives from it. Same column, same root cause, third surface — see
// internal/catalog/writerless_column_guard_test.go, which is what found this one.
//
// ⚠ AND THE LINE BELOW IT WAS THE SAME DEFECT AGAIN, ON A DIFFERENT COLUMN. GROUP BY
// prompt_hash grouped by a column no production writer has ever set: neither of the two INSERT
// statements in internal/alerts names prompt_hash, so every row carries that column's NOT NULL
// default, the empty string. Measured on a real database over the full migration chain — nine
// spend rows covering THREE distinct prompts produced ONE group, and Analyse returned a single
// insight with an empty pattern and HitCount 9. That count is the window's total
// non-cache-served request count, not a repetition count, and both handleModelsRecommendations
// and toolGetModelRecommendations multiply it into estimated_monthly_savings_usd.
//
// The added non-empty test on prompt_hash is the whole fix, and it is deliberately a FILTER
// rather than a rewrite: today it makes Analyse report nothing, which is the truth — production
// records no prompt identity to group by — and the moment a writer exists the query does exactly
// what it was always meant to, with no further change. See
// internal/learner/writerless_prompt_hash_test.go, whose census fails the build if that writer
// appears, so this comment cannot go quietly stale.
//
// ⚠ WHAT THIS DOES NOT DO, SAID PLAINLY: it does not make pattern recommendation work. Giving
// prompt_hash a writer means retaining a per-prompt fingerprint under the DEFAULT metadata
// logging policy, whose stated purpose is to strip prompt identity — a privacy decision, not a
// bug fix, and not a session's to take.
const analyseSQL = `SELECT prompt_hash, COUNT(*) as hit_count,
       AVG(input_tokens + output_tokens) as avg_tokens,
       MAX(created_at) as last_seen
FROM token_events
WHERE created_at > NOW() - INTERVAL '7 days'
  AND serve_source NOT LIKE 'cache_hit%'
  AND prompt_hash <> ''
  AND ($1 = '' OR workspace_id = $1)
GROUP BY prompt_hash
HAVING COUNT(*) >= 3
ORDER BY hit_count DESC
LIMIT 20`

// Analyse returns the most frequently-repeated uncached prompt hashes from
// the last 7 days. Each insight carries a human-readable recommendation
// suggesting the pattern be pre-warmed into the semantic cache.
func (l *Learner) Analyse(ctx context.Context) ([]PatternInsight, error) {
	return l.AnalyseWorkspace(ctx, "")
}

// AnalyseWorkspace is Analyse over one workspace's traffic ("" = every workspace).
func (l *Learner) AnalyseWorkspace(ctx context.Context, workspaceID string) ([]PatternInsight, error) {
	if l.pool == nil {
		return nil, errors.New("learner: no database pool configured")
	}
	rows, err := l.pool.Query(ctx, analyseSQL, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("learner: query token_events: %w", err)
	}
	defer rows.Close()

	var insights []PatternInsight
	for rows.Next() {
		var (
			promptHash string
			hitCount   int
			avgTokens  float64
			lastSeen   time.Time
		)
		if err := rows.Scan(&promptHash, &hitCount, &avgTokens, &lastSeen); err != nil {
			return nil, fmt.Errorf("learner: scan row: %w", err)
		}
		insights = append(insights, PatternInsight{
			PromptPattern:  promptHash,
			HitCount:       hitCount,
			AvgTokensSaved: int(avgTokens),
			Recommendation: fmt.Sprintf("Cache this pattern — seen %d times, saves ~%d tokens per hit",
				hitCount, int(avgTokens)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learner: iterate rows: %w", err)
	}

	// The SQL already orders by hit_count DESC, but defend against drivers
	// or proxies that don't preserve ordering.
	sort.SliceStable(insights, func(i, j int) bool {
		return insights[i].HitCount > insights[j].HitCount
	})

	return insights, nil
}

// StartBackground spawns a goroutine that runs Analyse on a fixed interval
// and logs the top insights. It returns immediately; the goroutine exits
// cleanly when ctx is cancelled.
func (l *Learner) StartBackground(ctx context.Context) {
	go l.runLoop(ctx, analyseInterval)
}

func (l *Learner) runLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			insights, err := l.Analyse(ctx)
			if err != nil {
				slog.Warn("learner: analyse failed",
					slog.String("err", err.Error()))
				continue
			}
			top := insights
			if len(top) > 3 {
				top = top[:3]
			}
			for i, ins := range top {
				slog.Info("learner: top pattern",
					slog.Int("rank", i+1),
					slog.String("pattern", ins.PromptPattern),
					slog.Int("hits", ins.HitCount),
					slog.Int("avg_tokens", ins.AvgTokensSaved),
					slog.String("recommendation", ins.Recommendation),
				)
			}
		}
	}
}
