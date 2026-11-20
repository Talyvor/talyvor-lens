package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/talyvor/lens/internal/paypool"
)

// paymentsPoolWeeks is what the payments pool's operator read reads (B63.1).
type paymentsPoolWeeks interface {
	Weeks(ctx context.Context, n int) ([]paypool.Week, error)
}

// newPaymentsPoolHandler is GET /v1/admin/payments-pool (B63.1): the payments pool's measures by week, newest first
// (?weeks=, 12 by default) — counts and sums only, every sum assumed and none of it money that moved.
func newPaymentsPoolHandler(pool paymentsPoolWeeks) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n, _ := strconv.Atoi(req.URL.Query().Get("weeks"))
		weeks, err := pool.Weeks(req.Context(), n)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"weeks": weeks, "assumed": true, "owners_floor": paypool.OwnersFloor})
	})
}
