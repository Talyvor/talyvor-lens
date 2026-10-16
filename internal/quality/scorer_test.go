package quality

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestScore_PerfectResponseIsCacheable(t *testing.T) {
	s := New(nil)
	prompt := "Explain quantum entanglement"
	response := "Quantum entanglement is a phenomenon where two particles share a state. " +
		"When one is measured, the state of the other is instantly determined. " +
		"This was famously discussed in the EPR paradox by Einstein, Podolsky, and Rosen."

	got := s.ScoreResponse(context.Background(), prompt, response, "openai", "gpt-4")

	if got.Score < 0.6 {
		t.Errorf("Score = %v, want >= 0.6 for a perfect response (reasons: %v)", got.Score, got.Reasons)
	}
	if !got.ShouldCache {
		t.Errorf("ShouldCache = false, want true (score: %v)", got.Score)
	}
	if got.ShouldEvict {
		t.Errorf("ShouldEvict = true, want false (score: %v)", got.Score)
	}
}

func TestScore_VeryShortResponseBelowCacheThreshold(t *testing.T) {
	s := New(nil)
	// A long prompt + a tiny refusal-shaped response stacks the length and
	// refusal penalties: -0.2 (short) - 0.3 (refusal) = 0.5 < 0.6.
	prompt := strings.Repeat("explain this in great detail ", 10)
	response := "I cannot help"

	got := s.ScoreResponse(context.Background(), prompt, response, "openai", "gpt-4")

	if got.Score >= 0.6 {
		t.Errorf("Score = %v, want < 0.6 for tiny refusal (reasons: %v)", got.Score, got.Reasons)
	}
	if got.ShouldCache {
		t.Error("ShouldCache should be false for low-quality response")
	}
}

func TestScore_RefusalIndicatorPenalised(t *testing.T) {
	s := New(nil)
	response := "I cannot help with that. It is outside my capabilities, regretfully."

	got := s.ScoreResponse(context.Background(), "Question about something", response, "openai", "gpt-4")

	if got.Score >= 1.0 {
		t.Errorf("Score = %v, want < 1.0 (refusal should be penalised)", got.Score)
	}
	reasonFound := false
	for _, r := range got.Reasons {
		if strings.Contains(strings.ToLower(r), "refusal") || strings.Contains(strings.ToLower(r), "error") {
			reasonFound = true
		}
	}
	if !reasonFound {
		t.Errorf("expected a refusal/error reason; got %v", got.Reasons)
	}
}

func TestScore_TruncatedResponsePenalised(t *testing.T) {
	s := New(nil)
	// > 100 chars, doesn't end with punctuation → truncation penalty.
	response := strings.Repeat("This sentence ends with a comma, ", 5) + "this one does not"

	got := s.ScoreResponse(context.Background(), "Question", response, "openai", "gpt-4")

	if got.Score >= 1.0 {
		t.Errorf("Score = %v, want < 1.0 (truncated response should be penalised)", got.Score)
	}
	reasonFound := false
	for _, r := range got.Reasons {
		if strings.Contains(strings.ToLower(r), "truncat") {
			reasonFound = true
		}
	}
	if !reasonFound {
		t.Errorf("expected a truncation reason; got %v", got.Reasons)
	}
}

func TestScore_RepeatedSentencesPenalised(t *testing.T) {
	s := New(nil)
	// Same sentence repeated 3 times.
	response := "Hello world. Hello world. Hello world."

	got := s.ScoreResponse(context.Background(), "Question", response, "openai", "gpt-4")

	if got.Score >= 1.0 {
		t.Errorf("Score = %v, want < 1.0 (repetition should be penalised)", got.Score)
	}
	reasonFound := false
	for _, r := range got.Reasons {
		if strings.Contains(strings.ToLower(r), "repeat") {
			reasonFound = true
		}
	}
	if !reasonFound {
		t.Errorf("expected a repetition reason; got %v", got.Reasons)
	}
}

func TestScore_StructuredResponseGetsBonus(t *testing.T) {
	s := New(nil)
	// Combine penalties so the bonus is observable in the final score.
	// A short refusal alone scores 0.5; the markdown bonus pushes it to 0.6.
	prompt := strings.Repeat("explain in detail ", 10)
	response := "```\nI cannot help with that\n```"

	got := s.ScoreResponse(context.Background(), prompt, response, "openai", "gpt-4")

	reasonFound := false
	for _, r := range got.Reasons {
		if strings.Contains(strings.ToLower(r), "structured") || strings.Contains(strings.ToLower(r), "markdown") {
			reasonFound = true
		}
	}
	if !reasonFound {
		t.Errorf("expected a structure/markdown reason; got %v", got.Reasons)
	}
}

func TestScore_LowScoreTriggersEviction(t *testing.T) {
	s := New(nil)
	// Stack penalties: short (-0.2) + two refusal indicators (-0.6) =
	// 1.0 - 0.8 = 0.2 → ShouldEvict.
	prompt := strings.Repeat("write me an essay about something deeply complex ", 10)
	response := "I cannot. I'm sorry, but no."

	got := s.ScoreResponse(context.Background(), prompt, response, "openai", "gpt-4")

	if got.Score >= 0.4 {
		t.Errorf("Score = %v, want < 0.4 (reasons: %v)", got.Score, got.Reasons)
	}
	if !got.ShouldEvict {
		t.Errorf("ShouldEvict = false, want true (score: %v)", got.Score)
	}
}

func TestScore_AlwaysClampedToZeroOne(t *testing.T) {
	s := New(nil)
	// Stack every possible penalty to make sure we never go negative.
	prompt := strings.Repeat("x", 200)
	response := "I cannot. I'm unable to. I don't know. I apologize. as an AI. I'm sorry, but. " +
		"Same. Same. Same.  more text"
	got := s.ScoreResponse(context.Background(), prompt, response, "openai", "gpt-4")

	if got.Score < 0 || got.Score > 1 {
		t.Errorf("Score = %v, must be in [0, 1]", got.Score)
	}
}

func TestRecordFeedback_PositiveIncrementsHitCount(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := newScorer(pool)

	pool.ExpectExec(`UPDATE prompt_embeddings SET hit_count = hit_count \+ 1`).
		WithArgs("abc", "ws-a").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if _, err := s.RecordFeedback(context.Background(), "ws-a", "abc", FeedbackPositive); err != nil {
		t.Fatalf("RecordFeedback: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRecordFeedback_RepeatTreatedAsNegative(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := newScorer(pool)

	pool.ExpectExec(`UPDATE prompt_embeddings SET hit_count = hit_count - 1`).
		WithArgs("abc", "ws-a").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if _, err := s.RecordFeedback(context.Background(), "ws-a", "abc", FeedbackRepeat); err != nil {
		t.Fatalf("RecordFeedback: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
