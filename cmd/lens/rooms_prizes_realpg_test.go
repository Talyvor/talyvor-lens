package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/ecbrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/rooms"
	"github.com/talyvor/lens/internal/taxprofile"
	"github.com/talyvor/lens/internal/tenant"
)

// B32.35 — awarding a $50 prize writes one billed prize row, the owner's workspace the buyer and the room wallet the
// agent, and a perpetual commercial licence for the owner; once the owner's invoice is paid the journal credits the
// winner 42,500,000 and the fee 7,500,000 µUSD — paid, as the testers pay it (B17.171), through the synthetic bill-pay
// route with the receipt main.go issues, which answered 500 behind PgBouncer until B39.1. A prize above the room's remaining budget is refused; a prize past its
// deadline closes, cannot be awarded and writes no row.
func TestRooms_PrizeIsAPurchaseOfTheWinningContribution(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, author = "ws-b3235-owner", "ws-b3235-author"
	for _, ws := range []string{owner, author} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance, cash_backed_ulxc) VALUES ($1, 2000000000, 2000000000)`, owner); err != nil {
		t.Fatal(err)
	}
	bank := economy.NewDualTokenStore(nil, pool, nil)
	marketStore := market.NewStore(pool)
	marketStore.SetReceipts(market.Receipts{Supplier: market.Supplier{LegalName: "TALYVOR LTD"},
		Buyers: taxprofile.NewStore(pool, partners.NewRegistry(nil)), Rates: ecbrate.New(pool, ecbrate.DailyURL)})
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)
	store.SetKeys(tenant.NewStore(pool))
	bank.SetRoomBudgets(store)
	var metered roomWalletMeter
	r := chi.NewRouter()
	mountRoomRoutes(r, store)
	mountAgentAccountRoutes(r, bank, tenant.NewStore(pool))
	mountRoomPrizeRoutes(r, store, &metered, bank)
	const syntheticKey = "b17171-synthetic-key"
	mountSyntheticDueRoutes(r, syntheticKey, syntheticDeps{audit: pool, due: syntheticDueDeps{db: pool, bills: marketStore}})
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if ws == "" {
			req.Header.Set(syntheticKeyHeader, syntheticKey)
		} else {
			req = req.WithContext(auth.WithAuthContext(req.Context(),
				&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	must := func(want int, ws, method, path, body string) string {
		t.Helper()
		code, out := call(ws, method, path, body)
		if code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, code, out, want)
		}
		return out
	}

	// The owner's room has a budget of 600 LXC ($60) a month; the author contributes the work that will win.
	var room rooms.Detail
	_ = json.Unmarshal([]byte(must(http.StatusCreated, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms",
		`{"title":"Launch film"}`)), &room)
	wallet := room.Wallet.AgentID
	must(http.StatusOK, owner, http.MethodPost, "/v1/workspaces/"+owner+"/agents/"+wallet+"/fund", `{"amount_ulxc":100000000}`)
	must(http.StatusOK, owner, http.MethodPut, "/v1/workspaces/"+owner+"/agents/"+wallet+"/rules",
		`{"monthly_limit_ulxc":600000000,"max_per_request_ulxc":1000000000,"approval_above_ulxc":1000000000}`)
	must(http.StatusCreated, author, http.MethodPost, "/v1/rooms/"+room.ID+"/join", `{"terms_version":1}`)
	var c rooms.Contribution
	_ = json.Unmarshal([]byte(must(http.StatusCreated, author, http.MethodPost, "/v1/rooms/"+room.ID+"/contributions",
		`{"kind":"prompt","title":"Opening shot","artifact":{"template":"An opening shot for {{product}}.","model":"gpt-4o-mini"}}`)), &c)

	type useRow struct {
		buyer, agent, room, actor, charge, kind, licence string
		price                                            int64
	}
	uses := func() []useRow {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT buyer_workspace_id, agent_id, room_id, actor_workspace_id, charge, use_kind, COALESCE(licence_id, ''),
			price_ulxc FROM market_uses WHERE listing_id = $1 ORDER BY used_at, id`, c.ListingID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []useRow
		for rows.Next() {
			var u useRow
			if err := rows.Scan(&u.buyer, &u.agent, &u.room, &u.actor, &u.charge, &u.kind, &u.licence, &u.price); err != nil {
				t.Fatal(err)
			}
			out = append(out, u)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	// $70 is more than the room's $60 budget has left: refused, and nothing is posted.
	if code, body := call(owner, http.MethodPost, "/v1/rooms/"+room.ID+"/prizes",
		`{"title":"Best opening","amount_usd_micros":70000000,"deadline":"`+deadline+`"}`); code != http.StatusForbidden || !strings.Contains(body, "budget") {
		t.Fatalf("a $70 prize on a $60 budget = %d %s, want 403 naming the budget", code, body)
	}

	// A $50 prize is posted and the room is told; only the owner posts one.
	var prize rooms.Prize
	_ = json.Unmarshal([]byte(must(http.StatusCreated, owner, http.MethodPost, "/v1/rooms/"+room.ID+"/prizes",
		`{"title":"Best opening","criteria":"The shot we open the film with.","amount_usd_micros":50000000,"deadline":"`+deadline+`"}`)), &prize)
	if prize.Status != rooms.PrizeOpen || prize.Message == nil || prize.Message.Kind != rooms.KindPrize || !strings.Contains(prize.Message.Body, "$50.00") {
		t.Fatalf("the posted prize = %+v, want it open with a prize message naming $50.00", prize)
	}
	if code, _ := call(author, http.MethodPost, "/v1/rooms/"+room.ID+"/prizes",
		`{"title":"Mine","amount_usd_micros":1000000,"deadline":"`+deadline+`"}`); code != http.StatusForbidden {
		t.Fatalf("a member posting a prize = %d, want 403", code)
	}

	// The owner awards it to the author's contribution: one billed prize row on the owner's bill, bought by the room's
	// wallet, and a perpetual commercial licence for the owner.
	var award rooms.Award
	_ = json.Unmarshal([]byte(must(http.StatusOK, owner, http.MethodPost, "/v1/rooms/"+room.ID+"/prizes/"+prize.ID+"/award",
		`{"contribution_id":"`+c.ID+`"}`)), &award)
	got := uses()
	if len(got) != 1 || got[0] != (useRow{buyer: owner, agent: wallet, room: room.ID, actor: owner, charge: market.ChargeBilled,
		kind: market.UseKindPrize, licence: award.Licence.ID, price: 500_000_000}) {
		t.Fatalf("the award wrote %+v, want one billed prize row of 500,000,000 µLXC bought by %s on %s with licence %s", got, owner, wallet, award.Licence.ID)
	}
	var licence, kind, source, buyer string
	var endsAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT licence, kind, source, buyer_workspace_id, ends_at FROM market_licences WHERE id = $1`, award.Licence.ID).
		Scan(&licence, &kind, &source, &buyer, &endsAt); err != nil {
		t.Fatal(err)
	}
	if licence != market.LicenceCommercial || kind != "prize" || source != market.SourcePrize || buyer != owner || endsAt != nil {
		t.Fatalf("the owner's licence = %s %s %s for %s ending %v, want a perpetual commercial prize licence for %s", licence, kind, source, buyer, endsAt, owner)
	}
	if award.Prize.Status != rooms.PrizeAwarded || award.Prize.WinnerWorkspaceID != author || award.Prize.UseID != award.Licence.UseID ||
		award.Prize.Message == nil || !strings.Contains(award.Prize.Message.Body, "Opening shot") || len(metered) != 1 || metered[0] != award.Licence.UseID {
		t.Fatalf("the award = %+v, metered %v; want it awarded to %s, announced, and its row on the owner's bill", award, metered, author)
	}
	if code, _ := call(owner, http.MethodPost, "/v1/rooms/"+room.ID+"/prizes/"+prize.ID+"/award", `{"contribution_id":"`+c.ID+`"}`); code != http.StatusConflict || len(uses()) != 1 {
		t.Fatalf("awarding it again = %d with %d rows, want 409 and still one row", code, len(uses()))
	}

	// The owner's bill is paid as the testers pay it: the prize clears 85/15 — 42,500,000 µUSD to the winner, 7,500,000
	// to Talyvor — and the paid bill has its receipt.
	var paid struct {
		InvoiceID   string `json:"invoice_id"`
		UsesCleared int    `json:"uses_cleared"`
	}
	_ = json.Unmarshal([]byte(must(http.StatusOK, "", http.MethodPost, "/v1/synthetic/workspaces/"+owner+"/marketplace/bill/pay", "")), &paid)
	var receipts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM market_receipts WHERE invoice_id = $1`, paid.InvoiceID).Scan(&receipts); err != nil ||
		paid.UsesCleared != 1 || receipts != 1 {
		t.Fatalf("paying the owner's bill = %+v with %d receipts, %v; want the prize cleared and one receipt", paid, receipts, err)
	}
	j, err := marketStore.JournalFor(ctx, award.Licence.UseID)
	if err != nil || len(j) != 1 || j[0].Kind != market.JournalClear {
		t.Fatalf("the prize's journal = %+v, %v; want one clear entry", j, err)
	}
	postings := map[string]int64{}
	for _, p := range j[0].Postings {
		postings[p.Account] = p.AmountUSDMicros
	}
	if postings[market.AccountStripeClearing] != 50_000_000 || postings[market.AccountMarketFee] != -7_500_000 ||
		postings[market.SellerHoldback(author)] != -42_500_000 {
		t.Fatalf("the prize's clear postings = %v; want +50,000,000 clearing, −7,500,000 Talyvor's fee, −42,500,000 to the winner", postings)
	}

	// A $5 prize nobody wins by its deadline closes, the room is told, and awarding it is refused with no row written.
	var late rooms.Prize
	_ = json.Unmarshal([]byte(must(http.StatusCreated, owner, http.MethodPost, "/v1/rooms/"+room.ID+"/prizes",
		`{"title":"Best closing","amount_usd_micros":5000000,"deadline":"`+deadline+`"}`)), &late)
	if n, err := store.ClosePrizes(ctx, "", time.Now().Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("closing prizes past their deadline = %d, %v; want 1", n, err)
	}
	if code, _ := call(owner, http.MethodPost, "/v1/rooms/"+room.ID+"/prizes/"+late.ID+"/award", `{"contribution_id":"`+c.ID+`"}`); code != http.StatusConflict || len(uses()) != 1 {
		t.Fatalf("awarding a closed prize = %d with %d rows, want 409 and still one row", code, len(uses()))
	}
	var list struct {
		Prizes []rooms.Prize `json:"prizes"`
	}
	_ = json.Unmarshal([]byte(must(http.StatusOK, author, http.MethodGet, "/v1/rooms/"+room.ID+"/prizes", "")), &list)
	if len(list.Prizes) != 2 || list.Prizes[0].ID != late.ID || list.Prizes[0].Status != rooms.PrizeClosed || list.Prizes[1].Status != rooms.PrizeAwarded {
		t.Fatalf("the room's prizes = %+v, want the closed one then the awarded one", list.Prizes)
	}
	var announced int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM room_messages WHERE room_id = $1 AND kind = 'prize'`, room.ID).Scan(&announced); err != nil {
		t.Fatal(err)
	}
	if announced != 4 { // posted, awarded, posted, closed
		t.Fatalf("the room got %d prize messages, want 4: two posts, the award and the close", announced)
	}
}
