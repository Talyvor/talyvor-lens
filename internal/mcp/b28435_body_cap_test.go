package mcp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// B28.435: /mcp refuses a message over maxRPCBody with 413 instead of decoding it.
func TestMCP_OversizedBodyIs413(t *testing.T) {
	srv := New(nil, nil, nil, nil, nil, "test")
	body := append([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`), bytes.Repeat([]byte("a"), maxRPCBody)...)
	body = append(body, `"}}`...)
	rec := httptest.NewRecorder()
	srv.HandleRPC(rec, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized /mcp = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
}
