package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/safehttp"
)

// B28.344 — signed, retried webhooks for wallet events (economy/wallet_webhooks.go).
//
//	GET    /v1/money/event-webhooks             — the endpoints, oldest first, without their secrets
//	POST   /v1/money/event-webhooks             {url, events} — add an https endpoint taking those events (none: every one);
//	                                        its signing secret comes back this once
//	POST   /v1/money/event-webhooks/{id}/rotate — a new signing secret, shown this once; the old one signs beside it for a day
//	DELETE /v1/money/event-webhooks/{id}        — remove an endpoint
//	GET    /v1/money/event-webhooks/events      — each event queued, delivered or failed to each endpoint, newest first
//
// The workspace is the key's; only its owner or an admin may use these (spendingOwner). Mounted in the authed group.

type walletWebhooksBank interface {
	AddWalletWebhook(ctx context.Context, workspaceID, url string, events []string, by string) (economy.WalletWebhook, error)
	WalletWebhooks(ctx context.Context, workspaceID string) ([]economy.WalletWebhook, error)
	RotateWalletWebhook(ctx context.Context, workspaceID, id string, now time.Time) (economy.WalletWebhook, error)
	DeleteWalletWebhook(ctx context.Context, workspaceID, id string) error
	QueueWalletWebhookEvents(ctx context.Context, workspaceID string) (int, error)
	WalletWebhookEvents(ctx context.Context, workspaceID string) ([]economy.WalletWebhookEvent, error)
}

func mountWalletWebhookRoutes(r chi.Router, bank walletWebhooksBank) {
	answer := func(w http.ResponseWriter, err error, ok func()) {
		switch {
		case errors.Is(err, economy.ErrWalletWebhook):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrWalletWebhookNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			ok()
		}
	}
	r.Get("/v1/money/event-webhooks", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		out, err := bank.WalletWebhooks(req.Context(), ws)
		answer(w, err, func() {
			writeJSONOK(w, http.StatusOK, map[string]any{"webhooks": out, "event_types": economy.WalletEventTypes})
		})
	})
	r.Post("/v1/money/event-webhooks", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		var in struct {
			URL    string   `json:"url"`
			Events []string `json:"events"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"url", "events"}`)
			return
		}
		out, err := bank.AddWalletWebhook(req.Context(), ws, in.URL, in.Events, who)
		answer(w, err, func() { writeJSONOK(w, http.StatusCreated, out) })
	})
	r.Post("/v1/money/event-webhooks/{id}/rotate", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		out, err := bank.RotateWalletWebhook(req.Context(), ws, chi.URLParam(req, "id"), time.Now())
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, out) })
	})
	r.Delete("/v1/money/event-webhooks/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		err := bank.DeleteWalletWebhook(req.Context(), ws, chi.URLParam(req, "id"))
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]string{"deleted": chi.URLParam(req, "id")}) })
	})
	r.Get("/v1/money/event-webhooks/events", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		if _, err := bank.QueueWalletWebhookEvents(req.Context(), ws); err != nil { // the sweep's minute need not be waited for
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, err := bank.WalletWebhookEvents(req.Context(), ws)
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]any{"events": out}) })
	})
}

// sweepWalletWebhooks queues and sends, every minute until ctx ends, the wallet events to every endpoint. They are
// reached through safehttp, so an endpoint's url can never reach inside Talyvor's network.
func sweepWalletWebhooks(ctx context.Context, store *economy.DualTokenStore) {
	client := safehttp.Client(10 * time.Second)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if queued, delivered, err := store.SweepWalletWebhooks(ctx, client, now); err != nil {
				slog.Warn("wallet webhooks: sweep failed", slog.String("err", err.Error()))
			} else if queued+delivered > 0 {
				slog.Info("wallet webhooks: swept", slog.Int("queued", queued), slog.Int("delivered", delivered))
			}
		}
	}
}
