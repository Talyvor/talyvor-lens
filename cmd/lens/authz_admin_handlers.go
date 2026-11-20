package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
)

// authz_admin_handlers.go — the #153 admin gate. The six global-config WRITE
// routes mutate PROCESS-WIDE state (local-routing endpoints, the provider key
// pool, fallback chains) and were reachable by any authenticated tenant; they
// are now wrapped in requireAdmin. Extracted to package level so the gate is
// provable over HTTP (the established testability pattern).

// adminAuthenticator is the subset of *auth.Manager requireAdmin needs.
type adminAuthenticator interface {
	Authenticate(r *http.Request) (*auth.AuthContext, error)
}

// requireAdmin gates next so only the global admin key reaches it. It FAILS
// CLOSED: a missing, invalid, or non-admin credential (or a nil context) → 401.
// Admin is the AuthContext.IsAdmin carrier resolved by auth.Manager.Authenticate
// — the same source every authz fix has used since #147, never a header or a
// config-string compare.
func requireAdmin(am adminAuthenticator, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, err := am.Authenticate(r)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if refuseModerator(w, actx, err) {
			return
		}
		if err != nil || actx == nil || !actx.IsAdmin {
			writeJSONErr(w, http.StatusUnauthorized, "admin credentials required")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// isAdmin answers requireAdmin's question without writing a response: true only for the admin key, so
// /healthz can show the operator what /metrics does and the public a bare health answer (B37.11).
func isAdmin(am adminAuthenticator, r *http.Request) bool {
	actx, err := am.Authenticate(r)
	return err == nil && actx != nil && actx.IsAdmin && actx.AuthMethod != auth.MethodModeratorKey
}

// requireAdminOrOperatorRead gates a CROSS-TENANT ADMIN READ. Admin reaches it exactly as before;
// the narrow operator read credential (auth.ScopeOperatorRead, LENS_OPERATOR_READ_KEY) reaches it
// too, but ONLY with a read method.
//
// ⚠ THE METHOD CHECK IS THE POINT, NOT DECORATION. Eight admin routes are registered with
// `r.Handle`, which mounts EVERY verb, and exactly one of them gates its own method
// (internal/econflags 405s a non-GET; `MethodNotAllowed` appears in one non-test file in the whole
// repository). So "read vs write" cannot be answered per PATH for them — granting a path would
// grant POST, PUT and DELETE on it as well. Answering per (path, METHOD) is what makes the grant
// describable: whatever the router mounted, this credential can only GET.
//
// Admin is deliberately NOT method-restricted. The gate must not change what the global key can do
// on any route it already reached — a narrowing that breaks admin costs more than the gap it
// closes, which is why every refusal in the tests is paired with an admin control.
//
// FAILS CLOSED like requireAdmin: missing, invalid or nil ⇒ 401.
func requireAdminOrOperatorRead(am adminAuthenticator, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, err := am.Authenticate(r)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if refuseModerator(w, actx, err) {
			return
		}
		if err != nil || actx == nil {
			writeJSONErr(w, http.StatusUnauthorized, "admin credentials required")
			return
		}
		if actx.IsAdmin {
			next.ServeHTTP(w, r)
			return
		}
		// ⚠ HasScope short-circuits to true for admin, so this branch is only ever reached by a
		// non-admin — a workspace key carrying the "admin" scope is NOT admin and lands here, where
		// it does not carry operator_read and is refused.
		if !actx.HasScope(auth.ScopeOperatorRead) {
			writeJSONErr(w, http.StatusUnauthorized, "admin credentials required")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSONErr(w, http.StatusMethodNotAllowed,
				"this credential may only read: GET or HEAD")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// refuseModerator answers 403 to a valid moderator key (B20.13) on an admin route that is not the
// marketplace review queue, and reports whether it did. 403, not 401: the key is genuine, it just
// may not do this.
func refuseModerator(w http.ResponseWriter, actx *auth.AuthContext, err error) bool {
	if err != nil || actx == nil || actx.AuthMethod != auth.MethodModeratorKey {
		return false
	}
	writeJSONErr(w, http.StatusForbidden, "a moderator key may only use the marketplace review queue")
	return true
}

// refuseAuthUnavailable answers 503 with Retry-After when the credential's store could not be asked
// (B28.243), and reports whether it did. A database hiccup is not a wrong key: refusing it 401 or 403
// tells a caller whose key is fine to sign in again.
func refuseAuthUnavailable(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, auth.ErrAuthUnavailable) {
		return false
	}
	auth.WriteAuthUnavailable(w)
	return true
}

// moderatorUseRecorder is the slice of *moderatorkey.Store requireAdminOrModerator needs.
type moderatorUseRecorder interface {
	RecordUse(ctx context.Context, keyID int64, operator, method, path string) error
}

// moderatorOperatorHeader names the person the web app is acting for; every moderator-key use records it.
const moderatorOperatorHeader = "X-Talyvor-Operator"

// requireAdminOrModerator gates the marketplace review queue (B20.13): the global admin key as before,
// or a moderator key — which must name the operator it acts for, and whose every use is recorded in
// moderator_key_uses BEFORE the handler runs. If the record cannot be written nothing is done (503):
// an unrecorded moderation is exactly what the audit log exists to rule out.
//
// FAILS CLOSED like requireAdmin: missing, invalid, revoked or nil ⇒ 401.
func requireAdminOrModerator(am adminAuthenticator, uses moderatorUseRecorder, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, err := am.Authenticate(r)
		if refuseAuthUnavailable(w, err) {
			return
		}
		if err != nil || actx == nil {
			writeJSONErr(w, http.StatusUnauthorized, "admin credentials required")
			return
		}
		if actx.IsAdmin {
			next.ServeHTTP(w, r)
			return
		}
		if actx.AuthMethod != auth.MethodModeratorKey || !actx.HasScope(auth.ScopeMarketModeration) {
			writeJSONErr(w, http.StatusUnauthorized, "admin credentials required")
			return
		}
		operator := strings.TrimSpace(r.Header.Get(moderatorOperatorHeader))
		if operator == "" {
			writeJSONErr(w, http.StatusBadRequest, "a moderator key must name the operator it acts for in "+moderatorOperatorHeader)
			return
		}
		if err := uses.RecordUse(r.Context(), actx.ModeratorKeyID, operator, r.Method, r.URL.Path); err != nil {
			writeJSONErr(w, http.StatusServiceUnavailable, "could not record this use in the audit log, so nothing was done")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// poolKeyRemover is the slice of *keypool.Pool the delete handler needs.
type poolKeyRemover interface {
	Remove(keyID string) bool
}

// newPoolKeyDeleteHandler — DELETE /v1/api/keys/pool/{keyID}. Extracted so the
// admin gate's behavior on a real mutating handler is provable (the #153 wiring
// proof): without the gate a tenant evicts a shared provider key.
func newPoolKeyDeleteHandler(pool poolKeyRemover) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := chi.URLParam(req, "keyID")
		if !pool.Remove(id) {
			writeJSONErr(w, http.StatusNotFound, "key not found")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// patternAdder is the slice of *injection.Detector the pattern handler needs.
type patternAdder interface {
	AddPattern(pattern string) error
}

// newInjectionPatternAddHandler — POST /v1/api/injection/patterns. The seventh
// member of the #153 class (folded in): AddPattern mutates the single PROCESS-
// WIDE injection detector and accepts arbitrary regex (a ReDoS vector), so it
// is admin-only like the other global-config writes.
func newInjectionPatternAddHandler(adder patternAdder) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Pattern string `json:"pattern"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := adder.AddPattern(in.Pattern); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}
