package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/documents"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B41.101 — bills the workspace receives (economy/money_bills.go).
//
//	POST /v1/money/bills?source=upload|photo&filename=  — the body is the invoice, its Content-Type its media type (PDF,
//	                          Word, HTML, text …); a photo is sent as its transcription (text/plain, source=photo). It is
//	                          read into a bill matched to a saved payee or marked new_payee: 201 with the bill, or 200 with
//	                          the bill already there when this invoice was added before
//	GET  /v1/money/bills      — the workspace's bills, due first, soonest due first
//	GET  /v1/money/bills/{id} — one bill
//	POST /v1/money/bills/pay  Idempotency-Key: <key>  {"client", "bill_ids": [...]} — B41.102: prepares paying those bills,
//	                          or every bill due when none is named, as one payment request: one card with the count, the
//	                          total and every payee, approved once with a passkey and each bill then paid under its own
//	                          key through /v1/money/payment-requests/{id}/approval and …/approve (B40.5). 201 with the
//	                          request; the same key again: 200, the same request
//
// The workspace is the key's; only its owner or an admin may use these. Mounted in the authed group. The invoice is
// read through conv — the isolated distill worker in production — never in this process.

type moneyBillBank interface {
	AddBill(ctx context.Context, workspaceID, source, filename string, document []byte, text string) (economy.Bill, bool, error)
	Bills(ctx context.Context, workspaceID string) ([]economy.Bill, error)
	BillByID(ctx context.Context, workspaceID, billID string) (economy.Bill, error)
	PrepareBillsPayment(ctx context.Context, workspaceID, by, idempotencyKey, client string, billIDs []string) (economy.PaymentRequest, bool, error)
}

func mountMoneyBillRoutes(r chi.Router, bank moneyBillBank, conv distill.IsolatedConverter) {
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		ws, _ := auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "bills are a workspace's: use a workspace's key")
			return "", false
		}
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may manage its bills")
			return "", false
		}
		return ws, true
	}
	r.Post("/v1/money/bills", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		source := req.URL.Query().Get("source")
		if source == "" {
			source = economy.BillSourceUpload
		}
		mediaType := strings.TrimSpace(strings.SplitN(req.Header.Get("Content-Type"), ";", 2)[0])
		format, ok := distill.FormatFromMediaType(mediaType)
		if !ok {
			writeJSONErr(w, http.StatusUnsupportedMediaType, fmt.Sprintf("Content-Type %q is not an invoice Talyvor can read: send a PDF, "+
				"Word, HTML or text document, or a photo's transcription as text/plain with source=photo", mediaType))
			return
		}
		document, err := io.ReadAll(http.MaxBytesReader(w, req.Body, documents.MaxBytes))
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeJSONErr(w, http.StatusRequestEntityTooLarge, "an invoice can be at most 25 MB")
			return
		case err != nil || len(document) == 0:
			writeJSONErr(w, http.StatusBadRequest, "the request body is the invoice, and it is empty or unreadable")
			return
		}
		res, err := conv.Convert(req.Context(), document, format)
		switch {
		case err != nil:
			writeJSONErr(w, http.StatusUnprocessableEntity, "could not read this invoice: "+err.Error())
			return
		case res.NeedsVision || strings.TrimSpace(res.Markdown) == "":
			writeJSONErr(w, http.StatusUnprocessableEntity, "this invoice has no text to read (a scan?): send a photo's transcription "+
				"as text/plain with source=photo")
			return
		}
		b, created, err := bank.AddBill(req.Context(), ws, source, req.URL.Query().Get("filename"), document, res.Markdown)
		switch {
		case errors.Is(err, economy.ErrBill):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		case created:
			writeJSONOK(w, http.StatusCreated, b)
		default:
			writeJSONOK(w, http.StatusOK, b)
		}
	})
	r.Get("/v1/money/bills", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		list, err := bank.Bills(req.Context(), ws)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"bills": list})
	})
	r.Get("/v1/money/bills/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		b, err := bank.BillByID(req.Context(), ws, chi.URLParam(req, "id"))
		switch {
		case errors.Is(err, economy.ErrBillNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, b)
		}
	})
	r.Post("/v1/money/bills/pay", func(w http.ResponseWriter, req *http.Request) {
		ws, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct {
			Client  string   `json:"client"`
			BillIDs []string `json:"bill_ids"`
		}
		dec := json.NewDecoder(io.LimitReader(req.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"client", "bill_ids": [...]}; leave bill_ids out to pay every bill due`)
			return
		}
		keyID := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			keyID = actx.APIKeyID
		}
		by, _ := storedanswers.OwnerOrAdmin(req.Context())
		client, err := economy.PaymentRequestClient(keyID, "", in.Client)
		var pr economy.PaymentRequest
		again := false
		if err == nil {
			pr, again, err = bank.PrepareBillsPayment(req.Context(), ws, by, req.Header.Get("Idempotency-Key"), client, in.BillIDs)
		}
		switch {
		case errors.Is(err, economy.ErrPaymentRequest):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrPaymentRequestConflict):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		case again:
			writeJSONOK(w, http.StatusOK, pr)
		default:
			writeJSONOK(w, http.StatusCreated, pr)
		}
	})
}
