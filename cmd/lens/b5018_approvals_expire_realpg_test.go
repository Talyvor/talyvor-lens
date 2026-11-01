package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B50.18 — an approval lasts 24 hours and approves only its own request, on the migrated schema.

// b5018Pending is the id of agentID's pending approval for the payment with memo.
func b5018Pending(t *testing.T, pool *pgxpool.Pool, agentID, memo string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM agent_approvals WHERE agent_id = $1 AND memo = $2 AND status = 'pending'`,
		agentID, memo).Scan(&id); err != nil {
		t.Fatalf("the pending approval of %q: %v", memo, err)
	}
	return id
}

// b5018Age moves approval id's clock back by d: filed, decided and expiring d earlier.
func b5018Age(t *testing.T, pool *pgxpool.Pool, id string, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE agent_approvals SET created_at = created_at - $2 * interval '1 second',
		decided_at = decided_at - $2 * interval '1 second', expires_at = expires_at - $2 * interval '1 second' WHERE id = $1`,
		id, int64(d/time.Second)); err != nil {
		t.Fatal(err)
	}
}

// Two requests from one agent to one payee for one amount: approving one leaves the other pending, and its payment
// is refused; "always allow" changes the rule only for requests made after it, so a waiting one still waits.
func TestB5018_ApprovingOneRequestApprovesOnlyThatOne(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5018-one"
	store, ag := b501Agents(t, pool, ws)
	if _, err := store.SetAgentRules(ctx, ws, ag[0].ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	for _, memo := range []string{"invoice A", "invoice B", "invoice C"} {
		if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, memo); !errors.Is(err, economy.ErrApprovalRequired) {
			t.Fatalf("%s: %v, want ErrApprovalRequired", memo, err)
		}
	}
	a, b := b5018Pending(t, pool, ag[0].ID, "invoice A"), b5018Pending(t, pool, ag[0].ID, "invoice B")
	if _, err := store.DecideAgentApproval(ctx, ws, a, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, "invoice B"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Errorf("the other request after one was approved: %v, want ErrApprovalRequired", err)
	}
	if got := b5018Pending(t, pool, ag[0].ID, "invoice B"); got != b {
		t.Errorf("the other request's approval is %s, want %s still pending", got, b)
	}
	if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, "invoice A"); err != nil {
		t.Fatalf("the approved payment: %v", err)
	}

	// Approve and always allow, on a fresh approval: B and C were waiting before it, and stay waiting.
	if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, "invoice D"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Fatal(err)
	}
	d := b5018Pending(t, pool, ag[0].ID, "invoice D")
	if _, err := store.DecideAgentApproval(ctx, ws, d, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AllowPayee(ctx, ws, d, economy.PayeeAllowance{ULXC: 50_000_000, Per: "day"}); err != nil {
		t.Fatal(err)
	}
	for _, memo := range []string{"invoice B", "invoice C"} {
		if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, memo); !errors.Is(err, economy.ErrApprovalRequired) {
			t.Errorf("%s, waiting before always allow: %v, want ErrApprovalRequired", memo, err)
		}
		b5018Pending(t, pool, ag[0].ID, memo)
	}
	for _, memo := range []string{"invoice D", "invoice E"} {
		if _, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, memo); err != nil {
			t.Errorf("%s, the approved one and one made after always allow: %v", memo, err)
		}
	}
	if n, sum := b501Postings(t, pool, "agent:"+ag[1].ID); n != 3 || sum != 15_000_000 {
		t.Errorf("the payee's postings = %d summing %d, want A, D and E", n, sum)
	}
	b501ZeroSum(t, pool)
}

// An approval is usable for 24 hours from the owner's tap: at 23h59 it pays, at 24h01 it is refused with "the approval
// expired, ask again" and a fresh request goes to the inbox. A request nobody answered expires after 24 hours too.
func TestB5018_AnApprovalLasts24Hours(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b5018-24h"
	store, ag := b501Agents(t, pool, ws)
	if _, err := store.SetAgentRules(ctx, ws, ag[0].ID, economy.AgentRules{ApprovalAboveULXC: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	pay := func(memo string) error {
		_, err := store.PayAgent(ctx, ws, ag[0].ID, ag[1].ID, 5_000_000, memo)
		return err
	}
	for _, memo := range []string{"early", "late", "unanswered"} {
		if err := pay(memo); !errors.Is(err, economy.ErrApprovalRequired) {
			t.Fatalf("%s: %v, want ErrApprovalRequired", memo, err)
		}
	}
	early, late, unanswered := b5018Pending(t, pool, ag[0].ID, "early"), b5018Pending(t, pool, ag[0].ID, "late"), b5018Pending(t, pool, ag[0].ID, "unanswered")
	b5018Age(t, pool, late, 5*time.Hour) // filed 5 hours before the tap: the 24 hours run from the tap
	for _, id := range []string{early, late} {
		a, err := store.DecideAgentApproval(ctx, ws, id, true)
		if err != nil {
			t.Fatal(err)
		}
		if a.ExpiresAt == nil || a.DecidedAt == nil || a.ExpiresAt.Sub(*a.DecidedAt) != 24*time.Hour {
			t.Errorf("approval %s decided %v expires %v, want 24 hours after the tap", id, a.DecidedAt, a.ExpiresAt)
		}
	}
	b5018Age(t, pool, early, 23*time.Hour+59*time.Minute)
	b5018Age(t, pool, late, 24*time.Hour+time.Minute)
	if err := pay("early"); err != nil {
		t.Errorf("an approval used at 23h59: %v", err)
	}
	err := pay("late")
	if !errors.Is(err, economy.ErrApprovalRequired) || err == nil || !strings.Contains(err.Error(), "the approval expired, ask again") {
		t.Errorf("an approval used at 24h01: %v, want ErrApprovalRequired saying the approval expired, ask again", err)
	}
	fresh := b5018Pending(t, pool, ag[0].ID, "late")
	if fresh == late {
		t.Error("no fresh request went to the inbox")
	}

	// Unanswered for 24 hours: it can no longer be approved, the inbox shows it expired, and the agent's retry asks again.
	b5018Age(t, pool, unanswered, 24*time.Hour+time.Minute)
	if _, err := store.DecideAgentApproval(ctx, ws, unanswered, true); !errors.Is(err, economy.ErrApprovalNotFound) {
		t.Errorf("approving a request 24h01 old: %v, want ErrApprovalNotFound", err)
	}
	list, err := store.ListAgentApprovals(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, a := range list {
		status[a.ID] = a.Status
	}
	if status[unanswered] != "expired" || status[late] != "expired" || status[early] != "used" || status[fresh] != "pending" {
		t.Errorf("the inbox shows %v, want unanswered and late expired, early used, the fresh one pending", status)
	}
	if err := pay("unanswered"); !errors.Is(err, economy.ErrApprovalRequired) {
		t.Errorf("the unanswered request retried: %v, want ErrApprovalRequired", err)
	}
	if again := b5018Pending(t, pool, ag[0].ID, "unanswered"); again == unanswered {
		t.Error("the retry of an expired request found the expired one, not a fresh one")
	}
	if n, sum := b501Postings(t, pool, "agent:"+ag[1].ID); n != 1 || sum != 5_000_000 {
		t.Errorf("the payee's postings = %d summing %d, want only the one used at 23h59", n, sum)
	}
	b501ZeroSum(t, pool)
}

// A batch approval releases only its own request's items: another request from the same agent to the same payee for the
// same amount stays waiting and pays nothing. A batch nobody answered for 24 hours can no longer be approved, and an
// approved one whose item failed is not paid on a retry once its approval is past its life.
func TestB5018_ABatchApprovalReleasesOnlyItsOwnItems(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const rpID, origin = "app.talyvor.com", "https://app.talyvor.com"
	b := b501Payer(t, pool, "ws-b5018-batch")
	ws := b.acct.WorkspaceID
	b.store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	// B50.20: a passkey enrols freely only as the workspace's first key, so the payer's stand-in device key goes first.
	if _, err := pool.Exec(ctx, `DELETE FROM workspace_device_keys WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	sign := b5018Passkey(t, b.store, ws, rpID, origin)
	prepare := func(key string, minor int64) (economy.PaymentRequest, string, string) {
		t.Helper()
		r, _, err := b.store.PreparePaymentRequest(ctx, ws, "ada", key, economy.PaymentRequestIn{Client: "chat", AgentID: b.acct.AgentID,
			Items: []economy.PaymentRequestItemIn{{PayeeID: b.payee.ID, Currency: "GBP", AmountMinor: minor, Reference: "Invoice " + key}}})
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "pending_approval" {
			t.Fatalf("request %s is %s, want pending_approval", key, r.Status)
		}
		statement, challenge, _, err := b.store.PrepareRequestApproval(ctx, ws, r.ID, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return r, statement, challenge
	}
	r1, st1, ch1 := prepare("batch-1", 2000)
	r2, _, _ := prepare("batch-2", 2000)
	if _, err := b.store.ApprovePaymentRequest(ctx, ws, r1.ID, "ada", st1, sign(ch1)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.PayPaymentRequest(ctx, ws, "", r2.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := b.store.PaymentRequest(ctx, ws, "", r2.ID); err != nil || got.Status != "pending_approval" || got.Items[0].PaymentID != "" {
		t.Errorf("the other request = %+v (%v), want still pending_approval and unpaid", got, err)
	}
	b.balance(t, 48000)

	// Unanswered for 24 hours: it can no longer be approved.
	if _, err := pool.Exec(ctx, `UPDATE payment_requests SET created_at = created_at - interval '24 hours 1 minute' WHERE id = $1`, r2.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := b.store.PrepareRequestApproval(ctx, ws, r2.ID, nil, ""); !errors.Is(err, economy.ErrPaymentRequest) ||
		!strings.Contains(err.Error(), "expired") {
		t.Errorf("approving a request 24h01 old: %v, want it expired", err)
	}

	// Approved, its item failed for want of money, then retried after the approval's life: refused, nothing moves.
	b.store.SetApprovalTTL(2 * time.Second)
	r3, st3, ch3 := prepare("batch-3", 60000)
	if _, err := b.store.ApprovePaymentRequest(ctx, ws, r3.ID, "ada", st3, sign(ch3)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	got, err := b.store.PayPaymentRequest(ctx, ws, "", r3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if it := got.Items[0]; it.PaymentID != "" || it.PayStatus != economy.PayRefused || !strings.Contains(it.PayDetail, "the approval expired, ask again") {
		t.Errorf("the item retried after the approval's life = %+v, want refused: the approval expired, ask again", it)
	}
	b.balance(t, 48000)
	b501ZeroSum(t, pool)
}

// b5018Passkey registers a passkey for ws and returns its signer: this test is the authenticator.
func b5018Passkey(t *testing.T, store *economy.DualTokenStore, ws, rpID, origin string) func(challenge string) *passkey.Assertion {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	regChallenge, _, err := store.PasskeyRegistrationChallenge(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	attested := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	attested = append(append(binary.BigEndian.AppendUint16(append(attested, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
	if _, err := store.RegisterPasskey(context.Background(), ws, "Ada's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}, nil); err != nil {
		t.Fatal(err)
	}
	counter := uint32(0)
	return func(challenge string) *passkey.Assertion {
		counter++
		cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		return &passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
	}
}
