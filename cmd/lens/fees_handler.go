package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/fees"
)

// publicFeesHandler is GET /v1/public/fees (B32.8): every fee Talyvor charges, as lens.env sets it, for /pricing.
func publicFeesHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONOK(w, http.StatusOK, fees.Current())
}

// publicPricingHandler is GET /v1/pricing (B51.2): whether a card can be charged — billing.CardPayments.
func publicPricingHandler(cardPayments string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONOK(w, http.StatusOK, map[string]string{"card_payments": cardPayments})
	}
}

// labelReader is every money capability's label: *economy.DualTokenStore.
type labelReader interface {
	Labels(ctx context.Context) (map[string]string, error)
}

// publicLabelsHandler is GET /v1/public/labels (B56.7): each money capability Live or Sandbox, worked out now from the
// rails and clearances, and whether card payments are live.
func publicLabelsHandler(store labelReader, cardPayments string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		labels, err := store.Labels(req.Context())
		if err != nil {
			slog.Error("public labels", "err", err)
			writeJSONErr(w, http.StatusInternalServerError, "labels cannot be read right now")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"labels": labels, "card_payments": cardPayments, "updated_at": time.Now().UTC()})
	}
}
