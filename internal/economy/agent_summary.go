package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// agent_summary.go — B57.12: everything one agent's page shows, read in one transaction at one moment.
//
// The page used to take eight reads (the book, rules, forecast, safe to spend, approvals, statement, card, currency
// accounts), each at its own moment, so its figures could disagree. Here every one is read from one REPEATABLE READ
// snapshot: the agent's balance less what waits for approval is its safe-to-spend figure, as both are read.

// AgentSummaryLines is how many of the statement's newest lines the summary carries.
const AgentSummaryLines = 3

// AgentSummary is one agent's page as of AsOf.
type AgentSummary struct {
	AsOf time.Time `json:"as_of"`
	// Agent is the agent as GET /agents lists it: its colour and face, balance, pause, owner and the rest.
	Agent Agent `json:"agent"`
	// MadeAt is when it was made and OwnerUserID who made it, which the apps turn into a name from the session.
	MadeAt      time.Time `json:"made_at"`
	OwnerUserID string    `json:"owner_user_id"`
	// AllPausedAt is set while every one of the workspace's agents is paused, this one with them.
	AllPausedAt *time.Time `json:"all_paused_at,omitempty"`
	// SafeToSpend is the agent's safe-to-spend figures (B41.105), LXC first and then by currency.
	SafeToSpend []SafeToSpendFigure `json:"safe_to_spend"`
	Month       AgentSummaryMonth   `json:"month"`
	// Approvals are its requests waiting for a person's approval, newest first: the ones its safe-to-spend takes off.
	Approvals []AgentApproval `json:"approvals"`
	// Statement is its statement's newest lines this month, grouped by model and day as B57.13 groups them.
	Statement         []StatementLine `json:"statement"`
	StatementTimeZone string          `json:"statement_time_zone"`
	// Rules is the version of its rules in force and when it was saved; null when its rules were never set.
	Rules            *AgentSummaryRules `json:"rules"`
	Card             AgentSummaryCard   `json:"card"`
	CurrencyAccounts []CurrencyAccount  `json:"currency_accounts"`
}

// AgentSummaryMonth is the agent's spend this month (UTC), where it is heading, and its monthly limit (0 is none).
type AgentSummaryMonth struct {
	Start            time.Time  `json:"start"`
	End              time.Time  `json:"end"`
	SpentULXC        int64      `json:"spent_ulxc"`
	ForecastULXC     int64      `json:"forecast_ulxc"`
	RunsOutAt        *time.Time `json:"runs_out_at"`
	RunsOutOn        *string    `json:"runs_out_on"`
	MonthlyLimitULXC int64      `json:"monthly_limit_ulxc"`
}

// AgentSummaryRules is the version of an agent's rules in force.
type AgentSummaryRules struct {
	Version int       `json:"version"`
	SavedAt time.Time `json:"saved_at"`
}

// AgentSummaryCard is the agent's card: none, active or frozen, and its last declined purchase.
type AgentSummaryCard struct {
	Status       string               `json:"status"` // none | active | frozen
	Currency     string               `json:"currency,omitempty"`
	Last4        string               `json:"last4,omitempty"`
	LastDeclined *AgentSummaryDecline `json:"last_declined"`
}

// AgentSummaryDecline is a declined card purchase, and why in words.
type AgentSummaryDecline struct {
	MerchantName string    `json:"merchant_name"`
	AmountMinor  int64     `json:"amount_minor"`
	Currency     string    `json:"currency"`
	Words        string    `json:"words"`  // "Declined: not one of its payees"
	Reason       string    `json:"reason"` // the reason as it was recorded
	At           time.Time `json:"at"`
}

// declineWords are a recorded decline reason's words, by what the reason says: the first that matches.
var declineWords = []struct{ says, words string }{
	{"may pay only the payees its rules name", "Declined: not one of its payees"},
	{"the agent may not pay ", "Declined: a payee its rules block"},
	{ErrAgentCardFrozen.Error(), "Declined: the card is frozen"},
	{"this purchase costs", "Declined: not enough in its balance"},
	{"limit per request", "Declined: above its limit per request"},
	{"the agent has spent ", "Declined: above its spending limit"},
	{"the agent has paid ", "Declined: above its limit for this payee"},
	{"the agent may spend only between", "Declined: outside its spending hours"},
	{"approved by the workspace's owner", "Declined: waiting for your approval"},
}

// DeclineWords is a recorded card decline reason in words: "Declined: " and what was wrong.
func DeclineWords(reason string) string {
	for _, d := range declineWords {
		if strings.Contains(reason, d.says) {
			return d.words
		}
	}
	reason = strings.TrimPrefix(reason, ErrAgentRule.Error()+": ")
	if reason == "" {
		return "Declined"
	}
	return "Declined: " + reason
}

// AgentSummary reads agentID's page in one snapshot; ErrAgentNotFound for an agent not in workspaceID.
func (s *DualTokenStore) AgentSummary(ctx context.Context, workspaceID, agentID string) (AgentSummary, error) {
	var out AgentSummary
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return out, err
	}
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&out.AsOf); err != nil {
		return out, fmt.Errorf("economy: agent summary: %w", err)
	}
	out.AsOf = out.AsOf.UTC()
	// The store's own reads, each on the snapshot: a read that begins its own transaction takes a savepoint in it,
	// and AgentBook's SET TRANSACTION names the level this one already has, which Postgres allows.
	snap := *s
	snap.pool = tx
	fail := func(err error) (AgentSummary, error) { return out, fmt.Errorf("economy: agent summary: %w", err) }

	book, err := snap.AgentBook(ctx, workspaceID)
	if err != nil {
		return out, err
	}
	found := false
	for _, a := range book.Agents {
		if a.ID == agentID {
			out.Agent, found = a, true
		}
	}
	if !found {
		return out, ErrAgentNotFound
	}
	out.MadeAt, out.OwnerUserID, out.AllPausedAt = out.Agent.CreatedAt, out.Agent.OwnerUserID, book.AllPausedAt

	rules, err := getAgentRules(ctx, tx, workspaceID, agentID)
	if err != nil {
		return out, err
	}
	loc, err := rules.location()
	if err != nil {
		return fail(err)
	}

	safe, err := snap.SafeToSpend(ctx, workspaceID, out.AsOf)
	if err != nil {
		return out, err
	}
	out.SafeToSpend = []SafeToSpendFigure{}
	for _, a := range safe.Agents {
		if a.AgentID == agentID {
			out.SafeToSpend = a.Figures
		}
	}

	forecast, err := snap.AgentSpendForecast(ctx, workspaceID, out.AsOf)
	if err != nil {
		return out, err
	}
	out.Month = AgentSummaryMonth{Start: forecast.MonthStart, End: forecast.MonthEnd, MonthlyLimitULXC: rules.MonthlyLimitULXC}
	for _, a := range forecast.Agents {
		if a.AgentID == agentID {
			out.Month.SpentULXC, out.Month.ForecastULXC, out.Month.RunsOutAt, out.Month.RunsOutOn = a.SpentULXC, a.ForecastULXC, a.RunsOutAt, a.RunsOutOn
		}
	}

	// What waits is what safe-to-spend takes off: the agent's requests still pending.
	rows, err := tx.Query(ctx, `SELECT `+agentApprovalColumns+` FROM agent_approvals
		WHERE workspace_id = $1 AND agent_id = $2 AND status = 'pending' ORDER BY created_at DESC, id`, workspaceID, agentID)
	if err != nil {
		return fail(err)
	}
	out.Approvals = []AgentApproval{}
	for rows.Next() {
		a, err := scanAgentApproval(rows)
		if err != nil {
			rows.Close()
			return fail(err)
		}
		out.Approvals = append(out.Approvals, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}

	st, err := snap.AgentPeriodStatement(ctx, workspaceID, agentID, forecast.MonthStart, out.AsOf)
	if err != nil {
		return out, err
	}
	if st, err = st.ByDay(loc, loc.String()).Page(AgentSummaryLines, ""); err != nil {
		return fail(err)
	}
	out.Statement, out.StatementTimeZone = st.Lines, st.TimeZone

	var version AgentSummaryRules
	err = tx.QueryRow(ctx, `SELECT version, created_at FROM agent_rules_versions WHERE agent_id = $1 ORDER BY version DESC LIMIT 1`,
		agentID).Scan(&version.Version, &version.SavedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fail(err)
	default:
		out.Rules = &version
	}

	out.Card = AgentSummaryCard{Status: "none"}
	var frozen bool
	err = tx.QueryRow(ctx, `SELECT currency, last4, frozen FROM agent_cards WHERE agent_id = $1`, agentID).
		Scan(&out.Card.Currency, &out.Card.Last4, &frozen)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fail(err)
	case frozen:
		out.Card.Status = "frozen"
	default:
		out.Card.Status = "active"
	}
	var d AgentSummaryDecline
	err = tx.QueryRow(ctx, `SELECT merchant_name, amount_minor, currency, reason, created_at FROM agent_card_authorizations
		WHERE workspace_id = $1 AND agent_id = $2 AND NOT approved ORDER BY created_at DESC, id LIMIT 1`, workspaceID, agentID).
		Scan(&d.MerchantName, &d.AmountMinor, &d.Currency, &d.Reason, &d.At)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fail(err)
	default:
		d.Words = DeclineWords(d.Reason)
		out.Card.LastDeclined = &d
	}

	if out.CurrencyAccounts, err = snap.CurrencyAccounts(ctx, workspaceID, agentID); err != nil {
		return out, err
	}
	return out, nil
}
