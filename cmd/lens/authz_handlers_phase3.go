package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/session"
)

// authz_handlers_phase3.go — the #146 Phase-3 IDOR / node-keyed sub-class
// (principle B from the recon): load the object, compare its OWNER workspace to
// the authenticated caller, refuse on mismatch, admin bypass. Reads return 404
// on cross-tenant access (no existence oracle — identical to a genuine
// not-found); writes/deletes refuse the action.

// callerOwns reports whether the authenticated caller may act on an object owned
// by ownerWS: the global admin may act on anything; every other caller only on
// objects in its OWN workspace. Empty caller or empty ownerWS → deny (fail
// closed). Identity comes from the credential (auth.WorkspaceIdentity), never
// caller input.
func callerOwns(req *http.Request, ownerWS string) bool {
	caller, isAdmin := auth.WorkspaceIdentity(req.Context())
	if isAdmin {
		return true
	}
	return caller != "" && ownerWS != "" && caller == ownerWS
}

// ── IDOR reads: 404 on cross-tenant (don't confirm the id exists to a non-owner) ──

type sessionGetter interface {
	GetSession(id string) (*session.Session, bool)
}

func newSessionGetHandler(g sessionGetter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		sess, ok := g.GetSession(chi.URLParam(req, "sessionID"))
		if !ok || !callerOwns(req, sess.WorkspaceID) {
			writeJSONErr(w, http.StatusNotFound, "session not found")
			return
		}
		writeJSONOK(w, http.StatusOK, sess)
	}
}

type evalRunGetter interface {
	GetRun(ctx context.Context, runID string) (*eval.RunSummary, error)
}

func newEvalRunGetHandler(g evalRunGetter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		run, err := g.GetRun(req.Context(), chi.URLParam(req, "runID"))
		if err != nil || run == nil || !callerOwns(req, run.WorkspaceID) {
			writeJSONErr(w, http.StatusNotFound, "run not found")
			return
		}
		writeJSONOK(w, http.StatusOK, run)
	}
}

type challengeGetter interface {
	Get(ctx context.Context, id string) (*povi.Challenge, error)
}

func newChallengeGetHandler(g challengeGetter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ch, err := g.Get(req.Context(), chi.URLParam(req, "id"))
		if err != nil || ch == nil || !callerOwns(req, ch.WorkspaceID) {
			writeJSONErr(w, http.StatusNotFound, "challenge not found")
			return
		}
		writeJSONOK(w, http.StatusOK, ch)
	}
}

type batchJobGetter interface {
	GetJobByRequestID(requestID string) *batch.BatchJob
}

func newBatchStatusHandler(g batchJobGetter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		job := g.GetJobByRequestID(chi.URLParam(req, "requestID"))
		if job == nil || !callerOwns(req, job.WorkspaceID) {
			writeJSONErr(w, http.StatusNotFound, "batch job not found")
			return
		}
		writeJSONOK(w, http.StatusOK, job)
	}
}

// ── api-key revoke: anti-oracle SILENT no-op on cross-tenant/missing ──

type apiKeyRevoker interface {
	WorkspaceForKeyID(ctx context.Context, keyID string) (string, bool, error)
	Revoke(ctx context.Context, keyID string) error
}

// newRevokeAPIKeyHandler closes the revoke-any-key DoS. Revoke fires ONLY when
// the key exists AND the caller owns it (or is admin). A cross-tenant or
// nonexistent key is a silent no-op returning the same {ok:true} — refusing the
// action with NO existence oracle (a non-owner can't distinguish a missing key
// from another tenant's, and the pre-existing nonexistent→ok behavior is kept).
func newRevokeAPIKeyHandler(ks apiKeyRevoker) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		keyID := chi.URLParam(req, "keyID")
		ownerWS, found, err := ks.WorkspaceForKeyID(req.Context(), keyID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if found && callerOwns(req, ownerWS) {
			if rErr := ks.Revoke(req.Context(), keyID); rErr != nil {
				writeJSONErr(w, http.StatusInternalServerError, rErr.Error())
				return
			}
		}
		writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// ── node-keyed ops: 403 on cross-tenant (resolve node→workspace, compare) ──

type nodeWorkspaceResolver interface {
	WorkspaceForNode(ctx context.Context, nodeID string) (string, error)
}

// requireNodeOwnership resolves the node's operator workspace and enforces that
// the caller owns it (admin bypass). Writes the refusal + returns false when the
// node is unknown (same 400 surface as before) or owned by another tenant (403).
func requireNodeOwnership(w http.ResponseWriter, req *http.Request, r nodeWorkspaceResolver, nodeID string) bool {
	ownerWS, err := r.WorkspaceForNode(req.Context(), nodeID)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return false
	}
	if !callerOwns(req, ownerWS) {
		writeJSONErr(w, http.StatusForbidden, "forbidden: not your node")
		return false
	}
	return true
}

// ── PoVI receipts named in a request (B42.2): check the workspace first, then act ──

type receiptProcessor interface {
	Process(ctx context.Context, r povi.Receipt) (povi.ProcessResult, error)
}

// newPoviReceiptSubmitHandler — POST /v1/workspaces/{wsID}/povi/receipts. The
// middleware authorizes only the {wsID} path segment, so the receipt's own
// workspace_id and node_id must both be that workspace's; anything else is 404,
// the same answer as a node that does not exist, and nothing is recorded.
func newPoviReceiptSubmitHandler(p receiptProcessor, nodes nodeWorkspaceResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var rec povi.Receipt
		if err := json.NewDecoder(req.Body).Decode(&rec); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		wsID := chi.URLParam(req, "wsID")
		if rec.WorkspaceID != wsID {
			writeJSONErr(w, http.StatusNotFound, "node not found")
			return
		}
		if owner, err := nodes.WorkspaceForNode(req.Context(), rec.NodeID); err != nil || owner != wsID {
			writeJSONErr(w, http.StatusNotFound, "node not found")
			return
		}
		res, err := p.Process(req.Context(), rec)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, res)
	}
}

type receiptGetter interface {
	GetReceipt(ctx context.Context, requestID string) (*povi.StoredReceipt, error)
}

type receiptChallenger interface {
	Challenge(ctx context.Context, rec povi.StoredReceipt) (*povi.Challenge, error)
}

// newChallengeIssueHandler — POST /v1/povi/challenges/issue. Another
// workspace's receipt is 404, identical to a receipt that does not exist.
func newChallengeIssueHandler(store receiptGetter, c receiptChallenger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			RequestID string `json:"request_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		rec, err := store.GetReceipt(req.Context(), in.RequestID)
		if err != nil || rec == nil || !callerOwns(req, rec.WorkspaceID) {
			writeJSONErr(w, http.StatusNotFound, "receipt not found")
			return
		}
		ch, err := c.Challenge(req.Context(), *rec)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, ch)
	}
}
