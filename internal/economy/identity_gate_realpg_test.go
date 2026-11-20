package economy

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/partners"
)

// B61.52 — an account that has not confirmed who it is moves no money, Sandbox money included: a Personal account
// until its identity is checked (L2), a Business until its company is (L3). A Test-provider pass confirms it.

// sandboxConfirmed records that each ws passed every level's check with the Test provider: its Sandbox money moves
// (B61.52) and its live money is judged as before, as Test passes count for nothing live.
func sandboxConfirmed(t *testing.T, pool *pgxpool.Pool, workspaces ...string) {
	t.Helper()
	for _, ws := range workspaces {
		for level, subject := range map[int]string{1: "contact", 2: "person", 3: "company"} {
			if _, err := pool.Exec(context.Background(), `INSERT INTO workspace_verifications (workspace_id, level, subject, method, status, evidence_ref)
				VALUES ($1, $2, $3, 'test', 'completed', $4)`, ws, level, subject, fmt.Sprintf("test-%s-%d", ws, level)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// identityGateSend is a fresh account of accountType whose agent holds 100 LXC of test money, and a send of 10 LXC
// from it to another owner's agent, counting the transfers on the ledger after it.
func identityGateSend(t *testing.T, accountType string) (s *DualTokenStore, ws string, send func() (int, error)) {
	t.Helper()
	pool := supplyPool(t)
	ctx := context.Background()
	s = NewDualTokenStore(nil, pool, nil)
	s.SetOwnerVerifier(earnVerified{})
	ws, payee := "ws-b6152-"+accountType, "ws-b6152-payee"
	for _, w := range []struct{ id, kind string }{{ws, accountType}, {payee, AccountTypePersonal}} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified, account_type) VALUES ($1, $1, $1, true, $2)`,
			w.id, w.kind); err != nil {
			t.Fatal(err)
		}
	}
	payer, err := s.CreateAgent(ctx, ws, "payer", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "stripe top-up", map[string]interface{}{"funding": FundingTest}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, payer.ID, 100_000_000); err != nil {
		t.Fatal(err)
	}
	bea, err := s.CreateAgent(ctx, payee, "bea's agent", "user-bea")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAgentHandle(ctx, payee, bea.ID, "bea-b6152"); err != nil {
		t.Fatal(err)
	}
	return s, ws, func() (int, error) {
		_, err := s.SendCredits(ctx, ws, payer.ID, "@bea-b6152", 10_000_000, "lunch")
		return moneyCount(t, pool, `SELECT count(*) FROM agent_transfers WHERE from_workspace_id = $1 AND to_agent_id = $2`, ws, bea.ID), err
	}
}

func refusedUnconfirmed(t *testing.T, what string, n int, err error) {
	t.Helper()
	if !errors.Is(err, ErrIdentityUnconfirmed) || err.Error() != ConfirmIdentitySentence {
		t.Fatalf("%s = %v; want refused: %q", what, err, ConfirmIdentitySentence)
	}
	if n != 0 {
		t.Fatalf("%s was refused and left %d transfers on the ledger; want none", what, n)
	}
}

// The CHECKS line: a fresh Personal account at L0 sending Sandbox money to another owner is refused, at L1 too; once
// the Test provider has checked its owner's identity the same send goes through, and the ledger holds it.
func TestB6152_AnUnconfirmedPersonalAccountSendsNoSandboxMoneyUntilItsIdentityIsChecked(t *testing.T) {
	s, ws, send := identityGateSend(t, AccountTypePersonal)
	ctx := context.Background()
	n, err := send()
	refusedUnconfirmed(t, "a send of test money by a Personal account at L0", n, err)

	kyc := partners.OneVerifier(&partners.TestKYCProvider{})
	if _, err := s.StartVerification(ctx, kyc, ws, "owner", VerificationRequest{Level: LevelContact, Email: "ada@example.com",
		Phone: "+447700900123"}); err != nil {
		t.Fatal(err)
	}
	n, err = send()
	refusedUnconfirmed(t, "the send at L1", n, err)

	if _, err := s.StartVerification(ctx, kyc, ws, "owner", VerificationRequest{Level: LevelIdentity, Name: "Ada Lovelace",
		Country: "GB", DateOfBirth: "1990-12-10"}); err != nil {
		t.Fatal(err)
	}
	if n, err = send(); err != nil || n != 1 {
		t.Fatalf("the send once the Test provider checked the owner's identity = %v, %d transfers; want it on the ledger once", err, n)
	}
}

// A Business at L2 is refused until its company is checked (L3), then the send goes through.
func TestB6152_ABusinessAtL2IsRefusedUntilItsCompanyIsChecked(t *testing.T) {
	s, ws, send := identityGateSend(t, AccountTypeBusiness)
	ctx := context.Background()
	kyc := partners.OneVerifier(&partners.TestKYCProvider{})
	if _, err := s.StartVerification(ctx, kyc, ws, "owner", VerificationRequest{Level: LevelContact, Email: "ada@example.com",
		Phone: "+447700900123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartVerification(ctx, kyc, ws, "owner", VerificationRequest{Level: LevelIdentity, Name: "Ada Lovelace",
		Country: "GB", DateOfBirth: "1990-12-10"}); err != nil {
		t.Fatal(err)
	}
	n, err := send()
	refusedUnconfirmed(t, "a send of test money by a Business at L2", n, err)

	if _, err := s.StartVerification(ctx, kyc, ws, "owner", VerificationRequest{Level: LevelCompany, Name: "Acme Ltd", Country: "GB",
		CompanyNumber: "01234567", Directors: []string{"Ada Lovelace"}, SignificantControl: []string{"Ada Lovelace"}}); err != nil {
		t.Fatal(err)
	}
	if n, err = send(); err != nil || n != 1 {
		t.Fatalf("the send once the company was checked = %v, %d transfers; want it on the ledger once", err, n)
	}
}

// A transfer between one owner's own agents — GREEN, asked through the connector as through a route — is refused too
// until the account has confirmed who it is.
func TestB6152_AnUnconfirmedAccountMovesNothingBetweenItsOwnAgents(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws = "ws-b6152-own"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, account_type) VALUES ($1, $1, $1, 'personal')`, ws); err != nil {
		t.Fatal(err)
	}
	from, err := s.CreateAgent(ctx, ws, "from", "owner")
	if err != nil {
		t.Fatal(err)
	}
	to, err := s.CreateAgent(ctx, ws, "to", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "stripe top-up", map[string]interface{}{"funding": FundingTest}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, from.ID, 100_000_000); err != nil {
		t.Fatal(err)
	}
	pay := func() (int, error) {
		_, err := s.PayAgent(ctx, ws, from.ID, to.ID, 10_000_000, "lunch")
		return moneyCount(t, pool, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND kind = 'pay' AND amount_ulxc > 0`, ws), err
	}
	n, err := pay()
	refusedUnconfirmed(t, "a move between an unconfirmed account's own agents", n, err)
	sandboxConfirmed(t, pool, ws)
	if n, err = pay(); err != nil || n != 1 {
		t.Fatalf("the move once the account confirmed who it is = %v, %d transfers; want it on the ledger once", err, n)
	}
}
