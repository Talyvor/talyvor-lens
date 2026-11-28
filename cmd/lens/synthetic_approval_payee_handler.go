package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B62.12 — the testers change an approved payment's payee on a test workspace, so the nightly reaches B62.1's stop:
//
//	POST /v1/synthetic/workspaces/{wsID}/agents/approvals/{approvalID}/payee  {"payee_id"} → {"approval_id", "payee_id"}:
//	                                     the approved, unused approval of an agent's payment out (POST /v1/money/payments
//	                                     from the agent's account) bound to the same payment to another of the
//	                                     workspace's saved payees. What the owner was shown is kept, so the agent's
//	                                     payment there is stopped as not the one approved.
//
// 404 for a workspace that is not a test one, another workspace's approval, one that is not of a payment out, or a
// payee not the workspace's; 409 for an approval that is not approved and unused. Unlike the other routes in
// synthetic_due_handler.go it is not under testOnly, so a real workspace is 404 like anything else not there, never
// 403. It changes no payment and no decision.

// syntheticApprovals is *economy.DualTokenStore.
type syntheticApprovals interface {
	RepointTestPaymentApproval(ctx context.Context, workspaceID, approvalID, payeeID string) error
}

func (d syntheticDueDeps) approvalPayee(w http.ResponseWriter, r *http.Request) (int, string) {
	var in struct {
		PayeeID string `json:"payee_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.PayeeID == "" {
		writeJSONErr(w, http.StatusBadRequest, "payee_id must name another saved payee of the workspace")
		return 0, "error: bad request"
	}
	approval := chi.URLParam(r, "approvalID")
	err := d.approvals.RepointTestPaymentApproval(r.Context(), chi.URLParam(r, "wsID"), approval, in.PayeeID)
	switch {
	case errors.Is(err, economy.ErrApprovalNotFound):
		writeJSONErr(w, http.StatusNotFound, "no approval of an agent's payment out in this test workspace has that id")
		return 0, "refused: no such approval"
	case errors.Is(err, economy.ErrPayeeNotFound):
		writeJSONErr(w, http.StatusNotFound, "no saved payee of this workspace has that id")
		return 0, "refused: no such payee"
	case errors.Is(err, economy.ErrApprovalNotApproved):
		writeJSONErr(w, http.StatusConflict, "this approval is not approved and unused")
		return 0, "refused: not approved"
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return 0, "error: " + err.Error()
	}
	writeJSONOK(w, http.StatusOK, map[string]any{"approval_id": approval, "payee_id": in.PayeeID})
	return 1, "ok"
}
