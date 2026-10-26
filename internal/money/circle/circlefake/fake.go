// Package circlefake is a local fake of the parts of Circle's developer-controlled wallets API the Circle adapter uses
// (B46.5), written from Circle's published API reference: wallets, balances, transfers, transactions, tokens, address
// screening, the entity public key and the notification signing key. It holds test money in memory and never calls
// Circle. A test pays into it with Deposit, signs a notification with Notify, and sets how an address screens with
// Screening.
package circlefake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/money"
)

// The fake's tokens: USDC on Base Sepolia, and a token that calls itself USDC at another contract address.
const (
	USDCTokenID   = "7b4e0c52-0000-4000-8000-00000000usdc"
	SpoofTokenID  = "7b4e0c52-0000-4000-8000-0000000spoof"
	USDCAddress   = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	spoofAddress  = "0x0000000000000000000000000000000000005900"
	Blockchain    = "BASE-SEPOLIA"
	NotifyKeyID   = "5d2f8a3e-1c4b-4f6a-9e7d-0a1b2c3d4e5f"
	entitySecretN = 32
)

// Fake is a running fake Circle.
type Fake struct {
	URL          string
	APIKey       string
	EntitySecret string // hex, as Circle's console shows it
	WalletSetID  string

	entityKey *rsa.PrivateKey
	notifyKey *ecdsa.PrivateKey

	mu          sync.Mutex
	wallets     map[string]*wallet
	txs         map[string]*tx
	idem        map[string]idem   // POST answers by idempotency key
	ciphertexts map[string]bool   // every ciphertext seen: Circle refuses one used twice
	screening   map[string]screen // lower-case addresses screening does not simply approve
	balances    map[string]int64  // USDC micro-units by wallet id
	spoof       map[string]int64  // the spoof token's balance by wallet id
	Screened    []string          // every address screened, in order
	n           int
}

type wallet struct {
	ID          string `json:"id"`
	Address     string `json:"address"`
	Blockchain  string `json:"blockchain"`
	WalletSetID string `json:"walletSetId"`
	CustodyType string `json:"custodyType"`
	State       string `json:"state"`
	AccountType string `json:"accountType"`
	RefID       string `json:"refId,omitempty"`
	Name        string `json:"name,omitempty"`
	CreateDate  string `json:"createDate"`
	UpdateDate  string `json:"updateDate"`
}

type tx struct {
	ID                 string   `json:"id"`
	State              string   `json:"state"`
	Blockchain         string   `json:"blockchain"`
	TransactionType    string   `json:"transactionType"`
	Operation          string   `json:"operation"`
	WalletID           string   `json:"walletId"`
	SourceAddress      string   `json:"sourceAddress,omitempty"`
	DestinationAddress string   `json:"destinationAddress"`
	Amounts            []string `json:"amounts"`
	TokenID            string   `json:"tokenId"`
	RefID              string   `json:"refId,omitempty"`
	TxHash             string   `json:"txHash,omitempty"`
	CreateDate         string   `json:"createDate"`
	UpdateDate         string   `json:"updateDate"`
}

type screen struct {
	result     string
	categories []string
}

type idem struct {
	body string // the request without its ciphertext
	resp any
}

// New starts a fake Circle, closed when t ends.
func New(t testing.TB) *Fake {
	t.Helper()
	ek, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	nk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, entitySecretN)
	_, _ = rand.Read(secret)
	f := &Fake{APIKey: "TEST_API_KEY:fake:secret", EntitySecret: hex.EncodeToString(secret), WalletSetID: "0a0b0c0d-0000-4000-8000-00000000set1",
		entityKey: ek, notifyKey: nk, wallets: map[string]*wallet{}, txs: map[string]*tx{}, idem: map[string]idem{},
		ciphertexts: map[string]bool{}, screening: map[string]screen{}, balances: map[string]int64{}, spoof: map[string]int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/w3s/config/entity/publicKey", f.entityPublicKey)
	mux.HandleFunc("POST /v1/w3s/developer/wallets", f.createWallets)
	mux.HandleFunc("GET /v1/w3s/wallets/{id}", f.getWallet)
	mux.HandleFunc("GET /v1/w3s/wallets/{id}/balances", f.getBalances)
	mux.HandleFunc("POST /v1/w3s/developer/transactions/transfer", f.transfer)
	mux.HandleFunc("GET /v1/w3s/transactions/{id}", f.getTransaction)
	mux.HandleFunc("GET /v1/w3s/transactions", f.listTransactions)
	mux.HandleFunc("GET /v1/w3s/tokens/{id}", f.getToken)
	mux.HandleFunc("POST /v1/w3s/compliance/screening/addresses", f.screen)
	mux.HandleFunc("GET /v2/notifications/publicKey/{id}", f.notifyPublicKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.APIKey {
			fail(w, http.StatusUnauthorized, 401, "Malformed authorization.")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// Deposit is amount of USDC arriving in walletID on chain as transaction txID, complete.
func (f *Fake) Deposit(walletID, txID string, amount money.Amount) {
	f.deposit(walletID, txID, USDCTokenID, amount.Minor)
}

// DepositSpoof is amount of the token that calls itself USDC at another address arriving in walletID.
func (f *Fake) DepositSpoof(walletID, txID string, minor int64) {
	f.deposit(walletID, txID, SpoofTokenID, minor)
}

func (f *Fake) deposit(walletID, txID, token string, minor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.wallets[walletID]
	if w == nil {
		panic("circlefake: a deposit into no wallet " + walletID)
	}
	if token == USDCTokenID {
		f.balances[walletID] += minor
	} else {
		f.spoof[walletID] += minor
	}
	now := stamp()
	f.txs[txID] = &tx{ID: txID, State: "COMPLETE", Blockchain: Blockchain, TransactionType: "INBOUND", Operation: "TRANSFER",
		WalletID: walletID, SourceAddress: "0x00000000000000000000000000000000000a11ce", DestinationAddress: w.Address,
		Amounts: []string{decimal(minor)}, TokenID: token, TxHash: "0x" + hex.EncodeToString([]byte(txID)), CreateDate: now, UpdateDate: now}
}

// Notify is the notification Circle sends about transaction txID, of notificationType kind, signed with the fake's
// notification key, and its headers.
func (f *Fake) Notify(notificationID, kind, txID string) ([]byte, http.Header) {
	f.mu.Lock()
	t := *f.txs[txID]
	f.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"subscriptionId": "9f8e7d6c-0000-4000-8000-000000000sub", "notificationId": notificationID,
		"notificationType": kind, "notification": t, "timestamp": stamp(), "version": 2})
	return body, f.Sign(body)
}

// Complete is transaction txID confirmed on chain.
func (f *Fake) Complete(txID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txs[txID].State = "COMPLETE"
}

// Sign is the headers Circle signs body with.
func (f *Fake) Sign(body []byte) http.Header {
	sum := sha256.Sum256(body)
	sig, err := ecdsa.SignASN1(rand.Reader, f.notifyKey, sum[:])
	if err != nil {
		panic(err)
	}
	return http.Header{"X-Circle-Signature": {base64.StdEncoding.EncodeToString(sig)}, "X-Circle-Key-Id": {NotifyKeyID}}
}

// Screening makes screening answer result (APPROVED or DENIED) for address, with a risk signal in each of categories
// (SANCTIONS, …).
func (f *Fake) Screening(address, result string, categories ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.screening[strings.ToLower(address)] = screen{result, categories}
}

// Outbound is how many transfers out the fake was asked to make.
func (f *Fake) Outbound() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.txs {
		if t.TransactionType == "OUTBOUND" {
			n++
		}
	}
	return n
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339) }

func (f *Fake) uuid(kind string) string {
	f.n++
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", f.n, len(kind))
}

func decimal(minor int64) string {
	s := fmt.Sprintf("%d.%06d", minor/1_000_000, minor%1_000_000)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

func micro(s string) (int64, bool) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 6 {
		return 0, false
	}
	var n int64
	for _, c := range whole + frac + strings.Repeat("0", 6-len(frac)) {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

func send(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func fail(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
}

func (f *Fake) entityPublicKey(w http.ResponseWriter, _ *http.Request) {
	der, _ := x509.MarshalPKIXPublicKey(&f.entityKey.PublicKey)
	send(w, http.StatusOK, map[string]string{"publicKey": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))})
}

func (f *Fake) notifyPublicKey(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != NotifyKeyID {
		fail(w, http.StatusNotFound, 404, "public key not found")
		return
	}
	der, _ := x509.MarshalPKIXPublicKey(&f.notifyKey.PublicKey)
	send(w, http.StatusOK, map[string]string{"id": NotifyKeyID, "algorithm": "ECDSA_SHA_256",
		"publicKey": base64.StdEncoding.EncodeToString(der), "createDate": stamp()})
}

// write reads a POST that Circle asks the entity secret for: its ciphertext must decrypt to the secret and be new, and
// a repeated idempotency key answers what it answered first, or 409 for a different request.
func (f *Fake) write(w http.ResponseWriter, r *http.Request, needsSecret bool, req map[string]any, do func() (int, any)) {
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, 2, "Invalid JSON")
		return
	}
	key, _ := req["idempotencyKey"].(string)
	if len(key) != 36 {
		fail(w, http.StatusBadRequest, 2, "idempotencyKey must be a UUID")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if needsSecret {
		ct, _ := req["entitySecretCiphertext"].(string)
		raw, err := base64.StdEncoding.DecodeString(ct)
		if err != nil {
			fail(w, http.StatusBadRequest, 156016, "entitySecretCiphertext is invalid")
			return
		}
		plain, err := rsa.DecryptOAEP(sha256.New(), nil, f.entityKey, raw, nil)
		if err != nil || hex.EncodeToString(plain) != f.EntitySecret {
			fail(w, http.StatusBadRequest, 156016, "entitySecretCiphertext is invalid")
			return
		}
		if f.ciphertexts[ct] {
			fail(w, http.StatusBadRequest, 156017, "entitySecretCiphertext was already used")
			return
		}
		f.ciphertexts[ct] = true
		delete(req, "entitySecretCiphertext")
	}
	b, _ := json.Marshal(req)
	if prev, ok := f.idem[r.URL.Path+key]; ok {
		if prev.body != string(b) {
			fail(w, http.StatusConflict, 409, "idempotencyKey was used for a different request")
			return
		}
		send(w, http.StatusOK, prev.resp)
		return
	}
	status, resp := do()
	if status >= 300 {
		fail(w, status, status, fmt.Sprint(resp))
		return
	}
	f.idem[r.URL.Path+key] = idem{body: string(b), resp: resp}
	send(w, status, resp)
}

func (f *Fake) createWallets(w http.ResponseWriter, r *http.Request) {
	req := map[string]any{}
	f.write(w, r, true, req, func() (int, any) {
		if req["walletSetId"] != f.WalletSetID {
			return http.StatusNotFound, "wallet set not found"
		}
		chains, _ := req["blockchains"].([]any)
		if len(chains) != 1 || chains[0] != Blockchain {
			return http.StatusBadRequest, "the fake makes wallets on " + Blockchain + " only"
		}
		var ref, name string
		if md, _ := req["metadata"].([]any); len(md) == 1 {
			m, _ := md[0].(map[string]any)
			ref, _ = m["refId"].(string)
			name, _ = m["name"].(string)
		}
		id := f.uuid("wallet")
		sum := sha256.Sum256([]byte(id))
		now := stamp()
		acct, _ := req["accountType"].(string)
		wl := &wallet{ID: id, Address: "0x" + hex.EncodeToString(sum[:20]), Blockchain: Blockchain, WalletSetID: f.WalletSetID,
			CustodyType: "DEVELOPER", State: "LIVE", AccountType: acct, RefID: ref, Name: name, CreateDate: now, UpdateDate: now}
		f.wallets[id] = wl
		return http.StatusCreated, map[string]any{"wallets": []*wallet{wl}}
	})
}

func (f *Fake) getWallet(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	wl := f.wallets[r.PathValue("id")]
	if wl == nil {
		fail(w, http.StatusNotFound, 156001, "wallet not found")
		return
	}
	send(w, http.StatusOK, map[string]any{"wallet": wl})
}

func (f *Fake) getBalances(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := r.PathValue("id")
	if f.wallets[id] == nil {
		fail(w, http.StatusNotFound, 156001, "wallet not found")
		return
	}
	var tbs []map[string]any
	if n := f.spoof[id]; n > 0 {
		tbs = append(tbs, map[string]any{"token": token(SpoofTokenID), "amount": decimal(n), "updateDate": stamp()})
	}
	if n := f.balances[id]; n > 0 {
		tbs = append(tbs, map[string]any{"token": token(USDCTokenID), "amount": decimal(n), "updateDate": stamp()})
	}
	send(w, http.StatusOK, map[string]any{"tokenBalances": tbs})
}

func token(id string) map[string]any {
	addr := USDCAddress
	if id == SpoofTokenID {
		addr = spoofAddress
	}
	return map[string]any{"id": id, "blockchain": Blockchain, "tokenAddress": addr, "standard": "ERC20", "name": "USD Coin",
		"symbol": "USDC", "decimals": 6, "isNative": false}
}

func (f *Fake) getToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id != USDCTokenID && id != SpoofTokenID {
		fail(w, http.StatusNotFound, 404, "token not found")
		return
	}
	send(w, http.StatusOK, map[string]any{"token": token(id)})
}

func (f *Fake) transfer(w http.ResponseWriter, r *http.Request) {
	req := map[string]any{}
	f.write(w, r, true, req, func() (int, any) {
		from, _ := req["walletId"].(string)
		if f.wallets[from] == nil {
			return http.StatusNotFound, "wallet not found"
		}
		if !strings.EqualFold(fmt.Sprint(req["tokenAddress"]), USDCAddress) || req["blockchain"] != Blockchain {
			return http.StatusBadRequest, "the fake moves USDC on " + Blockchain + " only"
		}
		amounts, _ := req["amounts"].([]any)
		if len(amounts) != 1 {
			return http.StatusBadRequest, "one amount"
		}
		n, ok := micro(fmt.Sprint(amounts[0]))
		if !ok || n <= 0 {
			return http.StatusBadRequest, "amounts must be positive decimals"
		}
		if f.balances[from] < n {
			return http.StatusBadRequest, "insufficient token balance"
		}
		dest, _ := req["destinationAddress"].(string)
		ref, _ := req["refId"].(string)
		f.balances[from] -= n
		id, now := f.uuid("tx"), stamp()
		f.txs[id] = &tx{ID: id, State: "INITIATED", Blockchain: Blockchain, TransactionType: "OUTBOUND", Operation: "TRANSFER",
			WalletID: from, SourceAddress: f.wallets[from].Address, DestinationAddress: dest, Amounts: []string{decimal(n)},
			TokenID: USDCTokenID, RefID: ref, CreateDate: now, UpdateDate: now}
		// A transfer to one of the fake's own wallets lands there.
		for _, to := range f.wallets {
			if strings.EqualFold(to.Address, dest) {
				f.balances[to.ID] += n
			}
		}
		return http.StatusCreated, map[string]any{"id": id, "state": "INITIATED"}
	})
}

func (f *Fake) getTransaction(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.txs[r.PathValue("id")]
	if t == nil {
		fail(w, http.StatusNotFound, 177001, "transaction not found")
		return
	}
	send(w, http.StatusOK, map[string]any{"transaction": t})
}

func (f *Fake) listTransactions(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	out := []*tx{}
	for _, t := range f.txs {
		if (q.Get("walletIds") == "" || q.Get("walletIds") == t.WalletID) && (q.Get("txType") == "" || q.Get("txType") == t.TransactionType) {
			out = append(out, t)
		}
	}
	send(w, http.StatusOK, map[string]any{"transactions": out})
}

func (f *Fake) screen(w http.ResponseWriter, r *http.Request) {
	req := map[string]any{}
	f.write(w, r, false, req, func() (int, any) {
		addr, _ := req["address"].(string)
		f.Screened = append(f.Screened, addr)
		result, actions, reasons := "APPROVED", []string{"APPROVE"}, []map[string]any{}
		if sc, ok := f.screening[strings.ToLower(addr)]; ok {
			if result = sc.result; result == "DENIED" {
				actions = []string{"DENY"}
			}
			for _, cat := range sc.categories {
				reasons = append(reasons, map[string]any{"source": "ADDRESS", "sourceValue": addr, "riskScore": "HIGH",
					"riskCategories": []string{cat}, "type": "OWNERSHIP", "signalSource": map[string]string{"rowId": f.uuid("row"), "pointer": "/"}})
			}
		}
		return http.StatusOK, map[string]any{"result": result, "id": req["idempotencyKey"], "address": addr, "chain": req["chain"],
			"decision": map[string]any{"screeningDate": stamp(), "ruleName": "fake sanctions", "actions": actions, "reasons": reasons},
			"details":  []any{}}
	})
}
