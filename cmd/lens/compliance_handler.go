package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/compliance"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/stepup"
)

// compliance_handler.go — B30.8: THE COMPLIANCE CASE FILE, FOR THE OPERATOR, BEHIND STEP-UP.
//
//	GET  /v1/admin/compliance/cases                      the cases, newest first: ?status=, ?kind=, ?workspace_id= (then
//	                                                     with the workspace's freeze)
//	GET  /v1/admin/compliance/cases/{caseID}             the case file: the case, the owner and agents involved, its
//	                                                     alerts and notes, the workspace's freeze and a timeline
//	POST /v1/admin/compliance/cases/{caseID}/notes       {"note"} adds a note
//	POST /v1/admin/compliance/cases/{caseID}/freeze      {"reason"} freezes the workspace's AMBER and RED capabilities
//	POST /v1/admin/compliance/cases/{caseID}/unfreeze    {"reason"} lifts the freeze
//	POST /v1/admin/compliance/cases/{caseID}/close       {"reason"} closes an open monitoring case
//	POST /v1/admin/compliance/cases/{caseID}/export      the case file as a report draft, text/plain, for a person to file
//
// Each write answers the case file as it now stands, and each action — the export too — is an operator audit row
// naming the case. The two reads are the admin key's; every action, and screening's release and refuse, is behind
// requireStepUp: a fresh code for each.

// stepUpHeader carries the six-digit code from the operator's authenticator app.
const stepUpHeader = "X-Talyvor-Step-Up"

// stepUpVerifier is *stepup.Verifier.
type stepUpVerifier interface {
	Verify(ctx context.Context, operator, code string) error
}

// requireStepUp gates an operator's compliance action: the global admin key, the operator named in
// X-Talyvor-Operator — the audit row's who — and a current step-up code in X-Talyvor-Step-Up, which nobody has used
// yet: one code, one action. A valid
// admin key without a good code is 403, so the web app knows to ask for one; no step-up secret set is 503.
//
// FAILS CLOSED like requireAdmin: missing, invalid or nil ⇒ 401; a moderator key or the operator read key ⇒ refused.
func requireStepUp(am adminAuthenticator, v stepUpVerifier, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, err := am.Authenticate(r)
		if refuseModerator(w, actx, err) {
			return
		}
		if err != nil || actx == nil || !actx.IsAdmin {
			writeJSONErr(w, http.StatusUnauthorized, "admin credentials required")
			return
		}
		operator := strings.TrimSpace(r.Header.Get(moderatorOperatorHeader))
		if operator == "" {
			writeJSONErr(w, http.StatusBadRequest, "a compliance action names the operator who takes it in "+moderatorOperatorHeader)
			return
		}
		switch err := v.Verify(r.Context(), operator, r.Header.Get(stepUpHeader)); {
		case errors.Is(err, stepup.ErrNotConfigured):
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
			return
		case err != nil:
			writeJSONErr(w, http.StatusForbidden, "step-up required: "+err.Error()+" (send the code from your authenticator app in "+stepUpHeader+")")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// complianceCaseFile is *compliance.Store.
type complianceCaseFile interface {
	Cases(ctx context.Context, f compliance.Filter, limit int) ([]screening.Case, error)
	Freeze(ctx context.Context, workspaceID string) (*economy.Freeze, error)
	File(ctx context.Context, id string) (compliance.File, error)
	AddNote(ctx context.Context, id, by, note string) error
	FreezeWorkspace(ctx context.Context, id, by, reason string) (economy.Freeze, error)
	UnfreezeWorkspace(ctx context.Context, id, by, reason string) error
	Close(ctx context.Context, id, by, reason string) (screening.Case, error)
	Export(ctx context.Context, id, by string) (string, error)
}

func writeComplianceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, screening.ErrCaseNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, compliance.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, compliance.ErrClosed), errors.Is(err, compliance.ErrNotClosable), errors.Is(err, compliance.ErrAlreadyFrozen),
		errors.Is(err, compliance.ErrNotFrozen):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

func newComplianceCasesHandler(cf complianceCaseFile) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		f := compliance.Filter{Status: strings.TrimSpace(q.Get("status")), Kind: strings.TrimSpace(q.Get("kind")),
			WorkspaceID: strings.TrimSpace(q.Get("workspace_id"))}
		switch f.Status {
		case "", screening.CaseBlocked, screening.CaseHeld, screening.CaseReleased, screening.CaseRefused, "open", compliance.CaseClosed:
		default:
			writeJSONErr(w, http.StatusBadRequest, "status is blocked, held, released, refused, open or closed")
			return
		}
		if f.Kind != "" && f.Kind != "screening" && f.Kind != "monitoring" {
			writeJSONErr(w, http.StatusBadRequest, "kind is screening or monitoring")
			return
		}
		cs, err := cf.Cases(req.Context(), f, 500)
		if err != nil {
			writeComplianceErr(w, err)
			return
		}
		out := map[string]any{"cases": cs}
		if f.WorkspaceID != "" {
			fz, err := cf.Freeze(req.Context(), f.WorkspaceID)
			if err != nil {
				writeComplianceErr(w, err)
				return
			}
			out["freeze"] = fz
		}
		writeJSONOK(w, http.StatusOK, out)
	})
}

func newComplianceCaseHandler(cf complianceCaseFile) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f, err := cf.File(req.Context(), chi.URLParam(req, "caseID"))
		if err != nil {
			writeComplianceErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, f)
	})
}

// newComplianceActionHandler runs one case action — note, freeze, unfreeze or close — on the body's text, as the
// operator requireStepUp let through, and answers the case file as it now stands.
func newComplianceActionHandler(cf complianceCaseFile, action string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Note   string `json:"note"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		ctx, id, by := req.Context(), chi.URLParam(req, "caseID"), strings.TrimSpace(req.Header.Get(moderatorOperatorHeader))
		var err error
		switch action {
		case compliance.ActionNote:
			err = cf.AddNote(ctx, id, by, in.Note)
		case compliance.ActionFreeze:
			_, err = cf.FreezeWorkspace(ctx, id, by, in.Reason)
		case compliance.ActionUnfreeze:
			err = cf.UnfreezeWorkspace(ctx, id, by, in.Reason)
		case compliance.ActionClose:
			_, err = cf.Close(ctx, id, by, in.Reason)
		}
		if err != nil {
			writeComplianceErr(w, err)
			return
		}
		f, err := cf.File(ctx, id)
		if err != nil {
			writeComplianceErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, f)
	})
}

func newComplianceExportHandler(cf complianceCaseFile) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := chi.URLParam(req, "caseID")
		text, err := cf.Export(req.Context(), id, strings.TrimSpace(req.Header.Get(moderatorOperatorHeader)))
		if err != nil {
			writeComplianceErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="compliance-case-`+safeFilename(id)+`-draft.txt"`)
		_, _ = w.Write([]byte(text)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- text/plain attachment; SecurityHeadersMiddleware sets nosniff on every response
	})
}

// safeFilename keeps a case id's letters, digits, '-' and '_' for a download's name.
func safeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return -1
	}, s)
}
