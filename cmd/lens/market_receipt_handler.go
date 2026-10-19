package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B32.40 — Talyvor's VAT receipt for every paid marketplace bill (internal/market/receipts.go).
//
//	GET /v1/workspaces/{wsID}/marketplace/receipts         the workspace's receipts, newest first
//	GET /v1/workspaces/{wsID}/marketplace/receipts/{id}    one receipt: JSON; ?format=html (or Accept: text/html) the
//	                                                        page; ?format=pdf (or Accept: application/pdf) the document
//
// A receipt is issued when the bill is paid and never changes; until Talyvor's VAT number is set it reads "VAT
// registration pending" and is a Preview. Both take the workspace's owner or an admin, as its tax profile does: a
// receipt prints the buyer's legal name, address and VAT number.
type marketReceipts interface {
	Receipt(ctx context.Context, buyerWorkspaceID, id string) (market.Receipt, error)
	ReceiptsOf(ctx context.Context, buyerWorkspaceID string) ([]market.ReceiptSummary, error)
}

func mountMarketReceiptRoutes(r chi.Router, store marketReceipts) {
	ownerOnly := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
				writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may read its receipts")
				return
			}
			next(w, req)
		}
	}
	r.Get("/v1/workspaces/{wsID}/marketplace/receipts", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		list, err := store.ReceiptsOf(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"receipts": list})
	}))
	r.Get("/v1/workspaces/{wsID}/marketplace/receipts/{receiptID}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		rc, err := store.Receipt(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "receiptID"))
		switch {
		case errors.Is(err, market.ErrNoReceipt):
			writeJSONErr(w, http.StatusNotFound, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		format, accept := req.URL.Query().Get("format"), req.Header.Get("Accept")
		switch {
		case format == "pdf" || (format == "" && strings.Contains(accept, "application/pdf")):
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `inline; filename="talyvor-receipt-`+rc.Number+`.pdf"`)
			_, _ = w.Write(rc.PDF()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/pdf rendered by Lens
		case format == "html" || (format == "" && strings.Contains(accept, "text/html")):
			page, err := rc.HTML()
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:")
			_, _ = w.Write(page) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- Receipt.HTML uses html/template, and the page carries a default-src none CSP
		case format == "" || format == "json":
			writeJSONOK(w, http.StatusOK, rc)
		default:
			writeJSONErr(w, http.StatusBadRequest, "format must be json, html or pdf")
		}
	}))
}
