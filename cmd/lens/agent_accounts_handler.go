package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// B19.1 — AGENT ACCOUNTS: every agent has its own balance (internal/economy/agent_accounts.go).
//
//	GET  /v1/workspaces/{wsID}/agents                        the agents, their balances, reconciled with the workspace
//	POST /v1/workspaces/{wsID}/agents             {"name"}   create an agent
//	POST /v1/workspaces/{wsID}/agents/{id}/keys   {"name"}   issue the agent a proxy key of its own
//	POST /v1/workspaces/{wsID}/agents/{id}/fund     {"amount_ulxc"}   move the workspace's LXC to the agent
//	POST /v1/workspaces/{wsID}/agents/{id}/withdraw {"amount_ulxc"}   take it back
//
// B19.2 — each agent's spending rules, and the approvals its rules ask a person for:
//
//	GET  /v1/workspaces/{wsID}/agents/{id}/rules              the agent's rules
//	PUT  /v1/workspaces/{wsID}/agents/{id}/rules   {rules}    replace them (economy.AgentRules)
//	POST /v1/workspaces/{wsID}/agents/{id}/rules/simulate {"amount_ulxc", "model"?, "provider"?, "payee"?, "licence"?, "at"?}
//	                                                           B28.306: would the rules let it through? allowed,
//	                                                           refused or approval_required, and why; moves nothing
//	GET  /v1/workspaces/{wsID}/agents/{id}/rules/history      B28.307: every version of the rules, newest first,
//	                                                           with who changed them and how
//	POST /v1/workspaces/{wsID}/agents/{id}/rules/rollback {"version"}   put the rules back exactly as they were
//	                                                           then; the rollback is a new version
//	GET  /v1/workspaces/{wsID}/agents/{id}/rules/boosts       B28.308: the limits raised for now, and until when
//	POST /v1/workspaces/{wsID}/agents/{id}/rules/boosts {"rule", "value", "until"}   raise a limit until a time,
//	                                                           after which it is the rules' again by itself
//	DELETE /v1/workspaces/{wsID}/agents/{id}/rules/boosts/{rule}   end a boost before its time
//	GET  /v1/workspaces/{wsID}/agents/approvals                requests that needed approval, newest first;
//	                                                           a payment's names its payee and memo (B23.5)
//	POST /v1/workspaces/{wsID}/agents/approvals/{id}/approve   let that request through, once; with
//	                                                           {"always_allow": {"amount_ulxc", "per": "day"|"month"}} (B41.104)
//	                                                           also let the agent pay that payee up to it without approval
//	POST /v1/workspaces/{wsID}/agents/approvals/{id}/deny      refuse it
//
// B19.3 — one company's agents pay each other, inside the closed loop:
//
//	POST /v1/workspaces/{wsID}/agents/{id}/pay  {"to_agent_id", "amount_ulxc", "memo"}   by the paying agent's own key, or the owner
//	                                            (to another company's agent: through the marketplace, on the monthly bill — B19.15)
//	GET  /v1/workspaces/{wsID}/agents/{id}/statement?limit=   the agent's account, newest first (default and at most 1000)
//
// B19.5 — statements an enterprise can audit (internal/economy/agent_statements.go), for the period
// [from, to), as JSON or CSV:
//
//	GET  /v1/workspaces/{wsID}/agents/{id}/statement?from=&to=&format=json|csv   one agent's account
//	GET  /v1/workspaces/{wsID}/agents/statement?from=&to=&format=json|csv        every agent wallet in the workspace
//
// from and to are RFC 3339 times or YYYY-MM-DD dates (midnight UTC); from defaults to the start of the
// current month and to to now, so September is from=2026-09-01&to=2026-10-01. On an agent's route any of
// the three asks for the period statement; with none it is the newest-first list above.
//
// B19.6 — unusual spend is flagged, an agent can be paused, and the month-end is forecast
// (internal/economy/agent_alerts.go states the rule):
//
//	GET  /v1/workspaces/{wsID}/agents/alerts                 unusual-spend alerts, newest first
//	GET  /v1/workspaces/{wsID}/agents/forecast?at=           each agent's and the workspace's month-end spend
//	                                                          B50.24: an agent's own key gets only its own entry, here, on
//	                                                          …/alerts and on the book (GET …/agents)
//	GET  /v1/workspaces/{wsID}/agents/safe-to-spend?at=      B41.105: each agent's and the company's safe to spend
//	                                                          this month, per currency (internal/economy/safe_to_spend.go)
//	                                                          B50.23: an agent's own key gets only its own entry
//	GET  /v1/workspaces/{wsID}/agents/briefing?at=           B41.106: the owner's daily briefing — approvals waiting, what is
//	                                                          blocked, bills due this week, yesterday's spend (internal/economy/briefing.go)
//	POST /v1/workspaces/{wsID}/agents/{id}/pause  {"reason"}  refuse its every movement until resumed
//	POST /v1/workspaces/{wsID}/agents/{id}/resume
//
// B19.7 — one switch pauses every agent, effective on each one's next request, buffered or streamed:
//
//	POST /v1/workspaces/{wsID}/agents/pause-all  {"reason"}   every agent, those created later included
//	POST /v1/workspaces/{wsID}/agents/resume-all              lifts it; an agent paused on its own stays paused
//	POST /v1/workspaces/{wsID}/agents/{agentID}/resume/challenge, …/agents/resume-all/challenge
//	                                                           B41.108: once the workspace has a passkey or device key,
//	                                                           resume and resume-all take {"assertion"} over this challenge
//
// B19.8 — scheduled payments and automatic top-ups (internal/economy/agent_schedules.go), run every minute:
//
//	POST   /v1/workspaces/{wsID}/agents/{id}/schedules  {"to_agent_id", "amount_ulxc", "memo", "every", "first_run_at"}
//	       — or a standing order to a saved outside payee (B30.18): {"to_payee_id", "amount_minor", "currency", "end_at"}
//	GET    /v1/workspaces/{wsID}/agents/schedules                  the workspace's schedules
//	GET    /v1/workspaces/{wsID}/agents/schedules/{sid}/runs       each tick, paid, refused or skipped, newest first
//	POST   /v1/workspaces/{wsID}/agents/schedules/{sid}/skip       skip its next tick
//	DELETE /v1/workspaces/{wsID}/agents/schedules/{sid}            stop it
//	PUT    /v1/workspaces/{wsID}/agents/{id}/topup  {"below_ulxc", "to_ulxc"}   below one, back up to the other
//	GET    /v1/workspaces/{wsID}/agents/{id}/topup
//	DELETE /v1/workspaces/{wsID}/agents/{id}/topup
//
// B19.16 — approvals signed with a passkey, and a web push when one is filed (economy/agent_approval_auth.go).
// Once a workspace has a passkey, approve and deny take {"assertion": {credential_id, client_data_json,
// authenticator_data, signature}} over the approval's challenge; all four are base64url.
//
//	POST   /v1/workspaces/{wsID}/agents/passkeys/challenge           a registration challenge and the RP ID
//	POST   /v1/workspaces/{wsID}/agents/passkeys   {credential_id, name, public_key (SPKI), client_data_json, authenticator_data}
//	GET    /v1/workspaces/{wsID}/agents/passkeys
//	POST   /v1/workspaces/{wsID}/agents/approvals/{id}/challenge     the challenge an approval's decision is signed over
//	GET    /v1/workspaces/{wsID}/agents/push/public-key              the VAPID key a browser subscribes with
//	POST   /v1/workspaces/{wsID}/agents/push/subscriptions  {endpoint, keys: {p256dh, auth}}   a device to tell
//	DELETE /v1/workspaces/{wsID}/agents/push/subscriptions  {endpoint}
//
// B19.11 — every agent is owned by the person who created it (its record's owner_user_id; an admin names
// one). An agent with no owner — made before owners were recorded — cannot be funded, topped up or paid
// until a person claims it; each agent's "verified" follows its owner's workspace's verification.
//
//	POST /v1/workspaces/{wsID}/agents/{id}/claim             the signed-in person becomes its owner
//
// B28.29 — rule templates a new agent can start from (internal/economy/agent_rule_templates.go). Each names
// every rule, so applying one replaces the agent's rules whole and it then holds exactly the template:
//
//	GET  /v1/workspaces/{wsID}/agents/rule-templates                 support-bot, researcher, coder
//	POST /v1/workspaces/{wsID}/agents/{id}/rules/template {"template"}   apply one; returns the rules
//	POST /v1/workspaces/{wsID}/agents  {"name", "template"}          create an agent that starts from one
//
// B46.4 — every agent has its own USDC wallet on the money layer (internal/economy/agent_wallets.go):
//
//	GET  /v1/workspaces/{wsID}/agents/{id}/wallet   its balance and the address that pays into it; mode TEST and a
//	                                                notice while the sandbox made the address up
//
// B48.1 — an agent is fiat, crypto or both, and owns one wallet of each kind it is (agent_wallet_kinds.go):
//
//	POST /v1/workspaces/{wsID}/agents  {"name", "wallet_kind"?: "fiat"|"crypto"|"both", "fiat_currency"?: "GBP"|"EUR"|"USD"}
//	                                                     create an agent of a kind (default crypto; fiat wallet in EUR)
//	GET  /v1/workspaces/{wsID}/agents/{id}/wallets       its wallets, their balances and how to pay into each
//	PUT  /v1/workspaces/{wsID}/agents/{id}/wallet-kind  {"wallet_kind", "fiat_currency"?}   add or remove a kind:
//	                                                     409 while a wallet it would close holds money
//	GET  /v1/workspaces/{wsID}/agents/owner-wallets      the owner's wallet in each asset their agents use
//
// B28.298 — an agent's lifecycle (internal/economy/agent_lifecycle.go):
//
//	PATCH /v1/workspaces/{wsID}/agents/{id}   {"name"?, "description"?}   rename it, describe it; returns the agent
//	POST  /v1/workspaces/{wsID}/agents/{id}/archive   retire it: its whole balance back to the workspace in one
//	                                                  withdraw entry, its keys revoked, its top-up and schedules
//	                                                  stopped; it is listed with archived_at from then on
//
// Mounted in the authed group, so {wsID} is bound to the caller's credential. Moving money, creating
// agents and issuing keys take the workspace's owner or an admin; reading takes any of its credentials.

type agentBank interface {
	CreateAgentOfKind(ctx context.Context, workspaceID, name, ownerUserID, kind, fiat string) (economy.Agent, error)
	AgentWallets(ctx context.Context, workspaceID, agentID string) (economy.AgentWallets, error)
	SetAgentWalletKind(ctx context.Context, workspaceID, agentID, kind, fiat string) (economy.AgentWallets, error)
	OwnerWallets(ctx context.Context, workspaceID string) (economy.AgentWallets, error)
	UpdateAgent(ctx context.Context, workspaceID, agentID string, name, description *string) (economy.Agent, error)
	ArchiveAgent(ctx context.Context, workspaceID, agentID string) (economy.AgentArchive, error)
	ClaimAgent(ctx context.Context, workspaceID, agentID, userID string) error
	AttachAgentKey(ctx context.Context, workspaceID, agentID, scopedKeyID string) error
	FundAgent(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error)
	WithdrawAgent(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error)
	AgentBook(ctx context.Context, workspaceID string) (economy.AgentBook, error)
	SetAgentRules(ctx context.Context, workspaceID, agentID string, r economy.AgentRules) (economy.AgentRules, error)
	GetAgentRules(ctx context.Context, workspaceID, agentID string) (economy.AgentRules, error)
	AgentWallet(ctx context.Context, workspaceID, agentID string) (economy.AgentWallet, error)
	SimulateAgentRules(ctx context.Context, workspaceID, agentID string, in economy.SimulatedRequest) (economy.RuleSimulation, error)
	AgentRulesHistory(ctx context.Context, workspaceID, agentID string) ([]economy.AgentRulesVersion, error)
	RollbackAgentRules(ctx context.Context, workspaceID, agentID string, version int) (economy.AgentRules, error)
	AgentBoosts(ctx context.Context, workspaceID, agentID string) ([]economy.AgentRuleBoost, error)
	BoostAgentRule(ctx context.Context, workspaceID, agentID string, b economy.AgentRuleBoost) (economy.AgentRuleBoost, error)
	EndAgentBoost(ctx context.Context, workspaceID, agentID, rule string) error
	ListAgentApprovals(ctx context.Context, workspaceID string) ([]economy.AgentApproval, error)
	DecideAgentApproval(ctx context.Context, workspaceID, approvalID string, approve bool) (economy.AgentApproval, error)
	DecideAgentApprovalSigned(ctx context.Context, workspaceID, approvalID string, approve bool, a passkey.Assertion) (economy.AgentApproval, error)
	AllowPayee(ctx context.Context, workspaceID, approvalID string, a economy.PayeeAllowance) (economy.AgentRules, error)
	PayAgent(ctx context.Context, workspaceID, fromAgentID, toAgentID string, amount int64, memo string) (economy.AgentPayment, error)
	AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error)
	AgentStatement(ctx context.Context, workspaceID, agentID string, limit int) ([]economy.AgentStatementLine, error)
	AgentPeriodStatement(ctx context.Context, workspaceID, agentID string, from, to time.Time) (economy.Statement, error)
	WorkspaceAgentStatement(ctx context.Context, workspaceID string, from, to time.Time) (economy.Statement, error)
	PauseAgent(ctx context.Context, workspaceID, agentID, reason string) error
	PauseAllAgents(ctx context.Context, workspaceID, reason string) error
	ResumeAllAgents(ctx context.Context, workspaceID string) error
	CreateAgentSchedule(ctx context.Context, workspaceID, fromAgentID, toAgentID string, amount int64, memo, every string, firstRunAt time.Time) (economy.AgentSchedule, error)
	CreateAgentListingSchedule(ctx context.Context, workspaceID, fromAgentID, listingID string, amount int64, memo, every string, firstRunAt time.Time) (economy.AgentSchedule, error)
	CreateStandingOrder(ctx context.Context, workspaceID, fromAgentID, payeeID string, amountMinor int64, currency, memo, every string, firstRunAt time.Time, endAt *time.Time) (economy.AgentSchedule, error)
	SkipAgentScheduleTick(ctx context.Context, workspaceID, scheduleID string) (economy.AgentSchedule, error)
	ListAgentSchedules(ctx context.Context, workspaceID string) ([]economy.AgentSchedule, error)
	ListAgentScheduleRuns(ctx context.Context, workspaceID, scheduleID string) ([]economy.AgentScheduleRun, error)
	CancelAgentSchedule(ctx context.Context, workspaceID, scheduleID string) error
	CreatePromptSchedule(ctx context.Context, workspaceID, agentID, prompt, provider, model, every string, firstRunAt time.Time) (economy.PromptSchedule, error)
	AgentKey(ctx context.Context, workspaceID, agentID string) (string, error)
	ListPromptSchedules(ctx context.Context, workspaceID string) ([]economy.PromptSchedule, error)
	StopPromptSchedule(ctx context.Context, workspaceID, scheduleID string) error
	SetAgentTopUp(ctx context.Context, workspaceID, agentID string, belowULXC, toULXC int64) (economy.AgentTopUp, error)
	GetAgentTopUp(ctx context.Context, workspaceID, agentID string) (economy.AgentTopUp, bool, error)
	RemoveAgentTopUp(ctx context.Context, workspaceID, agentID string) error
	AuthorizeApprovalDecision(ctx context.Context, workspaceID, approvalID string, a *passkey.Assertion) error
	PasskeyRegistrationChallenge(ctx context.Context, workspaceID string) (challenge, rpID string, err error)
	RegisterPasskey(ctx context.Context, workspaceID, name string, r passkey.Registration, a *passkey.Assertion) (economy.Passkey, error)
	ListPasskeys(ctx context.Context, workspaceID string) ([]economy.Passkey, error)
	ApprovalChallenge(ctx context.Context, workspaceID, approvalID string) (challenge string, credentialIDs []string, err error)
	PushPublicKey() (string, bool)
	SavePushSubscription(ctx context.Context, workspaceID, endpoint, p256dh, auth string) error
	DeletePushSubscription(ctx context.Context, workspaceID, endpoint string) error
	ResumeAgent(ctx context.Context, workspaceID, agentID string) error
	ResumeChallenge(ctx context.Context, workspaceID, agentID string) (challenge string, credentialIDs []string, err error)
	AuthorizeResume(ctx context.Context, workspaceID, agentID string, a *passkey.Assertion) error
	ListAgentSpendAlerts(ctx context.Context, workspaceID string) ([]economy.AgentSpendAlert, error)
	AgentSpendForecast(ctx context.Context, workspaceID string, at time.Time) (economy.SpendForecast, error)
	SafeToSpend(ctx context.Context, workspaceID string, at time.Time) (economy.SafeToSpend, error)
	DailyBriefing(ctx context.Context, workspaceID string, at time.Time) (economy.Briefing, error)
}

type agentKeyIssuer interface {
	CreateAPIKey(ctx context.Context, workspaceID, name string, scopes []string, expiresAt *time.Time) (string, *tenant.WorkspaceAPIKey, error)
}

func mountAgentAccountRoutes(r chi.Router, bank agentBank, keys agentKeyIssuer) {
	r.Get("/v1/workspaces/{wsID}/agents", func(w http.ResponseWriter, req *http.Request) {
		book, err := bank.AgentBook(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		own, err := keyAgent(req, bank)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		agents := []economy.Agent{}
		for _, a := range book.Agents {
			if own == "" || a.ID == own {
				agents = append(agents, a)
			}
		}
		book.Agents = agents
		if own != "" {
			book.WorkspaceBalanceULXC, book.AllocatedULXC, book.UnallocatedULXC, book.SpentULXC = 0, 0, 0, 0
		}
		writeJSONOK(w, http.StatusOK, book)
	})
	r.Post("/v1/workspaces/{wsID}/agents", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Name  string `json:"name"`
			Owner string `json:"owner_user_id"` // an admin credential names the person; anyone else owns what they create
			// B28.29: the rule template it starts from; the agent is created with exactly its rules.
			Template string `json:"template"`
			// B48.1: fiat, crypto (the default) or both, and the currency of its fiat wallet.
			WalletKind   string `json:"wallet_kind"`
			FiatCurrency string `json:"fiat_currency"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Name == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"name": "<agent name>"}`)
			return
		}
		tmpl, ok := economy.RuleTemplateByID(in.Template)
		if in.Template != "" && !ok {
			writeJSONErr(w, http.StatusBadRequest, unknownRuleTemplate(in.Template))
			return
		}
		owner := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			owner = actx.UserID
			if actx.IsAdmin && in.Owner != "" {
				owner = in.Owner
			}
		}
		if in.WalletKind == "" {
			in.WalletKind = economy.WalletKindCrypto
		}
		a, err := bank.CreateAgentOfKind(req.Context(), chi.URLParam(req, "wsID"), in.Name, owner, in.WalletKind, in.FiatCurrency)
		if errors.Is(err, economy.ErrWalletKind) {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, economy.ErrAgentOwnerless) {
			writeJSONErr(w, http.StatusBadRequest, "an agent needs an owner: create it signed in as the person who will own it")
			return
		}
		if writePlanRefusal(w, err) { // B32.12: past the agents the plan allows
			return
		}
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if in.Template == "" {
			writeJSONOK(w, http.StatusCreated, a)
			return
		}
		rules, err := bank.SetAgentRules(rulesChange(req, "template "+tmpl.ID), chi.URLParam(req, "wsID"), a.ID, tmpl.Rules)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, fmt.Sprintf("agent %s was created, but the %s template's rules were not saved: %v", a.ID, tmpl.ID, err))
			return
		}
		writeJSONOK(w, http.StatusCreated, struct {
			economy.Agent
			Template string             `json:"template"`
			Rules    economy.AgentRules `json:"rules"`
		}{a, tmpl.ID, rules})
	}))
	r.Get("/v1/workspaces/{wsID}/agents/rule-templates", func(w http.ResponseWriter, req *http.Request) {
		writeJSONOK(w, http.StatusOK, map[string]any{"templates": economy.RuleTemplates()})
	})
	r.Patch("/v1/workspaces/{wsID}/agents/{agentID}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		}
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || (in.Name == nil && in.Description == nil) {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"name": "<new name>", "description": "<what it is for>"}, either or both`)
			return
		}
		a, err := bank.UpdateAgent(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.Name, in.Description)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentDetails):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, a)
		}
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/archive", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		out, err := bank.ArchiveAgent(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentArchived), errors.Is(err, economy.ErrAgentPotsHeld):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/keys", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		wsID, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		var in struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		if in.Name == "" {
			in.Name = agentID
		}
		// The agent must exist here before a key is minted for it; AgentBook is the read that says so.
		book, err := bank.AgentBook(req.Context(), wsID)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		found, archived, room := false, false, false
		for _, a := range book.Agents {
			if a.ID == agentID {
				found, archived, room = true, a.ArchivedAt != nil, a.Kind == economy.AgentKindRoom
			}
		}
		if !found {
			writeJSONErr(w, http.StatusNotFound, economy.ErrAgentNotFound.Error())
			return
		}
		if archived {
			writeJSONErr(w, http.StatusConflict, economy.ErrAgentArchived.Error())
			return
		}
		if room { // B32.32: before a key is minted that it would refuse
			writeJSONErr(w, http.StatusConflict, economy.ErrRoomWalletKey.Error())
			return
		}
		raw, key, err := keys.CreateAPIKey(req.Context(), wsID, in.Name, []string{"proxy"}, nil)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := bank.AttachAgentKey(req.Context(), wsID, agentID, key.ID); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]any{
			"agent_id": agentID, "key": raw, "id": key.ID, "prefix": key.KeyPrefix,
			"warning": "Store this key securely. It will not be shown again.",
		})
	}))
	move := func(fn func(ctx context.Context, workspaceID, agentID string, amount int64) (int64, error)) http.HandlerFunc {
		return ownerOnly(func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				AmountULXC int64 `json:"amount_ulxc"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.AmountULXC <= 0 {
				writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_ulxc": <positive µLXC>}`)
				return
			}
			// B17.26: a move sent again with the same Idempotency-Key lands once.
			key := req.Header.Get("Idempotency-Key")
			if len(key) > 128 {
				writeJSONErr(w, http.StatusBadRequest, "the Idempotency-Key must be at most 128 characters")
				return
			}
			agentID := chi.URLParam(req, "agentID")
			bal, err := fn(economy.WithMoveKey(req.Context(), key), chi.URLParam(req, "wsID"), agentID, in.AmountULXC)
			switch {
			case errors.Is(err, economy.ErrAgentNotFound):
				writeJSONErr(w, http.StatusNotFound, err.Error())
			case errors.Is(err, economy.ErrAgentFunds), errors.Is(err, economy.ErrAgentOwnerless), errors.Is(err, economy.ErrAgentArchived),
				errors.Is(err, economy.ErrIdempotencyKeyReused):
				writeJSONErr(w, http.StatusConflict, err.Error())
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
			default:
				writeJSONOK(w, http.StatusOK, map[string]any{"agent_id": agentID, "balance_ulxc": bal})
			}
		})
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/fund", move(bank.FundAgent))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/withdraw", move(bank.WithdrawAgent))

	writeRules := func(w http.ResponseWriter, rules economy.AgentRules, err error) {
		switch {
		case writeRoomPlanLimit(w, err): // B32.32: a room wallet's monthly limit past its owner's plan
		case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrRulesVersionNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentRule):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, rules)
		}
	}
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/rules", func(w http.ResponseWriter, req *http.Request) {
		rules, err := bank.GetAgentRules(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		writeRules(w, rules, err)
	})
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/wallet", func(w http.ResponseWriter, req *http.Request) {
		wallet, err := bank.AgentWallet(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentWalletsOff):
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, money.ErrPartnerDown):
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, wallet)
		}
	})
	writeWallets := func(w http.ResponseWriter, out economy.AgentWallets, err error) {
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrWalletKind):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrWalletNotEmpty), errors.Is(err, economy.ErrAgentArchived):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrAgentWalletsOff):
			writeJSONErr(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, money.ErrPartnerDown):
			writeJSONErr(w, http.StatusBadGateway, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, out)
		}
	}
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/wallets", func(w http.ResponseWriter, req *http.Request) {
		out, err := bank.AgentWallets(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		writeWallets(w, out, err)
	})
	r.Put("/v1/workspaces/{wsID}/agents/{agentID}/wallet-kind", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			WalletKind   string `json:"wallet_kind"`
			FiatCurrency string `json:"fiat_currency"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.WalletKind == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"wallet_kind": "fiat"|"crypto"|"both", "fiat_currency"?: "GBP"|"EUR"|"USD"}`)
			return
		}
		out, err := bank.SetAgentWalletKind(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.WalletKind, in.FiatCurrency)
		writeWallets(w, out, err)
	}))
	r.Get("/v1/workspaces/{wsID}/agents/owner-wallets", func(w http.ResponseWriter, req *http.Request) {
		out, err := bank.OwnerWallets(req.Context(), chi.URLParam(req, "wsID"))
		writeWallets(w, out, err)
	})
	r.Put("/v1/workspaces/{wsID}/agents/{agentID}/rules", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in economy.AgentRules
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields() // a misspelt rule must not be silently no rule
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be the agent's rules: "+err.Error())
			return
		}
		rules, err := bank.SetAgentRules(rulesChange(req, "set"), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in)
		writeRules(w, rules, err)
	}))
	// B28.306: simulating moves nothing, so any of the workspace's credentials may ask, as any may read the rules.
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/rules/simulate", func(w http.ResponseWriter, req *http.Request) {
		var in economy.SimulatedRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"amount_ulxc", "model"?, "provider"?, "payee"?: {"kind", "id"}, "at"?}: `+err.Error())
			return
		}
		sim, err := bank.SimulateAgentRules(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrBadSimulation):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, sim)
		}
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/rules/template", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Template string `json:"template"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Template == "" {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"template": "<template id>"}`)
			return
		}
		tmpl, ok := economy.RuleTemplateByID(in.Template)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, unknownRuleTemplate(in.Template))
			return
		}
		rules, err := bank.SetAgentRules(rulesChange(req, "template "+tmpl.ID), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), tmpl.Rules)
		writeRules(w, rules, err)
	}))
	// B28.307: every change to an agent's rules is a version; any credential may read them, as any may read the rules.
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/rules/history", func(w http.ResponseWriter, req *http.Request) {
		versions, err := bank.AgentRulesHistory(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"versions": versions})
		}
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/rules/rollback", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Version int `json:"version"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Version <= 0 {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"version": <the version to roll back to>}`)
			return
		}
		rules, err := bank.RollbackAgentRules(rulesChange(req, ""), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.Version)
		writeRules(w, rules, err)
	}))
	// B28.308: a boost raises a limit until a time; the rules judge it at each request's time, so it reverts by itself.
	writeBoosts := func(w http.ResponseWriter, status int, body any, err error) {
		switch {
		case writeRoomPlanLimit(w, err): // B32.32: a room wallet's monthly limit past its owner's plan
		case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrBoostNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentRule):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		case body == nil:
			w.WriteHeader(status)
		default:
			writeJSONOK(w, status, body)
		}
	}
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/rules/boosts", func(w http.ResponseWriter, req *http.Request) {
		boosts, err := bank.AgentBoosts(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		writeBoosts(w, http.StatusOK, map[string]any{"boosts": boosts}, err)
	})
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/rules/boosts", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Rule  string    `json:"rule"`
			Value int64     `json:"value"`
			Until time.Time `json:"until"`
		}
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || in.Rule == "" || in.Until.IsZero() {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"rule": <the limit, e.g. daily_limit_ulxc>, "value": <raised to>, "until": <RFC 3339 time>}`)
			return
		}
		b, err := bank.BoostAgentRule(rulesChange(req, "boost"), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"),
			economy.AgentRuleBoost{Rule: in.Rule, Value: in.Value, Until: in.Until})
		writeBoosts(w, http.StatusCreated, b, err)
	}))
	r.Delete("/v1/workspaces/{wsID}/agents/{agentID}/rules/boosts/{rule}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		err := bank.EndAgentBoost(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), chi.URLParam(req, "rule"))
		writeBoosts(w, http.StatusNoContent, nil, err)
	}))
	r.Get("/v1/workspaces/{wsID}/agents/approvals", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListAgentApprovals(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"approvals": list})
	})
	decide := func(approve bool) http.HandlerFunc {
		return ownerOnly(func(w http.ResponseWriter, req *http.Request) {
			wsID, approvalID := chi.URLParam(req, "wsID"), chi.URLParam(req, "approvalID")
			// B19.16: once the workspace has a passkey, the decision carries an assertion over this approval's challenge.
			var in struct {
				Assertion   *passkey.Assertion      `json:"assertion"`
				AlwaysAllow *economy.PayeeAllowance `json:"always_allow"` // B41.104: approve, and make it a rule
			}
			_ = json.NewDecoder(req.Body).Decode(&in)
			if in.AlwaysAllow != nil && (!approve || in.AlwaysAllow.ULXC <= 0 || (in.AlwaysAllow.Per != "day" && in.AlwaysAllow.Per != "month")) {
				writeJSONErr(w, http.StatusBadRequest, `always_allow goes with approve, as {"amount_ulxc": <above 0>, "per": "day" | "month"}`)
				return
			}
			var a economy.AgentApproval
			var err error
			if in.Assertion != nil {
				// B45.28: checked and spent in the same transaction that keeps a signed approval as the owner's AP2 v0.2
				// closed Payment Mandate, so a mandate never carries a signature Lens did not verify.
				a, err = bank.DecideAgentApprovalSigned(req.Context(), wsID, approvalID, approve, *in.Assertion)
			} else if err = bank.AuthorizeApprovalDecision(req.Context(), wsID, approvalID, nil); err == nil {
				a, err = bank.DecideAgentApproval(req.Context(), wsID, approvalID, approve)
			}
			switch {
			case errors.Is(err, economy.ErrPasskeyRequired), errors.Is(err, passkey.ErrInvalid):
				writeJSONErr(w, http.StatusForbidden, err.Error())
			case errors.Is(err, economy.ErrApprovalNotFound):
				writeJSONErr(w, http.StatusNotFound, err.Error())
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
			case in.AlwaysAllow != nil:
				// After the decision, so a refused signature saves no rule; the approval stands either way.
				rules, err := bank.AllowPayee(rulesChange(req, "always allow"), wsID, approvalID, *in.AlwaysAllow)
				if errors.Is(err, economy.ErrAgentRule) {
					writeJSONErr(w, http.StatusConflict, "approved once, but not made a rule: "+err.Error())
					return
				}
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, "approved once, but not made a rule: "+err.Error())
					return
				}
				writeJSONOK(w, http.StatusOK, struct {
					economy.AgentApproval
					Rules economy.AgentRules `json:"rules"`
				}{a, rules})
			default:
				writeJSONOK(w, http.StatusOK, a)
			}
		})
	}
	r.Post("/v1/workspaces/{wsID}/agents/approvals/{approvalID}/approve", decide(true))
	r.Post("/v1/workspaces/{wsID}/agents/approvals/{approvalID}/deny", decide(false))

	r.Get("/v1/workspaces/{wsID}/agents/statement", func(w http.ResponseWriter, req *http.Request) {
		from, to, csv, err := statementPeriod(req, time.Now())
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		st, err := bank.WorkspaceAgentStatement(req.Context(), chi.URLParam(req, "wsID"), from, to)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeStatement(w, st, csv)
	})
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/statement", func(w http.ResponseWriter, req *http.Request) {
		if q := req.URL.Query(); q.Has("from") || q.Has("to") || q.Has("format") {
			from, to, csv, err := statementPeriod(req, time.Now())
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			st, err := bank.AgentPeriodStatement(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), from, to)
			switch {
			case errors.Is(err, economy.ErrAgentNotFound):
				writeJSONErr(w, http.StatusNotFound, err.Error())
			case err != nil:
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
			default:
				writeStatement(w, st, csv)
			}
			return
		}
		// B17.118: unasked, a read returns as many lines as it may. Every request writes a hold, a settle and its
		// fee, so 60 requests in a minute are 180 lines, and a page of 100 showed barely half of their holds.
		limit := 1000
		if q := req.URL.Query().Get("limit"); q != "" {
			n, err := strconv.Atoi(q)
			if err != nil || n < 1 || n > 1000 {
				writeJSONErr(w, http.StatusBadRequest, "limit must be between 1 and 1000")
				return
			}
			limit = n
		}
		lines, err := bank.AgentStatement(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), limit)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"agent_id": chi.URLParam(req, "agentID"), "lines": lines})
		}
	})
	r.Get("/v1/workspaces/{wsID}/agents/alerts", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListAgentSpendAlerts(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		own, err := keyAgent(req, bank)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if own != "" {
			mine := []economy.AgentSpendAlert{}
			for _, a := range list {
				if a.AgentID == own {
					mine = append(mine, a)
				}
			}
			list = mine
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"alerts": list, "rule": economy.UnusualSpendRule})
	})
	r.Get("/v1/workspaces/{wsID}/agents/forecast", func(w http.ResponseWriter, req *http.Request) {
		at := time.Now()
		if v := req.URL.Query().Get("at"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "at must be an RFC 3339 instant")
				return
			}
			at = t
		}
		f, err := bank.AgentSpendForecast(req.Context(), chi.URLParam(req, "wsID"), at)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		own, err := keyAgent(req, bank)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if own != "" {
			mine := []economy.AgentForecast{}
			for _, a := range f.Agents {
				if a.AgentID == own {
					mine = append(mine, a)
				}
			}
			f.Agents, f.SpentULXC, f.ForecastULXC = mine, 0, 0
		}
		writeJSONOK(w, http.StatusOK, f)
	})
	r.Get("/v1/workspaces/{wsID}/agents/safe-to-spend", func(w http.ResponseWriter, req *http.Request) {
		at := time.Now()
		if v := req.URL.Query().Get("at"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "at must be an RFC 3339 instant")
				return
			}
			at = t
		}
		out, err := bank.SafeToSpend(req.Context(), chi.URLParam(req, "wsID"), at)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// B50.23: an agent's own key reads only its own entry — never another agent's, the company's or its bills.
		if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
			if keyAgent, _, kerr := bank.AgentOfKey(req.Context(), actx.APIKeyID); kerr == nil {
				own := []economy.AgentSafeToSpend{}
				for _, a := range out.Agents {
					if a.AgentID == keyAgent {
						own = append(own, a)
					}
				}
				out.Company, out.Agents = []economy.SafeToSpendFigure{}, own
			} else if !errors.Is(kerr, economy.ErrAgentNotFound) {
				writeJSONErr(w, http.StatusInternalServerError, kerr.Error())
				return
			}
		}
		writeJSONOK(w, http.StatusOK, out)
	})
	// It names bills and who is paid, so it is the owner's or an admin's, as the bills are (money_bills_handler.go).
	r.Get("/v1/workspaces/{wsID}/agents/briefing", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		at := time.Now()
		if v := req.URL.Query().Get("at"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "at must be an RFC 3339 instant")
				return
			}
			at = t
		}
		b, err := bank.DailyBriefing(req.Context(), chi.URLParam(req, "wsID"), at)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, b)
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/pause", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		agentID := chi.URLParam(req, "agentID")
		writePaused(w, agentID, true, bank.PauseAgent(req.Context(), chi.URLParam(req, "wsID"), agentID, in.Reason))
	}))
	// B41.108: once the workspace has a passkey or device key, resuming carries {"assertion"} over its challenge.
	resumeChallenge := func(w http.ResponseWriter, req *http.Request, agentID string) {
		c, creds, err := bank.ResumeChallenge(req.Context(), chi.URLParam(req, "wsID"), agentID)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
		}
	}
	authorizeResume := func(w http.ResponseWriter, req *http.Request, agentID string) bool {
		var in struct {
			Assertion *passkey.Assertion `json:"assertion"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		err := bank.AuthorizeResume(req.Context(), chi.URLParam(req, "wsID"), agentID, in.Assertion)
		switch {
		case errors.Is(err, economy.ErrPasskeyRequired), errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		}
		return err == nil
	}
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/resume/challenge", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		resumeChallenge(w, req, chi.URLParam(req, "agentID"))
	}))
	r.Post("/v1/workspaces/{wsID}/agents/resume-all/challenge", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		resumeChallenge(w, req, "")
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/resume", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		agentID := chi.URLParam(req, "agentID")
		if !authorizeResume(w, req, agentID) {
			return
		}
		writePaused(w, agentID, false, bank.ResumeAgent(req.Context(), chi.URLParam(req, "wsID"), agentID))
	}))
	r.Post("/v1/workspaces/{wsID}/agents/pause-all", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		if err := bank.PauseAllAgents(req.Context(), chi.URLParam(req, "wsID"), in.Reason); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"all_paused": true})
	}))
	r.Post("/v1/workspaces/{wsID}/agents/resume-all", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		if !authorizeResume(w, req, "") {
			return
		}
		if err := bank.ResumeAllAgents(req.Context(), chi.URLParam(req, "wsID")); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"all_paused": false})
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/schedules", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		// B19.17: to_listing_id pays a marketplace listing instead of another agent — each tick one use, on the
		// company's monthly marketplace bill; amount_ulxc is then the most a tick pays (0: the price now).
		// B30.18: to_payee_id is a standing order to a saved outside payee, amount_minor of currency a tick until end_at.
		var in struct {
			ToAgentID   string     `json:"to_agent_id"`
			ToListingID string     `json:"to_listing_id"`
			ToPayeeID   string     `json:"to_payee_id"`
			AmountULXC  int64      `json:"amount_ulxc"`
			AmountMinor int64      `json:"amount_minor"`
			Currency    string     `json:"currency"`
			Memo        string     `json:"memo"`
			Every       string     `json:"every"`
			FirstRunAt  *time.Time `json:"first_run_at"`
			EndAt       *time.Time `json:"end_at"`
		}
		err := json.NewDecoder(req.Body).Decode(&in)
		payees := 0
		for _, id := range []string{in.ToAgentID, in.ToListingID, in.ToPayeeID} {
			if id != "" {
				payees++
			}
		}
		if err != nil || payees != 1 {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to_agent_id", "to_listing_id" or "to_payee_id" (with "amount_minor" and "currency"), "amount_ulxc", "memo", "every": "hour|day|week|month", "first_run_at": "<RFC 3339, default now>", "end_at"}`)
			return
		}
		first := time.Now()
		if in.FirstRunAt != nil {
			first = *in.FirstRunAt
		}
		wsID, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		var sc economy.AgentSchedule
		switch {
		case in.ToPayeeID != "":
			sc, err = bank.CreateStandingOrder(req.Context(), wsID, agentID, in.ToPayeeID, in.AmountMinor, in.Currency, in.Memo, in.Every, first, in.EndAt)
		case in.ToListingID != "":
			sc, err = bank.CreateAgentListingSchedule(req.Context(), wsID, agentID, in.ToListingID, in.AmountULXC, in.Memo, in.Every, first)
		default:
			sc, err = bank.CreateAgentSchedule(req.Context(), wsID, agentID, in.ToAgentID, in.AmountULXC, in.Memo, in.Every, first)
		}
		switch {
		case errors.Is(err, economy.ErrAgentNotFound), errors.Is(err, economy.ErrPayeeNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentRule), errors.Is(err, economy.ErrSameAgent), errors.Is(err, economy.ErrListingPayee):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrPayeeUnconfirmed):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, workspace.ErrMoneyWall):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, sc)
		}
	}))
	r.Get("/v1/workspaces/{wsID}/agents/schedules", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListAgentSchedules(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"schedules": list})
	})
	r.Get("/v1/workspaces/{wsID}/agents/schedules/{scheduleID}/runs", func(w http.ResponseWriter, req *http.Request) {
		runs, err := bank.ListAgentScheduleRuns(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "scheduleID"))
		switch {
		case errors.Is(err, economy.ErrScheduleNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"schedule_id": chi.URLParam(req, "scheduleID"), "runs": runs})
		}
	})
	r.Post("/v1/workspaces/{wsID}/agents/schedules/{scheduleID}/skip", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		sc, err := bank.SkipAgentScheduleTick(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "scheduleID"))
		switch {
		case errors.Is(err, economy.ErrScheduleNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, sc)
		}
	}))
	r.Delete("/v1/workspaces/{wsID}/agents/schedules/{scheduleID}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		err := bank.CancelAgentSchedule(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "scheduleID"))
		switch {
		case errors.Is(err, economy.ErrScheduleNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"schedule_id": chi.URLParam(req, "scheduleID"), "active": false})
		}
	}))
	// B28.125: a prompt scheduled from Chat, asked at its time as the agent's own key, so its wallet pays.
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/prompt-schedules", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Prompt     string    `json:"prompt"`
			Provider   string    `json:"provider"`
			Model      string    `json:"model"`
			Every      string    `json:"every"`
			FirstRunAt time.Time `json:"first_run_at"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"prompt", "provider", "model", "every": "once|day|week", "first_run_at": "<RFC 3339>"}`)
			return
		}
		sc, err := bank.CreatePromptSchedule(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.Prompt, in.Provider, in.Model,
			in.Every, in.FirstRunAt)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrBadPromptSchedule):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			// B17.232: an agent made on Agent Wallets has no key until one is issued; it is asked as a key Lens issues
			// it now and never shows, so the run is still the agent's own call, judged by its rules and paid by its wallet.
			if err := ensurePromptKey(req.Context(), bank, keys, chi.URLParam(req, "wsID"), sc.AgentID); err != nil {
				_ = bank.StopPromptSchedule(context.WithoutCancel(req.Context()), chi.URLParam(req, "wsID"), sc.ID)
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, sc)
		}
	}))
	r.Get("/v1/workspaces/{wsID}/agents/prompt-schedules", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListPromptSchedules(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"schedules": list})
	})
	r.Delete("/v1/workspaces/{wsID}/agents/prompt-schedules/{scheduleID}", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		err := bank.StopPromptSchedule(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "scheduleID"))
		switch {
		case errors.Is(err, economy.ErrPromptScheduleNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"id": chi.URLParam(req, "scheduleID"), "active": false})
		}
	}))
	r.Put("/v1/workspaces/{wsID}/agents/{agentID}/topup", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			BelowULXC int64 `json:"below_ulxc"`
			ToULXC    int64 `json:"to_ulxc"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"below_ulxc": <µLXC>, "to_ulxc": <µLXC above it>}`)
			return
		}
		t, err := bank.SetAgentTopUp(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), in.BelowULXC, in.ToULXC)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentRule):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, t)
		}
	}))
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/topup", func(w http.ResponseWriter, req *http.Request) {
		t, ok, err := bank.GetAgentTopUp(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"))
		switch {
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		case !ok:
			writeJSONErr(w, http.StatusNotFound, "the agent has no automatic top-up")
		default:
			writeJSONOK(w, http.StatusOK, t)
		}
	})
	r.Delete("/v1/workspaces/{wsID}/agents/{agentID}/topup", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		if err := bank.RemoveAgentTopUp(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"agent_id": chi.URLParam(req, "agentID"), "topup": nil})
	}))
	r.Post("/v1/workspaces/{wsID}/agents/passkeys/challenge", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		c, rpID, err := bank.PasskeyRegistrationChallenge(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// B50.20: once the workspace has a passkey, one of these signs c too, as the new key's "assertion".
		keys, err := bank.ListPasskeys(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		creds := []string{}
		for _, k := range keys {
			creds = append(creds, k.CredentialID)
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "rp_id": rpID, "alg": -7, "attestation": "none", "credential_ids": creds})
	}))
	r.Post("/v1/workspaces/{wsID}/agents/passkeys", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			CredentialID      string             `json:"credential_id"`
			Name              string             `json:"name"`
			PublicKey         string             `json:"public_key"`
			ClientDataJSON    string             `json:"client_data_json"`
			AuthenticatorData string             `json:"authenticator_data"`
			Assertion         *passkey.Assertion `json:"assertion"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		pk, err1 := passkey.Decode64(in.PublicKey)
		cdj, err2 := passkey.Decode64(in.ClientDataJSON)
		ad, err3 := passkey.Decode64(in.AuthenticatorData)
		if in.CredentialID == "" || err1 != nil || err2 != nil || err3 != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"credential_id", "name", "public_key", "client_data_json", "authenticator_data"}, base64url`)
			return
		}
		p, err := bank.RegisterPasskey(req.Context(), chi.URLParam(req, "wsID"), in.Name,
			passkey.Registration{CredentialID: in.CredentialID, PublicKey: pk, ClientDataJSON: cdj, AuthenticatorData: ad}, in.Assertion)
		switch {
		case errors.Is(err, economy.ErrPasskeyRequired):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, passkey.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, p)
		}
	}))
	r.Get("/v1/workspaces/{wsID}/agents/passkeys", func(w http.ResponseWriter, req *http.Request) {
		list, err := bank.ListPasskeys(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"passkeys": list})
	})
	r.Post("/v1/workspaces/{wsID}/agents/approvals/{approvalID}/challenge", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		c, creds, err := bank.ApprovalChallenge(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "approvalID"))
		switch {
		case errors.Is(err, economy.ErrApprovalNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"challenge": c, "allow_credentials": creds, "user_verification": "required"})
		}
	}))
	r.Get("/v1/workspaces/{wsID}/agents/push/public-key", func(w http.ResponseWriter, req *http.Request) {
		key, ok := bank.PushPublicKey()
		if !ok {
			writeJSONErr(w, http.StatusNotFound, economy.ErrPushNotConfigured.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"public_key": key})
	})
	r.Post("/v1/workspaces/{wsID}/agents/push/subscriptions", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		err := bank.SavePushSubscription(req.Context(), chi.URLParam(req, "wsID"), in.Endpoint, in.Keys.P256dh, in.Keys.Auth)
		switch {
		case errors.Is(err, economy.ErrPushNotConfigured):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentRule):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusCreated, map[string]any{"endpoint": in.Endpoint})
		}
	}))
	r.Delete("/v1/workspaces/{wsID}/agents/push/subscriptions", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Endpoint string `json:"endpoint"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		if err := bank.DeletePushSubscription(req.Context(), chi.URLParam(req, "wsID"), in.Endpoint); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"endpoint": in.Endpoint, "deleted": true})
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/claim", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		user := ""
		if actx := auth.GetAuthContext(req.Context()); actx != nil {
			user = actx.UserID
		}
		agentID := chi.URLParam(req, "agentID")
		err := bank.ClaimAgent(req.Context(), chi.URLParam(req, "wsID"), agentID, user)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, economy.ErrAgentOwnerless):
			writeJSONErr(w, http.StatusBadRequest, "claim an agent signed in as the person who will own it")
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, map[string]any{"agent_id": agentID, "owner_user_id": user})
		}
	}))
	r.Post("/v1/workspaces/{wsID}/agents/{agentID}/pay", func(w http.ResponseWriter, req *http.Request) {
		wsID, agentID := chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID")
		if _, owner := storedanswers.OwnerOrAdmin(req.Context()); !owner {
			keyAgent := ""
			if actx := auth.GetAuthContext(req.Context()); actx != nil && actx.APIKeyID != "" {
				keyAgent, _, _ = bank.AgentOfKey(req.Context(), actx.APIKeyID)
			}
			if keyAgent != agentID {
				writeJSONErr(w, http.StatusForbidden, "only the paying agent's own key, the workspace's owner or an admin may pay from an agent")
				return
			}
		}
		var in struct {
			ToAgentID  string `json:"to_agent_id"`
			AmountULXC int64  `json:"amount_ulxc"`
			Memo       string `json:"memo"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.ToAgentID == "" || in.AmountULXC <= 0 {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"to_agent_id": "<agent>", "amount_ulxc": <positive µLXC>, "memo": "<optional>"}`)
			return
		}
		// B50.1: a payment retried with the same Idempotency-Key lands once.
		key := req.Header.Get("Idempotency-Key")
		if len(key) > 128 {
			writeJSONErr(w, http.StatusBadRequest, "the Idempotency-Key must be at most 128 characters")
			return
		}
		pay, err := bank.PayAgent(economy.WithMoveKey(req.Context(), key), wsID, agentID, in.ToAgentID, in.AmountULXC, in.Memo)
		switch {
		case errors.Is(err, economy.ErrAgentNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, errNoCompanyBill): // B17.15
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case errors.Is(err, economy.ErrSameAgent):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, economy.ErrAgentFunds), errors.Is(err, economy.ErrAgentOwnerless), errors.Is(err, economy.ErrIdempotencyKeyReused):
			writeJSONErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, economy.ErrAgentRule), errors.Is(err, economy.ErrApprovalRequired), errors.Is(err, economy.ErrWashTrade),
			errors.Is(err, economy.ErrCapabilityNotCleared), errors.Is(err, workspace.ErrMoneyWall):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		case err != nil:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSONOK(w, http.StatusOK, pay)
		}
	})
}

func writePaused(w http.ResponseWriter, agentID string, paused bool, err error) {
	switch {
	case errors.Is(err, economy.ErrAgentNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSONOK(w, http.StatusOK, map[string]any{"agent_id": agentID, "paused": paused})
	}
}

// unknownRuleTemplate is the refusal of a template id Lens does not have; it names the ones it has.
func unknownRuleTemplate(id string) string {
	var ids []string
	for _, t := range economy.RuleTemplates() {
		ids = append(ids, t.ID)
	}
	return fmt.Sprintf("there is no rule template %q; the templates are %s", id, strings.Join(ids, ", "))
}

// keyAgent names the agent whose own key made req, or "" for an owner, a member or any other key (B50.24): an agent's
// key reads only its own entry on the workspace-wide agent routes.
func keyAgent(req *http.Request, bank agentBank) (string, error) {
	actx := auth.GetAuthContext(req.Context())
	if actx == nil || actx.APIKeyID == "" {
		return "", nil
	}
	id, _, err := bank.AgentOfKey(req.Context(), actx.APIKeyID)
	if errors.Is(err, economy.ErrAgentNotFound) {
		return "", nil
	}
	return id, err
}

// ownerOnly admits the workspace's owner or an admin — the rule stored-answer deletion uses.
// rulesChange is req's context naming who changes an agent's rules with it, and how (B28.307): the version the
// change makes records them.
func rulesChange(req *http.Request, what string) context.Context {
	who, _ := storedanswers.OwnerOrAdmin(req.Context())
	return economy.WithRulesChange(req.Context(), who, what)
}

func ownerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may manage its agents")
			return
		}
		next(w, req)
	}
}

// statementPeriod reads a statement's ?from=, ?to= and ?format=. Times are cut to the microsecond
// Postgres keeps, so the period a statement echoes is exactly the one it was built from.
func statementPeriod(req *http.Request, now time.Time) (from, to time.Time, csv bool, err error) {
	q := req.URL.Query()
	switch q.Get("format") {
	case "", "json":
	case "csv":
		csv = true
	default:
		return from, to, false, errors.New("format must be json or csv")
	}
	now = now.UTC()
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to = now.Truncate(time.Microsecond)
	for _, p := range []struct {
		name string
		into *time.Time
	}{{"from", &from}, {"to", &to}} {
		v := q.Get(p.name)
		if v == "" {
			continue
		}
		t, perr := time.Parse(time.RFC3339Nano, v)
		if perr != nil {
			if t, perr = time.Parse(time.DateOnly, v); perr != nil {
				return from, to, false, fmt.Errorf("%s must be an RFC 3339 time or a YYYY-MM-DD date", p.name)
			}
		}
		*p.into = t.UTC().Truncate(time.Microsecond)
	}
	if !from.Before(to) {
		return from, to, false, errors.New("from must be before to")
	}
	return from, to, csv, nil
}

// writeStatement answers with the statement as JSON, or as CSV: a header, each account's opening
// balance, every line, then each account's closing balance.
func writeStatement(w http.ResponseWriter, st economy.Statement, asCSV bool) {
	if !asCSV {
		writeJSONOK(w, http.StatusOK, st)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="agent-statement-%s-%s.csv"`,
		st.From.Format(time.DateOnly), st.To.Format(time.DateOnly)))
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	_ = cw.Write([]string{"posting_id", "entry_id", "at", "account", "kind", "amount_ulxc", "counterparty", "ref", "balance_after_ulxc"})
	for _, a := range st.Accounts {
		_ = cw.Write([]string{"", "", at(st.From), a.Account, "opening", "", "", "", i64(a.OpeningULXC)})
	}
	for _, l := range st.Lines {
		_ = cw.Write([]string{i64(l.PostingID), l.EntryID, at(l.At), l.Account, l.Kind, i64(l.AmountULXC), l.Counterparty, csvText(l.Ref), i64(l.BalanceAfterULXC)})
	}
	for _, a := range st.Accounts {
		_ = cw.Write([]string{"", "", at(st.To), a.Account, "closing", "", "", "", i64(a.ClosingULXC)})
	}
	cw.Flush()
}

// csvText keeps a memo a spreadsheet would run as a formula as text.
func csvText(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ensurePromptKey issues agentID a proxy key, its plaintext discarded, when it has no live key to ask a scheduled
// prompt with.
func ensurePromptKey(ctx context.Context, bank agentBank, keys agentKeyIssuer, wsID, agentID string) error {
	if key, err := bank.AgentKey(ctx, wsID, agentID); err != nil || key != "" {
		return err
	}
	_, key, err := keys.CreateAPIKey(ctx, wsID, "Scheduled prompts", []string{"proxy"}, nil)
	if err != nil {
		return fmt.Errorf("the agent's key for scheduled prompts: %w", err)
	}
	return bank.AttachAgentKey(ctx, wsID, agentID, key.ID)
}
