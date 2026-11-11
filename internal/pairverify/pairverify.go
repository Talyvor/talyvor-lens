// Package pairverify is B9.2's pair verifier. Given a candidate pooled match — the question that
// was stored and the question being asked — it asks a small model whether the two have exactly the
// same correct answer, and allows the serve only on an unambiguous YES.
//
// ⚠ IT LOOKS AT THE PAIR, which is what every earlier mechanism could not do. Similarity,
// typographic folding, canonicalisation and doc2query each compared ONE question to a stored key,
// and the danger — direction and negation, "enable" vs "disable" — lives in the difference between
// two questions, not in either alone.
//
// B9.2 allowed wiring only if the committed danger corpus is served ZERO times across three runs;
// cmd/pairverify is the measurement, docs/pool-b92-measured.md the result. B9.7 wired it: the pooled
// semantic read (cache.SemanticCache.GetPooled) serves only on its YES — docs/pool-b97-measured.md.
package pairverify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Prompt is the fixed instruction; the measurement is a property of this string as much as of the
// model, so it is committed beside the numbers it produced. %s, %s = the stored question, the asker's.
const Prompt = `A cached answer to Question A might be reused to answer Question B.

Question A: %s
Question B: %s

Would these two questions have exactly the same correct answer?
Any difference that could change the answer means NO: direction (enable/disable, to/from, increase/decrease), negation, a version, number, unit, date, place, person, product or other entity. Only a rewording of the same request means YES.

Reply with exactly one word: YES or NO.`

// StandalonePrompt is B16.2's check for a question asked MID-CONVERSATION: would its correct reply
// be the same had the conversation never happened? It is asked WITHOUT the stored question on
// purpose, and as a counterfactual — both measured on 27 Sep with claude-haiku-4-5 (docs/b162-
// standalone-answer-measured.md): one prompt judging "stands alone" and "same answer as Question A"
// together served 10 of 30 context-dependent traps (shown "and what about France?" twice, it judged
// the identical pair, not the conversation), and "could it be answered by someone who has not seen
// the conversation?" still served "how do I say hello?" after "I'm learning Spanish". The pair check
// (Prompt) stays a separate call. %s, %s = the conversation before the message, the message.
const StandalonePrompt = `Below is a conversation and the user's next message.

Conversation:
%s

Next message: %s

Would the correct reply to the next message be exactly the same if the conversation above had never happened?
Think about what the conversation establishes: a topic, something the user is learning or doing, a place, a language, a person or thing that "it", "that", "he", "there" or "the same" refers to, an instruction about format, length or tone, a preference or a personal detail. If any of it changes what the right reply is, the answer is NO.
Answer YES only if someone who never saw the conversation would give exactly the right reply.

Reply with exactly one word: YES or NO.`

// ContextVerifier checks whether a question asked after history stands alone. A Verifier that does
// not implement it leaves B16.2's lane closed.
type ContextVerifier interface {
	StandsAlone(ctx context.Context, history, asked string) (Verdict, error)
}

// Verdict is one check. Raw is kept because "the model said no" and "the model said something
// else" are different findings, and both refuse the serve.
type Verdict struct {
	Same      bool
	Raw       string
	InTokens  int
	OutTokens int
}

// Verifier checks one pair.
type Verifier interface {
	Verify(ctx context.Context, stored, asked string) (Verdict, error)
}

// Parse reads a reply. ONLY a bare YES (case, surrounding space and a final full stop ignored)
// allows a serve; "Yes, but…", an explanation, a refusal or an empty reply all refuse.
func Parse(raw string) bool {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".")
	return strings.EqualFold(s, "YES")
}

// AnthropicVerifier calls a small Anthropic model at temperature 0.
type AnthropicVerifier struct {
	APIKey string
	Model  string
	URL    string
	HTTP   *http.Client
}

// defaultModel is the cheap model the measurement ran on.
const defaultModel = "claude-haiku-4-5"

// maxVerdictBytes bounds the read of the verifier's answer (it is asked for 5 tokens).
const maxVerdictBytes = 1 << 20

func NewAnthropicVerifier(apiKey, model string) *AnthropicVerifier {
	if model == "" {
		model = defaultModel
	}
	return &AnthropicVerifier{APIKey: apiKey, Model: model, URL: "https://api.anthropic.com/v1/messages",
		HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (v *AnthropicVerifier) Verify(ctx context.Context, stored, asked string) (Verdict, error) {
	return v.ask(ctx, fmt.Sprintf(Prompt, stored, asked))
}

// StandsAlone is B16.2's check: could asked, at the end of history, be answered without it?
func (v *AnthropicVerifier) StandsAlone(ctx context.Context, history, asked string) (Verdict, error) {
	return v.ask(ctx, fmt.Sprintf(StandalonePrompt, history, asked))
}

func (v *AnthropicVerifier) ask(ctx context.Context, prompt string) (Verdict, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       v.Model,
		"max_tokens":  5,
		"temperature": 0,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL, bytes.NewReader(body))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", v.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return Verdict{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("pairverify: anthropic %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVerdictBytes)).Decode(&out); err != nil {
		return Verdict{}, err
	}
	var raw string
	for _, c := range out.Content {
		raw += c.Text
	}
	return Verdict{Same: Parse(raw), Raw: raw, InTokens: out.Usage.InputTokens, OutTokens: out.Usage.OutputTokens}, nil
}
