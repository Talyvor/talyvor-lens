package economy

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	capabilityterms "github.com/talyvor/lens/docs/terms"
)

// B30.9 — terms for each capability, accepted before first use.

// Every money capability B30 registers ships its terms, headed as a draft for legal review.
func TestCapabilityTerms_EveryMoneyCapabilityHasADraftText(t *testing.T) {
	texts := CapabilityTermsTexts(capabilityterms.FS)
	for key := range b30 {
		text, ok := texts[key]
		if !ok {
			t.Errorf("%s has no docs/terms/%s.md", key, key)
			continue
		}
		if !strings.HasPrefix(text.Body, TermsDraftHeading+"\n") {
			t.Errorf("%s's terms start %q; want %q", key, strings.SplitN(text.Body, "\n", 2)[0], TermsDraftHeading)
		}
	}
}

// A first currency conversion is refused, test money and all, until the workspace accepts the fx terms, and is refused
// again once a new version is published, until that version is accepted. A refused conversion writes no entry.
func TestCapabilityTerms_AConversionWaitsForItsTermsAndAgainForANewVersion(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := screenedStore(pool)
	const ws = "ws-b309-fx"
	sandboxConfirmed(t, pool, ws) // B61.52: they move money, so they have confirmed who they are
	fx := CapabilityTermsTexts(capabilityterms.FS)[CapabilityFX]
	if published, err := s.PublishFirstTerms(ctx, map[string]TermsText{CapabilityFX: fx}); err != nil || len(published) != 1 {
		t.Fatalf("PublishFirstTerms = %v, %v; want fx published", published, err)
	}

	gbp, eur := openMoney(t, s, ws, CurrencyGBP, MoneyCompany), openMoney(t, s, ws, CurrencyEUR, MoneyCompany)
	gbpPartner, eurPartner := openMoney(t, s, ws, CurrencyGBP, MoneyPartner), openMoney(t, s, ws, CurrencyEUR, MoneyPartner)
	// Money in needs no terms in this test: only fx has them.
	if _, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityCurrencyAccounts, Counterparty: "Acme Ltd",
		Kind: "payment_in", IdempotencyKey: "in-1", Funding: FundingTest, Postings: []MoneyPosting{
			{AccountID: gbp.ID, AmountMinor: 100_00}, {AccountID: gbpPartner.ID, AmountMinor: -100_00}}}); err != nil {
		t.Fatal(err)
	}
	convert := func(key string) error {
		_, err := s.PostMoney(ctx, MoneyEntry{WorkspaceID: ws, Capability: CapabilityFX, Counterparty: "Test FX partner",
			Kind: "conversion", IdempotencyKey: key, Funding: FundingTest, Postings: []MoneyPosting{
				{AccountID: gbp.ID, AmountMinor: -10_00}, {AccountID: gbpPartner.ID, AmountMinor: 10_00},
				{AccountID: eurPartner.ID, AmountMinor: -11_60}, {AccountID: eur.ID, AmountMinor: 11_60}}})
		return err
	}
	conversions := func() int {
		return moneyCount(t, pool, `SELECT count(*) FROM money_entries WHERE workspace_id = $1 AND capability = 'fx'`, ws)
	}
	refused := func(err error, version string) {
		t.Helper()
		var r *CapabilityRefusal
		if !errors.Is(err, ErrTermsNotAccepted) || !errors.As(err, &r) || r.Terms == nil ||
			!strings.Contains(err.Error(), "Converting between currencies") || !strings.Contains(err.Error(), "version "+version) {
			t.Fatalf("conversion = %v; want refused for its terms, naming version %s", err, version)
		}
	}

	refused(convert("fx-1"), "1")
	if n := conversions(); n != 0 {
		t.Fatalf("a refused conversion left %d entries; want none", n)
	}
	a, err := s.AcceptTerms(ctx, ws, CapabilityFX, 1, "jwt:user:owner-1", "203.0.113.7")
	if err != nil || a.Version != 1 || a.Person != "jwt:user:owner-1" {
		t.Fatalf("AcceptTerms = %+v, %v", a, err)
	}
	var ipHash string
	if err := pool.QueryRow(ctx, `SELECT ip_hash FROM capability_terms_acceptances WHERE workspace_id = $1`, ws).Scan(&ipHash); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(ipHash) || strings.Contains(ipHash, "203.0.113.7") {
		t.Fatalf("the acceptance keeps the address as %q; want an HMAC of it", ipHash)
	}
	if err := convert("fx-2"); err != nil {
		t.Fatalf("a conversion after the terms were accepted = %v", err)
	}
	if b := moneyBalance(t, s, ws, eur.ID); b.AmountMinor != 11_60 || conversions() != 1 {
		t.Fatalf("after one conversion EUR holds %+v in %d entries; want €11.60 in 1", b, conversions())
	}

	if _, err := s.PublishTerms(ctx, CapabilityFX, "operator-cli:test", fx); !errors.Is(err, ErrTermsUnchanged) {
		t.Fatalf("publishing the same text again = %v; want ErrTermsUnchanged", err)
	}
	v2 := TermsText{Path: fx.Path, Body: fx.Body + "\n11. **A new clause.** It asks again.\n"}
	if p, err := s.PublishTerms(ctx, CapabilityFX, "operator-cli:test", v2); err != nil || p.Version != 2 {
		t.Fatalf("PublishTerms = %+v, %v; want version 2", p, err)
	}
	refused(convert("fx-3"), "2")
	if n := conversions(); n != 1 {
		t.Fatalf("after a refused conversion there are %d; want the 1 before it", n)
	}
	wt, err := s.WorkspaceTermsFor(ctx, ws, CapabilityFX)
	if err != nil || wt.Version != 2 || wt.Accepted != nil || wt.PreviouslyAccepted != 1 || wt.Body != v2.Body {
		t.Fatalf("WorkspaceTermsFor = %+v, %v; want version 2's text, not accepted, version 1 accepted before", wt, err)
	}
	if _, err := s.AcceptTerms(ctx, ws, CapabilityFX, 1, "jwt:user:owner-1", ""); !errors.Is(err, ErrTermsVersionStale) {
		t.Fatalf("accepting version 1 again = %v; want ErrTermsVersionStale", err)
	}
	if _, err := s.AcceptTerms(ctx, ws, CapabilityFX, 2, "jwt:user:owner-1", "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	if err := convert("fx-4"); err != nil || conversions() != 2 {
		t.Fatalf("a conversion after version 2 was accepted = %v with %d entries; want it through, 2", err, conversions())
	}
}
