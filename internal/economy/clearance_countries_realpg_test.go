package economy

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/partners"
)

// B30.10 — each capability says where it may be used live.

// The DONE line: a live payment by a workspace verified in a country outside the clearance's list is refused naming
// the country, and moves nothing; the same payment on test money goes through; an owner verified in a country the
// clearance lists makes it live.
func TestClearanceCountries_ALivePaymentFromAnUnlistedCountryIsRefusedNamingIt(t *testing.T) {
	pool := supplyPool(t)
	s := screenedStore(pool)
	kyc := partners.OneVerifier(realKYC{&partners.TestKYCProvider{}})
	const fr, gbOwner = "ws-b3010-fr", "ws-b3010-gb-owner"
	gb := verifiedForLiveMoney(t, s, pool, fr)
	_ = verifiedForLiveMoney(t, s, pool, gbOwner)
	if _, err := s.ClearCapability(gb, CapabilityPaymentsOut, "nicolai", ClearanceTerms{Reference: "B30.10 test", Licence: "EMI-900123",
		Partner: "Test Payments Ltd", Countries: []string{"GB", "IE"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLevelLimit(gb, LevelContact, CurrencyGBP, 100_000, "nicolai", "B30.10 test"); err != nil {
		t.Fatal(err)
	}
	verify := func(ws, country string) {
		t.Helper()
		if _, err := s.StartVerification(gb, kyc, ws, "owner", VerificationRequest{Level: LevelContact, Email: "ada@example.com",
			Phone: "+447700900123"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartVerification(gb, kyc, ws, "owner", VerificationRequest{Level: LevelIdentity, Name: "Ada Lovelace",
			Country: country, DateOfBirth: "1990-12-10"}); err != nil {
			t.Fatal(err)
		}
	}
	verify(fr, "fr")
	verify(gbOwner, "GB")
	sandboxConfirmed(t, pool, fr, gbOwner) // B61.52: Businesses, so their company is checked too
	if v, err := s.Verification(gb, kyc, fr); err != nil || v.LiveCountry != "FR" {
		t.Fatalf("the French owner's record = %+v, %v; want live country FR", v, err)
	}

	opened := map[string][2]MoneyAccount{} // a company has one account in a currency (B30.13)
	pay := func(ws, key, funding string) (MoneyEntry, error) {
		if _, ok := opened[ws]; !ok {
			opened[ws] = [2]MoneyAccount{openMoney(t, s, ws, CurrencyGBP, MoneyCompany), openMoney(t, s, ws, CurrencyGBP, MoneyPartner)}
		}
		company, partner := opened[ws][0], opened[ws][1]
		return s.PostMoney(gb, MoneyEntry{WorkspaceID: ws, Capability: CapabilityPaymentsOut, Counterparty: "Acme Ltd", Kind: "payment_out",
			IdempotencyKey: key, Funding: funding, Postings: []MoneyPosting{{AccountID: partner.ID, AmountMinor: 12_000},
				{AccountID: company.ID, AmountMinor: -12_000}}})
	}
	_, err := pay(fr, "live-fr", FundingLive)
	var refusal *CapabilityRefusal
	if !errors.As(err, &refusal) || !errors.Is(err, ErrCapabilityNotCleared) || refusal.Country == nil || refusal.Country.Country != "FR" ||
		!strings.Contains(err.Error(), "only for owners verified in GB, IE; this workspace is verified in FR") {
		t.Fatalf("a live £120.00 payment by an owner verified in France = %v; want refused naming FR", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_entries WHERE workspace_id = $1`, fr); n != 0 {
		t.Fatalf("the refused live payment wrote %d entries; want none", n)
	}
	e, err := pay(fr, "test-fr", FundingTest)
	if err != nil {
		t.Fatalf("the same payment on test money = %v; want it through", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_postings WHERE entry_id = $1 AND funding = 'test' AND amount_minor = 12000`,
		e.ID); n != 1 {
		t.Fatalf("the test payment's posting to the partner is on the ledger %d times; want once", n)
	}

	e, err = pay(gbOwner, "live-gb", FundingLive)
	if err != nil {
		t.Fatalf("the live payment by an owner verified in Great Britain = %v; want it through", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_postings WHERE entry_id = $1 AND funding = 'live' AND amount_minor = -12000`,
		e.ID); n != 1 {
		t.Fatalf("the live payment's posting out of the company is on the ledger %d times; want once", n)
	}
}
