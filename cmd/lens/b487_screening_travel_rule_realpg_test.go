package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/sha3"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/circle"
	"github.com/talyvor/lens/internal/money/circle/circlefake"
	"github.com/talyvor/lens/internal/money/smartaccount"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// b487Screening is the Test screening provider with addresses flagged, for every capability.
type b487Screening map[string]string

func (f b487Screening) Screening(context.Context, string) (partners.ScreeningProvider, error) {
	return partners.TestScreeningProvider{Addresses: f}, nil
}

// b487Key is a key in a wallet the person holds themselves, and its address.
func b487Key(t *testing.T) (*secp256k1.PrivateKey, string) {
	t.Helper()
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	h := sha3.NewLegacyKeccak256()
	h.Write(k.PubKey().SerializeUncompressed()[1:])
	return k, smartaccount.Checksum(fmt.Sprintf("0x%x", h.Sum(nil)[12:]))
}

// b487PersonalSign is k's personal_sign (EIP-191) of message, r‖s‖v.
func b487PersonalSign(k *secp256k1.PrivateKey, message string) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(message)) + message))
	c := secpecdsa.SignCompact(k, h.Sum(nil), false) // v‖r‖s, v 27 or 28
	return append(c[1:], c[0])
}

// B48.7 — every USDC address an agent pays is screened before anything is held or sent: a mixer's is refused. A
// payment to an address a regulated provider holds carries the payer's and the payee's identity to that provider; one
// of more than €1,000 to a self-hosted address waits until the owner proves, once, that they hold its key. Every check
// is on the ledger's rows and on what Circle was asked.
// b487Payer is an agent with 5,000 USDC in its Circle wallet, paying through a store whose screening flags flagged
// and whose Travel Rule adapter is travel.
type b487Payer struct {
	store          *economy.DualTokenStore
	screener       *screening.Screener
	circle         *circlefake.Fake
	agentID        string
	walletRef      string
	partnerAccount string
	pushes         b468Pushes
}

func newB487Payer(t *testing.T, pool *pgxpool.Pool, ws string, flagged b487Screening, travel partners.TravelRuleProvider) b487Payer {
	t.Helper()
	ctx := context.Background()
	f := circlefake.New(t)
	reg := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, flagged)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(reg)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetTravelRule(travel)
	pushes := make(b468Pushes, 4)
	store.SetNativePush(pushes)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation, push_token)
		VALUES ($1, $2, 'test', '\x00', '\x00', 'tok')`, "dev_"+ws, ws); err != nil {
		t.Fatal(err)
	}
	layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, ""), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
		Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool), Switches: money.SwitchSet{
			{Capability: money.CapWallets, Partner: circle.Name}: true, {Capability: money.CapTransfers, Partner: circle.Name}: true}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := circle.New(circle.Config{BaseURL: f.URL, APIKey: f.APIKey, EntitySecret: f.EntitySecret, WalletSetID: f.WalletSetID,
		Blockchain: circlefake.Blockchain, Allowlist: store.AddressAllowlist()})
	if err != nil {
		t.Fatal(err)
	}
	if err := layer.Register(a); err != nil {
		t.Fatal(err)
	}
	store.SetMoneyLayer(layer)

	agent, err := store.CreateAgent(ctx, ws, "Payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.AgentWallet(ctx, ws, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	b := b487Payer{store: store, screener: screener, circle: f, agentID: agent.ID, pushes: pushes}
	if err := pool.QueryRow(ctx, `SELECT a.partner_account_ref, p.id FROM money_accounts a JOIN money_accounts p ON p.workspace_id = a.workspace_id
		AND p.partner_account_ref = a.partner_account_ref AND p.purpose = 'partner' WHERE a.id = $1`, w.AccountID).Scan(&b.walletRef, &b.partnerAccount); err != nil {
		t.Fatal(err)
	}
	f.Deposit(b.walletRef, "tx-in-"+ws, money.Amount{Minor: 5000_000000, Asset: money.USDC})
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: b.partnerAccount, AmountMinor: -5000_000000}, {AccountID: w.AccountID, AmountMinor: 5000_000000}}}); err != nil {
		t.Fatal(err)
	}
	return b
}

// save is a saved, confirmed payee at address.
func (b b487Payer) save(t *testing.T, pool *pgxpool.Pool, ws, name, address string) economy.OutsidePayee {
	t.Helper()
	ctx := context.Background()
	p, err := b.store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "GB", Network: "base", Address: address})
	if err != nil {
		t.Fatalf("save %s: %v", name, err)
	}
	// The owner approved it and confirmed they called its holder, with a passkey (B30.16 and B41.111, tested there).
	if _, err := pool.Exec(ctx, `UPDATE money_payees SET confirmed_at = now(), confirmed_by = 'passkey',
		called_at = now(), called_by = 'passkey' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	return p
}

func (b b487Payer) pay(ws string, payee economy.OutsidePayee, key string, minor int64) (economy.OutsidePayment, error) {
	return b.store.PayOutside(context.Background(), ws, economy.OutsidePayment{AgentID: b.agentID, PayeeID: payee.ID, Currency: money.USDC,
		AmountMinor: minor, Reference: "Invoice " + key, IdempotencyKey: key})
}

func TestB487_AnAgentsUSDCPaymentIsScreenedAndCarriesTheTravelRule(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b487-out"
	const mixer, exchange = "0x00000000000000000000000000000000000B4870", "0x00000000000000000000000000000000000B4871"
	travel := &partners.TestTravelRuleProvider{VASPs: map[string]string{strings.ToLower(exchange): "Example Exchange Ltd"}}
	b := newB487Payer(t, pool, ws, b487Screening{strings.ToLower(mixer): partners.AddressMixer}, travel)
	store, f, walletRef := b.store, b.circle, b.walletRef
	save := func(name, address string) economy.OutsidePayee { t.Helper(); return b.save(t, pool, ws, name, address) }
	pay := func(payee economy.OutsidePayee, key string, minor int64) (economy.OutsidePayment, error) {
		return b.pay(ws, payee, key, minor)
	}
	nothingMoved := func(key string) {
		t.Helper()
		if got := b468Postings(t, pool, ws, key); got != "" {
			t.Fatalf("payment %s posted %q, want nothing held or paid", key, got)
		}
	}

	// A mixer's address is refused before anything is held or Circle is asked to sign.
	if _, err := pay(save("Mixer", mixer), "mixer", 10_000000); !errors.As(err, new(*screening.Refusal)) || !errors.Is(err, screening.ErrBlocked) {
		t.Fatalf("pay a mixer's address = %v, want refused by screening", err)
	}
	nothingMoved("mixer")
	if f.Outbound() != 0 {
		t.Fatalf("Circle was asked %d times, want never", f.Outbound())
	}

	// An address at a regulated provider: the payer's and the payee's identity go to it with the payment.
	p, err := pay(save("Ada Lovelace", exchange), "vasp", 25_000000)
	if err != nil || p.Status != economy.PaymentSent {
		t.Fatalf("pay an exchange's address = %+v, %v; want sent", p, err)
	}
	var vasp, originator, beneficiary string
	if err := pool.QueryRow(ctx, `SELECT vasp, originator::text, beneficiary::text FROM travel_rule_transfers WHERE payment_id = $1 AND workspace_id = $2`,
		p.ID, ws).Scan(&vasp, &originator, &beneficiary); err != nil {
		t.Fatalf("the Travel Rule record of the payment: %v", err)
	}
	if vasp != "Example Exchange Ltd" || !strings.Contains(originator, walletRef) ||
		!strings.Contains(beneficiary, "Ada Lovelace") || !strings.Contains(strings.ToLower(beneficiary), strings.ToLower(exchange)) {
		t.Fatalf("the Travel Rule record = %s, %s, %s; want the payer's wallet and Ada at the exchange's address", vasp, originator, beneficiary)
	}
	if sent := travel.Sent(); len(sent) != 1 || sent[0].ID != p.ID || sent[0].Beneficiary.Name != "Ada Lovelace" {
		t.Fatalf("the Travel Rule provider was sent %+v, want this payment once", sent)
	}

	// More than €1,000 to a self-hosted address waits for the owner to prove, once, that it is theirs.
	key, own := b487Key(t)
	mine := save("My own wallet", own)
	if _, err := pay(mine, "self-1", 1500_000000); !errors.Is(err, economy.ErrAddressProofNeeded) {
		t.Fatalf("pay 1,500 USDC to a self-hosted address = %v, want the owner asked to prove it is theirs", err)
	}
	nothingMoved("self-1")
	msg := economy.AddressProofMessage(ws, "base", strings.ToLower(own))
	other, _ := b487Key(t)
	if err := store.ProveAddressControl(ctx, ws, mine.ID, b487PersonalSign(other, msg)); !errors.Is(err, economy.ErrAddressProof) {
		t.Fatalf("a proof signed with another key = %v, want refused", err)
	}
	if err := store.ProveAddressControl(ctx, ws, mine.ID, b487PersonalSign(key, msg)); err != nil {
		t.Fatalf("the owner's proof: %v", err)
	}
	for _, k := range []string{"self-2", "self-3"} {
		if p, err := pay(mine, k, 1500_000000); err != nil || p.Status != economy.PaymentSent {
			t.Fatalf("pay 1,500 USDC to the proved address (%s) = %+v, %v; want sent without asking again", k, p, err)
		}
	}
	if got, want := b468Postings(t, pool, ws, "self-2"), "payment_out_hold agent -1500000000; payment_out_hold hold 1500000000"; got != want {
		t.Fatalf("1,500 USDC to the proved address posted %q, want %q", got, want)
	}
	// Nor is the proof dodged in pieces: what is already paid to the address counts.
	_, split := b487Key(t)
	pieces := save("Split", split)
	if p, err := pay(pieces, "split-1", 600_000000); err != nil || p.Status != economy.PaymentSent {
		t.Fatalf("pay 600 USDC to a self-hosted address = %+v, %v; want sent", p, err)
	}
	if _, err := pay(pieces, "split-2", 600_000000); !errors.Is(err, economy.ErrAddressProofNeeded) {
		t.Fatalf("a second 600 USDC to it = %v, want the owner asked to prove it is theirs: 1,200 in all", err)
	}
	nothingMoved("split-2")
	if n := len(travel.Sent()); n != 1 {
		t.Fatalf("the Travel Rule provider was sent %d transfers, want only the exchange's: a self-hosted address has no provider", n)
	}
}

// b487Node is a node that has mined one USDC transfer, at block 100, and is at block head.
type b487Node struct {
	tx, token, from, to string
	value               int64
	head                int64
}

func (n *b487Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pad := func(a string) string {
		return "0x" + strings.Repeat("0", 24) + strings.ToLower(strings.TrimPrefix(a, "0x"))
	}
	var result any
	switch req.Method {
	case "eth_blockNumber":
		result = fmt.Sprintf("0x%x", n.head)
	case "eth_getTransactionReceipt":
		result = map[string]any{"status": "0x1", "blockNumber": "0x64", "blockHash": "0x" + strings.Repeat("b4", 32), "logs": []map[string]any{{
			"address": n.token, "topics": []string{smartaccount.TransferTopic, pad(n.from), pad(n.to)},
			"data": fmt.Sprintf("0x%064x", n.value), "transactionHash": n.tx}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

// B48.7 — a deposit into an agent's wallet from a sanctioned address is quarantined once it has its confirmations:
// never posted to the ledger, the owner told once, a held case for an operator. Released, it posts once.
func TestB487_ADepositFromAFlaggedAddressIsQuarantinedAndTheOwnerTold(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b487-in"
	const sanctioned = "0x00000000000000000000000000000000000B4872"
	screener := screening.NewScreener(pool, b487Screening{strings.ToLower(sanctioned): partners.AddressSanctioned})
	store := economy.NewDualTokenStore(nil, pool, nil)
	reg := partners.NewRegistry(nil)
	store.SetAccountPartners(reg)
	store.SetScreener(screener)
	pushes := make(b468Pushes, 4)
	store.SetNativePush(pushes)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_device_keys (id, workspace_id, platform, public_key, attestation, push_token)
		VALUES ($1, $2, 'test', '\x00', '\x00', 'tok')`, "dev_"+ws, ws); err != nil {
		t.Fatal(err)
	}
	agent, err := store.CreateAgent(ctx, ws, "treasurer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	wallet := smartaccount.Checksum("0x00000000000000000000000000000000000b4873")
	if _, err := pool.Exec(ctx, `INSERT INTO agent_smart_accounts (agent_id, workspace_id, address, factory, implementation, owner_credential_id,
		owner_public_key, agent_signer, provider, solana_address) VALUES ($1, $2, $3, '0x', '0x', 'cred', $4, '0x', 'cdp', '')`,
		agent.ID, ws, wallet, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	node := &b487Node{tx: "0x" + strings.Repeat("a7", 32), token: smartaccount.Checksum("0x00000000000000000000000000000000000c0de1"),
		from: sanctioned, to: wallet, value: 40_000000, head: 100}
	srv := httptest.NewServer(node)
	defer srv.Close()
	x, err := store.NewChainIndexer([]economy.ChainNetwork{{Name: "base", Chain: smartaccount.Chain{ID: 8453, RPCURL: srv.URL}, USDC: node.token,
		Confirmations: 3, Funding: economy.FundingTest}})
	if err != nil {
		t.Fatal(err)
	}
	held := func() int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(p.amount_minor), 0)::bigint FROM money_postings p
			JOIN money_accounts a ON a.id = p.account_id WHERE a.agent_id = $1 AND a.purpose = 'chain'`, agent.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	status := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM chain_movements WHERE tx_hash = $1`, node.tx).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	if n, err := x.Observe(ctx, "base", node.tx, "webhook"); err != nil || n != 1 {
		t.Fatalf("observe the deposit = %d, %v; want it seen", n, err)
	}
	node.head = 102
	if res, err := x.Confirm(ctx, "base"); err != nil || res.Posted != 0 {
		t.Fatalf("confirm a deposit from a sanctioned address = %+v, %v; want nothing posted", res, err)
	}
	if s, n := status(), held(); s != "quarantined" || n != 0 {
		t.Fatalf("the deposit is %s and the ledger holds %d; want it quarantined, nothing posted", s, n)
	}
	if m := pushes.next(t); m.Data["type"] != "deposit_quarantined" || !strings.Contains(m.Body, "40.00") {
		t.Fatalf("the owner was told %+v, want the 40 USDC deposit held for review", m)
	}
	node.head = 103
	if res, err := x.Confirm(ctx, "base"); err != nil || res.Posted != 0 || status() != "quarantined" {
		t.Fatalf("confirm again while it is held = %+v, %v; want it still quarantined", res, err)
	}
	select {
	case m := <-pushes:
		t.Fatalf("the owner was told again: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
	cases, err := screener.Cases(ctx, screening.CaseHeld, 10)
	if err != nil || len(cases) != 1 || cases[0].WorkspaceID != ws || cases[0].Direction != "in" {
		t.Fatalf("held cases = %+v, %v; want the deposit's, for an operator", cases, err)
	}
	if _, err := screener.Decide(ctx, cases[0].ID, true, "operator@talyvor", "the sender is the owner's own exchange account"); err != nil {
		t.Fatal(err)
	}
	if res, err := x.Confirm(ctx, "base"); err != nil || res.Posted != 1 || held() != 40_000000 {
		t.Fatalf("confirm once released = %+v, %v with %d held; want the deposit posted once", res, err, held())
	}
}

// b487FailingTravelRule is a Travel Rule adapter whose sends never go.
type b487FailingTravelRule struct {
	*partners.TestTravelRuleProvider
}

func (b487FailingTravelRule) Send(context.Context, partners.TravelRuleTransfer) (string, error) {
	return "", errors.New("the provider did not answer")
}

// B48.7 — a payment whose Travel Rule data keeps failing to go ends as failed after a bounded number of tries, its
// money back and the owner told; and each retry screens the address again, so one flagged meanwhile is refused.
func TestB487_ATravelRuleSendThatKeepsFailingEndsThePaymentAndARetryScreensAgain(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b487-retry"
	const exchange, other = "0x00000000000000000000000000000000000B4874", "0x00000000000000000000000000000000000B4875"
	flagged := b487Screening{}
	b := newB487Payer(t, pool, ws, flagged, b487FailingTravelRule{&partners.TestTravelRuleProvider{VASPs: map[string]string{
		strings.ToLower(exchange): "Example Exchange Ltd", strings.ToLower(other): "Other Exchange Ltd"}}})

	p, err := b.pay(ws, b.save(t, pool, ws, "Ada Lovelace", exchange), "retry", 25_000000)
	if err != nil || p.Status != economy.PaymentPending {
		t.Fatalf("pay while the Travel Rule data cannot go = %+v, %v; want it waiting, held", p, err)
	}
	for i := 2; i <= economy.TravelRuleMaxAttempts; i++ {
		if p, err = b.store.RefreshOutsidePayment(ctx, ws, "", p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if p.Status != economy.PaymentFailed || !strings.Contains(p.Detail, "Travel Rule") {
		t.Fatalf("after %d tries the payment is %+v; want it failed for the Travel Rule", economy.TravelRuleMaxAttempts, p)
	}
	if got, want := b468Postings(t, pool, ws, "retry"), "payment_out_hold agent -25000000; payment_out_hold hold 25000000; "+
		"payment_out_failed hold -25000000; payment_out_failed agent 25000000"; got != want {
		t.Fatalf("the payment posted %q, want %q: held, then given back", got, want)
	}
	if m := b.pushes.next(t); m.Data["type"] != "payment_failed" || m.Data["payment_id"] != p.ID {
		t.Fatalf("the owner was told %+v, want the payment failed", m)
	}
	if b.circle.Outbound() != 0 {
		t.Fatalf("Circle was asked %d times, want never: the identity data never went", b.circle.Outbound())
	}

	// The address is flagged after the first try: the retry screens it again and refuses it.
	q, err := b.pay(ws, b.save(t, pool, ws, "Other", other), "rescreen", 10_000000)
	if err != nil || q.Status != economy.PaymentPending {
		t.Fatalf("pay = %+v, %v; want it waiting", q, err)
	}
	flagged[strings.ToLower(other)] = partners.AddressSanctioned
	if q, err = b.store.RefreshOutsidePayment(ctx, ws, "", q.ID); err != nil || q.Status != economy.PaymentFailed || !strings.Contains(q.Detail, "screening") {
		t.Fatalf("the retry once the address is flagged = %+v, %v; want it refused by screening", q, err)
	}
}

// B48.7 — a payment in on a partner's rail (Circle's webhook here) from a flagged address is quarantined, as a chain
// deposit is: nothing posts (moneyLedger.PostCredit takes the notification), the owner is told once and an operator has a held case;
// released, it posts once.
func TestB487_AWebhookPaymentInFromAFlaggedAddressIsQuarantined(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b487-webhook"
	const mixer = "0x00000000000000000000000000000000000B4876"
	b := newB487Payer(t, pool, ws, b487Screening{strings.ToLower(mixer): partners.AddressMixer}, nil)
	deposit := economy.InboundPayment{PartnerAccountRef: b.walletRef, PartnerRef: "dep-b487", Payer: mixer, AmountMinor: 40_000000,
		Currency: money.USDC, Funding: economy.FundingTest, Partner: circle.Name}
	posted := func() string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(a.purpose || ' ' || p.amount_minor, '; ' ORDER BY p.line), '') FROM money_entries e
			JOIN money_postings p ON p.entry_id = e.id JOIN money_accounts a ON a.id = p.account_id
			WHERE e.workspace_id = $1 AND e.idempotency_key = 'payment_in:dep-b487'`, ws).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	for i := 0; i < 2; i++ { // the partner sends it twice
		if e, err := b.store.ReceivePayment(ctx, deposit); !errors.Is(err, economy.ErrDepositQuarantined) || e.WorkspaceID != ws {
			t.Fatalf("Circle's notification of a mixer's payment in = %+v, %v; want it quarantined in %s", e, err, ws)
		}
	}
	if got := posted(); got != "" {
		t.Fatalf("the quarantined payment in posted %q, want nothing", got)
	}
	if m := b.pushes.next(t); m.Data["type"] != "deposit_quarantined" || !strings.Contains(m.Body, "40.00") {
		t.Fatalf("the owner was told %+v, want the 40 USDC held for review", m)
	}
	select {
	case m := <-b.pushes:
		t.Fatalf("the owner was told again: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
	cases, err := b.screener.Cases(ctx, screening.CaseHeld, 10)
	var held *screening.Case
	for i := range cases {
		if cases[i].WorkspaceID == ws {
			held = &cases[i]
		}
	}
	if err != nil || held == nil || held.Direction != "in" || held.Name != mixer {
		t.Fatalf("held cases = %+v, %v; want the payment in's, for an operator", cases, err)
	}
	if n, err := b.store.PostReleasedDeposits(ctx); err != nil || n != 0 || posted() != "" {
		t.Fatalf("post released payments in before the decision = %d, %v; want nothing", n, err)
	}
	if _, err := b.screener.Decide(ctx, held.ID, true, "operator@talyvor", "the owner's own exchange account"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := b.store.PostReleasedDeposits(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := posted(), "partner -40000000; agent 40000000"; got != want {
		t.Fatalf("released, the payment in posted %q, want %q once", got, want)
	}
}

// B48.7 — earnings paid out in USDC take an agent's send's path: the payout address is screened, so a flagged one is
// refused and its money given back; and with the Travel Rule on, more than 1,000 USDC to a self-hosted address is held
// until its recipient proves, once, that they hold its key, and then sent.
func TestB487_AUSDCEarningsPayoutIsScreenedByAddressAndTakesTheTravelRule(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, flaggedSeller, ownSeller = "ws-b487-earnings", "ws-b487-flagged-seller", "ws-b487-own-seller"
	const mixer = "0x00000000000000000000000000000000000b4877"
	b := newB487Payer(t, pool, ws, b487Screening{mixer: partners.AddressMixer}, &partners.TestTravelRuleProvider{})
	key, own := b487Key(t)
	own = strings.ToLower(own)
	for seller, address := range map[string]string{flaggedSeller: mixer, ownSeller: own} {
		if d, err := b.store.SavePayoutDetails(ctx, seller, economy.PayoutDetails{Asset: money.USDC, Holder: "Sam Seller", Network: "base",
			Address: address}); err != nil || d.Status != economy.PayoutDetailsVerified {
			t.Fatalf("%s saves its USDC payout details = %+v, %v; want verified", seller, d, err)
		}
	}
	sale := func(seller, source string, minor int64) economy.EarningsPayout {
		t.Helper()
		return b4611Pay(t, b.store, ws, economy.Earnings{Kind: economy.EarningsSale, Source: source, AgentID: b.agentID, Currency: money.USDC,
			Shares: []economy.EarningsShare{{WorkspaceID: seller, AmountMinor: minor}}})[0]
	}

	// A mixer's payout address is refused: the money goes back to the account it came from.
	p := sale(flaggedSeller, "sale:b487-mixer", 10_000000)
	if p.Status != economy.PaymentFailed || !strings.Contains(p.Detail, "screening") {
		t.Fatalf("a payout to a mixer's address = %+v; want refused by screening", p)
	}
	if got := b4611Postings(t, pool, p.ID); strings.Contains(got, "partner") {
		t.Fatalf("the refused payout posted %q, want nothing paid out", got)
	}

	// 1,500 USDC to a self-hosted address waits for its recipient's proof, and is paid once it is given.
	p = sale(ownSeller, "sale:b487-own", 1500_000000)
	if p.Status != economy.PayoutHeld || !strings.Contains(p.Detail, "address-proof") {
		t.Fatalf("1,500 USDC to a self-hosted address = %+v; want it held for the recipient's proof", p)
	}
	if got := b4611Postings(t, pool, p.ID); strings.Contains(got, "partner") {
		t.Fatalf("the held payout posted %q, want nothing paid out", got)
	}
	msg := economy.AddressProofMessage(ownSeller, "base", own)
	if other, _ := b487Key(t); !errors.Is(b.store.ProvePayoutAddress(ctx, ownSeller, b487PersonalSign(other, msg)), economy.ErrAddressProof) {
		t.Fatal("a proof signed with another key was taken")
	}
	if err := b.store.ProvePayoutAddress(ctx, ownSeller, b487PersonalSign(key, msg)); err != nil {
		t.Fatalf("the recipient's proof: %v", err)
	}
	// Proved, it is asked of the payout partner. (The sandbox holds no Circle wallet, so it answers that it has none and
	// the payout fails, its money given back: what is asserted is that the proof let it go.)
	list, err := b.store.EarningsPayouts(ctx, ownSeller)
	if err != nil || len(list) != 1 || list[0].Status == economy.PayoutHeld || list[0].Partner == "" || strings.Contains(list[0].Detail, "address-proof") {
		t.Fatalf("the recipient's payouts once it proved its address = %+v, %v; want the 1,500 USDC asked of the partner", list, err)
	}
}

// B48.7 — with no Travel Rule adapter (LENS_TRAVEL_RULE unset, as in production until a partner is chosen) the step is
// off: more than 1,000 USDC to an address is sent, never a 409 nobody can clear, and its address is still screened.
func TestB487_WithTheTravelRuleOffAnAddressIsScreenedAndPaid(t *testing.T) {
	pool := agentRoutesDB(t)
	const ws = "ws-b487-off"
	const mixer = "0x00000000000000000000000000000000000B4879"
	b := newB487Payer(t, pool, ws, b487Screening{strings.ToLower(mixer): partners.AddressMixer}, nil)
	_, own := b487Key(t)
	if p, err := b.pay(ws, b.save(t, pool, ws, "My own wallet", own), "off", 1500_000000); err != nil || p.Status != economy.PaymentSent {
		t.Fatalf("pay 1,500 USDC with the Travel Rule off = %+v, %v; want sent", p, err)
	}
	if got, want := b468Postings(t, pool, ws, "off"), "payment_out_hold agent -1500000000; payment_out_hold hold 1500000000"; got != want {
		t.Fatalf("the payment posted %q, want %q", got, want)
	}
	if _, err := b.pay(ws, b.save(t, pool, ws, "Mixer", mixer), "off-mixer", 10_000000); !errors.Is(err, screening.ErrBlocked) {
		t.Fatalf("pay a mixer's address with the Travel Rule off = %v, want refused by screening", err)
	}
}
