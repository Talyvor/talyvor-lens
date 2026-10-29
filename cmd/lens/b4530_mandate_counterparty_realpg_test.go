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
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/passkey"
)

// B45.30 — a mandate's rules gain a "who". Signed with "only verified agents", it refuses an agent whose signature
// holds but whose business Talyvor has not checked, and an agent it knows nothing about, and allows a verified one;
// signed with an operator list, it allows an agent signed by a named domain and refuses one signed by another. Both
// are judged before the limits, and a mandate without a "who" keeps the form it was signed in before B45.30.
func TestB4530_MandateRulesOnTheCounterpartysTrustAndOperator(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws, rpID, origin = "ws-b4530-who", "app.talyvor.com", "https://app.talyvor.com"
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetApprovalAuth(passkey.RelyingParty{ID: rpID, Origins: []string{origin}}, nil)

	// The person's passkey signs the mandates: this test is the authenticator.
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
		ClientDataJSON: clientData("webauthn.create", regChallenge), AuthenticatorData: attested}); err != nil {
		t.Fatal(err)
	}
	var count uint32
	sign := func(who *economy.MandateCounterparty) (string, string) {
		t.Helper()
		canonical, challenge, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
			Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, Clients: []string{"chat"},
			Scope:  economy.MandateScope{Payees: []string{"payee_agent"}, Currencies: []string{"GBP"}},
			Limits: economy.MandateLimits{PerPayment: map[string]int64{"GBP": 10000}}, NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeConfirmationOfPayee},
			ExpiresAt: time.Now().Add(24 * time.Hour), Counterparty: who})
		if err != nil {
			t.Fatal(err)
		}
		count++
		cdj, ad := clientData("webauthn.get", challenge), binary.BigEndian.AppendUint32(append(append([]byte{}, rpHash[:]...), 0x05), count)
		h := sha256.Sum256(cdj)
		digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
		m, err := store.CreatePermissionMandate(ctx, ws, "ada", canonical, &passkey.Assertion{CredentialID: b64(credID),
			ClientDataJSON: b64(cdj), AuthenticatorData: b64(ad), Signature: b64(sig)})
		if err != nil {
			t.Fatal(err)
		}
		return m.ID, canonical
	}

	verified := &economy.Counterparty{Level: string(agentsig.Verified), Domain: "agents.acme.example", Business: "Acme Inc."}
	unchecked := &economy.Counterparty{Level: string(agentsig.Signed), Domain: "agents.acme.example"}
	stranger := &economy.Counterparty{Level: string(agentsig.Signed), Domain: "bots.stranger.example"}
	judge := func(mandate, name string, who *economy.Counterparty, amount int64, decision, rule string) {
		t.Helper()
		d, err := store.EvaluateMandatePayment(ctx, ws, mandate, economy.PreparedPayment{Client: "chat", PayeeID: "payee_agent",
			Currency: "GBP", AmountMinor: amount, Counterparty: who})
		if err != nil || d.Decision != decision || d.Rule != rule {
			t.Errorf("%s = %+v (%v), want %s by %q", name, d, err, decision, rule)
		}
	}

	onlyVerified, canonical := sign(&economy.MandateCounterparty{MinTrust: "verified"})
	if !strings.Contains(canonical, `"counterparty":{"min_trust":"verified"}`) {
		t.Errorf("the signed mandate does not carry its who: %s", canonical)
	}
	judge(onlyVerified, "a verified agent", verified, 9000, economy.MandateAllow, "")
	judge(onlyVerified, "a signed agent whose business is not checked", unchecked, 9000, economy.MandateBlock, "counterparty.min_trust")
	judge(onlyVerified, "an agent Talyvor knows nothing about", nil, 9000, economy.MandateBlock, "counterparty.min_trust")
	judge(onlyVerified, "a signed agent over the limit", unchecked, 12000, economy.MandateBlock, "counterparty.min_trust")

	trusted, _ := sign(&economy.MandateCounterparty{Operators: []string{" Agents.ACME.example "}})
	judge(trusted, "an agent signed by a trusted operator", unchecked, 9000, economy.MandateAllow, "")
	judge(trusted, "an agent signed by another operator", stranger, 9000, economy.MandateBlock, "counterparty.operators")
	judge(trusted, "an unsigned agent", &economy.Counterparty{Level: string(agentsig.Unverified)}, 9000, economy.MandateBlock, "counterparty.operators")

	anyone, canonical := sign(nil)
	if strings.Contains(canonical, "counterparty") {
		t.Errorf("a mandate with no who changed its signed form: %s", canonical)
	}
	judge(anyone, "any agent under a mandate with no who", stranger, 9000, economy.MandateAllow, "")

	if _, _, _, err := store.PreparePermissionMandate(ctx, ws, economy.PermissionMandateTerms{
		Principal: economy.MandatePrincipal{Type: "person", ID: "ada", Name: "Ada Lovelace"}, Clients: []string{"chat"},
		Scope:    economy.MandateScope{Payees: []string{"payee_agent"}, Currencies: []string{"GBP"}},
		NewPayee: economy.MandateNewPayee{Policy: economy.NewPayeeAlwaysAsk}, ExpiresAt: time.Now().Add(time.Hour),
		Counterparty: &economy.MandateCounterparty{MinTrust: "trusted"}}); err == nil {
		t.Error(`a mandate asking for agents "trusted" was prepared, want refused: the levels are signed and verified`)
	}
}
