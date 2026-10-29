package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// search.go — B41.107: ONE SEARCH ACROSS A WORKSPACE'S MONEY.
//
// One query, matched case-insensitively as a substring, across the workspace's agents, payees, payments, bills,
// invoices, rules and listings, grouped by kind in that order. A supplier's name finds its payee, the bills it sent
// and the payments made to it — a bill or payment linked to a matched payee is found under the payee's name even
// when it was read or sent under another. A rule is found by its agent's name or by a payee it allows or blocks.
// Each result carries its kind and id (and its agent's, where its screen is the agent's) so it opens its own screen.

// Search kinds, in the order the groups are listed.
const (
	SearchAgent    = "agent"
	SearchPayee    = "payee"
	SearchPayment  = "payment"
	SearchBill     = "bill"
	SearchInvoice  = "invoice"
	SearchRule     = "rule"
	SearchListing  = "listing"
	searchMaxQuery = 100
	// SearchDefaultLimit and SearchMaxLimit bound the results in each group.
	SearchDefaultLimit = 5
	SearchMaxLimit     = 20
)

// ErrSearchQuery is a query too short or too long to search.
var ErrSearchQuery = errors.New("economy: search")

// SearchResult is one thing a search found.
type SearchResult struct {
	Kind        string     `json:"kind"`
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Detail      string     `json:"detail"`
	AgentID     string     `json:"agent_id,omitempty"`
	AmountMinor int64      `json:"amount_minor,omitempty"` // in Currency's minor units
	Currency    string     `json:"currency,omitempty"`
	Status      string     `json:"status,omitempty"`
	Date        *time.Time `json:"date,omitempty"` // a bill's or invoice's due date, a payment's
}

// SearchGroup is a search's results of one kind.
type SearchGroup struct {
	Kind    string         `json:"kind"`
	Results []SearchResult `json:"results"`
}

// SearchResults is what a search found, its groups in kind order; a kind with nothing found is left out.
type SearchResults struct {
	Query  string        `json:"query"`
	Groups []SearchGroup `json:"groups"`
}

// searchQueries are each kind's query: $1 the workspace, $2 the lowered query, $3 the limit. Each returns id, title,
// detail, agent_id, amount_minor, currency, status and date.
var searchQueries = []struct{ kind, sql string }{
	{SearchAgent, `SELECT id, name, description, '', 0, '', CASE WHEN paused_at IS NULL THEN 'active' ELSE 'paused' END, NULL::timestamptz
		FROM agent_accounts WHERE workspace_id = $1 AND kind = 'agent' AND archived_at IS NULL
		AND (strpos(lower(name), $2) > 0 OR strpos(lower(COALESCE(handle, '')), $2) > 0 OR strpos(lower(description), $2) > 0)
		ORDER BY lower(name), id LIMIT $3`},
	{SearchPayee, `SELECT id, name, country, '', 0, '', CASE WHEN confirmed_at IS NULL AND check_result <> 'exact_match' THEN 'unconfirmed' ELSE 'confirmed' END,
		NULL::timestamptz FROM money_payees WHERE workspace_id = $1 AND strpos(lower(name), $2) > 0
		ORDER BY lower(name), id LIMIT $3`},
	{SearchPayment, `SELECT id, payee_name, reference, agent_id, amount_minor, currency, status, created_at
		FROM money_payments WHERE workspace_id = $1
		AND (strpos(lower(payee_name), $2) > 0 OR strpos(lower(reference), $2) > 0
		     OR payee_id IN (SELECT id FROM money_payees WHERE workspace_id = $1 AND strpos(lower(name), $2) > 0))
		ORDER BY created_at DESC, id LIMIT $3`},
	{SearchBill, `SELECT id, supplier, invoice_number, '', amount_minor, currency, status, due_date::timestamptz
		FROM money_bills WHERE workspace_id = $1
		AND (strpos(lower(supplier), $2) > 0 OR strpos(lower(invoice_number), $2) > 0 OR strpos(lower(filename), $2) > 0
		     OR (payee_id <> '' AND payee_id IN (SELECT id FROM money_payees WHERE workspace_id = $1 AND strpos(lower(name), $2) > 0)))
		ORDER BY status = 'paid', due_date NULLS LAST, created_at, id LIMIT $3`},
	{SearchInvoice, `SELECT id, customer_name, 'Invoice ' || number || ' · ' || reference, agent_id, total_minor, currency, status, due_date::timestamptz
		FROM money_invoices WHERE workspace_id = $1
		AND (strpos(lower(customer_name), $2) > 0 OR strpos(lower(customer_email), $2) > 0 OR strpos(lower(reference), $2) > 0
		     OR strpos(lower(memo), $2) > 0 OR ('invoice ' || number) = $2)
		ORDER BY created_at DESC, id LIMIT $3`},
	{SearchRule, `SELECT r.agent_id, a.name, 'Spending rules', r.agent_id, 0, '', '', r.updated_at
		FROM agent_rules r JOIN agent_accounts a ON a.id = r.agent_id AND a.workspace_id = r.workspace_id
		WHERE r.workspace_id = $1 AND a.archived_at IS NULL
		AND (strpos(lower(a.name), $2) > 0 OR EXISTS (SELECT 1 FROM unnest(r.allowed_payees || r.blocked_payees) p
		     WHERE strpos(lower(p), $2) > 0
		        OR p IN (SELECT id FROM agent_accounts WHERE workspace_id = $1 AND strpos(lower(name), $2) > 0)
		        OR p IN (SELECT id FROM market_listings WHERE workspace_id = $1 AND strpos(lower(title), $2) > 0)))
		ORDER BY lower(a.name), r.agent_id LIMIT $3`},
	{SearchListing, `SELECT id, title, kind, '', 0, '', visibility, updated_at
		FROM market_listings WHERE workspace_id = $1 AND (strpos(lower(title), $2) > 0 OR strpos(lower(description), $2) > 0)
		ORDER BY lower(title), id LIMIT $3`},
}

// Search finds query among workspaceID's agents, payees, payments, bills, invoices, rules and listings, at most
// limit of each (SearchDefaultLimit when 0, never more than SearchMaxLimit).
func (s *DualTokenStore) Search(ctx context.Context, workspaceID, query string, limit int) (SearchResults, error) {
	query = strings.TrimSpace(query)
	if n := utf8.RuneCountInString(query); n < 2 || n > searchMaxQuery {
		return SearchResults{}, fmt.Errorf("%w: search for 2 to %d characters", ErrSearchQuery, searchMaxQuery)
	}
	if limit <= 0 {
		limit = SearchDefaultLimit
	}
	limit = min(limit, SearchMaxLimit)
	out := SearchResults{Query: query, Groups: []SearchGroup{}}
	lowered := strings.ToLower(query)
	for _, q := range searchQueries {
		rows, err := s.pool.Query(ctx, q.sql, workspaceID, lowered, limit)
		if err != nil {
			return SearchResults{}, fmt.Errorf("economy: search %ss: %w", q.kind, err)
		}
		g := SearchGroup{Kind: q.kind, Results: []SearchResult{}}
		for rows.Next() {
			r := SearchResult{Kind: q.kind}
			if err := rows.Scan(&r.ID, &r.Title, &r.Detail, &r.AgentID, &r.AmountMinor, &r.Currency, &r.Status, &r.Date); err != nil {
				rows.Close()
				return SearchResults{}, fmt.Errorf("economy: search %ss: %w", q.kind, err)
			}
			g.Results = append(g.Results, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return SearchResults{}, fmt.Errorf("economy: search %ss: %w", q.kind, err)
		}
		if len(g.Results) > 0 {
			out.Groups = append(out.Groups, g)
		}
	}
	return out, nil
}
