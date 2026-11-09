package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
)

// B48.14 — history, statements and tax exports for every wallet (internal/economy/wallet_statements.go), for the owner:
//
//	GET /v1/workspaces/{wsID}/agents/{agentID}/wallet/activity?limit=     one agent's wallets, newest first (default 100, at most 1000)
//	GET /v1/workspaces/{wsID}/wallet/activity?limit=                      every wallet the owner has
//	GET /v1/workspaces/{wsID}/agents/{agentID}/wallet/statement?month=YYYY-MM&format=json|csv|pdf   one agent's month
//	GET /v1/workspaces/{wsID}/wallet/statement?month=YYYY-MM&format=json|csv|pdf                    every wallet's month
//	GET /v1/workspaces/{wsID}/wallet/tax?from=YYYY-MM-DD&to=YYYY-MM-DD&format=json|csv   disposals of non-stable tokens,
//	                                                                     first in, first out, in the owner's currency
//
// month defaults to the current month, so far; the tax export's period to the current calendar year.
func mountWalletStatementRoutes(r chi.Router, bank agentBank) {
	activity := func(w http.ResponseWriter, req *http.Request) {
		limit := 100
		if q := req.URL.Query().Get("limit"); q != "" {
			n, err := strconv.Atoi(q)
			if err != nil || n < 1 || n > 1000 {
				writeJSONErr(w, http.StatusBadRequest, "limit must be between 1 and 1000")
				return
			}
			limit = n
		}
		lines, err := bank.WalletActivity(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), limit)
		if !walletErr(w, err) {
			writeJSONOK(w, http.StatusOK, map[string]any{"lines": lines})
		}
	}
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/wallet/activity", ownerOnly(activity))
	r.Get("/v1/workspaces/{wsID}/wallet/activity", ownerOnly(activity))

	statement := func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		now := time.Now().UTC()
		from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		if m := q.Get("month"); m != "" {
			t, err := time.Parse("2006-01", m)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "month must be YYYY-MM")
				return
			}
			from = t.UTC()
		}
		format := q.Get("format")
		if format != "" && format != "json" && format != "csv" && format != "pdf" {
			writeJSONErr(w, http.StatusBadRequest, "format must be json, csv or pdf")
			return
		}
		to := from.AddDate(0, 1, 0)
		if to.After(now) { // the month so far: a movement recorded later may still fall in it
			to = now.Truncate(time.Microsecond)
		}
		st, err := bank.WalletStatement(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "agentID"), from, to)
		if walletErr(w, err) {
			return
		}
		name := fmt.Sprintf("wallet-statement-%s", from.Format("2006-01"))
		switch format {
		case "csv":
			writeCSV(w, name+".csv", walletStatementCSV(st))
		case "pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.pdf"`)
			_, _ = w.Write(walletStatementPDF(st)) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/pdf rendered by Lens
		default:
			writeJSONOK(w, http.StatusOK, st)
		}
	}
	r.Get("/v1/workspaces/{wsID}/agents/{agentID}/wallet/statement", ownerOnly(statement))
	r.Get("/v1/workspaces/{wsID}/wallet/statement", ownerOnly(statement))

	r.Get("/v1/workspaces/{wsID}/wallet/tax", ownerOnly(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		now := time.Now().UTC()
		from, to := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC), time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
		for _, p := range []struct {
			name string
			into *time.Time
		}{{"from", &from}, {"to", &to}} {
			if v := q.Get(p.name); v != "" {
				t, err := time.Parse(time.DateOnly, v)
				if err != nil {
					writeJSONErr(w, http.StatusBadRequest, p.name+" must be a YYYY-MM-DD date")
					return
				}
				*p.into = t
			}
		}
		if !from.Before(to) {
			writeJSONErr(w, http.StatusBadRequest, "from must be before to")
			return
		}
		format := q.Get("format")
		if format != "" && format != "json" && format != "csv" {
			writeJSONErr(w, http.StatusBadRequest, "format must be json or csv")
			return
		}
		x, err := bank.WalletTaxExport(req.Context(), chi.URLParam(req, "wsID"), from, to)
		if walletErr(w, err) {
			return
		}
		if format == "csv" {
			writeCSV(w, fmt.Sprintf("wallet-tax-%s-%s.csv", from.Format(time.DateOnly), to.Format(time.DateOnly)), walletTaxCSV(x))
			return
		}
		writeJSONOK(w, http.StatusOK, x)
	}))
}

// walletErr answers err, and is whether there was one.
func walletErr(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, economy.ErrAgentNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	default:
		return false
	}
	return true
}

func writeCSV(w http.ResponseWriter, filename string, rows [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.WriteAll(rows)
}

// walletAccountName is how a statement names an account: its own name, or its purpose and currency.
func walletAccountName(name, purpose, currency string) string {
	if name != "" {
		return name + " " + currency
	}
	return purpose + " " + currency
}

// walletStatementCSV is a header, each account's opening balance, every line, then each account's closing balance.
func walletStatementCSV(st economy.WalletStatement) [][]string {
	i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	rows := [][]string{{"entry_id", "line", "at", "account_id", "agent_id", "currency", "kind", "amount_minor", "funding", "counterparty", "ref", "memo", "balance_after_minor"}}
	for _, a := range st.Accounts {
		rows = append(rows, []string{"", "", at(st.From), a.AccountID, a.AgentID, a.Currency, "opening", "", "", "", "", "", i64(a.OpeningMinor)})
	}
	for _, l := range st.Lines {
		rows = append(rows, []string{l.EntryID, strconv.Itoa(l.Line), at(l.At), l.AccountID, l.AgentID, l.Currency, l.Kind, i64(l.AmountMinor), l.Funding,
			csvText(l.Counterparty), csvText(l.Ref), csvText(l.Memo), i64(l.BalanceAfterMinor)})
	}
	for _, a := range st.Accounts {
		rows = append(rows, []string{"", "", at(st.To), a.AccountID, a.AgentID, a.Currency, "closing", "", "", "", "", "", i64(a.ClosingMinor)})
	}
	return rows
}

// walletStatementPDF is the month as a document: each account's opening, in, out and closing, then every line.
func walletStatementPDF(st economy.WalletStatement) []byte {
	names := map[string]string{}
	t := market.PDFTable{Label: "WALLET STATEMENT", Title: st.From.Format("January 2006"),
		Lines: []string{fmt.Sprintf("From %s to %s (UTC)", st.From.Format("2 January 2006 15:04"), st.To.Format("2 January 2006 15:04"))},
		Columns: []market.PDFColumn{{Name: "DATE", Width: 62}, {Name: "ACCOUNT", Width: 92}, {Name: "WHAT", Width: 84},
			{Name: "WITH", Width: 107}, {Name: "AMOUNT", Width: 77, Right: true}, {Name: "BALANCE", Width: 77.28, Right: true}},
		Footer: "Amounts are in each account's own currency."}
	if st.AgentID != "" {
		t.Lines = append(t.Lines, "Agent "+st.AgentID)
	}
	for _, a := range st.Accounts {
		names[a.AccountID] = walletAccountName(a.AccountName, a.Purpose, a.Currency)
		t.Lines = append(t.Lines, fmt.Sprintf("%s: opening %s, in %s, out %s, closing %s", names[a.AccountID],
			economy.FormatMinor(a.OpeningMinor, a.Currency), economy.FormatMinor(a.InMinor, a.Currency),
			economy.FormatMinor(a.OutMinor, a.Currency), economy.FormatMinor(a.ClosingMinor, a.Currency)))
	}
	for _, l := range st.Lines {
		with := l.Counterparty
		if with == "" {
			with = l.Memo
		}
		t.Rows = append(t.Rows, []string{l.At.Format("2 Jan 15:04"), names[l.AccountID], l.Kind, with,
			economy.FormatMinor(l.AmountMinor, l.Currency), economy.FormatMinor(l.BalanceAfterMinor, l.Currency)})
	}
	return t.PDF()
}

// walletTaxCSV is each disposal with its cost basis and gain, each unpriced movement, and the totals.
func walletTaxCSV(x economy.TaxExport) [][]string {
	i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
	rows := [][]string{{"line", "at", "asset", "network", "agent_id", "tx_hash", "direction", "quantity_minor", "proceeds_minor",
		"cost_basis_minor", "gain_minor", "no_basis_quantity_minor", "currency"}}
	for _, d := range x.Disposals {
		noBasis := "0"
		if d.NoBasisQuantityMinor != nil {
			noBasis = d.NoBasisQuantityMinor.String()
		}
		rows = append(rows, []string{"disposal", d.At.Format(time.RFC3339), d.Asset, d.Network, d.AgentID, d.TxHash, d.Direction,
			d.QuantityMinor.String(), i64(d.ProceedsMinor), i64(d.CostBasisMinor), i64(d.GainMinor), noBasis, x.Currency})
	}
	for _, m := range x.Unpriced {
		rows = append(rows, []string{"unpriced", m.At.Format(time.RFC3339), m.Asset, m.Network, m.AgentID, m.TxHash, m.Direction,
			m.QuantityMinor.String(), "", "", "", "", x.Currency})
	}
	return append(rows, []string{"total", "", "", "", "", "", "", "", i64(x.ProceedsMinor), i64(x.CostBasisMinor), i64(x.GainMinor), "", x.Currency})
}
