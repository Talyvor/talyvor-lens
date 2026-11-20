package main

import (
	"context"
	"encoding/base32"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/stepup"
)

// B28.461 — the step-up code cannot be guessed. Whoever holds the admin key used to be able to send every six-digit
// code in turn and be answered 403 each time until one opened the action; now five wrong codes and every step-up action
// waits, answering 429 with Retry-After.
func TestStepUp_TwoThousandWrongCodesAreNotEachAnswered403(t *testing.T) {
	key := []byte("0123456789abcdef0123")
	v, err := stepup.New(base32.StdEncoding.EncodeToString(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	type lockRow struct{ operator, target string }
	var rows []lockRow
	v.OnLock(func(_ context.Context, operator, target string) error {
		rows = append(rows, lockRow{operator, target})
		return nil
	})
	ran := 0
	r := chi.NewRouter()
	r.Post("/v1/admin/lxc/grant", requireStepUp(headerAdmin{}, v, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran++
		w.WriteHeader(http.StatusOK)
	})))
	good := stepup.CodeAt(key, time.Now())
	answered403, answered429 := 0, 0
	for i := 0; i < 2000; i++ {
		guess := fmt.Sprintf("%06d", i)
		if guess == good {
			continue
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/lxc/grant", nil)
		req.Header.Set("Authorization", "Bearer admin-key")
		req.Header.Set(moderatorOperatorHeader, "nicolai")
		req.Header.Set(stepUpHeader, guess)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusForbidden:
			answered403++
		case http.StatusTooManyRequests:
			answered429++
			if rec.Header().Get("Retry-After") == "" {
				t.Fatalf("guess %d: 429 without Retry-After", i)
			}
		default:
			t.Fatalf("guess %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if answered403 > 4 || ran != 0 {
		t.Fatalf("%d wrong codes were answered 403 and %d 429, and the action ran %d times; after four wrong codes every one should wait",
			answered403, answered429, ran)
	}
	if len(rows) != 1 || rows[0] != (lockRow{"nicolai", "POST /v1/admin/lxc/grant"}) {
		t.Fatalf("the wait wrote audit rows %v; want exactly one, for nicolai on POST /v1/admin/lxc/grant", rows)
	}

	// A good code during the wait is 429 too, and Retry-After is the wait in seconds.
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/lxc/grant", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	req.Header.Set(moderatorOperatorHeader, "nicolai")
	req.Header.Set(stepUpHeader, stepup.CodeAt(key, time.Now()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if secs, _ := strconv.Atoi(rec.Header().Get("Retry-After")); rec.Code != http.StatusTooManyRequests || secs < 1 || secs > int(stepup.LockFor.Seconds()) || ran != 0 {
		t.Fatalf("a good code during the wait: %d, Retry-After %q, the action ran %d times; want 429 and the wait",
			rec.Code, rec.Header().Get("Retry-After"), ran)
	}
}
