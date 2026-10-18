package main

import (
	"encoding/json"
	"testing"
)

// B28.186 — a listing's instructions go first in an Anthropic request's system prompt, before the chat's own.
func TestWithSystem_AnthropicPutsTheInstructionsFirst(t *testing.T) {
	for in, want := range map[string]string{
		`{"messages":[]}`:                             `[{"text":"Be terse.","type":"text"}]`,
		`{"system":"Mine.","messages":[]}`:            `[{"text":"Be terse.","type":"text"},{"text":"Mine.","type":"text"}]`,
		`{"system":[{"type":"text","text":"Mine."}]}`: `[{"text":"Be terse.","type":"text"},{"type":"text","text":"Mine."}]`,
	} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(in), &body)
		if err := withSystem(body, chatShapeAnthropic, "Be terse."); err != nil || string(body["system"]) != want {
			t.Errorf("%s: system = %s (err %v), want %s", in, body["system"], err, want)
		}
	}
}
