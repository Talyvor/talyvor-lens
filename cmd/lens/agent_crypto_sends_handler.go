package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/passkey"
)

// B48.9 — an agent sends USDC from its smart account to another Talyvor agent or to any address
// (economy/agent_crypto_sends.go).
//
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/quote       {to, network, token, amount_minor} — before sending
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/challenge   the same, with its Idempotency-Key — the challenge
//	                                                                     the owner signs their send over (B48.49)
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends             the same, with an Idempotency-Key and, from the
//	                                                                     owner, its signed assertion: sends it once
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends             what the agent sent and was sent
//	GET  /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{id}        one send, its receipt read again
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{id}/approve  {test_amount_minor?, assertion} — a send to a new address
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{id}/approve/challenge  {test_amount_minor?} — what that yes is signed over
//	POST /v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{id}/decline
//
// `to` is a 0x address, an ENS name (name.eth, read on the network's own registry), the name of an address the owner saved
// (B48.49), or a person on Talyvor's @name, email or phone number, which answers only the wallet they chose to receive
// into on that network (B48.32) and, for anyone else, the one answer for nobody found.
//
// B48.49: once the workspace has a passkey or the phone's key, the owner's own send is signed by one of them over its
// challenge, which names the address `to` resolved to, and so is their yes to a send the agent asked for to a new
// address, and a test amount sent there first; an agent's own key signs nothing, as its sends go through the owner's
// signed spend permission.
//
// Quoting, sending and reading take the agent's own key or the workspace's owner; only the owner, signed in, says yes
// or no to a send to a new address, and only their own send there goes without asking. Mounted in the authed group.

type agentCryptoSendBank interface {
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	QuoteAgentCryptoSend(ctx context.Context, workspaceID, agentID string, in economy.CryptoSendRequest) (economy.CryptoSendQuote, error)
	SendAgentCrypto(ctx context.Context, workspaceID, agentID, by string, byOwner bool, key string, in economy.CryptoSendRequest) (economy.CryptoSend, error)
	CryptoSendChallenge(ctx context.Context, workspaceID, agentID, key string, in economy.CryptoSendRequest) (string, []string, economy.CryptoSendQuote, error)
	SendAgentCryptoSigned(ctx context.Context, workspaceID, agentID, by, key string, in economy.CryptoSendRequest, a *passkey.Assertion) (economy.CryptoSend, error)
	AgentCryptoSends(ctx context.Context, workspaceID, agentID string) ([]economy.CryptoSend, error)
	AgentCryptoSend(ctx context.Context, workspaceID, agentID, id string) (economy.CryptoSend, error)
	AnswerAgentCryptoSend(ctx context.Context, workspaceID, agentID, id, by string, approve bool, testMinor int64, a *passkey.Assertion) (economy.CryptoSend, error)
	CryptoSendApprovalChallenge(ctx context.Context, workspaceID, agentID, id string, testMinor int64) (string, []string, error)
}

// signedInOwner is the workspace owner's signed-in session, as keyOwnerOnly admits it: never a key, admin-scoped or not.
func signedInOwner(req *http.Request) (string, bool) {
	actx := auth.GetAuthContext(req.Context())
	if actx == nil || actx.AuthMethod == auth.MethodWorkspaceKey || actx.AuthMethod == auth.MethodSessionKey || !actx.HasScope(auth.ScopeKeys) {
		return "", false
	}
	return "user:" + actx.UserID, true
}

func mountAgentCryptoSendRoutes(r chi.Router, bank agentCryptoSendBank) {
	// caller is who may move the agent's money: its owner, signed in, or the agent's own key.
	caller := func(w http.ResponseWriter, req *http.Request, agentID string) (by string, owner, ok bool) {
		if by, owner := signedInOwner(req); owner {
			return by, true, true
		}
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			if keyAgent, _, _ := bank.AgentOfKey(req.Context(), actx.APIKeyID); keyAgent == agentID {
				return "agent:" + agentID, false, true
			}
		}
		writeJSONErr(w, http.StatusForbidden, "only the agent's own key or the workspace's owner may send its money")
		return "", false, false
	}
	signedBody := func(w http.ResponseWriter, req *http.Request) (economy.CryptoSendRequest, *passkey.Assertion, bool) {
		var in struct {
			economy.CryptoSendRequest
			Assertion *passkey.Assertion `json:"assertion"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to": "0x…, name.eth, a saved name, @name or email", "network": "<network>", "token": "USDC", "amount_minor": <positive>}`)
			return in.CryptoSendRequest, nil, false
		}
		return in.CryptoSendRequest, in.Assertion, true
	}
	body := func(w http.ResponseWriter, req *http.Request) (economy.CryptoSendRequest, bool) {
		in, _, ok := signedBody(w, req)
		return in, ok
	}

	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/quote", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if _, _, ok := caller(w, req, agentID); !ok {
			return
		}
		in, ok := body(w, req)
		if !ok {
			return
		}
		q, err := bank.QuoteAgentCryptoSend(req.Context(), chi.URLParam(req, "wsID"), agentID, in)
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, q)
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/challenge", func(w http.ResponseWriter, req *http.Request) {
		if _, ok := signedInOwner(req); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, signed in, signs a send: an agent's key sends under its spend permission")
			return
		}
		in, ok := body(w, req)
		if !ok {
			return
		}
		c, creds, q, err := bank.CryptoSendChallenge(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"),
			req.Header.Get("Idempotency-Key"), in)
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required", "quote": q})
	})
	r.With(withTask).Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		by, owner, ok := caller(w, req, agentID)
		if !ok {
			return
		}
		in, a, ok := signedBody(w, req)
		if !ok {
			return
		}
		ws, key := chi.URLParam(req, "wsID"), req.Header.Get("Idempotency-Key")
		var c economy.CryptoSend
		var err error
		if owner { // B48.49: the owner's own send is signed; the agent's is held to its signed spend permission
			c, err = bank.SendAgentCryptoSigned(req.Context(), ws, agentID, by, key, in, a)
		} else {
			c, err = bank.SendAgentCrypto(req.Context(), ws, agentID, by, false, key, in)
		}
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeCryptoSend(w, c)
	})
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if _, _, ok := caller(w, req, agentID); !ok {
			return
		}
		list, err := bank.AgentCryptoSends(req.Context(), chi.URLParam(req, "wsID"), agentID)
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"sends": list})
	})
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{sendID}", func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if _, _, ok := caller(w, req, agentID); !ok {
			return
		}
		c, err := bank.AgentCryptoSend(req.Context(), chi.URLParam(req, "wsID"), agentID, chi.URLParam(req, "sendID"))
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeCryptoSend(w, c)
	})
	// answerBody is an answer's {test_amount_minor?, assertion?}; an empty body is a whole yes or no, unsigned.
	answerBody := func(w http.ResponseWriter, req *http.Request) (testMinor int64, a *passkey.Assertion, ok bool) {
		var in struct {
			TestAmountMinor int64              `json:"test_amount_minor"`
			Assertion       *passkey.Assertion `json:"assertion"`
		}
		if req.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil || in.TestAmountMinor < 0 {
				writeJSONErr(w, http.StatusBadRequest, `body is empty, or {"test_amount_minor": <positive, less than the send>} to send that much first, with the owner's "assertion"`)
				return 0, nil, false
			}
		}
		return in.TestAmountMinor, in.Assertion, true
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{sendID}/approve/challenge", func(w http.ResponseWriter, req *http.Request) {
		if _, ok := signedInOwner(req); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, signed in, answers a send to a new address")
			return
		}
		testMinor, _, ok := answerBody(w, req)
		if !ok {
			return
		}
		c, creds, err := bank.CryptoSendApprovalChallenge(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"),
			chi.URLParam(req, "sendID"), testMinor)
		if err != nil {
			writeCryptoSendErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
	})
	for _, answer := range []string{"approve", "decline"} {
		approve := answer == "approve"
		r.Post("/v1/workspaces/{wsID}/agents/{agentID}/crypto-sends/{sendID}/"+answer, func(w http.ResponseWriter, req *http.Request) {
			by, ok := signedInOwner(req)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, signed in, answers a send to a new address")
				return
			}
			testMinor, a, ok := answerBody(w, req)
			if !ok {
				return
			}
			c, err := bank.AnswerAgentCryptoSend(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"),
				chi.URLParam(req, "sendID"), by, approve, testMinor, a)
			if err != nil {
				writeCryptoSendErr(w, err)
				return
			}
			writeCryptoSend(w, c)
		})
	}
}

// writeCryptoSend answers a send with what its status says: done, on its way, refused or failed.
func writeCryptoSend(w http.ResponseWriter, c economy.CryptoSend) {
	code := http.StatusAccepted // waiting for the owner, or on its way
	switch c.Status {
	case "landed":
		code = http.StatusOK
	case "refused":
		code = http.StatusForbidden
	case "failed":
		code = http.StatusBadGateway
	}
	writeJSONOK(w, code, c)
}

func writeCryptoSendErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrPersonNotFound): // B48.32: nobody, and a person who has not said yes, answer the same
		writePhoneRefusal(w, http.StatusNotFound, "not_found", economy.PersonNotFoundWords)
	case errors.Is(err, economy.ErrPersonLookupLimit):
		writeJSONErr(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, economy.ErrPersonPhoneSandbox):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, economy.ErrCryptoSendsOff), errors.Is(err, economy.ErrSmartAccountsOff):
		writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, economy.ErrCryptoSendUnsigned), errors.Is(err, passkey.ErrInvalid):
		writeJSONErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, economy.ErrCryptoSendNotFound), errors.Is(err, economy.ErrCryptoNameNotFound),
		errors.Is(err, economy.ErrSmartAccountNotFound), errors.Is(err, economy.ErrAgentNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, economy.ErrCryptoSend), errors.Is(err, smartaccount.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrIdempotencyKeyReused):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, smartaccount.ErrRefused):
		writeJSONErr(w, http.StatusBadGateway, "the network did not take it: try again under the same Idempotency-Key")
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
