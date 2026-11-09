package economy

import (
	"cmp"
	"strings"
	"time"
)

// agent_journal.go — B28.329: A STATEMENT AS A DOUBLE-ENTRY JOURNAL, for an accounting package (Xero, QuickBooks).
//
// Each entry on the statement is one journal: its postings are its lines, a posting into an account its debit and
// one out of it its credit. Where the statement holds only some of an entry's postings — an agent's statement holds
// the agent's side alone, and a transfer to another workspace's agent leaves that side out of the workspace's — the
// journal closes with one line on the posting's counterparty for what is missing, so every journal balances. The
// statement's accounts' debits are then their in_ulxc and their credits their out_ulxc.
//
// An entry's cost centre is the agent it moved money for: its agent:<id> account, else its counterparty's.

// JournalLine is one debit or one credit.
type JournalLine struct {
	Journal     string    `json:"journal"` // the entry's id
	At          time.Time `json:"at"`
	Account     string    `json:"account"`
	Description string    `json:"description"`
	DebitULXC   int64     `json:"debit_ulxc"`
	CreditULXC  int64     `json:"credit_ulxc"`
	CostCentre  string    `json:"cost_centre"` // the agent's id; empty when no agent was in the entry
	Ref         string    `json:"ref,omitempty"`
}

// Journal is a statement's period as journals, with what its lines debit and credit in all.
type Journal struct {
	WorkspaceID string        `json:"workspace_id"`
	AgentID     string        `json:"agent_id,omitempty"`
	From        time.Time     `json:"from"`
	To          time.Time     `json:"to"`
	Lines       []JournalLine `json:"lines"`
	DebitULXC   int64         `json:"debit_ulxc"`
	CreditULXC  int64         `json:"credit_ulxc"`
}

// StatementJournal is st as journals, one per entry, in the order the statement first shows each.
func StatementJournal(st Statement) Journal {
	j := Journal{WorkspaceID: st.WorkspaceID, AgentID: st.AgentID, From: st.From, To: st.To, Lines: []JournalLine{}}
	var order []string
	legs := map[string][]StatementLine{}
	for _, l := range st.Lines {
		if _, seen := legs[l.EntryID]; !seen {
			order = append(order, l.EntryID)
		}
		legs[l.EntryID] = append(legs[l.EntryID], l)
	}
	agent := func(account string) string {
		if id, ok := strings.CutPrefix(account, "agent:"); ok {
			return id
		}
		return ""
	}
	add := func(l StatementLine, account, centre string, amount int64) {
		line := JournalLine{Journal: l.EntryID, At: l.At, Account: account, Description: cmp.Or(l.Label, l.Memo, l.Kind),
			CostCentre: centre, Ref: l.Ref}
		if amount > 0 {
			line.DebitULXC, j.DebitULXC = amount, j.DebitULXC+amount
		} else {
			line.CreditULXC, j.CreditULXC = -amount, j.CreditULXC-amount
		}
		j.Lines = append(j.Lines, line)
	}
	for _, e := range order {
		var centre string
		var sum int64
		for _, l := range legs[e] {
			centre = cmp.Or(centre, agent(l.Account), agent(l.Counterparty))
			sum += l.AmountULXC
		}
		for _, l := range legs[e] {
			add(l, l.Account, centre, l.AmountULXC)
		}
		if first := legs[e][0]; sum != 0 {
			add(first, first.Counterparty, centre, -sum)
		}
	}
	return j
}
