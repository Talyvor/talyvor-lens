// Package swanfake is a local fake of the parts of Swan's partner API the swan adapter uses (B46.7), written from
// Swan's published reference (docs.swan.io): the OAuth client-credentials token, and the GraphQL operations for
// accounts, credit transfers with server-to-server consent, transactions and account holder onboarding. It answers
// each operation by its operationName and holds every account, payment and transaction in memory. It never calls Swan.
package swanfake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/talyvor/lens/internal/money"
)

// Payer is who the fake's deposits come from.
const Payer = "Ada Lovelace"

// PayerIBAN is the account the fake's deposits come from, at another bank.
const PayerIBAN = "DE89370400440532013000"

// Fake is Swan, in memory.
type Fake struct {
	URL, TokenURL, ClientID, ClientSecret, ProjectID, ConsentKey, WebhookSecret string
	HolderID                                                                    string // a verified account holder
	AccountLimit                                                                int    // accounts per holder, as projectInfo.multipleAccountsSettings caps them

	consentPub *ecdsa.PublicKey
	token      string

	mu         sync.Mutex
	n          int
	holders    map[string]bool // id → verified
	accounts   map[string]*acct
	order      []string
	txs        map[string]*tx
	payments   map[string]*payment
	byKey      map[string]string // idempotency key → payment id
	consents   map[string]string // consent id → payment id
	onboarding map[string]*onboard
	sent       int
}

type acct struct {
	ID, Holder, Name, IBAN, Status string
	Available                      int64
}

type tx struct {
	ID, Account, Type, Status, Counterparty, Label, Debtor string
	Amount                                                 int64
}

type payment struct {
	ID, Account, Status, Reason, Consent, Challenge, Credit string
	Amount                                                  int64
	IBAN, Name, Label                                       string
	Own                                                     bool
}

type onboard struct {
	ID, Holder string
	Verified   bool
}

// New is a fake Swan that lives as long as t.
func New(t testing.TB) *Fake {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	f := &Fake{ClientID: "client-" + rnd(), ClientSecret: rnd(), ProjectID: "project-" + rnd(), WebhookSecret: rnd(), AccountLimit: 6,
		ConsentKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), consentPub: &k.PublicKey, token: rnd(),
		holders: map[string]bool{}, accounts: map[string]*acct{}, txs: map[string]*tx{}, payments: map[string]*payment{},
		byKey: map[string]string{}, consents: map[string]string{}, onboarding: map[string]*onboard{}}
	f.HolderID = f.id("holder")
	f.holders[f.HolderID] = true
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth2/token", f.serveToken)
	mux.HandleFunc("POST /graphql", f.serveGraphQL)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.URL, f.TokenURL = srv.URL+"/graphql", srv.URL+"/oauth2/token"
	return f
}

func rnd() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *Fake) id(kind string) string {
	f.n++
	return fmt.Sprintf("%s-%04d", kind, f.n)
}

// iban is a French IBAN at Swan with valid check digits for account number n.
func iban(n int) string {
	bban := fmt.Sprintf("9999900100%011d82", n)
	num := new(big.Int)
	num.SetString(bban+"152700", 10) // F=15 R=27, then 00
	check := 98 - new(big.Int).Mod(num, big.NewInt(97)).Int64()
	return fmt.Sprintf("FR%02d%s", check, bban)
}

func (f *Fake) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil || r.PostForm.Get("grant_type") != "client_credentials" ||
		r.PostForm.Get("client_id") != f.ClientID || r.PostForm.Get("client_secret") != f.ClientSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.token, "expires_in": 3600, "token_type": "bearer"})
}

func (f *Fake) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, `{"errors":[{"message":"Unauthorized"}]}`, http.StatusUnauthorized)
		return
	}
	var req struct {
		OperationName string                     `json:"operationName"`
		Variables     map[string]json.RawMessage `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	str := func(name string) string {
		var s string
		_ = json.Unmarshal(req.Variables[name], &s)
		return s
	}
	var input map[string]any
	_ = json.Unmarshal(req.Variables["input"], &input)
	f.mu.Lock()
	defer f.mu.Unlock()
	var data map[string]any
	switch req.OperationName {
	case "Account":
		data = map[string]any{"account": f.accountJSON(str("id"))}
	case "AccountHolderAccounts":
		data = map[string]any{"accountHolder": f.holderJSON(str("id"))}
	case "OpenAccount":
		data = map[string]any{"openAccount": f.openAccount(input)}
	case "CloseAccount":
		data = map[string]any{"closeAccount": f.closeAccount(input)}
	case "InitiateCreditTransfers":
		data = map[string]any{"initiateCreditTransfers": f.initiate(input)}
	case "GrantConsentWithServerSignature":
		data = map[string]any{"grantConsentWithServerSignature": f.grant(input)}
	case "Payment":
		var p any
		if pm := f.payments[str("id")]; pm != nil {
			p = paymentJSON(pm)
		}
		data = map[string]any{"payment": p}
	case "Transaction":
		data = map[string]any{"transaction": f.txJSON(str("id"))}
	case "CreateIndividualAccountHolderOnboarding", "CreateCompanyAccountHolderOnboarding":
		field := strings.ToLower(req.OperationName[:1]) + req.OperationName[1:]
		data = map[string]any{field: f.startOnboarding(req.OperationName, input)}
	case "AccountHolderOnboarding":
		var o any
		if ob := f.onboarding[str("id")]; ob != nil {
			o = f.onboardingJSON(ob)
		}
		data = map[string]any{"accountHolderOnboarding": o}
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "unknown operation " + req.OperationName}}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func rejection(typename, msg string) map[string]any {
	return map[string]any{"__typename": typename, "message": msg}
}

func decimal(c int64) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

func (f *Fake) accountJSON(id string) any {
	a := f.accounts[id]
	if a == nil {
		return nil
	}
	return map[string]any{"id": a.ID, "name": a.Name, "IBAN": a.IBAN, "BIC": "SWNBFR22", "currency": "EUR",
		"statusInfo": map[string]any{"status": a.Status}, "holder": map[string]any{"id": a.Holder, "info": map[string]any{"name": "Ada Ltd"}},
		"balances": map[string]any{"available": map[string]any{"value": decimal(a.Available), "currency": "EUR"}}}
}

func (f *Fake) holderJSON(id string) any {
	if _, ok := f.holders[id]; !ok {
		return nil
	}
	edges := []any{}
	for _, aid := range f.order {
		if f.accounts[aid].Holder == id {
			edges = append(edges, map[string]any{"node": f.accountJSON(aid)})
		}
	}
	return map[string]any{"accounts": map[string]any{"edges": edges, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}}
}

func (f *Fake) openAccount(in map[string]any) any {
	holder, _ := in["accountHolderId"].(string)
	verified, ok := f.holders[holder]
	switch {
	case !ok:
		return rejection("AccountHolderNotFoundRejection", "no account holder "+holder)
	case !verified:
		return rejection("AccountHolderVerificationStatusNotEligibleRejection", "the holder is not verified")
	}
	n := 0
	for _, a := range f.accounts {
		if a.Holder == holder {
			n++
		}
	}
	if n >= f.AccountLimit {
		return rejection("AccountHolderAccountsCreationLimitRejection", "the holder has opened all its accounts")
	}
	name, _ := in["name"].(string)
	if name == "" {
		name = "My account"
	}
	a := &acct{ID: f.id("account"), Holder: holder, Name: name, Status: "Opened"}
	a.IBAN = iban(len(f.order) + 1)
	f.accounts[a.ID], f.order = a, append(f.order, a.ID)
	return map[string]any{"__typename": "OpenAccountSuccessPayload", "account": f.accountJSON(a.ID)}
}

func (f *Fake) closeAccount(in map[string]any) any {
	id, _ := in["accountId"].(string)
	a := f.accounts[id]
	if a == nil {
		return rejection("AccountNotFoundRejection", "no account "+id)
	}
	a.Status = "Closing"
	return map[string]any{"__typename": "CloseAccountSuccessPayload", "account": f.accountJSON(id)}
}

func paymentJSON(p *payment) any {
	si := map[string]any{"__typename": "Payment" + p.Status, "status": p.Status}
	switch p.Status {
	case "ConsentPending":
		si["consent"] = map[string]any{"id": p.Consent, "challenge": p.Challenge}
	case "Rejected":
		si["reason"] = p.Reason
	}
	return map[string]any{"id": p.ID, "statusInfo": si}
}

func (f *Fake) initiate(in map[string]any) any {
	key, _ := in["idempotencyKey"].(string)
	if id, ok := f.byKey[key]; ok && key != "" { // Swan answers a repeated key with the payment it made
		return map[string]any{"__typename": "InitiateCreditTransfersSuccessPayload", "payment": paymentJSON(f.payments[id])}
	}
	from, _ := in["accountId"].(string)
	if a := f.accounts[from]; a == nil || a.Status != "Opened" {
		return rejection("AccountNotFoundRejection", "no account "+from)
	}
	cts, _ := in["creditTransfers"].([]any)
	if len(cts) != 1 {
		return rejection("ValidationRejection", "the fake takes one credit transfer")
	}
	ct, _ := cts[0].(map[string]any)
	amt, _ := ct["amount"].(map[string]any)
	ben, _ := ct["sepaBeneficiary"].(map[string]any)
	value, _ := amt["value"].(string)
	whole, frac, _ := strings.Cut(value, ".")
	cents, err := strconv.ParseInt(whole+(frac + "00")[:2], 10, 64)
	if err != nil || len(frac) > 2 || amt["currency"] != "EUR" || cents <= 0 {
		return rejection("ValidationRejection", "amount "+value)
	}
	p := &payment{ID: f.id("payment"), Account: from, Status: "ConsentPending", Consent: f.id("consent"), Challenge: rnd(), Amount: cents}
	p.IBAN, _ = ben["iban"].(string)
	p.Name, _ = ben["name"].(string)
	p.Own, _ = ben["isMyOwnIban"].(bool)
	p.Label, _ = ct["label"].(string)
	f.payments[p.ID], f.consents[p.Consent] = p, p.ID
	if key != "" {
		f.byKey[key] = p.ID
	}
	return map[string]any{"__typename": "InitiateCreditTransfersSuccessPayload", "payment": paymentJSON(p)}
}

// grant checks the server signature on the consent's challenge, then makes the payment: SEPA out of the account, and
// into the destination at once when it is one of the fake's accounts.
func (f *Fake) grant(in map[string]any) any {
	cid, _ := in["consentId"].(string)
	sig, _ := in["signature"].(string)
	p := f.payments[f.consents[cid]]
	if p == nil {
		return rejection("ConsentNotFoundRejection", "no consent "+cid)
	}
	tok, err := jwt.Parse(sig, func(*jwt.Token) (any, error) { return f.consentPub, nil }, jwt.WithValidMethods([]string{"ES256"}))
	if err != nil {
		return rejection("ServerConsentSignatureNotValidRejection", err.Error())
	}
	if c, _ := tok.Claims.(jwt.MapClaims); c["challenge"] != p.Challenge {
		return rejection("ServerConsentSignatureNotValidRejection", "the signature is of another challenge")
	}
	if p.Status != "ConsentPending" {
		return map[string]any{"__typename": "GrantConsentWithServerSignatureSuccessPayload"}
	}
	from := f.accounts[p.Account]
	if from.Available < p.Amount {
		p.Status, p.Reason = "Rejected", "AccountBalanceInsufficient"
		return map[string]any{"__typename": "GrantConsentWithServerSignatureSuccessPayload"}
	}
	from.Available -= p.Amount
	p.Status = "Initiated"
	f.sent++
	out := &tx{ID: f.id("tx"), Account: from.ID, Type: "SepaCreditTransferOut", Status: "Pending", Counterparty: p.Name, Label: p.Label, Amount: p.Amount}
	f.txs[out.ID] = out
	for _, to := range f.accounts {
		if to.IBAN == p.IBAN && to.Status == "Opened" {
			to.Available += p.Amount
			in := &tx{ID: f.id("tx"), Account: to.ID, Type: "SepaInstantCreditTransferIn", Status: "Booked", Counterparty: "Ada Ltd",
				Label: p.Label, Debtor: from.IBAN, Amount: p.Amount}
			f.txs[in.ID], p.Credit = in, in.ID
		}
	}
	return map[string]any{"__typename": "GrantConsentWithServerSignatureSuccessPayload"}
}

func (f *Fake) txJSON(id string) any {
	t := f.txs[id]
	if t == nil {
		return nil
	}
	j := map[string]any{"id": t.ID, "type": t.Type, "amount": map[string]any{"value": decimal(t.Amount), "currency": "EUR"},
		"statusInfo": map[string]any{"status": t.Status}, "account": map[string]any{"id": t.Account}, "counterparty": t.Counterparty, "label": t.Label}
	if t.Debtor != "" {
		j["debtor"] = map[string]any{"IBAN": t.Debtor}
	}
	return j
}

func (f *Fake) startOnboarding(op string, in map[string]any) any {
	info, _ := in["accountInfo"].(map[string]any)
	if c, _ := info["country"].(string); len(c) != 3 {
		return rejection("ValidationRejection", "accountInfo.country")
	}
	if op == "CreateCompanyAccountHolderOnboarding" {
		if c, _ := in["company"].(map[string]any); c["name"] == nil {
			return rejection("ValidationRejection", "company.name")
		}
	}
	o := &onboard{ID: f.id("onboarding")}
	f.onboarding[o.ID] = o
	return map[string]any{"__typename": op + "SuccessPayload", "onboarding": f.onboardingJSON(o)}
}

func (f *Fake) onboardingJSON(o *onboard) any {
	j := map[string]any{"id": o.ID, "onboardingUrl": "https://onboarding.example/" + o.ID, "accountHolder": nil}
	if o.Holder != "" {
		st := "Pending"
		if o.Verified {
			st = "Verified"
		}
		j["accountHolder"] = map[string]any{"id": o.Holder, "verificationStatus": st}
	}
	return j
}

// Finish is the person completing onboarding id and Swan verifying the account holder it made.
func (f *Fake) Finish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.onboarding[id]
	o.Holder, o.Verified = f.id("holder"), true
	f.holders[o.Holder] = true
}

// Deposit books a SEPA credit of amount into account ref from PayerIBAN, as transaction txID.
func (f *Fake) Deposit(ref, txID string, amount money.Amount) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[ref].Available += amount.Minor
	f.txs[txID] = &tx{ID: txID, Account: ref, Type: "SepaCreditTransferIn", Status: "Booked", Counterparty: Payer, Debtor: PayerIBAN, Amount: amount.Minor}
}

// Pend puts transaction txID back to pending, as a credit Swan has not booked yet is.
func (f *Fake) Pend(txID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txs[txID].Status = "Pending"
}

// CreditOf is the transaction a payment booked into one of the fake's accounts.
func (f *Fake) CreditOf(paymentID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payments[paymentID].Credit
}

// Sent is how many payments the fake made.
func (f *Fake) Sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent
}

// Notify is Swan's webhook that transaction txID was booked: its body and headers.
func (f *Fake) Notify(eventID, txID string) ([]byte, http.Header) {
	body, _ := json.Marshal(map[string]any{"eventType": "Transaction.Booked", "eventId": eventID, "eventDate": time.Now().UTC().Format(time.RFC3339),
		"projectId": f.ProjectID, "resourceId": txID})
	return body, http.Header{"X-Swan-Secret": {f.WebhookSecret}}
}
