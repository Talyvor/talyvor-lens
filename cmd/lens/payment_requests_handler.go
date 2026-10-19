package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B40.3 — prepare, never pay: payment requests an assistant or an agent prepares for the person to approve
// (economy/payment_requests.go). Nothing here moves money.
//
//	POST /v1/money/payment-requests            Idempotency-Key: <key>   {"client", "items": [{"payee_id", "currency",
//	                                           "amount_minor", "reference"} | {"listing_id", "reference"}]} — the request
//	                                           with each item auto, needs_approval or blocked, its frozen payload and
//	                                           hash, and the approval links (201; the same key and body again: 200, the
//	                                           same request; the same key with another body: 409)
//	GET  /v1/money/payment-requests/{id}       — its status
//	POST /v1/money/payment-requests/{id}/cancel — cancelled
//
// The owner or an admin names the client ("chat", "voice", "mcp:<client id>"; their own key's by default); an agent's
// key is always judged as itself, "key:<its id>", and sees only its own requests. Mounted in the authed group.

type paymentRequestBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	PreparePaymentRequest(ctx context.Context, workspaceID, by, idempotencyKey string, in economy.PaymentRequestIn) (economy.PaymentRequest, bool, error)
	PaymentRequest(ctx context.Context, workspaceID, agentID, id string) (economy.PaymentRequest, error)
	CancelPaymentRequest(ctx context.Context, workspaceID, agentID, id, by string) (economy.PaymentRequest, error)
}

func mountPaymentRequestRoutes(r chi.Router, bank paymentRequestBank) {
	caller := func(w http.ResponseWriter, req *http.Request) (ws, agentID, keyID, by string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			keyID = actx.APIKeyID
		}
		if ws == "" {
			writeJSONErr(w, http.StatusBadRequest, "payment requests are a workspace's: use a workspace's key")
			return "", "", "", "", false
		}
		if who, owner := storedanswers.OwnerOrAdmin(req.Context()); owner {
			return ws, "", keyID, who, true
		}
		if keyID != "" {
			agentID, _, _ = bank.AgentOfKey(req.Context(), keyID)
		}
		if agentID == "" {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, an admin or an agent's own key may prepare payment requests")
			return "", "", "", "", false
		}
		return ws, agentID, keyID, "agent:" + agentID, true
	}
	refuse := func(w http.ResponseWriter, err error) bool {
		switch {
		case errors.Is(err, economy.ErrPaymentRequest):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrPaymentRequestConflict):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrPaymentRequestNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			return false
		}
		return true
	}
	r.Post("/v1/money/payment-requests", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, keyID, by, ok := caller(w, req)
		if !ok {
			return
		}
		raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "the body could not be read")
			return
		}
		if refuse(w, economy.RefuseAssistantPayeeDetails(raw)) {
			return
		}
		var in economy.PaymentRequestIn
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"client", "items": [{"payee_id", "currency", "amount_minor", "reference"} | {"listing_id", "reference"}]}`)
			return
		}
		if in.Client, err = economy.PaymentRequestClient(keyID, agentID, in.Client); refuse(w, err) {
			return
		}
		in.AgentID = agentID
		pr, again, err := bank.PreparePaymentRequest(req.Context(), ws, by, req.Header.Get("Idempotency-Key"), in)
		if refuse(w, err) {
			return
		}
		code := http.StatusCreated
		if again {
			code = http.StatusOK
		}
		writeJSONOK(w, code, pr)
	})
	r.Get("/v1/money/payment-requests/{id}", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, _, _, ok := caller(w, req)
		if !ok {
			return
		}
		pr, err := bank.PaymentRequest(req.Context(), ws, agentID, chi.URLParam(req, "id"))
		if refuse(w, err) {
			return
		}
		writeJSONOK(w, http.StatusOK, pr)
	})
	r.Post("/v1/money/payment-requests/{id}/cancel", func(w http.ResponseWriter, req *http.Request) {
		ws, agentID, _, by, ok := caller(w, req)
		if !ok {
			return
		}
		pr, err := bank.CancelPaymentRequest(req.Context(), ws, agentID, chi.URLParam(req, "id"), by)
		if refuse(w, err) {
			return
		}
		writeJSONOK(w, http.StatusOK, pr)
	})
}
