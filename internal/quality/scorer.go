package quality

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type QualityScore struct {
	Score       float64
	Reasons     []string
	ShouldCache bool
	ShouldEvict bool
}

type FeedbackSignal string

const (
	FeedbackPositive FeedbackSignal = "positive"
	FeedbackNegative FeedbackSignal = "negative"
	FeedbackRepeat   FeedbackSignal = "repeat"
)

// pgxDB is the subset of *pgxpool.Pool that Scorer needs. Tests pass nil
// (for pure-scoring tests) or a pgxmock pool (for feedback tests).
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Scorer struct {
	pool pgxDB
}

func New(pool *pgxpool.Pool) *Scorer {
	// Avoid the typed-nil interface trap: a (*pgxpool.Pool)(nil) stored
	// in pgxDB compares != nil but panics on call.
	if pool == nil {
		return newScorer(nil)
	}
	return newScorer(pool)
}

func newScorer(pool pgxDB) *Scorer {
	return &Scorer{pool: pool}
}

// Tunables — keep at file scope so heuristic changes are visible in one place.
const (
	cacheThreshold    = 0.6
	evictThreshold    = 0.4
	shortRatio        = 0.1
	shortAbsoluteMax  = 50
	truncationMinLen  = 100
	repetitionTrigger = 2 // sentence appearing > N times penalises
	maxErrorPenalty   = 0.6
	perErrorPenalty   = 0.3
	lengthPenalty     = 0.2
	truncationPenalty = 0.2
	repetitionPenalty = 0.2
	structureBonus    = 0.1
)

// errorIndicators are the phrases that signal a refusal/error. Each is
// matched case-insensitively but counts at most once per response, so the
// same phrase appearing repeatedly doesn't stack penalty.
var errorIndicators = []string{
	"I cannot",
	"I'm unable to",
	"I don't know",
	"I apologize",
	"as an AI",
	"I'm sorry, but",
}

// sentenceSplitter splits on sentence terminators so repetition is detected
// regardless of which terminator the LLM picked.
var sentenceSplitter = regexp.MustCompile(`[.!?]+`)

func (s *Scorer) ScoreResponse(_ context.Context, prompt, response, _, _ string) QualityScore {
	score := 1.0
	var reasons []string

	// 1. Length check.
	if len(response) < int(float64(len(prompt))*shortRatio) && len(response) < shortAbsoluteMax {
		score -= lengthPenalty
		reasons = append(reasons, "Response too short relative to prompt")
	}

	// 2. Error indicators — each unique phrase counts once, capped at -0.6.
	lower := strings.ToLower(response)
	matched := 0
	for _, phrase := range errorIndicators {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			matched++
		}
	}
	if matched > 0 {
		penalty := float64(matched) * perErrorPenalty
		if penalty > maxErrorPenalty {
			penalty = maxErrorPenalty
		}
		score -= penalty
		reasons = append(reasons, "Response contains refusal/error indicator")
	}

	// 3. Truncation check.
	if len(response) > truncationMinLen && !endsWithTerminator(response) {
		score -= truncationPenalty
		reasons = append(reasons, "Response appears truncated")
	}

	// 4. Repetition check.
	if hasRepeatedSentence(response) {
		score -= repetitionPenalty
		reasons = append(reasons, "Response contains repeated content")
	}

	// 5. Structure bonus.
	if hasMarkdownStructure(response) {
		score += structureBonus
		reasons = append(reasons, "Well-structured response")
	}

	// Clamp to [0, 1].
	if score < 0 {
		score = 0
	} else if score > 1 {
		score = 1
	}

	return QualityScore{
		Score:       score,
		Reasons:     reasons,
		ShouldCache: score >= cacheThreshold,
		ShouldEvict: score < evictThreshold,
	}
}

func endsWithTerminator(s string) bool {
	s = strings.TrimRight(s, " \t\n\r")
	if s == "" {
		return false
	}
	last := s[len(s)-1]
	return last == '.' || last == '!' || last == '?' || last == '"'
}

func hasRepeatedSentence(response string) bool {
	parts := sentenceSplitter.Split(response, -1)
	counts := make(map[string]int, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		counts[p]++
		if counts[p] > repetitionTrigger {
			return true
		}
	}
	return false
}

func hasMarkdownStructure(response string) bool {
	if strings.Contains(response, "```") || strings.Contains(response, "##") {
		return true
	}
	// List markers must follow a newline so prose like "5 - 3" doesn't
	// falsely trigger the bonus. Also accept a list at the start of the
	// response.
	if strings.HasPrefix(response, "- ") || strings.HasPrefix(response, "* ") {
		return true
	}
	if strings.Contains(response, "\n- ") || strings.Contains(response, "\n* ") {
		return true
	}
	return false
}

const incrementHitsSQL = `UPDATE prompt_embeddings SET hit_count = hit_count + 1 WHERE prompt_hash = $1 AND workspace_id = $2`
const decrementHitsSQL = `UPDATE prompt_embeddings SET hit_count = hit_count - 1 WHERE prompt_hash = $1 AND workspace_id = $2`
const evictSQL = `DELETE FROM prompt_embeddings WHERE prompt_hash = $1`

// RecordFeedback scores workspaceID's own stored answer for promptHash and reports whether it has one;
// another workspace's answer is never touched.
func (s *Scorer) RecordFeedback(ctx context.Context, workspaceID, promptHash string, signal FeedbackSignal) (bool, error) {
	if s.pool == nil {
		return true, nil
	}
	// FeedbackRepeat means the caller asked the same question again — the
	// cached answer wasn't satisfying, treat as a thumbs-down.
	query := incrementHitsSQL
	if signal == FeedbackNegative || signal == FeedbackRepeat {
		query = decrementHitsSQL
	}
	tag, err := s.pool.Exec(ctx, query, promptHash, workspaceID)
	if err != nil {
		return false, fmt.Errorf("quality: record feedback: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Scorer) EvictLowQuality(ctx context.Context, promptHash string) error {
	if s.pool == nil {
		return nil
	}
	if _, err := s.pool.Exec(ctx, evictSQL, promptHash); err != nil {
		return fmt.Errorf("quality: evict: %w", err)
	}
	return nil
}
