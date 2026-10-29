package mcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
)

// B45.14 — "Transfer $500 to Mike through Taly": prepare_transfer_to_contact finds the person's one saved, verified
// contact by name and prepares one payment for approval; two matches, an unconfirmed one or none ask the assistant
// back and prepare nothing; payment details in the chat are refused; once approved on test money it pays once.
func TestPrepareTransferToContact_OneVerifiedContactOnePayment(t *testing.T) {
	pool := savingsTestPool(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4514", "app.talyvor.com", "https://app.talyvor.com"
	registry := partners.NewRegistry(nil)
	screener := screening.NewScreener(pool, registry)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screener)
	store.SetScreener(screener)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)
	company, err := store.OpenCurrencyAccount(ctx, ws, "", "USD", "test")
	if err != nil {
		t.Fatal(err)
	}
	var partnerAccount string
	if err := pool.QueryRow(ctx, `SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'partner'`, ws).Scan(&partnerAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PostMoney(ctx, economy.MoneyEntry{WorkspaceID: ws, Capability: economy.CapabilityPaymentsIn, Kind: "test_top_up",
		IdempotencyKey: "top-up", Funding: economy.FundingTest, Counterparty: "Test payer", Postings: []economy.MoneyPosting{
			{AccountID: partnerAccount, AmountMinor: -200000}, {AccountID: company.ID, AmountMinor: 200000}}}); err != nil {
		t.Fatal(err)
	}
	payee := map[string]string{}
	for _, name := range []string{"Mike Jones", "Alan Turing", "Alan Kay", "Linus TESTNOMATCH"} {
		p, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: name, Country: "US", RoutingNumber: "021000021", AccountNumber: "123456789"})
		if err != nil {
			t.Fatal(err)
		}
		payee[name] = p.ID
	}

	srv := newServer(pool, nil, nil, nil, nil, "test")
	srv.SetPaymentRequests(store)
	owner := &auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "ada", Scopes: []string{auth.ScopeKeys}}
	call := func(args string) (bool, string) {
		t.Helper()
		resp := srv.dispatch(auth.WithAuthContext(ctx, owner), rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call",
			Params: json.RawMessage(`{"name":"prepare_transfer_to_contact","arguments":` + args + `}`)})
		raw, _ := json.Marshal(resp)
		var out struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out.Result.Content) == 0 {
			t.Fatalf("prepare_transfer_to_contact %s = %s", args, raw)
		}
		return out.Result.IsError, out.Result.Content[0].Text
	}
	requests := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_requests WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// "Mike": exactly one saved, verified contact — one payment of $500 to him, waiting for the person's approval.
	isErr, text := call(`{"idempotency_key":"t1","client":"chat","contact":"mike","currency":"USD","amount_minor":50000,"reference":"Dinner"}`)
	var prepared economy.PaymentRequest
	if isErr || json.Unmarshal([]byte(text), &prepared) != nil || prepared.Status != "pending_approval" || len(prepared.Items) != 1 ||
		prepared.Items[0].PayeeID != payee["Mike Jones"] || prepared.Items[0].AmountMinor != 50000 || prepared.ApprovalURL == "" {
		t.Fatalf("a transfer to Mike = %s; want one $500 payment to Mike Jones waiting for approval", text)
	}

	// Two Alans, an unconfirmed Linus, or a name no contact has: a question back, and nothing prepared.
	for contact, want := range map[string]string{"Alan": "Which Alan?", "Linus": "not confirmed", "Bob": "No saved contact"} {
		isErr, text := call(`{"idempotency_key":"q-` + contact + `","client":"chat","contact":"` + contact + `","currency":"USD","amount_minor":50000}`)
		if isErr || !strings.Contains(text, want) || strings.Contains(text, "approval_url") {
			t.Errorf("a transfer to %s = %s; want a question containing %q and nothing prepared", contact, text, want)
		}
	}
	if _, text := call(`{"idempotency_key":"q2","contact":"Alan","currency":"USD","amount_minor":1}`); !strings.Contains(text, payee["Alan Turing"]) ||
		!strings.Contains(text, payee["Alan Kay"]) {
		t.Errorf("Which Alan? = %s; want both saved Alans to choose from", text)
	}

	// Payment details from the chat: refused as arguments, and never a match when pasted into the name.
	if isErr, text := call(`{"idempotency_key":"d1","contact":"Mike","routing_number":"026009593","account_number":"987654321","currency":"USD","amount_minor":50000}`); !isErr ||
		!strings.Contains(text, "payee details supplied by the assistant") {
		t.Errorf("a transfer with account details = %s; want it refused", text)
	}
	if _, text := call(`{"idempotency_key":"d2","contact":"Mike 026009593 987654321","currency":"USD","amount_minor":50000}`); !strings.Contains(text, "No saved contact") {
		t.Errorf("a transfer to details pasted as the name = %s; want no contact found", text)
	}
	if n := requests(); n != 1 {
		t.Fatalf("%d payment requests were prepared, want only Mike's", n)
	}

	// The person approves with a passkey on test money: one payment out of $500; approving again pays nothing.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	b64 := base64.RawURLEncoding.EncodeToString
	rpHash := sha256.Sum256([]byte(rpID))
	clientData := func(typ, challenge string) []byte {
		return []byte(`{"type":"` + typ + `","challenge":"` + challenge + `","origin":"` + origin + `"}`)
	}
	regChallenge, _, err := store.PasskeyRegistrationChallenge(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	attested := binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x45), 0)
	attested = append(append(binary.BigEndian.AppendUint16(append(attested, make([]byte, 16)...), uint16(len(credID))), credID...), 0xa0)
	if _, err := store.RegisterPasskey(ctx, ws, "Ada's phone", passkey.Registration{CredentialID: b64(credID), PublicKey: spki,
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}, nil); err != nil {
		t.Fatal(err)
	}
	sign := func(challenge string, counter uint32) *passkey.Assertion {
		cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), counter)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		return &passkey.Assertion{CredentialID: b64(credID), ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)}
	}
	statement, challenge, _, err := store.PrepareRequestApproval(ctx, ws, prepared.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApprovePaymentRequest(ctx, ws, prepared.ID, "ada", statement, sign(challenge, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApprovePaymentRequest(ctx, ws, prepared.ID, "ada", statement, sign(challenge, 2)); !errors.Is(err, economy.ErrPaymentRequestApproved) {
		t.Errorf("approving again = %v, want ErrPaymentRequestApproved", err)
	}
	var n int
	var minor int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(p.amount_minor), 0)::bigint FROM money_entries e JOIN money_postings p ON p.entry_id = e.id
		WHERE e.workspace_id = $1 AND e.kind = 'payment_out' AND p.account_id = $2`, ws, partnerAccount).Scan(&n, &minor); err != nil {
		t.Fatal(err)
	}
	var to []string
	rows, err := pool.Query(ctx, `SELECT payee_id FROM money_payments WHERE workspace_id = $1`, ws)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		to = append(to, id)
	}
	rows.Close()
	if n != 1 || minor != 50000 || len(to) != 1 || to[0] != payee["Mike Jones"] {
		t.Fatalf("the ledger has %d payments out of %d, to %v; want one of 50000 to Mike Jones", n, minor, to)
	}
}
