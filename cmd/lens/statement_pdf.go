package main

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// statement_pdf.go — B61.131: the money, agent and wallet statements as the one branded statement document
// (market.BrandedStatement, DESIGN.md 16.3), each balance a section of it, in a person's words (economy.KindWords).

// brandedPeriod is a section's title, range and period in full, and its first and last days, for [from, to).
func brandedPeriod(sec *market.BrandedSection, from, to time.Time, now time.Time) {
	from, to = from.UTC(), to.UTC()
	last := to.Add(-time.Nanosecond)
	if last.Before(from) {
		last = from
	}
	day := func(t time.Time) string { return t.Format("2 January 2006") }
	switch {
	case from.Year() == last.Year() && from.Month() == last.Month():
		sec.Range = fmt.Sprintf("%d to %d %s", from.Day(), last.Day(), from.Format("January 2006"))
	case from.Year() == last.Year():
		sec.Range = fmt.Sprintf("%d %s to %s", from.Day(), from.Format("January"), day(last))
	default:
		sec.Range = day(from) + " to " + day(last)
	}
	sec.Title = sec.Range
	if from.Equal(time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)) && !to.After(from.AddDate(0, 1, 0)) {
		sec.Title = from.Format("January 2006")
	}
	sec.Period = day(from) + " to " + day(last)
	sec.Issued = "Issued " + day(now.UTC())
	sec.OpenDay, sec.CloseDay = from.Format("02 Jan"), last.Format("02 Jan")
	sec.From, sec.Last = from, last
}

// brandedMoney is a section's way of writing currency's amounts.
func brandedMoney(sec *market.BrandedSection, currency string) {
	sec.Money = func(v int64) string { return economy.Money(v, currency) }
	sec.Figure = func(v int64) string { return economy.MoneyFigure(v, currency) }
	sec.Number = func(v int64) string { return economy.MoneyNumber(v, currency) }
	sec.CurrencyWords = economy.CurrencyWords(currency)
	if currency == "LXC" {
		sec.CurrencyWords = economy.CurrencyWords(economy.CurrencyUSD)
	}
}

// codeShaped is a string that is one of Lens's own keys, not words: an id ("macc_…", a UUID) or a key ("spend",
// "escrow:…", "test_money:…"). A chain address (0x…) is who it was with, and stays.
var codeShaped = regexp.MustCompile(`^(spend|[a-z]+_[0-9A-Za-z-]{6,}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[a-z_]+:\S*)$`)

// brandedWith is who a line was with, in words: the company, an agent or its account by the agent's name, or the
// payer or payee outside; "" for one of Talyvor's own accounts or a key, where what the line was leads instead.
func brandedWith(counterparty string, who economy.StatementWho) string {
	switch {
	case counterparty == "workspace" || strings.HasPrefix(counterparty, "workspace:"):
		return "The company"
	case strings.HasPrefix(counterparty, "agent:"):
		return who.Agents[strings.TrimPrefix(counterparty, "agent:")]
	case who.Accounts[counterparty] != "":
		return who.Accounts[counterparty]
	case codeShaped.MatchString(counterparty):
		return ""
	}
	return counterparty
}

// brandedRef is a partner's reference as a line writes it, "Ref INV-2034"; "" for none, or for one of Lens's own keys.
func brandedRef(ref string) string {
	if ref == "" || codeShaped.MatchString(ref) {
		return ""
	}
	return "Ref " + ref
}

// brandedRow is a line: its name is who it was with, else what it was; its detail what it was and its words.
func brandedRow(at time.Time, kind, with string, details []string, state, task string, amount, balance int64) market.BrandedRow {
	what := economy.KindWords(kind, amount)
	name := cmp.Or(with, what)
	var parts, refs []string
	if with != "" {
		parts = append(parts, what)
	}
	for _, d := range details {
		if d != "" && d != name {
			parts, refs = append(parts, d), append(refs, strings.TrimPrefix(d, "Ref ")) // under a Reference head
		}
	}
	return market.BrandedRow{Day: at.UTC().Format("02 Jan"), Time: at.UTC().Format("15:04"), Name: name, State: economy.StateWords(state),
		Detail: strings.Join(parts, " · "), Task: economy.TaskShown(task), Amount: amount, Balance: balance,
		At: at.UTC(), What: what, Reference: strings.Join(refs, " · ")}
}

// brandedSection is a section for who over [from, to), in currency.
func brandedSection(who economy.StatementWho, currency string, from, to, now time.Time) market.BrandedSection {
	sec := market.BrandedSection{Holder: who.Holder, HolderKind: who.AccountType, WhoHead: "Account"}
	brandedPeriod(&sec, from, to, now)
	brandedMoney(&sec, currency)
	return sec
}

// brandedAgent makes sec an agent's: AGENT, its name, and what it pays in.
func brandedAgent(sec *market.BrandedSection, who economy.StatementWho, agentID, currency string) {
	sec.WhoHead, sec.WhoName = "Agent", cmp.Or(who.Agents[agentID], "Agent")
	sec.WhoLines = []string{"Pays in " + economy.CurrencyWords(currency)}
}

// statementWhoReader is a bank that can say who a statement is for (economy.DualTokenStore).
type statementWhoReader interface {
	StatementWho(ctx context.Context, workspaceID, capability string) (economy.StatementWho, error)
}

// statementWho reads, when asked, who workspaceID's agent statement is for; a bank that cannot say leaves it Sandbox,
// unnamed.
func statementWho(req *http.Request, bank any, workspaceID string) func() economy.StatementWho {
	return func() economy.StatementWho {
		return statementWhoFor(req.Context(), bank, workspaceID, "spend_on_talyvor")
	}
}

// statementWhoFor is who workspaceID's statement of capability's money is for.
func statementWhoFor(ctx context.Context, bank any, workspaceID, capability string) economy.StatementWho {
	if r, ok := bank.(statementWhoReader); ok {
		who, _ := r.StatementWho(ctx, workspaceID, capability)
		return who
	}
	return economy.StatementWho{Agents: map[string]string{}, Accounts: map[string]string{}, Sandbox: true, AccountType: "Business account"}
}
