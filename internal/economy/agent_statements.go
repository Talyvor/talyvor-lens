package economy

import (
	"context"
	"fmt"
	"time"
)

// agent_statements.go — B19.5: STATEMENTS AN ENTERPRISE CAN AUDIT.
//
// A statement covers the half-open period [From, To) and is built from agent_postings alone: the
// append-only, double-entry record every agent movement is written to (0140). Nothing it reads can be
// changed or deleted, so asking for the same closed period again re-derives the same lines and totals,
// whatever was posted since. Each line carries its posting's id and entry id — the row of the audit log
// it came from — and a spend, hold, settle or release its request or reservation id, the same id its
// lxc_ledger row carries.
//
// A posting belongs to the period its created_at falls in (the time its transaction began), and lines
// are ordered by (created_at, id), so a line's balance is its account's opening balance plus every line
// before it. A period that has not ended yet is not closed: a movement recorded later can still fall
// inside it.

// StatementLine is one posting in a statement.
type StatementLine struct {
	PostingID        int64     `json:"posting_id"` // agent_postings.id
	EntryID          string    `json:"entry_id"`
	At               time.Time `json:"at"`
	Account          string    `json:"account"` // workspace | spend | agent:<id>
	Kind             string    `json:"kind"`    // fund | withdraw | spend | hold | settle | release | pay | card | platform_fee | lending_fee
	AmountULXC       int64     `json:"amount_ulxc"`
	Counterparty     string    `json:"counterparty"`
	Ref              string    `json:"ref,omitempty"`
	BalanceAfterULXC int64     `json:"balance_after_ulxc"`
	// Label names a fee line with its rate: "Platform fee 3%" (B32.11), "Lending fee 1%" (B56.22). Empty on every other kind.
	Label string `json:"label,omitempty"`
	// Memo is what the posting was for, in words: on a room's wallet, the room and the member who spent (B32.32).
	Memo string `json:"memo,omitempty"`
	// Model is the model a question's posting was for, as its caps know it (B28.301). Empty on every other posting.
	Model string `json:"model,omitempty"`
	// Day, Requests and Postings are set on a line of a statement read by day (B57.13): the day it covers in the
	// statement's time zone, how many questions it is, and how many postings it sums. PostingID, At and
	// BalanceAfterULXC are then its newest posting's.
	Day      string `json:"day,omitempty"`
	Requests int64  `json:"requests,omitempty"`
	Postings int    `json:"postings,omitempty"`
}

// StatementAccount is one account's totals over the period: Opening + In − Out = Closing.
type StatementAccount struct {
	Account     string `json:"account"`
	OpeningULXC int64  `json:"opening_ulxc"`
	InULXC      int64  `json:"in_ulxc"`
	OutULXC     int64  `json:"out_ulxc"`
	ClosingULXC int64  `json:"closing_ulxc"`
}

// Statement is an agent's account, or every agent wallet a workspace has, over [From, To).
type Statement struct {
	WorkspaceID string             `json:"workspace_id"`
	AgentID     string             `json:"agent_id,omitempty"`
	From        time.Time          `json:"from"`
	To          time.Time          `json:"to"`
	Accounts    []StatementAccount `json:"accounts"`
	Lines       []StatementLine    `json:"lines"`
	// Loans the agent or workspace lent or borrowed that were open in the period (B22.5): their terms, and the
	// events that fell in it. Their money is in Lines, as transfers.
	Loans []Loan `json:"loans"`
	// Escrows the agent or workspace paid in or was owed that were open in the period (B22.6): each with the
	// states it passed into in it. Their money is in Lines, on the payer's escrow:<id> account.
	Escrows []Escrow `json:"escrows"`
	// Pots is, on an agent's statement, each of its pots and what it held at the end of the period (B22.7).
	// Their movements are lines: pot_in and pot_out.
	Pots []Pot `json:"pots,omitempty"`
	// MarketplaceBill is, on an agent's statement, what it used on the marketplace and paid another company's agents in
	// the period (B56.14). Each is a line on the company's monthly marketplace bill, and moves no balance here.
	MarketplaceBill []MarketplaceBillLine `json:"marketplace_bill,omitempty"`
	// Group is "day" when the lines are read by day, TimeZone the zone those days are counted in (B57.13).
	Group    string `json:"group,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`
	// NextCursor reads the page after this one; empty on the last.
	NextCursor string `json:"next_cursor,omitempty"`
}

// OnMarketplaceBill is how a statement names a line billed to the company rather than taken from the agent's balance.
const OnMarketplaceBill = "on the marketplace bill"

// MarketplaceBillLine is one billed use or payment to another company's agent (market_uses).
type MarketplaceBillLine struct {
	UseID        string    `json:"use_id"`
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"` // pay, for a payment to another company's agent; else use, buy, rent, subscribe or renewal
	ListingID    string    `json:"listing_id,omitempty"`
	PayeeAgentID string    `json:"payee_agent_id,omitempty"`
	AmountULXC   int64     `json:"amount_ulxc"`
	Memo         string    `json:"memo,omitempty"`
	Label        string    `json:"label"` // OnMarketplaceBill
}

// AgentPeriodStatement is one agent's statement for [from, to).
func (s *DualTokenStore) AgentPeriodStatement(ctx context.Context, workspaceID, agentID string, from, to time.Time) (Statement, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return Statement{}, fmt.Errorf("economy: agent statement: %w", err)
	}
	if !exists {
		return Statement{}, ErrAgentNotFound
	}
	st, err := s.periodStatement(ctx, workspaceID, agentAccount(agentID), from, to)
	st.AgentID = agentID
	if err == nil && len(st.Accounts) == 0 { // nothing ever posted: the account still has a (zero) line
		st.Accounts = []StatementAccount{{Account: agentAccount(agentID)}}
	}
	if err == nil {
		st.Loans, err = s.statementLoans(ctx, workspaceID, agentID, from, to)
	}
	if err == nil {
		st.Escrows, err = s.statementEscrows(ctx, workspaceID, agentID, from, to)
	}
	if err == nil {
		st.Pots, err = s.pots(ctx, workspaceID, agentID, "", to)
	}
	if err == nil {
		st.MarketplaceBill, err = s.marketplaceBillLines(ctx, workspaceID, agentID, from, to)
	}
	return st, err
}

// marketplaceBillLines is what agentID put on its company's marketplace bill in [from, to): the billed uses that ran,
// and its payments to another company's agents, in the order they were made.
func (s *DualTokenStore) marketplaceBillLines(ctx context.Context, workspaceID, agentID string, from, to time.Time) ([]MarketplaceBillLine, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, used_at, CASE WHEN payee_agent_id <> '' THEN 'pay' ELSE use_kind END,
		listing_id, payee_agent_id, price_ulxc, memo
		FROM market_uses WHERE buyer_workspace_id = $1 AND agent_id = $2 AND charge = 'billed' AND ran_at IS NOT NULL
		AND used_at >= $3 AND used_at < $4 ORDER BY used_at, id`, workspaceID, agentID, from, to)
	if err != nil {
		return nil, fmt.Errorf("economy: marketplace bill lines: %w", err)
	}
	defer rows.Close()
	out := []MarketplaceBillLine{}
	for rows.Next() {
		l := MarketplaceBillLine{Label: OnMarketplaceBill}
		if err := rows.Scan(&l.UseID, &l.At, &l.Kind, &l.ListingID, &l.PayeeAgentID, &l.AmountULXC, &l.Memo); err != nil {
			return nil, fmt.Errorf("economy: marketplace bill lines: %w", err)
		}
		l.At = l.At.UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// WorkspaceAgentStatement is the statement of every agent wallet in a workspace — the
// workspace's side, each agent, and what they spent — for [from, to). Every entry sums to zero, so the
// accounts' movements over any period do too.
func (s *DualTokenStore) WorkspaceAgentStatement(ctx context.Context, workspaceID string, from, to time.Time) (Statement, error) {
	st, err := s.periodStatement(ctx, workspaceID, "", from, to)
	if err == nil {
		st.Loans, err = s.statementLoans(ctx, workspaceID, "", from, to)
	}
	if err == nil {
		st.Escrows, err = s.statementEscrows(ctx, workspaceID, "", from, to)
	}
	return st, err
}

// statementEscrows is the escrows workspaceID (its agent agentID, when not "") paid in or was owed that were
// open at some time in [from, to), each with the events that fell in it.
func (s *DualTokenStore) statementEscrows(ctx context.Context, workspaceID, agentID string, from, to time.Time) ([]Escrow, error) {
	list, err := s.escrows(ctx, `WHERE ((payer_workspace_id = $1 AND ($2 = '' OR payer_agent_id = $2))
		OR (payee_workspace_id = $1 AND ($2 = '' OR payee_agent_id = $2)))
		AND created_at < $4 AND (status IN ('held', 'disputed') OR decided_at >= $3)`, workspaceID, agentID, from, to)
	if err != nil {
		return nil, err
	}
	for i := range list {
		events := []EscrowEvent{}
		for _, e := range list[i].Events {
			if !e.At.Before(from) && e.At.Before(to) {
				events = append(events, e)
			}
		}
		list[i].Events = events
	}
	return list, nil
}

// statementLoans is the loans workspaceID (its agent agentID, when not "") lent or borrowed that were open at
// some time in [from, to), each with the events that fell in it.
func (s *DualTokenStore) statementLoans(ctx context.Context, workspaceID, agentID string, from, to time.Time) ([]Loan, error) {
	loans, err := s.loans(ctx, `WHERE ((lender_workspace_id = $1 AND ($2 = '' OR lender_agent_id = $2))
		OR (borrower_workspace_id = $1 AND ($2 = '' OR borrower_agent_id = $2)))
		AND offered_at < $4 AND (status IN ('offered', 'active', 'late') OR decided_at >= $3
		  OR EXISTS (SELECT 1 FROM agent_loan_events e WHERE e.loan_id = agent_loans.id AND e.at >= $3))`, workspaceID, agentID, from, to)
	if err != nil {
		return nil, err
	}
	out := []Loan{}
	for _, l := range loans {
		events := []LoanEvent{}
		for _, e := range l.Events {
			if !e.At.Before(from) && e.At.Before(to) {
				events = append(events, e)
			}
		}
		l.Events = events
		out = append(out, l)
	}
	return out, nil
}

// periodStatement reads [from, to) for one account, or for every account when account is "".
func (s *DualTokenStore) periodStatement(ctx context.Context, workspaceID, account string, from, to time.Time) (Statement, error) {
	st := Statement{WorkspaceID: workspaceID, From: from, To: to, Accounts: []StatementAccount{}, Lines: []StatementLine{}, Loans: []Loan{}, Escrows: []Escrow{}}
	rows, err := s.pool.Query(ctx, `
		SELECT account,
		       COALESCE(sum(amount_ulxc) FILTER (WHERE created_at < $3), 0)::bigint,
		       COALESCE(sum(amount_ulxc) FILTER (WHERE created_at >= $3 AND amount_ulxc > 0), 0)::bigint,
		       COALESCE(-sum(amount_ulxc) FILTER (WHERE created_at >= $3 AND amount_ulxc < 0), 0)::bigint
		  FROM agent_postings
		 WHERE workspace_id = $1 AND ($2 = '' OR account = $2) AND created_at < $4
		 GROUP BY account ORDER BY account`, workspaceID, account, from, to)
	if err != nil {
		return st, fmt.Errorf("economy: statement totals: %w", err)
	}
	opening := map[string]int64{}
	for rows.Next() {
		var a StatementAccount
		if err := rows.Scan(&a.Account, &a.OpeningULXC, &a.InULXC, &a.OutULXC); err != nil {
			rows.Close()
			return st, err
		}
		a.ClosingULXC = a.OpeningULXC + a.InULXC - a.OutULXC
		opening[a.Account] = a.OpeningULXC
		st.Accounts = append(st.Accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("economy: statement totals: %w", err)
	}

	rows, err = s.pool.Query(ctx, `
		SELECT p.id, p.entry_id::text, p.created_at, p.account, p.kind, p.amount_ulxc,
		       COALESCE((SELECT o.account FROM agent_postings o WHERE o.entry_id = p.entry_id AND o.id <> p.id ORDER BY o.id LIMIT 1), ''),
		       p.ref,
		       sum(p.amount_ulxc) OVER (PARTITION BY p.account ORDER BY p.created_at, p.id)::bigint, p.fee_bps, COALESCE(p.memo, ''),
		       COALESCE(p.model, '')
		  FROM agent_postings p
		 WHERE p.workspace_id = $1 AND ($2 = '' OR p.account = $2) AND p.created_at >= $3 AND p.created_at < $4
		 ORDER BY p.created_at, p.id`, workspaceID, account, from, to)
	if err != nil {
		return st, fmt.Errorf("economy: statement lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l StatementLine
		var running int64
		var feeBPS *int64
		if err := rows.Scan(&l.PostingID, &l.EntryID, &l.At, &l.Account, &l.Kind, &l.AmountULXC, &l.Counterparty, &l.Ref, &running, &feeBPS, &l.Memo, &l.Model); err != nil {
			return st, err
		}
		if l.Kind == LXCTypePlatformFee && feeBPS != nil {
			l.Label = FeeLabel(*feeBPS)
		}
		if l.Kind == LXCTypeLendingFee && feeBPS != nil {
			l.Label = LendingFeeLabel(*feeBPS)
		}
		l.At = l.At.UTC()
		l.BalanceAfterULXC = opening[l.Account] + running
		st.Lines = append(st.Lines, l)
	}
	return st, rows.Err()
}
