package main

import (
	"encoding/csv"
	"math/big"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

// B61.133 — a money statement's CSV keeps its 13 columns as they were, in order, with the opening and closing rows,
// and three more at the end a person reads without dividing: what (the line in words), amount and balance_after
// (decimals in the currency's places, a minus for money out), DESIGN.md 16.5.
func TestMoneyStatementCSV_ThreeReadableColumnsAtTheEnd(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		currency string
		lines    []economy.MoneyStatementLine
		opening  int64
		want     [][3]string // what, amount, balance_after: the opening row, each line, the closing row
	}{
		{currency: "USD", opening: 1_331_597, lines: []economy.MoneyStatementLine{
			{EntryID: "e1", Line: 1, At: from.Add(time.Hour), Kind: "card_charge", AmountMinor: -29_100, Funding: "test", Counterparty: "Larch Coworking",
				State: "completed", Task: "Desk for the month"},
			{EntryID: "e2", Line: 1, At: from.Add(2 * time.Hour), Kind: "invoice_payment", AmountMinor: 200_300, Counterparty: "Halden Retail",
				Memo: "INV-2034", State: "completed"},
			{EntryID: "e3", Line: 2, At: from.Add(3 * time.Hour), Kind: "payment_out", AmountMinor: -5, Counterparty: "Kite", PartnerRef: "pr_1", State: "pending"},
			{EntryID: "e4", Line: 1, At: from.Add(4 * time.Hour), Kind: "something_new", AmountMinor: 7, State: "completed"},
		}, want: [][3]string{{"Opening balance", "", "13315.97"}, {"Card payment", "-291.00", "13024.97"}, {"Invoice paid", "2003.00", "15027.97"},
			{"Paid out", "-0.05", "15027.92"}, {"Money in", "0.07", "15027.99"}, {"Closing balance", "", "15027.99"}}},
		{currency: "USDC", opening: 0, lines: []economy.MoneyStatementLine{
			{EntryID: "u1", Line: 1, At: from.Add(time.Hour), Kind: "payment_in", AmountMinor: 1_500_000_000, Counterparty: "Nick’s team", State: "completed"},
			{EntryID: "u2", Line: 1, At: from.Add(2 * time.Hour), Kind: "payout", AmountMinor: -1_234_567, State: "returned"},
		}, want: [][3]string{{"Opening balance", "", "0.000000"}, {"Paid in", "1500.000000", "1500.000000"}, {"Withdrawal", "-1.234567", "1498.765433"},
			{"Closing balance", "", "1498.765433"}}},
	} {
		st := economy.MoneyStatement{AccountID: "acct-1", Currency: c.currency, From: from, To: from.AddDate(0, 1, 0), OpeningMinor: c.opening}
		bal := c.opening
		for _, l := range c.lines {
			bal += l.AmountMinor
			l.BalanceAfterMinor = bal
			st.Lines = append(st.Lines, l)
		}
		st.ClosingMinor = bal
		rec := httptest.NewRecorder()
		writeMoneyStatement(rec, st)
		rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
		if err != nil {
			t.Fatalf("%s: the CSV does not read: %v", c.currency, err)
		}
		// The 13 columns as the server wrote them before B61.133, row for row.
		i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
		at := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
		old := [][]string{{"entry_id", "line", "at", "kind", "amount_minor", "currency", "funding", "counterparty", "memo", "partner_ref", "balance_after_minor", "task", "state"},
			{"", "", at(st.From), "opening", "", st.Currency, "", "", "", "", i64(st.OpeningMinor), "", ""}}
		for _, l := range st.Lines {
			old = append(old, []string{l.EntryID, strconv.Itoa(l.Line), at(l.At), l.Kind, i64(l.AmountMinor), st.Currency, l.Funding,
				csvText(l.Counterparty), csvText(l.Memo), csvText(l.PartnerRef), i64(l.BalanceAfterMinor), csvText(economy.TaskShown(l.Task)), l.State})
		}
		old = append(old, []string{"", "", at(st.To), "closing", "", st.Currency, "", "", "", "", i64(st.ClosingMinor), "", ""})
		if len(rows) != len(old) {
			t.Fatalf("%s: the CSV has %d rows; want %d: %q", c.currency, len(rows), len(old), rows)
		}
		if head := strings.Join(rows[0], ","); head != strings.Join(old[0], ",")+",what,amount,balance_after" {
			t.Errorf("%s: the head is %q; want the 13 columns, then what,amount,balance_after", c.currency, head)
		}
		scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(economy.MoneyCurrencies[c.currency])), nil))
		for r, row := range rows {
			if len(row) != 16 {
				t.Errorf("%s row %d has %d fields; want 16: %q", c.currency, r, len(row), row)
				continue
			}
			if strings.Join(row[:13], "\x00") != strings.Join(old[r], "\x00") {
				t.Errorf("%s row %d's first 13 fields are %q; want them as they were, %q", c.currency, r, row[:13], old[r])
			}
			if r == 0 {
				continue
			}
			if got := [3]string(row[13:]); got != c.want[r-1] {
				t.Errorf("%s row %d ends %q; want %q", c.currency, r, got, c.want[r-1])
			}
			// amount is amount_minor over the currency's places, and balance_after is balance_after_minor over them.
			for _, p := range [][2]int{{4, 14}, {10, 15}} {
				if row[p[0]] == "" && row[p[1]] == "" {
					continue
				}
				minor, ok1 := new(big.Rat).SetString(row[p[0]])
				dec, ok2 := new(big.Rat).SetString(row[p[1]])
				if !ok1 || !ok2 || new(big.Rat).Quo(minor, scale).Cmp(dec) != 0 {
					t.Errorf("%s row %d: %s %q is not %s %q over 10^%d", c.currency, r, rows[0][p[1]], row[p[1]], rows[0][p[0]], row[p[0]],
						economy.MoneyCurrencies[c.currency])
				}
			}
		}
	}
}
