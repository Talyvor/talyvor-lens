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

// B28.338 — where the workspace's alerts are sent (economy/alert_channels.go).
//
//	GET    /v1/money/alerts/channels      — the channels, oldest first, without their secrets
//	POST   /v1/money/alerts/channels      {kind, url} — add push, webhook (an https url; its signing secret comes back
//	                                        this once) or slack (its incoming webhook); email is refused until an
//	                                        email integration exists
//	DELETE /v1/money/alerts/channels/{id} — remove a channel
//	GET    /v1/money/alerts/deliveries    — each alert queued or sent to each channel, newest first, with its status
//
// The workspace is the key's; only its owner or an admin may use these (spendingOwner). Mounted in the authed group.

type alertChannelsBank interface {
	AddAlertChannel(ctx context.Context, workspaceID string, c economy.AlertChannel, by string) (economy.AlertChannel, error)
	AlertChannels(ctx context.Context, workspaceID string) ([]economy.AlertChannel, error)
	DeleteAlertChannel(ctx context.Context, workspaceID, id string) error
	QueueAlertDeliveries(ctx context.Context, workspaceID string) (int, error)
	AlertDeliveries(ctx context.Context, workspaceID string) ([]economy.AlertDelivery, error)
}

func mountAlertChannelRoutes(r chi.Router, bank alertChannelsBank) {
	answer := func(w http.ResponseWriter, err error, ok func()) {
		switch {
		case errors.Is(err, economy.ErrAlertChannel):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrAlertChannelNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			ok()
		}
	}
	r.Get("/v1/money/alerts/channels", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		out, err := bank.AlertChannels(req.Context(), ws)
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]any{"channels": out}) })
	})
	r.Post("/v1/money/alerts/channels", func(w http.ResponseWriter, req *http.Request) {
		ws, who, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		var in economy.AlertChannel
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"kind", "url"}`)
			return
		}
		out, err := bank.AddAlertChannel(req.Context(), ws, economy.AlertChannel{Kind: in.Kind, URL: in.URL}, who)
		answer(w, err, func() { writeJSONOK(w, http.StatusCreated, out) })
	})
	r.Delete("/v1/money/alerts/channels/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		err := bank.DeleteAlertChannel(req.Context(), ws, chi.URLParam(req, "id"))
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]string{"deleted": chi.URLParam(req, "id")}) })
	})
	r.Get("/v1/money/alerts/deliveries", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := spendingOwner(w, req)
		if !ok {
			return
		}
		if _, err := bank.QueueAlertDeliveries(req.Context(), ws); err != nil { // the sweep's minute need not be waited for
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out, err := bank.AlertDeliveries(req.Context(), ws)
		answer(w, err, func() { writeJSONOK(w, http.StatusOK, map[string]any{"deliveries": out}) })
	})
}

// sweepAlertChannels queues and sends, every minute until ctx ends, the alerts raised to every channel. Webhooks and
// Slack are reached through safehttp, so a channel's url can never reach inside Talyvor's network.
func sweepAlertChannels(ctx context.Context, store *economy.DualTokenStore) {
	client := safehttp.Client(10 * time.Second)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if queued, sent, err := store.SweepAlertChannels(ctx, client, now); err != nil {
				slog.Warn("alert channels: sweep failed", slog.String("err", err.Error()))
			} else if queued+sent > 0 {
				slog.Info("alert channels: swept", slog.Int("queued", queued), slog.Int("sent", sent))
			}
		}
	}
}
