package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/auth"
)

// ask_as_key.go — B28.125: a prompt scheduled from Chat is asked at its time by Lens itself, as the paying agent's
// key — whose plaintext nobody holds — through the provider's own handler. The request carries the identity
// AuthMiddleware stamps for a workspace key, so the agent's rules judge it and its wallet pays, as for a call the
// agent makes with that key; nothing here charges or skips anything.

// askMaxTokens bounds an answer where the provider requires a bound (Anthropic, Bedrock), as Chat's tasks do.
const askMaxTokens = 4096

// AskAsKey asks provider's model the prompt as scopedKeyID of workspaceID, under requestID, and returns the answer.
// Anything but a 2xx is an error carrying Lens's own sentence.
func (p *Proxy) AskAsKey(ctx context.Context, workspaceID, scopedKeyID, provider, model, prompt, requestID string) (string, error) {
	handler, path := p.HandleExtraProvider(provider), "v1/chat/completions"
	switch provider {
	case "openai":
		handler = p.HandleOpenAI
	case "anthropic":
		handler, path = p.HandleAnthropic, "v1/messages"
	case "google":
		handler = p.HandleGoogle
	case "bedrock":
		handler = p.HandleBedrock
	}
	payload := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}}}
	if provider == "anthropic" || provider == "bedrock" {
		payload["max_tokens"] = askMaxTokens
	}
	body, _ := json.Marshal(payload)
	actx := &auth.AuthContext{WorkspaceID: workspaceID, AuthMethod: auth.MethodWorkspaceKey, APIKeyID: scopedKeyID, Scopes: []string{auth.ScopeProxy}}
	ctx = auth.WithAPIKey(auth.WithAuthContext(ctx, actx),
		&auth.APIKey{ID: "global", WorkspaceID: workspaceID, Name: actx.AuthMethod, Active: true, CreatedAt: time.Now().UTC()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/proxy/"+provider+"/"+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", workspaceID)
	req.Header.Set("X-Talyvor-Request-ID", requestID)
	req.Header.Set("X-Talyvor-Feature", "scheduled-prompt")
	rec := httptest.NewRecorder()
	handler(rec, req)
	var out struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	decodeErr := json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code/100 != 2 {
		return "", errors.New(lensSentence(rec.Code, out.Error, rec.Body.String()))
	}
	if decodeErr != nil {
		return "", fmt.Errorf("the model's answer could not be read")
	}
	if len(out.Choices) > 0 {
		return out.Choices[0].Message.Content, nil
	}
	var text strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return text.String(), nil
}

// lensSentence is what a refused call said: {"error": "…"} or {"error": {"message": "…"}}, else the status.
func lensSentence(status int, raw json.RawMessage, body string) string {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Message != "" {
		return m.Message
	}
	if body = strings.TrimSpace(body); len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("Lens answered %d %s", status, body)
}
