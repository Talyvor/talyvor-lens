package issuerfake_test

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/issuerfake"
	"github.com/talyvor/lens/internal/money/moneytest"
)

// TestCardContract: the fake issuer passes the card-partner contract every real card adapter must pass (B53.10).
func TestCardContract(t *testing.T) {
	moneytest.CardContract(t, func(decide moneytest.Decide) (money.CardIssuer, moneytest.CardPartner) {
		i := issuerfake.New(decide)
		return i, i
	}, money.GBP)
}

// TestNothingReadsAsACardNumber: no ref, id or token the fake makes ever reads as a card number, however many it makes
// (B59.48): 100,000 cards, each with a phone-wallet token, its two events and a charge, all through the contract's guard.
func TestNothingReadsAsACardNumber(t *testing.T) {
	ctx := context.Background()
	var auths []money.Authorization
	i := issuerfake.New(func(_ context.Context, _ string, a money.Authorization) (bool, string, error) {
		auths = append(auths, a)
		return true, "", nil
	})
	guard := func(what string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if n := moneytest.CardNumber.Find(b); n != nil {
			t.Fatalf("the fake made %s that reads as a card number (%s in %s)", what, n, b)
		}
	}
	for n := range 100_000 {
		c, err := i.Issue(ctx, money.IssueCard{Key: "issue-" + strconv.Itoa(n), Wallet: "w", Cap: money.Amount{Asset: money.GBP, Minor: 100}})
		if err != nil {
			t.Fatal(err)
		}
		guard("a card", c)
		tok, err := i.AddToWallet(ctx, c.Ref, money.WalletApplePay)
		if err != nil {
			t.Fatal(err)
		}
		guard("a phone-wallet token", tok)
		if _, _, err := i.Charge(ctx, c.Ref, "m", money.Amount{Asset: money.GBP, Minor: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range auths {
		guard("an authorisation request", a)
	}
	for _, e := range i.TokenEvents() {
		guard("a token event", e)
	}
	if len(auths) != 100_000 || len(i.TokenEvents()) != 200_000 {
		t.Fatalf("made %d authorisation requests and %d token events, want 100,000 and 200,000", len(auths), len(i.TokenEvents()))
	}
}
