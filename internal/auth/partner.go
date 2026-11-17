package auth

import (
	"errors"
	"net/http"
	"slices"
)

// B62.5 — the account partner's read credential reaches the partner's view and nothing else. The check lives here, in
// Authenticate, as the Connect token's does (connect.go): every handler, those behind AuthMiddleware and those that
// call Authenticate themselves, refuses the key everywhere else.

// PartnerRoutes are the only paths a partner key reaches, and only to read them.
var PartnerRoutes = []string{
	"/v1/partner/agent-activity",
	"/v1/partner/agent-activity/export",
	"/v1/partner/agent-activity/summary",
}

// ErrPartnerRoute is a partner key used anywhere but the partner's view, or to write: AuthMiddleware answers it 403.
var ErrPartnerRoute = errors.New("auth: a partner key may only read the partner's view of agent activity")

// partnerContext is the identity partner key keyID resolves to on r, or ErrPartnerRoute when r is not a read of one of
// PartnerRoutes.
func partnerContext(r *http.Request, keyID int64) (*AuthContext, error) {
	// An escaped path (RawPath set) is refused: chi routes on it, so it could name another route than Path does.
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.URL.RawPath != "" || !slices.Contains(PartnerRoutes, r.URL.Path) {
		return nil, ErrPartnerRoute
	}
	return &AuthContext{Scopes: []string{ScopePartnerRead}, AuthMethod: MethodPartnerKey, PartnerKeyID: keyID}, nil
}
