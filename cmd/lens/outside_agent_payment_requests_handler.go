package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/economy"
)

// B45.29 — getting paid by outside agents. An agent outside Talyvor asks a Talyvor agent's owner to pay it:
//
//	POST /v1/a2a/agents/{agentID}/payment-requests   Idempotency-Key: <key>
//	     {"items": [{"payee_id", "currency", "amount_minor", "reference"}]}
//
// signed under Talyvor's money profile (docs/agent-signatures.md), or not at all. It lands in the owner's approvals
// (GET /v1/money/payment-requests?status=pending_approval) as a payment request whose agent_trust says who asked —
// "Verified agent of <business>", "Signed by <domain> (business not checked)" or "Unverified agent" — and nothing is
// paid until the owner approves it with a passkey. The payee is one the owner saved; bank details are refused. The
// asker gets {"id", "status", "agent_trust"} back, never how the owner's rules judged it. Each signing domain's
// Idempotency-Keys are its own; unsigned agents share one space. Mounted in the public, rate-limited group.

type outsidePaymentBank interface {
	AgentCardFacts(ctx context.Context, agentID string) (economy.AgentCardFacts, error)
	PreparePaymentRequest(ctx context.Context, workspaceID, by, idempotencyKey string, in economy.PaymentRequestIn) (economy.PaymentRequest, bool, error)
}

func mountOutsideAgentPaymentRoutes(r chi.Router, v *agentsig.Verifier, bank outsidePaymentBank) {
	r.With(v.Middleware(agentsig.Money)).Post("/v1/a2a/agents/{agentID}/payment-requests", func(w http.ResponseWriter, req *http.Request) {
		trust, _ := agentsig.FromContext(req.Context())
		f, err := bank.AgentCardFacts(req.Context(), chi.URLParam(req, "agentID"))
		switch {
		case errors.Is(err, economy.ErrAgentNotFound) || (err == nil && f.Archived):
			writeJSONErr(w, http.StatusNotFound, "no such agent")
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not read the agent")
			return
		}
		key := strings.TrimSpace(req.Header.Get("Idempotency-Key"))
		if key == "" {
			writeJSONErr(w, http.StatusBadRequest, "an Idempotency-Key is required")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(req.Body, 64<<10))
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "the body could not be read")
			return
		}
		if err := economy.RefuseAssistantPayeeDetails(raw); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		var in struct {
			Items []economy.PaymentRequestItemIn `json:"items"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		const shape = `body must be {"items": [{"payee_id", "currency", "amount_minor", "reference"}]}`
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, shape)
			return
		}
		for _, it := range in.Items {
			if it.ListingID != "" || it.BillID != "" {
				writeJSONErr(w, http.StatusBadRequest, shape+": an outside agent is paid to a payee the owner saved")
				return
			}
		}
		asker := "a2a:unverified"
		if trust.Domain != "" {
			asker = "a2a:" + trust.Domain
		}
		trustJSON, _ := json.Marshal(trust)
		pr, again, err := bank.PreparePaymentRequest(req.Context(), f.WorkspaceID, asker, asker+":"+key,
			economy.PaymentRequestIn{Client: asker, Items: in.Items, AgentTrust: trustJSON})
		switch {
		case errors.Is(err, economy.ErrPaymentRequest):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		case errors.Is(err, economy.ErrPaymentRequestConflict):
			writeJSONErr(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, "could not record the payment request")
			return
		}
		code := http.StatusCreated
		if again {
			code = http.StatusOK
		}
		writeJSONOK(w, code, map[string]any{"id": pr.ID, "status": pr.Status, "agent_trust": pr.AgentTrust})
	})
}
