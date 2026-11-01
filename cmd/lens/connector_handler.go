package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/connector"
)

// connector_handler.go — B41.178: ADD A LISTING TO CLAUDE, CHATGPT OR CURSOR IN ONE CLICK.
//
// The assistant is given https://<lens>/mcp/listings/{id}. Unauthenticated, that answers 401 naming its resource
// metadata; from there the assistant finds this OAuth 2.1 server, registers itself, and sends its user to
// /oauth/authorize, which opens a grant and redirects to the connect page on app.talyvor.com. The owner picks an agent
// and approves (…/connector-requests/{id}/approve); the assistant swaps the code, with its PKCE verifier, at
// /oauth/token for that agent's own key, and calls the listing's one tool with it.

// connectURLBase is the connect page where the owner decides a grant (talyvor-suite, B41.78).
var connectURLBase = "https://app.talyvor.com/connect"

type connectorGrants interface {
	Register(ctx context.Context, name string, redirectURIs []string) (connector.Client, error)
	CheckClient(ctx context.Context, clientID, redirectURI string) error
	Begin(ctx context.Context, clientID, redirectURI, codeChallenge, state, resource, listingID string) (string, error)
	Pending(ctx context.Context, id string) (connector.Request, error)
	Decide(ctx context.Context, id, workspaceID, agentID string, approve bool) (string, error)
	Redeem(ctx context.Context, code, clientID, redirectURI, verifier string) (connector.Grant, error)
	KeyIssued(ctx context.Context, grantID, keyID string) error
}

// listingResourcePrefix is where a listing's own MCP endpoint lives under Lens's public URL.
const listingResourcePrefix = "/mcp/listings/"

// mountConnectorPublicRoutes is the unauthenticated half: discovery, registration, authorize and token.
func mountConnectorPublicRoutes(r chi.Router, grants connectorGrants, bank agentBank, keys agentKeyIssuer, publicURL string) {
	publicURL = strings.TrimRight(publicURL, "/")
	resource := func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		writeJSONOK(w, http.StatusOK, map[string]any{
			"resource":                 publicURL + strings.TrimPrefix(req.URL.Path, "/.well-known/oauth-protected-resource"),
			"authorization_servers":    []string{publicURL},
			"bearer_methods_supported": []string{"header"},
			"resource_name":            "Talyvor",
		})
	}
	r.Get("/.well-known/oauth-protected-resource", resource)
	r.Get("/.well-known/oauth-protected-resource/*", resource)
	r.Get("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		writeJSONOK(w, http.StatusOK, map[string]any{
			"issuer":                                publicURL,
			"authorization_endpoint":                publicURL + "/oauth/authorize",
			"token_endpoint":                        publicURL + "/oauth/token",
			"registration_endpoint":                 publicURL + "/oauth/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})

	r.Post("/oauth/register", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			ClientName   string   `json:"client_name"`
			RedirectURIs []string `json:"redirect_uris"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be JSON client metadata")
			return
		}
		c, err := grants.Register(req.Context(), in.ClientName, in.RedirectURIs)
		switch {
		case errors.Is(err, connector.ErrRedirectRefused):
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
		case errors.Is(err, connector.ErrInvalidRequest):
			oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, map[string]any{"client_id": c.ID, "client_name": c.Name, "redirect_uris": c.RedirectURIs,
				"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code"}, "response_types": []string{"code"}})
		}
	})

	r.Get("/oauth/authorize", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		clientID, redirectURI := q.Get("client_id"), q.Get("redirect_uri")
		if err := grants.CheckClient(req.Context(), clientID, redirectURI); err != nil {
			// Never redirect to a URI the client did not register.
			status := http.StatusBadRequest
			if !errors.Is(err, connector.ErrInvalidClient) {
				status = http.StatusInternalServerError
			}
			writeJSONErr(w, status, err.Error())
			return
		}
		back := func(code, desc string) {
			u, _ := url.Parse(redirectURI)
			v := u.Query()
			v.Set("error", code)
			v.Set("error_description", desc)
			if s := q.Get("state"); s != "" {
				v.Set("state", s)
			}
			u.RawQuery = v.Encode()
			http.Redirect(w, req, u.String(), http.StatusFound)
		}
		res := q.Get("resource")
		listingID, isListing := strings.CutPrefix(res, publicURL+listingResourcePrefix)
		switch {
		case q.Get("response_type") != "code":
			back("unsupported_response_type", "only response_type=code")
		case q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43:
			back("invalid_request", "PKCE with code_challenge_method=S256 is required")
		case len(q.Get("state")) > 512:
			back("invalid_request", "state is too long")
		case res != "" && res != publicURL+"/mcp" && (!isListing || listingID == "" || strings.Contains(listingID, "/")):
			back("invalid_target", "resource must be "+publicURL+"/mcp or a listing under "+publicURL+listingResourcePrefix)
		default:
			id, err := grants.Begin(req.Context(), clientID, redirectURI, q.Get("code_challenge"), q.Get("state"), res, listingID)
			if err != nil {
				back("server_error", "the request could not be opened")
				return
			}
			http.Redirect(w, req, connectURLBase+"?request="+url.QueryEscape(id), http.StatusFound)
		}
	})

	r.Post("/oauth/token", func(w http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(w, req.Body, 16<<10)
		if err := req.ParseForm(); err != nil {
			oauthErr(w, http.StatusBadRequest, "invalid_request", "the body must be application/x-www-form-urlencoded")
			return
		}
		f := req.PostForm
		if f.Get("grant_type") != "authorization_code" {
			oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "only grant_type=authorization_code")
			return
		}
		g, err := grants.Redeem(req.Context(), f.Get("code"), f.Get("client_id"), f.Get("redirect_uri"), f.Get("code_verifier"))
		if errors.Is(err, connector.ErrInvalidGrant) {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", err.Error())
			return
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		raw, key, status, err := mintAgentKey(req.Context(), bank, keys, g.WorkspaceID, g.AgentID, g.ClientName+" connector")
		if err != nil {
			if status < http.StatusInternalServerError {
				oauthErr(w, http.StatusBadRequest, "invalid_grant", err.Error()) // the agent was archived since the owner approved
				return
			}
			writeJSONErr(w, status, err.Error())
			return
		}
		if err := grants.KeyIssued(req.Context(), g.ID, key.ID); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSONOK(w, http.StatusOK, map[string]any{"access_token": raw, "token_type": "Bearer"})
	})
}

// mountConnectorOwnerRoutes is what the connect page calls, as the signed-in owner.
func mountConnectorOwnerRoutes(r chi.Router, grants connectorGrants, bank agentBank) {
	const base = "/v1/workspaces/{wsID}/connector-requests/{requestID}"
	r.Get(base, func(w http.ResponseWriter, req *http.Request) {
		p, err := grants.Pending(req.Context(), chi.URLParam(req, "requestID"))
		writeConnectorAnswer(w, p, err)
	})
	r.Post(base+"/approve", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		var in struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.AgentID == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"agent_id": "<the agent the assistant will act as>"}`)
			return
		}
		if status, err := agentKeyRefusal(req.Context(), bank, wsID, in.AgentID); err != nil {
			writeJSONErr(w, status, err.Error())
			return
		}
		to, err := grants.Decide(req.Context(), chi.URLParam(req, "requestID"), wsID, in.AgentID, true)
		writeConnectorAnswer(w, map[string]string{"redirect_to": to}, err)
	}))
	r.Post(base+"/deny", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		to, err := grants.Decide(req.Context(), chi.URLParam(req, "requestID"), chi.URLParam(req, "wsID"), "", false)
		writeConnectorAnswer(w, map[string]string{"redirect_to": to}, err)
	}))
}

func writeConnectorAnswer(w http.ResponseWriter, body any, err error) {
	switch {
	case errors.Is(err, connector.ErrGrantNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSONOK(w, http.StatusOK, body)
	}
}

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSONOK(w, status, map[string]string{"error": code, "error_description": desc})
}

// listingChallenge tells an assistant without a key where to get one (RFC 9728): the 401 names the listing's
// resource metadata.
func listingChallenge(publicURL string) func(http.Handler) http.Handler {
	publicURL = strings.TrimRight(publicURL, "/")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(challengeWriter{w, `Bearer resource_metadata="` + publicURL + "/.well-known/oauth-protected-resource" + req.URL.Path + `"`}, req)
		})
	}
}

type challengeWriter struct {
	http.ResponseWriter
	challenge string
}

func (c challengeWriter) WriteHeader(status int) {
	if status == http.StatusUnauthorized {
		c.Header().Set("WWW-Authenticate", c.challenge)
	}
	c.ResponseWriter.WriteHeader(status)
}
