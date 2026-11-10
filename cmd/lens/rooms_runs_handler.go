package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/workspace"
)

// B32.33 — runs in a room (internal/rooms/runs.go):
//
//	POST /v1/rooms/{roomID}/runs  {target, version, input, variables, model, pay, max_price_usd_micros}   a member that
//	                              is not a viewer runs one of the room's contributions, or any listing it may use
//	POST /v1/rooms/{roomID}/ask   {question, model, pay}   a member asks the room's AI, which reads the room's latest
//	                              LENS_ROOM_CONTEXT_MESSAGES messages (default 30) with the question
//
// pay is "room" or "self". Paying room — the room's owner, or under members_with_spend a member given may_spend — the
// owner is the buyer and the room's wallet the agent: a listing's price is judged against the wallet's rules and goes on
// the owner's marketplace bill with the room and the member recorded, and every model call is made with the wallet's
// key, so it is judged and spent on the wallet. Paying self, the member's own workspace is the buyer and its own
// credential calls the models. Either way the room gets a run message naming the use, the charge and who paid; 200 with
// the use or the answer and that message. A run the wallet's rules refuse is 403 naming the rule and writes nothing; one
// above its approval amount is 403 with the approval the owner is asked for.

// roomWalletProxy serves a room wallet's model calls in process: the proxy's own routes, as the wallet's key, which
// the request's context carries — the identity AuthMiddleware stamps for a workspace key, which the wallet's would be
// were its plaintext kept. It trusts the context it is given, so it is never mounted on the router.
func roomWalletProxy(openai, anthropic http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			actx := auth.GetAuthContext(req.Context())
			if actx == nil || actx.AuthMethod != auth.MethodWorkspaceKey || actx.APIKeyID == "" || actx.WorkspaceID == "" {
				writeJSONErr(w, http.StatusUnauthorized, "a room wallet's model call carries the wallet's key")
				return
			}
			if !actx.HasScope(auth.ScopeProxy) {
				writeJSONErr(w, http.StatusForbidden, "forbidden: missing scope "+auth.ScopeProxy)
				return
			}
			req.Header.Set("X-Talyvor-Workspace", actx.WorkspaceID)
			ctx := auth.WithAPIKey(req.Context(), &auth.APIKey{ID: "global", WorkspaceID: actx.WorkspaceID, Name: actx.AuthMethod,
				Active: true, CreatedAt: time.Now().UTC()})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Post("/v1/proxy/openai/*", openai)
	r.Post("/v1/proxy/anthropic/*", anthropic)
	return r
}

// roomRunDeps answers what a run paid by each payer needs: a room wallet's model calls go through wallet, the in-process
// proxy (roomWalletProxy), as the wallet's key; anyone else's through lens with the credential req came in with.
func roomRunDeps(lens, wallet http.Handler, meter market.Meter, agents marketAgents, req *http.Request) rooms.DepsFor {
	return func(p rooms.Payer) market.UseDeps {
		m := meter
		if k, ok := meter.(stripeByKind); ok {
			m = k.meterFor(p.WorkspaceID) // B25.6: a test workspace's paid uses go on its Stripe test-mode bill
		}
		run := proxyRunner{lens: lens, from: req}
		if p.KeyID != "" {
			run = proxyRunner{lens: wallet, from: req, as: &auth.AuthContext{WorkspaceID: p.WorkspaceID, Scopes: p.KeyScopes,
				AuthMethod: auth.MethodWorkspaceKey, APIKeyID: p.KeyID}}
		}
		return market.UseDeps{Runner: run, Meter: m, Agents: agents}
	}
}

// mcpRoomDeps gives the MCP room tools (B32.36) what the routes below give a run, with the credential the tool call
// came in with: an agent paying itself runs on its own key.
type mcpRoomDeps struct {
	lens, wallet http.Handler
	meter        market.Meter
	agents       marketAgents
}

func (d mcpRoomDeps) RunDeps(ctx context.Context) rooms.DepsFor {
	return roomRunDeps(d.lens, d.wallet, d.meter, d.agents, mcp.CallerRequest(ctx))
}

// mountRoomRunRoutes mounts a room's runs: lens is the router a member's own model calls go through, wallet the
// in-process proxy a room wallet's go through (roomWalletProxy).
func mountRoomRunRoutes(r chi.Router, store *rooms.Store, lens, wallet http.Handler, meter market.Meter, agents marketAgents) {
	depsFor := func(req *http.Request) rooms.DepsFor { return roomRunDeps(lens, wallet, meter, agents, req) }
	actor := func(w http.ResponseWriter, req *http.Request) (ws, user string, ok bool) {
		ws, _ = auth.WorkspaceIdentity(req.Context())
		if ws == "" {
			writeJSONErr(w, http.StatusForbidden, "acting in a room needs a workspace's credential")
			return "", "", false
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			user = actx.UserID
		}
		return ws, user, true
	}
	answer := func(w http.ResponseWriter, roomID string, res rooms.RunResult, err error) {
		if err != nil {
			writeRoomRunErr(w, err)
			return
		}
		if res.MessageError != "" {
			slog.Warn("rooms: a run ran but the room did not get its message", "room", roomID, "err", res.MessageError)
		}
		if res.Use != nil && res.Use.MeterError != "" {
			slog.Warn("market: a room's use ran but is not yet on the buyer's bill; the next pass bills it",
				"use", res.Use.ID, "workspace", res.PayerWorkspaceID, "err", res.Use.MeterError)
		}
		writeJSONOK(w, http.StatusOK, res)
	}

	r.Post("/v1/rooms/{roomID}/runs", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.RunRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 256<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"target", "version", "input", "variables", "model", "pay"}: `+err.Error())
			return
		}
		roomID := chi.URLParam(req, "roomID")
		res, err := store.Run(req.Context(), depsFor(req), ws, user, roomID, in)
		answer(w, roomID, res, err)
	}))
	r.Post("/v1/rooms/{roomID}/ask", roomActorOnly(func(w http.ResponseWriter, req *http.Request) {
		ws, user, ok := actor(w, req)
		if !ok {
			return
		}
		var in rooms.AskRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"question", "model", "pay"}: `+err.Error())
			return
		}
		roomID := chi.URLParam(req, "roomID")
		res, err := store.Ask(req.Context(), depsFor(req), ws, user, roomID, in)
		answer(w, roomID, res, err)
	}))
}

// writeRoomRunErr answers a run's refusal: the wallet's rules and the marketplace's first, then the room's.
func writeRoomRunErr(w http.ResponseWriter, err error) {
	var need *economy.ApprovalNeededError
	var ran *runError
	var refusal *rooms.ScanRefusal
	switch {
	case errors.As(err, &need):
		writeJSONOK(w, http.StatusForbidden, map[string]any{"error": api.Refusal(err.Error()), "approval_id": need.ApprovalID})
	case errors.Is(err, economy.ErrAgentRule), errors.Is(err, workspace.ErrMoneyWall), errors.Is(err, rooms.ErrForbidden),
		errors.Is(err, market.ErrNotSoldHere):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case writeRoomPlanLimit(w, err):
	case errors.As(err, &refusal):
		writeJSONOK(w, http.StatusUnprocessableEntity, map[string]any{"error": api.Refusal(refusal.Reason), "scan": refusal.Scan})
	case errors.Is(err, market.ErrNotFound), errors.Is(err, rooms.ErrNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, market.ErrTakenDown):
		writeJSONErr(w, http.StatusGone, err.Error())
	case errors.Is(err, market.ErrNotSoldPerUse), errors.Is(err, market.ErrOverMaxPrice), errors.Is(err, rooms.ErrConflict):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, market.ErrInvalid), errors.Is(err, market.ErrNoModel), errors.Is(err, rooms.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, market.ErrNotRunnable):
		writeJSONErr(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, market.ErrNoBill):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &ran):
		// The payer's own limits pass through — no credit, rate limited, a model the wallet may not use, the wallet's
		// rules refusing a model call. A 401 is the provider's, and a bad gateway.
		status := http.StatusBadGateway
		if ran.status >= 400 && ran.status < 500 && ran.status != http.StatusUnauthorized {
			status = ran.status
		}
		writeJSONErr(w, status, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
