package main

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/tenant"
)

// fixedPrices is a price adapter with one figure for ETH, in every currency.
type fixedPrices struct{ eth *big.Rat }

func (p fixedPrices) Price(context.Context, string, string) (*big.Rat, error) { return p.eth, nil }

// B48.28 on a local devnet and the real schema: ETH (a mock 18-decimal token standing in for it) moves into an agent's
// wallet — 12 of it, past what a BIGINT counts in wei — and later 5 of it out. The node's logs find both, each waits for
// the large confirmations a token other than USDC needs, and each is valued in the owner's currency as it confirms: the
// deposit at the test figure the server wires ($3,000), the send at $3,500. Neither moves the ledger, whose chain
// accounts are in USDC. The accountant's export then shows the send's gain, first in, first out.
func TestB4828_EveryTokenSeenAndValuedWhenItMoves(t *testing.T) {
	dir := os.Getenv("LENS_TEST_DEVNET_CONTRACTS")
	if dir == "" {
		t.Skip("LENS_TEST_DEVNET_CONTRACTS is not set: run scripts/devnet-contracts.sh DIR and set it to DIR")
	}
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b4828"
	store := economy.NewDualTokenStore(nil, pool, nil)
	const flagged = "0x00000000000000000000000000000000000B482F"
	store.SetScreener(screening.NewScreener(pool, b487Screening{strings.ToLower(flagged): partners.AddressSanctioned}))
	agent, err := store.CreateAgent(ctx, ws, "trader", "owner")
	if err != nil {
		t.Fatal(err)
	}
	wallet := smartaccount.Checksum("0x00000000000000000000000000000000000b4828")
	if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id,
		owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x', '0x', 'cred', $4, '0x', 'cdp', '')`,
		agent.ID, ws, wallet, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}

	chain := devnet(t)
	eth := chain.deploy(t, dir, "Ether", "ETH", 18)
	wei := func(whole int64) *big.Int {
		return new(big.Int).Mul(big.NewInt(whole), big.NewInt(1_000_000_000_000_000_000))
	}
	chain.send(t, devnetFunded, eth, "mint(address,uint256)", devnetFunded, wei(100))
	idx, err := newChainIndex(store, `{"webhook_secret": "whsec_b4828", "networks": [{"name": "base", "chain_id": 8453, "rpc_url": "`+
		chain.RPCURL+`", "usdc": "0x0000000000000000000000000000000000B4828C", "tokens": {"ETH": "`+eth+`"},
		"confirmations": 3, "large_confirmations": 6, "large_from_minor": 1000000000, "funding": "test"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	// step finds what moved, confirms it once it is six blocks deep, and answers the movement of tx.
	step := func(tx string) (amount, asset, status, currency string, value int64) {
		t.Helper()
		if n, err := idx.x.Poll(ctx, "base"); err != nil || n != 1 {
			t.Fatalf("poll = %d, %v; want the movement seen", n, err)
		}
		chain.mine(t, 2)
		if res, err := idx.x.Confirm(ctx, "base"); err != nil || res.Posted != 0 {
			t.Fatalf("ETH with 3 confirmations: %+v, %v; want it waiting for 6", res, err)
		}
		chain.mine(t, 3)
		if res, err := idx.x.Confirm(ctx, "base"); err != nil || res.Posted != 1 {
			t.Fatalf("ETH with 6 confirmations: %+v, %v; want it confirmed", res, err)
		}
		if err := pool.QueryRow(ctx, `SELECT amount_minor::text, asset, status, value_currency, COALESCE(value_minor, -1) FROM chain_movements
			WHERE tx_hash = $1`, tx).Scan(&amount, &asset, &status, &currency, &value); err != nil {
			t.Fatal(err)
		}
		return
	}

	deposit := chain.send(t, devnetFunded, eth, "transfer(address,uint256)", wallet, wei(12))
	if amount, asset, status, currency, value := step(deposit); amount != "12000000000000000000" || asset != "ETH" ||
		status != "confirmed" || currency != "USD" || value != 3_600_000 {
		t.Fatalf("the deposit's movement = %s %s %s, worth %d %s; want 12 ETH confirmed, worth 3600000 USD", amount, asset, status, value, currency)
	}

	idx.x.SetPrices(fixedPrices{big.NewRat(3500, 1)}, nil)
	chain.impersonate(t, wallet)
	send := chain.send(t, wallet, eth, "transfer(address,uint256)", "0x00000000000000000000000000000000000B4829", wei(5))
	if amount, asset, status, currency, value := step(send); amount != "5000000000000000000" || asset != "ETH" ||
		status != "confirmed" || currency != "USD" || value != 1_750_000 {
		t.Fatalf("the send's movement = %s %s %s, worth %d %s; want 5 ETH confirmed, worth 1750000 USD", amount, asset, status, value, currency)
	}
	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_entries WHERE workspace_id = $1`, ws).Scan(&entries); err != nil || entries != 0 {
		t.Fatalf("ledger entries = %d, %v; want none: the ledger's chain accounts are in USDC", entries, err)
	}

	// The send's cost is 5/12 of the deposit's $36,000: $15,000 against proceeds of $17,500.
	r := chi.NewRouter()
	mountAgentAccountRoutes(r, store, tenant.NewStore(pool))
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/wallet/tax", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT,
		UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var x economy.TaxExport
	if err := json.Unmarshal(w.Body.Bytes(), &x); err != nil || w.Code != http.StatusOK || len(x.Disposals) != 1 {
		t.Fatalf("the tax export = %d %s, %v; want the send", w.Code, w.Body, err)
	}
	if d := x.Disposals[0]; d.TxHash != send || d.QuantityMinor.String() != "5000000000000000000" || d.ProceedsMinor != 1_750_000 ||
		d.CostBasisMinor != 1_500_000 || d.GainMinor != 250_000 || x.Currency != "USD" || len(x.Unpriced) != 0 {
		t.Fatalf("the tax export = %+v; want the send's 5 ETH, proceeds 1750000, basis 1500000, gain 250000 USD", x)
	}

	// ETH from a flagged address is screened at its value, in a currency screening counts, and held as USDC is (B48.7).
	chain.send(t, devnetFunded, eth, "mint(address,uint256)", flagged, wei(1))
	chain.impersonate(t, flagged)
	held := chain.send(t, flagged, eth, "transfer(address,uint256)", wallet, wei(1))
	if n, err := idx.x.Poll(ctx, "base"); err != nil || n != 1 {
		t.Fatalf("poll = %d, %v; want the flagged deposit seen", n, err)
	}
	chain.mine(t, 5)
	if res, err := idx.x.Confirm(ctx, "base"); err != nil || res.Posted != 0 {
		t.Fatalf("the flagged deposit with 6 confirmations: %+v, %v; want it held", res, err)
	}
	var status, currency string
	var amount int64
	if err := pool.QueryRow(ctx, `SELECT m.status, c.currency, c.amount_minor FROM chain_movements m
		JOIN compliance_cases c ON c.subject_id = 'chain:' || m.id || ':1' WHERE m.tx_hash = $1`, held).Scan(&status, &currency, &amount); err != nil ||
		status != "quarantined" || currency != "USD" || amount != 350_000 {
		t.Fatalf("the flagged deposit is %s, its case %d %s, %v; want it quarantined, screened at 350000 USD", status, amount, currency, err)
	}
}
