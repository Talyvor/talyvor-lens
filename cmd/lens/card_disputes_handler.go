package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
)

// B59.24 — card disputes and refunds, and the card export (economy/card_disputes.go). A card payment is named by the
// issuer's id for it, as the statement's card lines carry it (partner_ref).
//
//	GET  /v1/money/card-disputes                     the workspace's disputes, newest first, and where each stands
//	POST /v1/money/card-disputes                     {payment_id, reason} — dispute a card payment: 201, the dispute, with
//	                                                 the issuer once it took it; the same payment again is its one dispute
//	POST /v1/money/card-disputes/{id}/sandbox-outcome {won} — the Sandbox decides a dispute it took, as an issuer would
//	GET  /v1/money/card-refunds                      the workspace's merchant refunds, newest first, and what each credited
//	POST /v1/money/card-refunds                      {payment_id, amount_minor, idempotency_key} — the Sandbox's merchant
//	                                                 refunds a card payment, as an issuer would report it: 201
//	GET  /v1/money/card-exports?from=&to=            the card payments, disputes and refunds made from one UTC day to
//	                                                 another, both included, as CSV
//
// The owner or an admin only. Mounted in the authed group.

type cardDisputeBank interface {
	CardDisputes(ctx context.Context, workspaceID string) ([]economy.CardDispute, error)
	OpenCardDispute(ctx context.Context, workspaceID, paymentID, reason, by string) (economy.CardDispute, error)
	SandboxCardDisputeOutcome(ctx context.Context, workspaceID, id string, won bool) (economy.CardDispute, error)
	CardRefunds(ctx context.Context, workspaceID string) ([]economy.CardPaymentRefund, error)
	SandboxCardRefund(ctx context.Context, workspaceID, paymentID string, amountMinor int64, key string) (economy.CardPaymentRefund, error)
	CardExport(ctx context.Context, workspaceID string, from, to time.Time) ([]economy.CardExportRow, error)
}

func writeCardDisputeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrCardDisputeNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrCardDisputeDecided):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrCardDispute), errors.Is(err, money.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

func mountCardDisputeRoutes(r chi.Router, bank cardDisputeBank) {
	r.Get("/v1/money/card-disputes", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		list, err := bank.CardDisputes(req.Context(), ws)
		if err != nil {
			writeCardDisputeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"card_disputes": list})
	}))
	r.Post("/v1/money/card-disputes", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		var in struct {
			PaymentID string `json:"payment_id"`
			Reason    string `json:"reason"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || ws == "" || in.PaymentID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"payment_id", "reason"}`)
			return
		}
		by := ""
		if ac := auth.GetAuthContext(req.Context()); ac != nil {
			by = ac.UserID
		}
		d, err := bank.OpenCardDispute(req.Context(), ws, in.PaymentID, in.Reason, by)
		if err != nil {
			writeCardDisputeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, d)
	}))
	r.Post("/v1/money/card-disputes/{id}/sandbox-outcome", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		var in struct {
			Won *bool `json:"won"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || ws == "" || in.Won == nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"won": true | false}`)
			return
		}
		d, err := bank.SandboxCardDisputeOutcome(req.Context(), ws, chi.URLParam(req, "id"), *in.Won)
		if err != nil {
			writeCardDisputeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, d)
	}))
	r.Get("/v1/money/card-refunds", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		list, err := bank.CardRefunds(req.Context(), ws)
		if err != nil {
			writeCardDisputeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"card_refunds": list})
	}))
	r.Post("/v1/money/card-refunds", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		var in struct {
			PaymentID      string `json:"payment_id"`
			AmountMinor    int64  `json:"amount_minor"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || ws == "" || in.PaymentID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"payment_id", "amount_minor", "idempotency_key"}`)
			return
		}
		re, err := bank.SandboxCardRefund(req.Context(), ws, in.PaymentID, in.AmountMinor, in.IdempotencyKey)
		if err != nil {
			writeCardDisputeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, re)
	}))
	r.Get("/v1/money/card-exports", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		from, err1 := time.Parse(time.DateOnly, req.URL.Query().Get("from"))
		to, err2 := time.Parse(time.DateOnly, req.URL.Query().Get("to"))
		if err1 != nil || err2 != nil || ws == "" || to.Before(from) {
			writeJSONErr(w, http.StatusBadRequest, "from and to must be days as YYYY-MM-DD, to on or after from")
			return
		}
		list, err := bank.CardExport(req.Context(), ws, from, to.AddDate(0, 0, 1))
		if err != nil {
			writeCardDisputeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="card-payments-%s-%s.csv"`,
			from.Format(time.DateOnly), to.Format(time.DateOnly)))
		w.WriteHeader(http.StatusOK)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"type", "at", "id", "payment_id", "card_id", "agent_id", "merchant", "amount", "currency", "status", "credited", "reason"})
		for _, x := range list {
			credited := ""
			if x.Type != "payment" {
				credited = economy.MinorDecimal(x.CreditedMinor, x.Currency)
			}
			_ = cw.Write([]string{x.Type, x.At.UTC().Format(time.RFC3339), csvText(x.ID), csvText(x.PaymentID), x.CardID, x.AgentID,
				csvText(x.Merchant), economy.MinorDecimal(x.AmountMinor, x.Currency), x.Currency, x.Status, credited, csvText(x.Reason)})
		}
		cw.Flush()
	}))
}
