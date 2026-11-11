package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// B28.435: a synthetic call whose body is over syntheticMaxBody is refused with 413 before its action runs.
func TestSyntheticGuard_OversizedBodyIs413(t *testing.T) {
	ran := false
	h := syntheticGuard("k", "plan", &callLimiter{max: 10, per: syntheticCallsPer}, syntheticDeps{},
		func(w http.ResponseWriter, r *http.Request) (int, string) {
			ran = true
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON")
			}
			return 0, "ok"
		})
	body := append([]byte(`{"plan":"`), bytes.Repeat([]byte("a"), syntheticMaxBody)...)
	body = append(body, `"}`...)
	req := httptest.NewRequest(http.MethodPost, "/v1/synthetic/workspaces/ws/plan", bytes.NewReader(body))
	req.Header.Set(syntheticKeyHeader, "k")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || ran {
		t.Fatalf("oversized synthetic call = %d (action ran: %v), want 413 without running", rec.Code, ran)
	}
}
