package main

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/partners"
)

// B53.1 — a fake-issuer charge stores the merchant category code, the merchant's country, the channel and the phone
// wallet on its authorisation and on its posting, and the card its form factor; a row from before B53.1 reads
// "unknown"; and the charge is decided, and posts, exactly as before.
func TestB531_AFakeIssuerChargeCarriesCategoryCountryChannelAndWallet(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b531"
	var issuer *issuerfake.Issuer
	b := newB468Payer(t, pool, ws, "GBP", economy.OutsidePayee{Name: "Ada Lovelace", Country: "GB", SortCode: "200000", AccountNumber: "55779911"},
		func(store *economy.DualTokenStore, reg *partners.Registry) *money.Registry {
			layer, err := money.NewRegistry(money.Config{Sandbox: money.NewSandbox(reg, "whsec_b531"), Clearances: b468Cleared{}, Limits: store.MoneyLimits(),
				Audit: store, Ledger: moneyLedger{store}, Replays: money.NewReplayStore(pool),
				Switches: money.SwitchSet{{Capability: money.CapCards, Partner: issuerfake.Name}: true}})
			if err != nil {
				t.Fatal(err)
			}
			issuer = issuerfake.New(store.AuthorizeCardCharge)
			if err := layer.Register(issuer.Adapter()); err != nil {
				t.Fatal(err)
			}
			return layer
		})
	batch, err := b.store.RequestCardBatch(ctx, ws, economy.CardBatch{AgentID: b.acct.AgentID, Currency: "GBP", IdempotencyKey: "lunch",
		Cards: []economy.MoneyCard{{Merchant: "Pret A Manger", CapMinor: 2000}}})
	if err != nil {
		t.Fatal(err)
	}
	if batch, err = b.store.ApproveCardBatch(ctx, ws, batch.ID, "owner"); err != nil || batch.Cards[0].FormFactor != money.CardVirtual {
		t.Fatalf("approve = %+v %v, want the card issued as a virtual card", batch, err)
	}
	ref := issuerRef(t, pool, batch.Cards[0].ID)
	if ok, reason, err := issuer.Charge(ctx, ref, "Pret A Manger", money.Amount{Minor: 850, Asset: "GBP"}); !ok || err != nil {
		t.Fatalf("lunch at Pret = %v %q %v, want approved", ok, reason, err)
	}

	want := money.CardDetails{MCC: "5814", Country: "GB", Channel: money.ChannelContactless, Wallet: money.WalletApplePay}
	var auth, posted money.CardDetails
	if err := pool.QueryRow(ctx, `SELECT mcc, merchant_country, channel, wallet FROM money_card_authorizations WHERE workspace_id = $1`, ws).
		Scan(&auth.MCC, &auth.Country, &auth.Channel, &auth.Wallet); err != nil || auth != want {
		t.Fatalf("the authorisation stored %+v (%v), want %+v", auth, err, want)
	}
	if err := pool.QueryRow(ctx, `SELECT card_mcc, card_country, card_channel, card_wallet FROM money_entries WHERE workspace_id = $1 AND kind = 'card_charge'`, ws).
		Scan(&posted.MCC, &posted.Country, &posted.Channel, &posted.Wallet); err != nil || posted != want {
		t.Fatalf("the card_charge posting stored %+v (%v), want %+v", posted, err, want)
	}
	// Nothing about deciding the charge changed: it spent from the hold, gave the rest back, and closed the card.
	if got, want := b4610Postings(t, pool, ws), "card_hold agent -2000; card_hold hold 2000; card_charge hold -2000; card_charge partner 850; card_charge agent 1150"; got != want {
		t.Fatalf("the card posted %q, want %q", got, want)
	}

	// A row written before B53.1 — an authorisation, a card and a card posting that name none of the four — reads "unknown".
	if _, err := pool.Exec(ctx, `INSERT INTO money_card_authorizations (partner, id, workspace_id, merchant, amount_minor, currency, approved)
		VALUES ('issuerfake', 'iauth_old', $1, 'Old Merchant', 100, 'GBP', false)`, ws); err != nil {
		t.Fatal(err)
	}
	unknown := money.CardDetails{MCC: money.Unknown, Country: money.Unknown, Channel: money.Unknown, Wallet: money.Unknown}
	if err := pool.QueryRow(ctx, `SELECT mcc, merchant_country, channel, wallet FROM money_card_authorizations WHERE id = 'iauth_old'`).
		Scan(&auth.MCC, &auth.Country, &auth.Channel, &auth.Wallet); err != nil || auth != unknown {
		t.Fatalf("an old authorisation reads %+v (%v), want unknown", auth, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO money_cards (id, batch_id, workspace_id, agent_id, line, merchant, cap_minor, currency, account_id, wallet_ref, status)
		SELECT 'mcard_old', batch_id, workspace_id, agent_id, 2, 'Old Merchant', cap_minor, currency, account_id, wallet_ref, 'awaiting_approval'
		FROM money_cards WHERE id = $1`, batch.Cards[0].ID); err != nil {
		t.Fatal(err)
	}
	if batch, err = b.store.CardBatch(ctx, ws, "", batch.ID); err != nil || len(batch.Cards) != 2 || batch.Cards[1].FormFactor != money.Unknown {
		t.Fatalf("an old card reads %+v (%v), want its form factor unknown", batch.Cards, err)
	}
	var partner string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner' AND currency = 'GBP'`, ws).Scan(&partner); err != nil {
		t.Fatal(err)
	}
	oldCharge := economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "card_charge", IdempotencyKey: "old-card-charge",
		Funding: economy.FundingTest, Counterparty: "Old Merchant", Postings: []economy.MoneyPosting{
			{AccountID: partner, AmountMinor: -100}, {AccountID: b.acct.ID, AmountMinor: 100}}}
	if _, err := b.store.PostMoney(ctx, oldCharge); err != nil {
		t.Fatal(err)
	}
	old, err := b.store.PostMoney(ctx, oldCharge) // the same key again answers the entry as read back
	if err != nil {
		t.Fatal(err)
	}
	if old.Card == nil || *old.Card != unknown {
		t.Fatalf("a card posting without details reads %+v, want unknown", old.Card)
	}
}
