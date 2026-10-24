package proxy

import (
	"encoding/json"
	"strings"
)

// B17.217 — the fallback chain sends a failed Anthropic request (429, 5xx, overloaded) to OpenAI or Google, which
// read and answer the OpenAI chat shape. Before these two, the Anthropic body went over as it was and the chat
// answer came back as it was: an Anthropic client read no `content` and saw a 200 with an empty answer.

// anthropicToOpenAIChat rewrites an Anthropic Messages body as an OpenAI chat body. ok is false when the body
// holds what a text rewrite would lose — tools, images, tool or thinking blocks — so that body is not sent on.
func anthropicToOpenAIChat(body []byte) ([]byte, bool) {
	var in struct {
		Model    string          `json:"model"`
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens     int             `json:"max_tokens"`
		Temperature   *float64        `json:"temperature"`
		TopP          *float64        `json:"top_p"`
		StopSequences []string        `json:"stop_sequences"`
		Tools         json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &in); err != nil || len(in.Messages) == 0 {
		return nil, false
	}
	if t := strings.TrimSpace(string(in.Tools)); t != "" && t != "null" && t != "[]" {
		return nil, false
	}
	var msgs []map[string]string
	if sys, ok := anthropicText(in.System); !ok {
		return nil, false
	} else if sys != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": sys})
	}
	for _, m := range in.Messages {
		text, ok := anthropicText(m.Content)
		if !ok {
			return nil, false
		}
		msgs = append(msgs, map[string]string{"role": m.Role, "content": text})
	}
	out := map[string]any{"model": in.Model, "messages": msgs}
	if in.MaxTokens > 0 {
		out["max_tokens"] = in.MaxTokens
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.StopSequences) > 0 {
		out["stop"] = in.StopSequences
	}
	b, err := json.Marshal(out)
	return b, err == nil
}

// anthropicText reads an Anthropic content (or system) value — a string, or text blocks — as one string.
func anthropicText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" {
			return "", false
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), true
}

// openAIChatToAnthropic rewrites an OpenAI chat completion as an Anthropic message. A body that is not one is
// returned unchanged.
func openAIChatToAnthropic(body []byte, model string) []byte {
	var in struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil || len(in.Choices) == 0 {
		return body
	}
	stop := "end_turn"
	if in.Choices[0].FinishReason == "length" {
		stop = "max_tokens"
	}
	out := map[string]any{
		"id": in.ID, "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]string{{"type": "text", "text": in.Choices[0].Message.Content}},
		"stop_reason": stop, "stop_sequence": nil,
	}
	if in.Usage != nil {
		out["usage"] = map[string]int{"input_tokens": in.Usage.PromptTokens, "output_tokens": in.Usage.CompletionTokens}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return body
	}
	return b
}
