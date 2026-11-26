package main

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

// pdfTexts is every string a PDF's pages show, in the order they are drawn, as UTF-8.
func pdfTexts(pdf []byte) []string {
	winAnsi := map[byte]string{0x80: "€", 0x85: "…", 0x91: "‘", 0x92: "’", 0x93: "“", 0x94: "”", 0x96: "–", 0x97: "—"}
	var out []string
	for _, m := range regexp.MustCompile(`\(((?:[^()\\]|\\.)*)\) Tj`).FindAllSubmatch(pdf, -1) {
		var b strings.Builder
		s := m[1]
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				if i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '7' {
					n, _ := strconv.ParseUint(string(s[i+1:i+4]), 8, 8)
					c, i = byte(n), i+3
				} else {
					c, i = s[i+1], i+1
				}
			}
			switch w, ok := winAnsi[c]; {
			case ok:
				b.WriteString(w)
			default:
				b.WriteRune(rune(c))
			}
		}
		out = append(out, b.String())
	}
	return out
}

// addsUp reads the statement's "How this adds up" lines from its texts: opening, in, out and closing as printed, and
// fails unless opening + in − out = closing exactly, to the last digit printed.
func addsUp(t *testing.T, texts []string) (sums [][4]string) {
	t.Helper()
	amount := func(s string) *big.Rat {
		v, ok := new(big.Rat).SetString(strings.NewReplacer("$", "", "£", "", "€", "", ",", "", " USDC", "").Replace(s))
		if !ok {
			t.Fatalf("%q is not an amount", s)
		}
		return v
	}
	for i, s := range texts {
		if s != "HOW THIS ADDS UP" {
			continue
		}
		if i+11 >= len(texts) || texts[i+2] != "opening" || texts[i+3] != "+" || texts[i+5] != "in" || texts[i+6] != "–" ||
			texts[i+8] != "out" || texts[i+9] != "=" || texts[i+11] != "closing" {
			t.Fatalf("how it adds up does not read opening + in – out = closing: %q", texts[i:min(i+12, len(texts))])
		}
		got := new(big.Rat).Sub(new(big.Rat).Add(amount(texts[i+1]), amount(texts[i+4])), amount(texts[i+7]))
		if got.Cmp(amount(texts[i+10])) != 0 {
			t.Errorf("%s + %s - %s is %s, but the statement prints %s closing", texts[i+1], texts[i+4], texts[i+7], got.FloatString(7), texts[i+10])
		}
		sums = append(sums, [4]string{texts[i+1], texts[i+4], texts[i+7], texts[i+10]})
	}
	return sums
}

// B61.131 — a money statement too long for one page is the branded statement: the person's words and never the
// server's codes, its opening and closing balances, its pages numbered, in the three statement fonts alone.
func TestStatementPDF_ALongMoneyStatementIsBranded(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	st := economy.MoneyStatement{AccountID: "acct-1", Currency: "USD", From: from, To: from.AddDate(0, 1, 0), OpeningMinor: 1_331_597}
	bal := st.OpeningMinor
	kinds := []string{"card_charge", "payment_out", "payment_in", "test_money", "invoice_payment"}
	for i := range 30 {
		amount := int64(-12_345 - 101*i)
		if i%5 >= 2 {
			amount = int64(20_050 + 7*i)
			st.InMinor += amount
		} else {
			st.OutMinor -= amount
		}
		bal += amount
		l := economy.MoneyStatementLine{At: from.Add(time.Duration(i) * 20 * time.Hour), Kind: kinds[i%5], AmountMinor: amount,
			Counterparty: fmt.Sprintf("Supplier %d", i), State: "completed", BalanceAfterMinor: bal}
		if i == 4 {
			l.State = "pending"
		}
		if i == 6 {
			l.Task = "A market report"
		}
		st.Lines = append(st.Lines, l)
	}
	st.ClosingMinor = bal
	pdf := moneyStatementPDF(st, economy.CurrencyAccountDetails{}, economy.StatementWho{Holder: "Northwind Studio", AccountType: "Business account", Sandbox: true},
		time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))
	texts := pdfTexts(pdf)
	all := strings.Join(texts, "\n")
	for _, never := range []string{"card_charge", "payment_out", "payment_in", "test_money", "invoice_payment", "completed", "pending", "LXC"} {
		if strings.Contains(all, never) {
			t.Errorf("the statement prints %q; a person never reads a code:\n%s", never, all)
		}
	}
	if strings.Contains(strings.ToLower(all), "test money") {
		t.Errorf("the statement says test money:\n%s", all)
	}
	closing := economy.MoneyFigure(st.ClosingMinor, "USD")
	for _, want := range []string{"Opening balance", "Closing balance", closing, economy.Money(st.ClosingMinor, "USD"), "Page 1 of 2", "Page 2 of 2",
		"Card payment", "Paid out", "Pending", "The agent says: A market report", "Sandbox statement. No real money moved.", "October 2026", "Northwind Studio"} {
		if !strings.Contains(all, want) {
			t.Errorf("the statement does not print %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "Page 1 of 1") || strings.Contains(all, "Page 3 of") {
		t.Errorf("the statement is not two pages:\n%s", all)
	}
	fonts := regexp.MustCompile(`/BaseFont /([A-Za-z-]+)`).FindAllSubmatch(pdf, -1)
	var names []string
	for _, f := range fonts {
		names = append(names, string(f[1]))
	}
	if strings.Join(names, " ") != "FigtreeStatement-Regular FigtreeStatement-SemiBold BricolageStatement-Bold" ||
		strings.Contains(string(pdf), "Helvetica") || strings.Contains(string(pdf), "Courier") || strings.Count(string(pdf), "/FontFile2 ") != 3 {
		t.Errorf("the statement's fonts are %v; want the three statement fonts embedded, and no Helvetica or Courier", names)
	}
	if sums := addsUp(t, texts); len(sums) != 1 || sums[0][3] != economy.Money(st.ClosingMinor, "USD") {
		t.Errorf("how it adds up: %v; want one closing at %s", sums, economy.Money(st.ClosingMinor, "USD"))
	}
}

// B61.131 — a money, an agent's and a wallet statement each print opening + in − out = closing, and it is true to
// the last digit printed: an agent's statement to the µLXC its dollars are written to.
func TestStatementPDF_EachStatementAddsUp(t *testing.T) {
	from, now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	who := economy.StatementWho{Holder: "Northwind Studio", Agents: map[string]string{"ag-1": "Atlas"}}

	money := economy.MoneyStatement{Currency: "GBP", From: from, To: to, OpeningMinor: 120_00, InMinor: 30_55, OutMinor: 25_10, ClosingMinor: 125_45,
		Lines: []economy.MoneyStatementLine{
			{At: from.Add(time.Hour), Kind: "payment_in", AmountMinor: 30_55, Counterparty: "Halden Retail", State: "completed", BalanceAfterMinor: 150_55},
			{At: from.Add(2 * time.Hour), Kind: "payment_out", AmountMinor: -25_10, Counterparty: "workspace:ws-1", State: "completed", BalanceAfterMinor: 125_45}}}
	if sums := addsUp(t, pdfTexts(moneyStatementPDF(money, economy.CurrencyAccountDetails{}, who, now))); len(sums) != 1 || sums[0][3] != "£125.45" {
		t.Errorf("the money statement adds up as %v; want one statement closing at £125.45", sums)
	}

	agent := economy.Statement{AgentID: "ag-1", From: from, To: to,
		Accounts: []economy.StatementAccount{{Account: "agent:ag-1", OpeningULXC: 15_500_001, InULXC: 2_000_000, OutULXC: 401_234, ClosingULXC: 17_098_767}},
		Lines: []economy.StatementLine{
			{At: from.Add(time.Hour), Account: "agent:ag-1", Kind: "fund", AmountULXC: 2_000_000, Counterparty: "workspace", BalanceAfterULXC: 17_500_001},
			{At: from.Add(2 * time.Hour), Account: "agent:ag-1", Kind: "spend", AmountULXC: -1_234, Counterparty: "spend", BalanceAfterULXC: 17_498_767},
			{At: from.Add(3 * time.Hour), Account: "agent:ag-1", Kind: "spend", AmountULXC: -400_000, Counterparty: "spend", BalanceAfterULXC: 17_098_767}}}
	texts := pdfTexts(agentStatementPDF(agent, who, now))
	if sums := addsUp(t, texts); len(sums) != 1 || sums[0][3] != economy.LXCMoney(17_098_767) {
		t.Errorf("the agent's statement adds up as %v; want one statement closing at %s", sums, economy.LXCMoney(17_098_767))
	}
	if all := strings.Join(texts, "\n"); !strings.Contains(all, "Atlas") || !strings.Contains(all, "Funded") || strings.Contains(all, "spend") {
		t.Errorf("the agent's statement does not name Atlas and its lines in words:\n%s", all)
	}

	wallet := economy.WalletStatement{From: from, To: to, Accounts: []economy.WalletAccount{
		{AccountID: "a-usd", AgentID: "ag-1", Currency: "USD", OpeningMinor: 10_00, InMinor: 5_00, OutMinor: 2_50, ClosingMinor: 12_50},
		{AccountID: "a-usdc", Currency: "USDC", OpeningMinor: 1_500_000, InMinor: 250_001, OutMinor: 0, ClosingMinor: 1_750_001}},
		Lines: []economy.WalletLine{
			{At: from.Add(time.Hour), AccountID: "a-usd", Currency: "USD", Kind: "payment_in", AmountMinor: 5_00, Counterparty: "Cloud Co", BalanceAfterMinor: 15_00},
			{At: from.Add(2 * time.Hour), AccountID: "a-usd", Currency: "USD", Kind: "card_charge", AmountMinor: -2_50, Counterparty: "Paperly", BalanceAfterMinor: 12_50},
			{At: from.Add(3 * time.Hour), AccountID: "a-usdc", Currency: "USDC", Kind: "chain_deposit", AmountMinor: 250_001, Counterparty: "0xabc", BalanceAfterMinor: 1_750_001}}}
	sums := addsUp(t, pdfTexts(walletStatementPDF(wallet, who, now)))
	if len(sums) != 2 || sums[0][3] != "$12.50" || sums[1][3] != "1.750001 USDC" {
		t.Errorf("the wallet statement adds up as %v; want a section closing at $12.50 and one at 1.750001 USDC", sums)
	}
}
