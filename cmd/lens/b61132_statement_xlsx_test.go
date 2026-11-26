package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

// xlsxCell is one cell of a workbook's sheet as the file stores it.
type xlsxCell struct {
	Ref     string `xml:"r,attr"`
	Type    string `xml:"t,attr"`
	Formula string `xml:"f"`
	Value   string `xml:"v"`
	Inline  string `xml:"is>t"`
}

// xlsxSheet opens book as a zip and reads its n-th sheet's cells by reference; it fails unless every part is XML.
func xlsxSheet(t *testing.T, book []byte, n int) map[string]xlsxCell {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(book), int64(len(book)))
	if err != nil {
		t.Fatalf("the workbook is not a zip: %v", err)
	}
	var sheet []byte
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		if strings.HasSuffix(f.Name, ".xml") || strings.HasSuffix(f.Name, ".rels") || strings.HasSuffix(f.Name, ".vml") {
			d := xml.NewDecoder(bytes.NewReader(body))
			for {
				if _, err := d.Token(); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("%s is not XML: %v", f.Name, err)
				}
			}
		}
		if f.Name == fmt.Sprintf("xl/worksheets/sheet%d.xml", n) {
			sheet = body
		}
	}
	var ws struct {
		Rows []struct {
			Cells []xlsxCell `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(sheet, &ws); err != nil {
		t.Fatalf("sheet %d: %v", n, err)
	}
	cells := map[string]xlsxCell{}
	for _, r := range ws.Rows {
		for _, c := range r.Cells {
			cells[c.Ref] = c
		}
	}
	return cells
}

// B61.132 — a money statement as an Excel workbook: a zip whose Statement sheet holds one row per statement line
// between the opening and closing rows, its Closing balance stored as the closing figure in pounds, and a
// counterparty that starts with "=" stored as a text cell, never a formula.
func TestStatementXLSX_AMoneyStatementIsASheetThatAddsUp(t *testing.T) {
	from, now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st := economy.MoneyStatement{AccountID: "acct-1", Currency: "GBP", From: from, To: from.AddDate(0, 1, 0), OpeningMinor: 1_331_597}
	balance := st.OpeningMinor
	for i := range 23 {
		amount, kind, with := int64(-(1_234 + 997*int64(i))), "card_charge", "Larch Coworking"
		if i%4 == 1 {
			amount, kind, with = 200_300, "payment_in", "Halden Retail"
		}
		if i == 5 {
			with = `=HYPERLINK("http://example.invalid","Pay here")`
		}
		balance += amount
		if amount > 0 {
			st.InMinor += amount
		} else {
			st.OutMinor -= amount
		}
		st.Lines = append(st.Lines, economy.MoneyStatementLine{At: from.Add(time.Duration(i)*29*time.Hour + 41*time.Minute), Kind: kind, AmountMinor: amount,
			Counterparty: with, PartnerRef: fmt.Sprintf("INV-%d", 2000+i), State: "completed", BalanceAfterMinor: balance, Task: "A market report"})
	}
	st.ClosingMinor = balance
	book := moneyStatementDoc(st, economy.CurrencyAccountDetails{}, economy.StatementWho{Holder: "Northwind Studio", AccountType: "Business account", Sandbox: true}, now).XLSX()
	cells := xlsxSheet(t, book, 1)

	if c := cells["C14"]; c.Inline != "Opening balance" {
		t.Errorf("C14 = %+v; want the opening row", c)
	}
	closeRow := 15 + len(st.Lines)
	if c := cells[fmt.Sprintf("C%d", closeRow)]; c.Inline != "Closing balance" {
		t.Fatalf("C%d = %+v; want the closing row after one row per line (%d lines)", closeRow, c, len(st.Lines))
	}
	for i := range st.Lines {
		if c := cells[fmt.Sprintf("J%d", 15+i)]; c.Formula != fmt.Sprintf("J%d+N(I%d)-N(H%d)", 14+i, 15+i, 15+i) || c.Value != economy.MoneyNumber(st.Lines[i].BalanceAfterMinor, "GBP") {
			t.Errorf("line %d's balance = %+v; want a running balance formula stored as %s", i, c, economy.MoneyNumber(st.Lines[i].BalanceAfterMinor, "GBP"))
		}
	}
	want := big.NewRat(st.ClosingMinor, 100) // st.ClosingMinor / 100, exactly
	isClosing := func(v string) bool { r, ok := new(big.Rat).SetString(v); return ok && r.Cmp(want) == 0 }
	if c := cells["J10"]; cells["H10"].Inline != "Closing balance" || c.Formula != "J7+J8-J9" || !isClosing(c.Value) {
		t.Errorf("the summary's Closing balance = %+v %+v; want the formula J7+J8-J9 stored as %s", cells["H10"], c, want.FloatString(2))
	}
	if c := cells[fmt.Sprintf("J%d", closeRow)]; !isClosing(c.Value) {
		t.Errorf("the closing row's balance = %+v; want %s", c, want)
	}
	if c := cells["J11"]; c.Type != "str" || c.Value != "Adds up" {
		t.Errorf("Check = %+v; want Adds up", c)
	}
	if c := cells["J7"]; c.Formula != "" || c.Value != "13315.97" {
		t.Errorf("Opening balance = %+v; want the one figure typed in, 13315.97", c)
	}
	if c := cells["C20"]; c.Type != "inlineStr" || c.Formula != "" || !strings.HasPrefix(c.Inline, "=HYPERLINK") {
		t.Errorf("a counterparty that starts with = is stored as %+v; want a text cell, never a formula", c)
	}
	for ref, c := range cells {
		if c.Formula != "" && ref[0] < 'H' {
			t.Errorf("%s holds the formula %q; only the figures are formulas", ref, c.Formula)
		}
	}
	if got := statementFileName("", st.Currency, st.From, st.To); got != "talyvor-statement-GBP-2026-10" {
		t.Errorf("the file is named %q; want talyvor-statement-GBP-2026-10", got)
	}
	if got := statementFileName("Atlas Buyer", "USD", time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); got != "talyvor-statement-atlas-buyer-USD-2026-08-03-to-2026-10-31" {
		t.Errorf("an agent's statement over a range is named %q", got)
	}
}

// B61.132 — an agent's and a wallet statement's sheets close where the statement does, to the last digit: the
// agent's dollars to the µLXC, each wallet balance on a sheet of its own.
func TestStatementXLSX_AgentAndWalletSheetsCloseAtTheStatement(t *testing.T) {
	from, now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	who := economy.StatementWho{Holder: "Northwind Studio", Agents: map[string]string{"ag-1": "Atlas"}}

	agent := economy.Statement{AgentID: "ag-1", From: from, To: to,
		Accounts: []economy.StatementAccount{{Account: "agent:ag-1", OpeningULXC: 15_500_001, InULXC: 2_000_000, OutULXC: 401_234, ClosingULXC: 17_098_767}},
		Lines: []economy.StatementLine{
			{At: from.Add(time.Hour), Account: "agent:ag-1", Kind: "fund", AmountULXC: 2_000_000, Counterparty: "workspace", BalanceAfterULXC: 17_500_001},
			{At: from.Add(2 * time.Hour), Account: "agent:ag-1", Kind: "spend", AmountULXC: -1_234, Counterparty: "spend", BalanceAfterULXC: 17_498_767},
			{At: from.Add(3 * time.Hour), Account: "agent:ag-1", Kind: "spend", AmountULXC: -400_000, Counterparty: "spend", BalanceAfterULXC: 17_098_767}}}
	cells := xlsxSheet(t, agentStatementDoc(agent, who, now).XLSX(), 1)
	if c := cells["J10"]; c.Value != economy.MoneyNumber(17_098_767, "LXC") || cells["J8"].Value != economy.MoneyNumber(2_000_000, "LXC") ||
		cells["J9"].Value != economy.MoneyNumber(401_234, "LXC") || cells["C9"].Inline != "Atlas" || cells["E16"].Inline != "Atlas" {
		t.Errorf("the agent's sheet: in %+v out %+v closing %+v agent %+v paid by %+v", cells["J8"], cells["J9"], c, cells["C9"], cells["E16"])
	}

	wallet := economy.WalletStatement{From: from, To: to, Accounts: []economy.WalletAccount{
		{AccountID: "a-usd", AgentID: "ag-1", Currency: "USD", OpeningMinor: 10_00, InMinor: 5_00, OutMinor: 2_50, ClosingMinor: 12_50},
		{AccountID: "a-usdc", Currency: "USDC", OpeningMinor: 1_500_000, InMinor: 250_001, OutMinor: 0, ClosingMinor: 1_750_001}},
		Lines: []economy.WalletLine{
			{At: from.Add(time.Hour), AccountID: "a-usd", Currency: "USD", Kind: "payment_in", AmountMinor: 5_00, Counterparty: "Cloud Co", BalanceAfterMinor: 15_00},
			{At: from.Add(2 * time.Hour), AccountID: "a-usd", Currency: "USD", Kind: "card_charge", AmountMinor: -2_50, Counterparty: "Paperly", BalanceAfterMinor: 12_50},
			{At: from.Add(3 * time.Hour), AccountID: "a-usdc", Currency: "USDC", Kind: "chain_deposit", AmountMinor: 250_001, Counterparty: "0xabc", BalanceAfterMinor: 1_750_001}}}
	book := walletStatementDoc(wallet, who, now).XLSX()
	if usd, usdc := xlsxSheet(t, book, 1), xlsxSheet(t, book, 2); usd["J10"].Value != "12.5" || usdc["J10"].Value != "1.750001" || usdc["D15"].Inline != "Paid in" {
		t.Errorf("the wallet's sheets close at %+v and %+v; want 12.5 and 1.750001", usd["J10"], usdc["J10"])
	}
}
