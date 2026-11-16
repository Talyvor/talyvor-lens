package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/partners"
)

// B30.4 — verification levels for people and companies, and the level each capability needs for live money.

// realKYC is a verification provider that is not the Test one: what it passes counts for live money. It answers as
// the Test provider does.
type realKYC struct{ *partners.TestKYCProvider }

func (realKYC) Name() string { return "kyc-partner" }

// liveVerified records that ws passed every level's check with a real provider, in Great Britain, and gives L1 a limit
// in every currency that no test reaches: then live money asks nothing more of ws's verification.
func liveVerified(t *testing.T, pool *pgxpool.Pool, ws string) { liveVerifiedIn(t, pool, ws, "GB") }

// liveVerifiedIn is liveVerified with the identity and company checks confirming country ("" confirms none).
func liveVerifiedIn(t *testing.T, pool *pgxpool.Pool, ws, country string) {
	t.Helper()
	ctx := context.Background()
	for level, subject := range map[int]string{1: "contact", 2: "person", 3: "company"} {
		c := country
		if level == 1 {
			c = ""
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref, country)
			VALUES ($1, $2, $3, 'kyc-partner', 'completed', $4, $5)`, ws, level, subject, fmt.Sprintf("ref-%s-%d", ws, level), c); err != nil {
			t.Fatal(err)
		}
	}
	for cur := range MoneyCurrencies {
		if _, err := pool.Exec(ctx, `INSERT INTO verification_level_limits (level, currency, limit_minor, operator, reference)
			VALUES (1, $1, $2, 'test', 'no test reaches it')`, cur, int64(1)<<62); err != nil {
			t.Fatal(err)
		}
	}
}

// verifiedForLiveMoney gives ws a payments-out clearance for GB on a plan with live money: all live money needs
// besides a verification level and a limit.
func verifiedForLiveMoney(t *testing.T, s *DualTokenStore, pool *pgxpool.Pool, ws string) context.Context {
	t.Helper()
	gb := WithUseCountry(context.Background(), "GB")
	if _, err := pool.Exec(gb, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	planGatesOnPlan(t, pool, ws, "team", false)
	if _, err := s.ClearCapability(gb, CapabilityPaymentsOut, "nicolai", ClearanceTerms{Reference: "B30.4 test", Licence: "EMI-900123",
		Partner: "Test Payments Ltd", Countries: []string{"GB"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return gb
}

// The DONE line: a workspace at L1 is refused a live payment, naming the level it needs; the same payment on test
// money goes through; the level and the check's evidence reference are on the owner's record. At L2 the live payment
// goes through, and the ledger holds it.
func TestVerification_AWorkspaceAtL1IsRefusedALivePaymentAndTestMoneyGoesThrough(t *testing.T) {
	pool := supplyPool(t)
	s := screenedStore(pool)
	const ws = "ws-b304-done"
	gb := verifiedForLiveMoney(t, s, pool, ws)
	kyc := partners.OneVerifier(realKYC{&partners.TestKYCProvider{}})
	// L1 has a limit, so only its level keeps the live payment out.
	if _, err := s.SetLevelLimit(gb, LevelContact, CurrencyGBP, 100_000, "nicolai", "B30.4 test"); err != nil {
		t.Fatal(err)
	}

	contact, err := s.StartVerification(gb, kyc, ws, "owner:user:ada", VerificationRequest{Level: LevelContact,
		Email: "ada@example.com", Phone: "+44 7700 900123"})
	if err != nil || contact.Status != "completed" || contact.Level != LevelContact {
		t.Fatalf("the email and phone check = %+v, %v", contact, err)
	}
	if _, err := s.StartVerification(gb, kyc, ws, "owner:user:ada", VerificationRequest{Level: LevelCompany, Name: "Acme Ltd",
		Country: "GB", CompanyNumber: "01234567", Directors: []string{"Ada Lovelace"}}); !errors.Is(err, ErrVerificationOrder) {
		t.Fatalf("a company check at L1 = %v; want refused until the identity check", err)
	}

	company, partner := openMoney(t, s, ws, CurrencyGBP, MoneyCompany), openMoney(t, s, ws, CurrencyGBP, MoneyPartner)
	pay := func(key, funding string) (MoneyEntry, error) {
		return s.PostMoney(gb, MoneyEntry{WorkspaceID: ws, Capability: CapabilityPaymentsOut, Counterparty: "Acme Ltd", Kind: "payment_out", IdempotencyKey: key,
			Funding: funding, Postings: []MoneyPosting{{AccountID: partner.ID, AmountMinor: 12_000}, {AccountID: company.ID, AmountMinor: -12_000}}})
	}
	_, err = pay("live-1", FundingLive)
	if !errors.Is(err, ErrVerificationNeeded) || !errors.Is(err, ErrCapabilityNotCleared) ||
		!strings.Contains(err.Error(), "needs verification level L2 (identity checked)") || !strings.Contains(err.Error(), "this workspace is at L1") {
		t.Fatalf("a live £120.00 payment at L1 = %v; want refused naming L2", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_entries WHERE workspace_id = $1`, ws); n != 0 {
		t.Fatalf("the refused live payment wrote %d entries; want none", n)
	}
	e, err := pay("test-1", FundingTest)
	if err != nil {
		t.Fatalf("the same payment on test money = %v; want it through", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_postings WHERE entry_id = $1 AND funding = 'test' AND amount_minor = 12000
		AND account_id = $2`, e.ID, partner.ID); n != 1 {
		t.Fatalf("the test payment's posting to the partner is on the ledger %d times; want once", n)
	}

	v, err := s.Verification(gb, kyc, ws)
	if err != nil {
		t.Fatal(err)
	}
	if v.Level != LevelContact || v.LiveLevel != LevelContact || len(v.Checks) != 1 || v.Checks[0].EvidenceRef != contact.EvidenceRef ||
		v.Checks[0].EvidenceRef == "" || v.Checks[0].Method != "kyc-partner" || v.Checks[0].Test {
		t.Fatalf("the owner's record = %+v; want L1 with the check's evidence reference %q", v, contact.EvidenceRef)
	}

	if _, err := s.StartVerification(gb, kyc, ws, "owner:user:ada", VerificationRequest{Level: LevelIdentity, Name: "Ada Lovelace",
		Country: "gb", DateOfBirth: "1990-12-10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pay("live-1", FundingLive); err != nil {
		t.Fatalf("the live payment at L2 = %v; want it through", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM money_postings p JOIN money_entries e ON e.id = p.entry_id
		WHERE e.workspace_id = $1 AND e.idempotency_key = 'live-1' AND p.funding = 'live' AND p.account_id = $2 AND p.amount_minor = -12000`,
		ws, company.ID); n != 1 {
		t.Fatalf("the live payment's posting out of the company is on the ledger %d times; want once", n)
	}
}

// A pass by the Test provider shows on the record and counts for test money only; a real level takes no live money
// until a limit is set for it, then no more than the limit in one movement; a pass the provider withdraws lowers the
// level at the next read.
func TestVerification_TestPassesLimitsAndAWithdrawnPass(t *testing.T) {
	pool := supplyPool(t)
	s := screenedStore(pool)
	const ws = "ws-b304-limits"
	gb := verifiedForLiveMoney(t, s, pool, ws)
	company, partner := openMoney(t, s, ws, CurrencyGBP, MoneyCompany), openMoney(t, s, ws, CurrencyGBP, MoneyPartner)
	pay := func(key string, minor int64) error {
		_, err := s.PostMoney(gb, MoneyEntry{WorkspaceID: ws, Capability: CapabilityPaymentsOut, Counterparty: "Acme Ltd", Kind: "payment_out", IdempotencyKey: key,
			Funding: FundingLive, Postings: []MoneyPosting{{AccountID: partner.ID, AmountMinor: minor}, {AccountID: company.ID, AmountMinor: -minor}}})
		return err
	}
	verify := func(kyc partners.Verifiers, name string) {
		t.Helper()
		if _, err := s.StartVerification(gb, kyc, ws, "owner", VerificationRequest{Level: LevelContact, Email: "ada@example.com",
			Phone: "+447700900123"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartVerification(gb, kyc, ws, "owner", VerificationRequest{Level: LevelIdentity, Name: name, Country: "GB",
			DateOfBirth: "1990-12-10"}); err != nil {
			t.Fatal(err)
		}
	}

	test := partners.OneVerifier(&partners.TestKYCProvider{})
	verify(test, "Ada Lovelace")
	if v, err := s.Verification(gb, test, ws); err != nil || v.Level != LevelIdentity || v.LiveLevel != LevelSignedIn || !v.Checks[0].Test {
		t.Fatalf("after the Test provider's passes the record = %+v, %v; want L2 shown, L0 live", v, err)
	}
	if err := pay("p1", 12_000); !errors.Is(err, ErrVerificationNeeded) || !strings.Contains(err.Error(), "this workspace is at L0") {
		t.Fatalf("a live payment on the Test provider's L2 = %v; want refused at L0", err)
	}

	real := partners.OneVerifier(realKYC{&partners.TestKYCProvider{}})
	verify(real, "Ada TESTRETURN Lovelace")
	if err := pay("p2", 12_000); !errors.Is(err, ErrVerificationNeeded) || !strings.Contains(err.Error(), "no live limit") {
		t.Fatalf("a live payment at L2 with no limit set = %v; want refused for want of a limit", err)
	}
	if _, err := s.SetLevelLimit(gb, LevelContact, CurrencyGBP, 100_000, "nicolai", "B30.4 test"); err != nil {
		t.Fatal(err)
	}
	if err := pay("p3", 150_000); !errors.Is(err, ErrVerificationNeeded) ||
		!strings.Contains(err.Error(), "moves 1500.00 GBP of real money, over the 1000.00 GBP limit for one movement at verification level L1") {
		t.Fatalf("a live £1500.00 payment over L1's £1000.00, which L2 has = %v", err)
	}
	if err := pay("p4", 100_000); err != nil {
		t.Fatalf("a live £1000.00 payment at the limit = %v; want it through", err)
	}

	v, err := s.Verification(gb, real, ws)
	if err != nil || v.LiveLevel != LevelContact || v.Checks[0].Status != "returned" {
		t.Fatalf("after the provider withdrew the identity pass the record = %+v, %v; want live L1, the check returned", v, err)
	}
	if err := pay("p5", 12_000); !errors.Is(err, ErrVerificationNeeded) || !strings.Contains(err.Error(), "this workspace is at L1") {
		t.Fatalf("a live payment after the identity pass was withdrawn = %v; want refused at L1", err)
	}
	if n := moneyCount(t, pool, `SELECT count(*) FROM workspace_verifications WHERE workspace_id = $1`, ws); n != 5 {
		t.Fatalf("%d verification rows; want 5: four checks and the withdrawal, appended", n)
	}
	if _, err := pool.Exec(gb, `UPDATE workspace_verifications SET status = 'completed' WHERE workspace_id = $1`, ws); err == nil {
		t.Fatal("a verification row was rewritten; the record is append-only")
	}
}

// recordingKYC is the Test provider, keeping what each check asked it.
type recordingKYC struct {
	*partners.TestKYCProvider
	got []partners.KYCRequest
}

func (r *recordingKYC) StartCheck(ctx context.Context, req partners.KYCRequest) (partners.Result, error) {
	r.got = append(r.got, req)
	return r.TestKYCProvider.StartCheck(ctx, req)
}

// B61.45 — an identity check takes the person's home address: it reaches the provider, tidied, and Lens keeps none
// of it, as with the date of birth. An address without its lines, town, postcode or country is refused before any
// provider is asked.
func TestVerification_AnIdentityCheckCarriesTheHomeAddressToTheProviderAndKeepsNoneOfIt(t *testing.T) {
	pool := supplyPool(t)
	s := screenedStore(pool)
	const ws = "ws-b6145-address"
	ctx := verifiedForLiveMoney(t, s, pool, ws)
	kyc := &recordingKYC{TestKYCProvider: &partners.TestKYCProvider{}}
	verifiers := partners.OneVerifier(kyc)
	if _, err := s.StartVerification(ctx, verifiers, ws, "owner", VerificationRequest{Level: LevelContact, Email: "ada@example.com",
		Phone: "+447700900123"}); err != nil {
		t.Fatal(err)
	}
	identity := VerificationRequest{Level: LevelIdentity, Name: "Ada Lovelace", Country: "GB", DateOfBirth: "1990-12-10",
		Address: &HomeAddress{Lines: []string{" 12 Marlborough Road ", "", "Flat 3"}, Town: " Twickenham ", Postcode: " tw1 3aa ", Country: "gb"}}
	c, err := s.StartVerification(ctx, verifiers, ws, "owner", identity)
	if err != nil || c.Status != "completed" {
		t.Fatalf("an identity check with a home address = %+v, %v; want it passed", c, err)
	}
	want := partners.KYCAddress{Lines: []string{"12 Marlborough Road", "Flat 3"}, Town: "Twickenham", Postcode: "TW1 3AA", Country: "GB"}
	got := kyc.got[len(kyc.got)-1].Address
	if got == nil || strings.Join(got.Lines, "|") != strings.Join(want.Lines, "|") || got.Town != want.Town ||
		got.Postcode != want.Postcode || got.Country != want.Country {
		t.Fatalf("the provider was asked with the address %+v; want %+v", got, want)
	}
	var row string
	if err := pool.QueryRow(ctx, `SELECT string_agg(to_jsonb(v)::text, ' ') FROM workspace_verifications v WHERE workspace_id = $1`,
		ws).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"Marlborough", "Flat 3", "Twickenham", "TW1"} {
		if strings.Contains(strings.ToUpper(row), strings.ToUpper(kept)) {
			t.Fatalf("the verification record keeps %q of the home address: %s", kept, row)
		}
	}

	asked := len(kyc.got)
	for name, a := range map[string]HomeAddress{
		"no lines":    {Town: "Twickenham", Postcode: "TW1 3AA", Country: "GB"},
		"no town":     {Lines: []string{"12 Marlborough Road"}, Postcode: "TW1 3AA", Country: "GB"},
		"no postcode": {Lines: []string{"12 Marlborough Road"}, Town: "Twickenham", Country: "GB"},
		"no country":  {Lines: []string{"12 Marlborough Road"}, Town: "Twickenham", Postcode: "TW1 3AA", Country: "Britain"},
	} {
		in := identity
		in.Address = &a
		if _, err := s.StartVerification(ctx, verifiers, ws, "owner", in); !errors.Is(err, ErrVerificationInvalid) {
			t.Fatalf("an identity check whose home address has %s = %v; want it refused", name, err)
		}
	}
	if len(kyc.got) != asked {
		t.Fatalf("the provider was asked %d more times about incomplete addresses; want none", len(kyc.got)-asked)
	}
}
