package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B30.21 — bulk payouts (economy/money_payouts.go).
//
//	POST /v1/money/payouts              up to 1,000 payments out at once, validated row by row and held for one approval:
//	                                    JSON {rows: [{payee_id, amount_minor, currency, reference}], funding, idempotency_key},
//	                                    or CSV (Content-Type: text/csv) with the columns payee_id (or payee), amount — a
//	                                    decimal such as 12.34 — currency and reference, and the key as the Idempotency-Key
//	                                    header. Answers the batch with every row: valid, or invalid with the reason.
//	POST /v1/money/payouts/{id}/approve the owner or an admin approves it: every valid row is paid, with a result each
//	GET  /v1/money/payouts              ?agent_id= — the batches, newest first, without their rows
//	GET  /v1/money/payouts/{id}         one batch with every row: its validation, or its payment and where it stood
//
// The workspace is the key's. Its owner or an admin uploads from and reads any of its accounts; an agent's own key
// uploads from its own, and only the owner or an admin approves. Mounted in the authed group.

type moneyPayoutBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	CreatePayoutBatch(ctx context.Context, workspaceID string, in economy.PayoutBatch) (economy.PayoutBatch, error)
	ApprovePayoutBatch(ctx context.Context, workspaceID, id, by string) (economy.PayoutBatch, error)
	PayoutBatch(ctx context.Context, workspaceID, agentID, id string) (economy.PayoutBatch, error)
	PayoutBatches(ctx context.Context, workspaceID, agentID string) ([]economy.PayoutBatch, error)
}

func mountMoneyPayoutRoutes(r chi.Router, bank moneyPayoutBank) {
	caller := func(w http.ResponseWriter, req *http.Request) (ws, agentID string, ok bool) {
		return moneyCaller(w, req, bank.AgentOfKey)
	}
	r.Post("/v1/money/payouts", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		var in economy.PayoutBatch
		var err error
		if strings.HasPrefix(req.Header.Get("Content-Type"), "text/csv") {
			in.Rows, err = parsePayoutCSV(io.LimitReader(req.Body, 1<<20))
			in.Funding = req.URL.Query().Get("funding")
		} else if err = json.NewDecoder(req.Body).Decode(&in); err != nil {
			err = errors.New(`body must be {"rows": [{"payee_id", "amount_minor", "currency", "reference"}], "funding", "idempotency_key"}, or a text/csv upload`)
		}
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if k := req.Header.Get("Idempotency-Key"); k != "" {
			in.IdempotencyKey = k
		}
		b, err := bank.CreatePayoutBatch(req.Context(), ws, economy.PayoutBatch{AgentID: agentID, Funding: in.Funding, IdempotencyKey: in.IdempotencyKey, Rows: in.Rows})
		if err != nil {
			writePayoutErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, b)
	})
	r.Post("/v1/money/payouts/{id}/approve", func(w http.ResponseWriter, req *http.Request) {
		ws, _, ok := caller(w, req)
		if !ok {
			return
		}
		who, owner := storedanswers.OwnerOrAdmin(req.Context())
		if !owner {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin approves a payout batch")
			return
		}
		b, err := bank.ApprovePayoutBatch(req.Context(), ws, chi.URLParam(req, "id"), who)
		if err != nil {
			writePayoutErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, b)
	})
	r.Get("/v1/money/payouts", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		if agentID == "" {
			agentID = req.URL.Query().Get("agent_id")
		}
		list, err := bank.PayoutBatches(req.Context(), ws, agentID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"payouts": list})
	})
	r.Get("/v1/money/payouts/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, ok := caller(w, req)
		if !ok {
			return
		}
		b, err := bank.PayoutBatch(req.Context(), ws, agentID, chi.URLParam(req, "id"))
		if err != nil {
			writePayoutErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, b)
	})
}

// parsePayoutCSV reads rows from a CSV with a header naming payee_id (or payee), amount, currency and reference. A row
// whose amount is not a decimal amount of its currency is kept, with the reason, for the batch to report as invalid.
func parsePayoutCSV(r io.Reader) ([]economy.PayoutRow, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord, cr.TrimLeadingSpace = -1, true
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("the CSV could not be read: %v", err)
	}
	if len(recs) < 2 {
		return nil, errors.New("the CSV has a header row — payee_id,amount,currency,reference — and then a row per payment")
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if i, ok := col["payee"]; ok {
		col["payee_id"] = i
	}
	for _, want := range []string{"payee_id", "amount", "currency"} {
		if _, ok := col[want]; !ok {
			return nil, fmt.Errorf("the CSV's header names payee_id, amount, currency and reference; %q is missing", want)
		}
	}
	field := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	rows := make([]economy.PayoutRow, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		row := economy.PayoutRow{PayeeID: field(rec, "payee_id"), Currency: strings.ToUpper(field(rec, "currency")), Reference: field(rec, "reference")}
		decimals, ok := economy.MoneyCurrencies[row.Currency]
		if !ok {
			decimals = 2
		}
		if row.AmountMinor, err = parseMinor(field(rec, "amount"), decimals); err != nil {
			row.Detail = err.Error()
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseMinor reads a decimal amount such as "12.34" as minor units with decimals places: 1234.
func parseMinor(s string, decimals int) (int64, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > decimals || whole == "" || strings.Trim(whole+frac, "0123456789") != "" {
		return 0, fmt.Errorf("amount %q is not a positive decimal amount with up to %d decimal places, such as 12.34", s, decimals)
	}
	n, err := strconv.ParseInt(whole+frac+strings.Repeat("0", decimals-len(frac)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q is more than a payment can be", s)
	}
	return n, nil
}

func writePayoutErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrPayoutBatch):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrPayoutBatchNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	default:
		writePaymentErr(w, err)
	}
}
