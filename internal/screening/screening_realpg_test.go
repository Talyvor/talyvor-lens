package screening_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/testschema"
	"github.com/talyvor/lens/migrations"
)

// B30.6 — DONE: a payment to a payee on the fixture list is refused before any posting and a case exists; a failed
// download keeps the old list. The lists are testdata's, served by a local server: never the network.

func screeningPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_screening_%d", time.Now().UnixNano())
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	mc, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	// B61.52: the accounts these tests move money for have confirmed who they are.
	if _, err := mc.Exec(ctx, testschema.ConfirmedAccountsSQL); err != nil {
		_ = mc.Close(ctx)
		t.Fatal(err)
	}
	_ = mc.Close(ctx)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

// listServer serves testdata's lists as the publishers would; a file set down answers 503, one set broken answers an
// error page with 200.
type listServer struct {
	*httptest.Server
	mu    sync.Mutex
	state map[string]string
}

func newListServer(t *testing.T) *listServer {
	s := &listServer{state: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		state := s.state[r.URL.Path]
		s.mu.Unlock()
		switch state {
		case "down":
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case "broken":
			_, _ = w.Write([]byte("<html><body>We are updating the list</body></html>"))
		default:
			http.ServeFile(w, r, "testdata"+r.URL.Path)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *listServer) set(path, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[path] = state
}

func (s *listServer) store(pool *pgxpool.Pool) *screening.Store {
	return screening.NewStore(pool, []screening.Source{screening.UKSource(s.URL + "/uk.csv"),
		screening.OFACSource(s.URL+"/ofac_sdn.csv", s.URL+"/ofac_alt.csv")}, 0.9, 48*time.Hour)
}

func listByName(t *testing.T, store *screening.Store) map[string]screening.ListStatus {
	t.Helper()
	ls, err := store.Lists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]screening.ListStatus{}
	for _, l := range ls {
		out[l.List] = l
	}
	return out
}

func exactly(t *testing.T, store *screening.Store, name, list, entry string) {
	t.Helper()
	ms, err := store.Match(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.List == list && m.Entry == entry && m.ScoreBPS == 10_000 {
			return
		}
	}
	t.Fatalf("%s does not match %s %s exactly: %+v", name, list, entry, ms)
}

func TestRefresh_AFailedDownloadKeepsTheListAlreadyLoaded(t *testing.T) {
	ctx := context.Background()
	pool := screeningPool(t)
	srv := newListServer(t)
	store := srv.store(pool)

	if _, err := store.Match(ctx, "Banco Nacional de Cuba"); !errors.Is(err, partners.ErrScreeningUnavailable) {
		t.Fatalf("screened against lists never downloaded: %v", err)
	}
	if err := store.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	lists := listByName(t, store)
	if uk, ofac := lists[screening.ListUK], lists[screening.ListOFAC]; uk.Entries != 14 || uk.Stale || uk.Published != "08-Oct-2026" ||
		ofac.Entries != 6 || ofac.Stale || uk.LoadedAt == nil {
		t.Fatalf("loaded: %+v", lists)
	}
	exactly(t, store, "Banco Nacional de Cuba", screening.ListOFAC, "306")
	loadedAt := *lists[screening.ListUK].LoadedAt

	// The UK list's server is down and OFAC's answers an error page: both keep the copy in force, and say why.
	srv.set("/uk.csv", "down")
	srv.set("/ofac_sdn.csv", "broken")
	err := store.Refresh(ctx)
	if err == nil || !strings.Contains(err.Error(), "UK") || !errors.Is(err, screening.ErrNotTheList) {
		t.Fatalf("a failed refresh answered %v", err)
	}
	lists = listByName(t, store)
	uk, ofac := lists[screening.ListUK], lists[screening.ListOFAC]
	if !uk.Stale || !strings.Contains(uk.LastError, "503") || uk.Entries != 14 || !uk.LoadedAt.Equal(loadedAt) || uk.FailedAt == nil {
		t.Fatalf("UK after a failed download: %+v", uk)
	}
	if !ofac.Stale || !strings.Contains(ofac.LastError, "not the list") || ofac.Entries != 6 {
		t.Fatalf("OFAC after a broken file: %+v", ofac)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM screening_entries`).Scan(&rows); err != nil || rows != 20 {
		t.Fatalf("%d entries kept, %v", rows, err)
	}
	exactly(t, store, "ANWARI, Muhammad Taher", screening.ListUK, "AFG0009")
	exactly(t, store, "National Bank of Cuba", screening.ListOFAC, "306")

	// Within the hour a failed list is not tried again; once it downloads it is current.
	if err := store.RefreshDue(ctx); err != nil {
		t.Fatalf("a list that failed within the hour was tried again: %v", err)
	}
	srv.set("/uk.csv", "")
	srv.set("/ofac_sdn.csv", "")
	if err := store.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if lists = listByName(t, store); lists[screening.ListUK].Stale || lists[screening.ListOFAC].LastError != "" {
		t.Fatalf("after a good download: %+v", lists)
	}
}

// payments is a workspace's GBP company and partner accounts, with £500.00 paid in, its outside payments screened
// against the fixture lists.
type payments struct {
	pool             *pgxpool.Pool
	money            *economy.DualTokenStore
	screener         *screening.Screener
	store            *screening.Store
	ws               string
	company, partner string
}

func newPayments(t *testing.T) payments {
	t.Helper()
	ctx := context.Background()
	pool := screeningPool(t)
	store := newListServer(t).store(pool)
	if err := store.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	registry := partners.NewRegistry(nil)
	registry.UseScreeningList(store)
	p := payments{pool: pool, money: economy.NewDualTokenStore(nil, pool, nil), screener: screening.NewScreener(pool, registry), store: store, ws: "ws-b306"}
	p.screener.HoldWhenListsStale(store)
	p.money.SetScreener(p.screener)
	for _, a := range []*string{&p.company, &p.partner} {
		purpose := economy.MoneyCompany
		if a == &p.partner {
			purpose = economy.MoneyPartner
		}
		acct, err := p.money.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: p.ws, Currency: economy.CurrencyGBP, Purpose: purpose, Name: purpose})
		if err != nil {
			t.Fatal(err)
		}
		*a = acct.ID
	}
	if _, err := p.money.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: p.ws, Capability: economy.CapabilityPaymentsIn, Kind: "payment_in",
		IdempotencyKey: "in-1", Funding: economy.FundingTest, Counterparty: "Ada Lovelace", Postings: []economy.MoneyPosting{
			{AccountID: p.partner, AmountMinor: -50_000}, {AccountID: p.company, AmountMinor: 50_000}}}); err != nil {
		t.Fatalf("money in from someone on no list: %v", err)
	}
	return p
}

func (p payments) payOut(key, payee string, pence int64) (economy.MoneyEntry, error) {
	return p.money.PostMoney(context.Background(), economy.MoneyEntry{WorkspaceID: p.ws, Capability: economy.CapabilityPaymentsOut,
		Kind: "payment_out", IdempotencyKey: key, Funding: economy.FundingTest, Counterparty: payee, Postings: []economy.MoneyPosting{
			{AccountID: p.company, AmountMinor: -pence}, {AccountID: p.partner, AmountMinor: pence}}})
}

func (p payments) companyPence(t *testing.T) int64 {
	t.Helper()
	b, err := p.money.Balance(context.Background(), p.ws, p.company)
	if err != nil {
		t.Fatal(err)
	}
	return b.AmountMinor
}

// posted is how many entries and postings the idempotency key wrote.
func (p payments) posted(t *testing.T, key string) (entries, postings int) {
	t.Helper()
	if err := p.pool.QueryRow(context.Background(), `SELECT count(DISTINCT e.id), count(mp.entry_id) FROM money_entries e
		LEFT JOIN money_postings mp ON mp.entry_id = e.id WHERE e.workspace_id = $1 AND e.idempotency_key = $2`, p.ws, key).Scan(&entries, &postings); err != nil {
		t.Fatal(err)
	}
	return entries, postings
}

func refusal(t *testing.T, err, want error) screening.Case {
	t.Helper()
	var r *screening.Refusal
	if !errors.Is(err, want) || !errors.As(err, &r) {
		t.Fatalf("answered %v, want %v", err, want)
	}
	return r.Case
}

func TestPayment_ToAListedPayeeIsRefusedBeforeAnyPostingAndACaseExists(t *testing.T) {
	ctx := context.Background()
	p := newPayments(t)

	_, err := p.payOut("pay-bnc", "Banco Nacional de Cuba", 12_000)
	c := refusal(t, err, screening.ErrBlocked)
	if e, ps := p.posted(t, "pay-bnc"); e != 0 || ps != 0 {
		t.Fatalf("a refused payment wrote %d entries and %d postings", e, ps)
	}
	if got := p.companyPence(t); got != 50_000 {
		t.Fatalf("the company holds %d pence after a refused payment, want 50000", got)
	}
	var status, direction, currency, matches string
	var amount int64
	if err := p.pool.QueryRow(ctx, `SELECT status, direction, amount_minor, currency, matches::text FROM compliance_cases
		WHERE workspace_id = $1 AND subject_kind = 'payment' AND subject_id = 'pay-bnc'`, p.ws).Scan(&status, &direction, &amount, &currency, &matches); err != nil {
		t.Fatalf("no compliance case: %v", err)
	}
	if status != screening.CaseBlocked || direction != "out" || amount != 12_000 || currency != "GBP" || !strings.Contains(matches, `"entry": "306"`) ||
		c.Status != screening.CaseBlocked {
		t.Fatalf("the case: %s %s %d %s %s", status, direction, amount, currency, matches)
	}

	// Tried again it is refused again, on the same case.
	c2 := refusal(t, func() error { _, err := p.payOut("pay-bnc", "Banco Nacional de Cuba", 12_000); return err }(), screening.ErrBlocked)
	var cases int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM compliance_cases WHERE workspace_id = $1 AND subject_id = 'pay-bnc'`, p.ws).Scan(&cases); err != nil || cases != 1 || c2.ID != c.ID {
		t.Fatalf("%d cases after a retry (%s, %s), %v", cases, c.ID, c2.ID, err)
	}
	// An outside payment names who it is to; and money out through one partner and in through another, netting to
	// nothing, is screened both ways.
	if _, err := p.payOut("pay-nobody", "", 1_000); err == nil || !strings.Contains(err.Error(), "counterparty") {
		t.Fatalf("an outside payment naming nobody: %v", err)
	}
	other, err := p.money.OpenMoneyAccount(ctx, economy.MoneyAccount{WorkspaceID: p.ws, Currency: economy.CurrencyGBP, Purpose: economy.MoneyPartner, Name: "other"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.money.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: p.ws, Capability: economy.CapabilityPaymentsOut, Kind: "payment_out",
		IdempotencyKey: "pay-netted", Funding: economy.FundingTest, Counterparty: "Banco Nacional de Cuba", Postings: []economy.MoneyPosting{
			{AccountID: p.partner, AmountMinor: 3_000}, {AccountID: other.ID, AmountMinor: -3_000}}})
	refusal(t, err, screening.ErrBlocked)
}

func TestPayment_ACloseMatchIsHeldUntilAnOperatorDecides(t *testing.T) {
	ctx := context.Background()
	p := newPayments(t)

	// One letter from a listed alias: held, and nothing moves until an operator releases it.
	c := refusal(t, func() error { _, err := p.payOut("pay-anwari", "Mohamad Taher Anwari", 7_500); return err }(), screening.ErrHeld)
	if e, _ := p.posted(t, "pay-anwari"); e != 0 || c.Status != screening.CaseHeld || len(c.Matches) == 0 || c.Matches[0].Entry != "AFG0009" {
		t.Fatalf("a held payment wrote %d entries; case %+v", e, c)
	}
	refusal(t, func() error { _, err := p.payOut("pay-anwari", "Mohamad Taher Anwari", 7_500); return err }(), screening.ErrHeld)
	released, err := p.screener.Decide(ctx, c.ID, true, "nicolai", "a different person: date of birth checked")
	if err != nil || released.Status != screening.CaseReleased || released.DecidedBy != "nicolai" || released.DecidedAt == nil {
		t.Fatalf("release: %+v, %v", released, err)
	}
	// The release is for that payment: the same id carrying more money is not what was released.
	refusal(t, func() error { _, err := p.payOut("pay-anwari", "Mohamad Taher Anwari", 40_000); return err }(), screening.ErrNotWhatWasReleased)
	if e, _ := p.posted(t, "pay-anwari"); e != 0 {
		t.Fatalf("a payment other than the one released wrote %d entries", e)
	}
	e, err := p.payOut("pay-anwari", "Mohamad Taher Anwari", 7_500)
	if err != nil || e.Counterparty != "Mohamad Taher Anwari" {
		t.Fatalf("a released payment: %+v, %v", e, err)
	}
	if got := p.companyPence(t); got != 42_500 {
		t.Fatalf("the company holds %d pence after paying £75.00 of £500.00", got)
	}
	if _, err := p.screener.Decide(ctx, c.ID, false, "nicolai", ""); !errors.Is(err, screening.ErrCaseDecided) {
		t.Fatalf("a decided case decided again: %v", err)
	}

	// An exact match on a low-quality alias is held, not blocked; refused, the payment never moves.
	held := refusal(t, func() error { _, err := p.payOut("pay-mudir", "Mudir", 1_000); return err }(), screening.ErrHeld)
	if _, err := p.screener.Decide(ctx, held.ID, false, "nicolai", "it is them"); err != nil {
		t.Fatal(err)
	}
	refusal(t, func() error { _, err := p.payOut("pay-mudir", "Mudir", 1_000); return err }(), screening.ErrRefused)
	if e, _ := p.posted(t, "pay-mudir"); e != 0 || p.companyPence(t) != 42_500 {
		t.Fatalf("a refused payment moved money: %d entries", e)
	}

	// A payee is screened when it is created, by name.
	if err := p.screener.ScreenPayee(ctx, p.ws, "payee-1", "NATIONAL BANK OF CUBA", economy.CapabilityPaymentsOut); !errors.Is(err, screening.ErrBlocked) {
		t.Fatalf("a listed payee: %v", err)
	}
	if err := p.screener.ScreenPayee(ctx, p.ws, "payee-2", "Ada Lovelace", economy.CapabilityPaymentsOut); err != nil {
		t.Fatalf("a payee on no list: %v", err)
	}
}

// B37.4 — DONE: with a list last downloaded 49 hours ago — over the 48 LENS_SCREENING_MAX_AGE_HOURS defaults to — a
// payment to someone on no list holds, writes nothing to the ledger, and opens a case; a fresh download clears
// payments again.
func TestPayment_StaleListsHoldEveryPaymentUntilTheyDownloadAgain(t *testing.T) {
	ctx := context.Background()
	p := newPayments(t)
	if _, err := p.pool.Exec(ctx, `UPDATE screening_lists SET loaded_at = now() - interval '49 hours' WHERE list = 'UK'`); err != nil {
		t.Fatal(err)
	}
	if age, stale, err := p.store.ListsAge(ctx); err != nil || !stale || age/time.Hour != 49 {
		t.Fatalf("lists 49 h old: %v old, stale %v, %v", age, stale, err)
	}

	c := refusal(t, func() error { _, err := p.payOut("pay-stale", "Ada Lovelace", 2_000); return err }(), screening.ErrListsStale)
	if e, ps := p.posted(t, "pay-stale"); e != 0 || ps != 0 || p.companyPence(t) != 50_000 {
		t.Fatalf("a payment held for stale lists wrote %d entries and %d postings", e, ps)
	}
	var outcome, status string
	if err := p.pool.QueryRow(ctx, `SELECT outcome, status FROM compliance_cases WHERE id = $1 AND subject_id = 'pay-stale'`, c.ID).
		Scan(&outcome, &status); err != nil || outcome != screening.OutcomeStale || status != screening.CaseHeld {
		t.Fatalf("the case: %s %s, %v", outcome, status, err)
	}

	// A fresh download clears payments again; the one held waits for an operator, and moves once released.
	if err := p.store.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.payOut("pay-fresh", "Ada Lovelace", 2_000); err != nil {
		t.Fatalf("a payment with fresh lists: %v", err)
	}
	if e, ps := p.posted(t, "pay-fresh"); e != 1 || ps != 2 || p.companyPence(t) != 48_000 {
		t.Fatalf("a payment with fresh lists wrote %d entries and %d postings", e, ps)
	}
	refusal(t, func() error { _, err := p.payOut("pay-stale", "Ada Lovelace", 2_000); return err }(), screening.ErrListsStale)
	if _, err := p.screener.Decide(ctx, c.ID, true, "nicolai", "lists downloaded again; Ada Lovelace is on neither"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.payOut("pay-stale", "Ada Lovelace", 2_000); err != nil || p.companyPence(t) != 46_000 {
		t.Fatalf("a released payment: %v", err)
	}
}
