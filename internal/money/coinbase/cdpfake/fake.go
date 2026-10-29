// Package cdpfake is a local fake of the parts of Coinbase Developer Platform the Coinbase adapter uses (B46.6),
// written from CDP's published v2 API reference: EVM accounts, smart accounts, token balances, prepare-and-send user
// operations, the JWTs every call carries, idempotency keys, a Base node's eth_getTransactionReceipt, and signed
// onchain-activity webhooks. It holds test money in memory and never calls Coinbase. A test pays into it with Deposit
// and signs a notification with Notify.
package cdpfake

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	secpecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/sha3"

	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/coinbase"
)

// The fake's chain and tokens: USDC on Base Sepolia, and a token that calls itself USDC at another contract address.
const (
	Network       = "base-sepolia"
	USDCAddress   = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	SpoofAddress  = "0x0000000000000000000000000000000000005900"
	Payer         = "0x00000000000000000000000000000000000a11ce"
	transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef" // Transfer(address,address,uint256)
	ethAddress    = "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE"
)

// Fake is a running fake CDP.
type Fake struct {
	URL           string
	RPCURL        string
	APIKeyID      string
	APIKeySecret  string // base64 of the Ed25519 seed and public key, as CDP's portal gives it
	WalletSecret  string // base64 DER PKCS#8 of a P-256 key, as CDP's portal gives it
	WebhookSecret string

	apiKey    ed25519.PublicKey
	walletKey *ecdsa.PublicKey

	mu       sync.Mutex
	accounts map[string]*account // by lower-case address
	names    map[string]string   // account name → lower-case address
	usdc     map[string]int64    // USDC base units by lower-case address
	spoof    map[string]int64
	receipts map[string]receipt // by transaction hash, and by user operation hash
	idem     map[string]reply
	sent     int
	signed   int
	n        int
}

type account struct {
	Address string   `json:"address"`
	Name    string   `json:"name"`
	Owners  []string `json:"owners,omitempty"`
	smart   bool
	key     *secp256k1.PrivateKey // an EVM account's, which CDP keeps inside its enclave
	solana  ed25519.PrivateKey    // a Solana account's
}

type log struct {
	Address  string   `json:"address"`
	Topics   []string `json:"topics"`
	Data     string   `json:"data"`
	LogIndex string   `json:"logIndex"`
}

type receipt struct {
	TransactionHash string `json:"transactionHash"`
	Status          string `json:"status"`
	Logs            []log  `json:"logs"`
}

type reply struct {
	hash string
	code int
	body []byte
}

// New starts a fake CDP for t.
func New(t *testing.T) *Fake {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(wk)
	if err != nil {
		t.Fatal(err)
	}
	f := &Fake{APIKeyID: "organizations/fake/apiKeys/" + hex.EncodeToString(pub[:4]), APIKeySecret: base64.StdEncoding.EncodeToString(priv),
		WalletSecret: base64.StdEncoding.EncodeToString(der), WebhookSecret: "whsec_" + hex.EncodeToString(pub[4:12]),
		apiKey: pub, walletKey: &wk.PublicKey, accounts: map[string]*account{}, names: map[string]string{},
		usdc: map[string]int64{}, spoof: map[string]int64{}, receipts: map[string]receipt{}, idem: map[string]reply{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL, f.RPCURL = srv.URL, srv.URL+"/rpc/fake-client-key"
	return f
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func cdpError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]string{"errorType": typ, "errorMessage": msg, "correlationId": "fake"})
}

// canonical is body with its keys sorted at every depth, compact: what reqHash is the SHA-256 of.
func canonical(body []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// authorised checks the bearer JWT every call carries, and the wallet JWT every POST carries.
func (f *Fake) authorised(r *http.Request, body []byte) error {
	uri := r.Method + " " + r.Host + r.URL.Path
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return fmt.Errorf("no bearer token")
	}
	var c struct {
		jwt.RegisteredClaims
		URI string `json:"uri"`
	}
	if _, err := jwt.ParseWithClaims(tok, &c, func(*jwt.Token) (any, error) { return f.apiKey, nil },
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("cdp"), jwt.WithAudience("cdp_service"), jwt.WithSubject(f.APIKeyID), jwt.WithExpirationRequired()); err != nil {
		return err
	}
	if c.URI != uri {
		return fmt.Errorf("bearer token is for %q, not %q", c.URI, uri)
	}
	if r.Method != http.MethodPost {
		return nil
	}
	var wc struct {
		jwt.RegisteredClaims
		URIs    []string `json:"uris"`
		ReqHash string   `json:"reqHash"`
	}
	if _, err := jwt.ParseWithClaims(r.Header.Get("X-Wallet-Auth"), &wc, func(*jwt.Token) (any, error) { return f.walletKey, nil },
		jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuedAt()); err != nil {
		return fmt.Errorf("wallet auth: %w", err)
	}
	cb, err := canonical(body)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(cb)
	if !slices.Contains(wc.URIs, uri) || wc.ReqHash != hex.EncodeToString(sum[:]) || wc.ID == "" {
		return fmt.Errorf("wallet auth is not for this request")
	}
	return nil
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,34}[A-Za-z0-9]$`)

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		body = b.Bytes()
	}
	if strings.HasPrefix(r.URL.Path, "/rpc/") {
		f.rpc(w, body)
		return
	}
	if err := f.authorised(r, body); err != nil {
		cdpError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Header.Get("X-Idempotency-Key")
	sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\x00"), body...))
	if prev, ok := f.idem[key]; ok && key != "" {
		if prev.hash != hex.EncodeToString(sum[:]) {
			cdpError(w, http.StatusUnprocessableEntity, "idempotency_error", "the key was used for another request")
			return
		}
		writeJSON(w, prev.code, json.RawMessage(prev.body))
		return
	}
	rec := httptest.NewRecorder()
	f.route(rec, r, body)
	if key != "" && r.Method == http.MethodPost && rec.Code < 500 {
		f.idem[key] = reply{hash: hex.EncodeToString(sum[:]), code: rec.Code, body: rec.Body.Bytes()}
	}
	writeJSON(w, rec.Code, json.RawMessage(rec.Body.Bytes()))
}

func (f *Fake) route(w http.ResponseWriter, r *http.Request, body []byte) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/evm/"), "/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v2/evm/accounts":
		var req struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &req) != nil || !nameRE.MatchString(req.Name) {
			cdpError(w, http.StatusBadRequest, "invalid_request", "invalid account name")
			return
		}
		f.create(w, req.Name, nil)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/evm/smart-accounts":
		var req struct {
			Owners []string `json:"owners"`
			Name   string   `json:"name"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Owners) != 1 || f.accounts[strings.ToLower(req.Owners[0])] == nil || !nameRE.MatchString(req.Name) {
			cdpError(w, http.StatusBadRequest, "invalid_request", "Invalid owner address or account name provided.")
			return
		}
		f.create(w, req.Name, req.Owners)
	case r.Method == http.MethodPost && r.URL.Path == "/v2/solana/accounts":
		var req struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &req) != nil || !nameRE.MatchString(req.Name) {
			cdpError(w, http.StatusBadRequest, "invalid_request", "invalid account name")
			return
		}
		f.createSolana(w, req.Name)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "accounts" && parts[2] == "sign":
		f.sign(w, strings.ToLower(parts[1]), body)
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "smart-accounts":
		a := f.accounts[strings.ToLower(parts[1])]
		if a == nil || !a.smart {
			cdpError(w, http.StatusNotFound, "not_found", "Smart Account not found.")
			return
		}
		writeJSON(w, http.StatusOK, a)
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "token-balances":
		f.balances(w, r, parts[1], strings.ToLower(parts[2]))
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "smart-accounts" && parts[2] == "user-operations" && parts[3] == "prepare-and-send":
		f.send(w, strings.ToLower(parts[1]), body)
	default:
		cdpError(w, http.StatusNotFound, "not_found", "no such endpoint in the fake: "+r.Method+" "+r.URL.Path)
	}
}

func (f *Fake) create(w http.ResponseWriter, name string, owners []string) {
	if f.names[name] != "" {
		cdpError(w, http.StatusConflict, "already_exists", "an account with this name already exists")
		return
	}
	f.n++
	a := &account{Address: fmt.Sprintf("0x5A%038x", f.n), Name: name, Owners: owners, smart: owners != nil}
	if !a.smart { // an EVM account is a real key, so what it signs a chain accepts
		k, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			cdpError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		h := sha3.NewLegacyKeccak256()
		h.Write(k.PubKey().SerializeUncompressed()[1:])
		a.Address, a.key = "0x"+hex.EncodeToString(h.Sum(nil)[12:]), k
	}
	f.accounts[strings.ToLower(a.Address)], f.names[name] = a, strings.ToLower(a.Address)
	writeJSON(w, http.StatusCreated, a)
}

// createSolana makes a Solana account: an Ed25519 key, its address the public key in base58.
func (f *Fake) createSolana(w http.ResponseWriter, name string) {
	if f.names[name] != "" {
		cdpError(w, http.StatusConflict, "already_exists", "an account with this name already exists")
		return
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		cdpError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	a := &account{Address: base58(pub), Name: name, solana: priv}
	f.accounts[strings.ToLower(a.Address)], f.names[name] = a, strings.ToLower(a.Address)
	writeJSON(w, http.StatusCreated, a)
}

// base58 is b in Bitcoin's base58 alphabet, as Solana writes an address.
func base58(b []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	n := new(big.Int).SetBytes(b)
	var out []byte
	for mod := new(big.Int); n.Sign() > 0; {
		n.DivMod(n, big.NewInt(58), mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	slices.Reverse(out)
	return string(out)
}

// sign is CDP's signature of a hash with EVM account addr's key: r‖s‖v, v 27 or 28.
func (f *Fake) sign(w http.ResponseWriter, addr string, body []byte) {
	var req struct {
		Hash string `json:"hash"`
	}
	a := f.accounts[addr]
	if a == nil || a.key == nil {
		cdpError(w, http.StatusNotFound, "not_found", "EVM account not found.")
		return
	}
	if json.Unmarshal(body, &req) != nil || !strings.HasPrefix(req.Hash, "0x") {
		cdpError(w, http.StatusBadRequest, "invalid_request", "hash must be 0x hex")
		return
	}
	h, err := hex.DecodeString(req.Hash[2:])
	if err != nil || len(h) != 32 {
		cdpError(w, http.StatusBadRequest, "invalid_request", "hash must be 32 bytes")
		return
	}
	c := secpecdsa.SignCompact(a.key, h, false) // v‖r‖s
	f.signed++
	writeJSON(w, http.StatusOK, map[string]string{"signature": "0x" + hex.EncodeToString(append(c[1:], c[0]))})
}

// Signed is how many hashes the fake signed.
func (f *Fake) Signed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signed
}

// PrivateKey is EVM account addr's private key as hex, which only a test asks for: to prove it is nowhere else.
func (f *Fake) PrivateKey(addr string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[strings.ToLower(addr)]; a != nil && a.key != nil {
		return hex.EncodeToString(a.key.Serialize())
	} else if a != nil && a.solana != nil {
		return hex.EncodeToString(a.solana.Seed())
	}
	return ""
}

// balances is CDP's token balances, two to a page: ETH and the spoof token first, USDC on the second page.
func (f *Fake) balances(w http.ResponseWriter, r *http.Request, network, addr string) {
	if network != Network {
		cdpError(w, http.StatusBadRequest, "invalid_request", "unsupported network")
		return
	}
	type amount struct {
		Amount   string `json:"amount"`
		Decimals int    `json:"decimals"`
	}
	type token struct {
		Network         string `json:"network"`
		Symbol          string `json:"symbol"`
		Name            string `json:"name"`
		ContractAddress string `json:"contractAddress"`
	}
	type balance struct {
		Amount amount `json:"amount"`
		Token  token  `json:"token"`
	}
	page := []balance{
		{amount{"0", 18}, token{Network, "ETH", "ether", ethAddress}},
		{amount{strconv.FormatInt(f.spoof[addr], 10), 6}, token{Network, "USDC", "USD Coin", SpoofAddress}},
	}
	next := "page-2"
	if r.URL.Query().Get("pageToken") == "page-2" {
		page, next = []balance{{amount{strconv.FormatInt(f.usdc[addr], 10), 6}, token{Network, "USDC", "USD Coin", USDCAddress}}}, ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"balances": page, "nextPageToken": next})
}

// send is prepare-and-send for one call: a USDC transfer, which is all the adapter asks for.
func (f *Fake) send(w http.ResponseWriter, from string, body []byte) {
	a := f.accounts[from]
	if a == nil || !a.smart {
		cdpError(w, http.StatusNotFound, "not_found", "Smart Account not found.")
		return
	}
	var req struct {
		Network string `json:"network"`
		Calls   []struct {
			To    string `json:"to"`
			Value string `json:"value"`
			Data  string `json:"data"`
		} `json:"calls"`
	}
	if json.Unmarshal(body, &req) != nil || req.Network != Network || len(req.Calls) != 1 {
		cdpError(w, http.StatusBadRequest, "invalid_request", "invalid user operation")
		return
	}
	c := req.Calls[0]
	data := strings.TrimPrefix(c.Data, "0x")
	if !strings.EqualFold(c.To, USDCAddress) || c.Value != "0" || len(data) != 8+64+64 || data[:8] != "a9059cbb" {
		cdpError(w, http.StatusBadRequest, "invalid_request", "the fake sends USDC transfers only")
		return
	}
	to := "0x" + data[8+24:8+64]
	n, ok := new(big.Int).SetString(data[8+64:], 16)
	if !ok || !n.IsInt64() || n.Int64() <= 0 {
		cdpError(w, http.StatusBadRequest, "invalid_request", "bad amount")
		return
	}
	if f.usdc[from] < n.Int64() {
		cdpError(w, http.StatusBadRequest, "invalid_request", "execution reverted: ERC20: transfer amount exceeds balance")
		return
	}
	f.usdc[from] -= n.Int64()
	f.usdc[to] += n.Int64()
	f.sent++
	sum := sha256.Sum256(append([]byte(from+to), body...))
	hash := "0x" + hex.EncodeToString(sum[:])
	f.receipts[hash] = transferReceipt(hash, USDCAddress, from, to, n.Int64())
	writeJSON(w, http.StatusOK, map[string]any{"network": Network, "userOpHash": hash, "calls": req.Calls, "status": "broadcast"})
}

func word(hexed string) string {
	return "0x" + strings.Repeat("0", 64-len(hexed)) + hexed
}

func transferReceipt(hash, token, from, to string, amount int64) receipt {
	return receipt{TransactionHash: hash, Status: "0x1", Logs: []log{{Address: token, LogIndex: "0x0", Data: word(strconv.FormatInt(amount, 16)),
		Topics: []string{transferTopic, word(strings.ToLower(strings.TrimPrefix(from, "0x"))), word(strings.ToLower(strings.TrimPrefix(to, "0x")))}}}}
}

// rpc is a Base node's JSON-RPC: eth_getTransactionReceipt, null for a transaction it has not seen.
func (f *Fake) rpc(w http.ResponseWriter, body []byte) {
	var req struct {
		ID     any      `json:"id"`
		Method string   `json:"method"`
		Params []string `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || req.Method != "eth_getTransactionReceipt" || len(req.Params) != 1 {
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	f.mu.Lock()
	rc, ok := f.receipts[req.Params[0]]
	f.mu.Unlock()
	var result any
	if ok {
		result = rc
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// Deposit pays amount of USDC into wallet from Payer, in transaction txHash.
func (f *Fake) Deposit(wallet, txHash string, amount money.Amount) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usdc[strings.ToLower(wallet)] += amount.Minor
	f.receipts[txHash] = transferReceipt(txHash, USDCAddress, Payer, wallet, amount.Minor)
}

// DepositSpoof pays amount of the token that calls itself USDC into wallet, in transaction txHash.
func (f *Fake) DepositSpoof(wallet, txHash string, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spoof[strings.ToLower(wallet)] += amount
	f.receipts[txHash] = transferReceipt(txHash, SpoofAddress, Payer, wallet, amount)
}

// Sent is how many user operations CDP sent.
func (f *Fake) Sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent
}

// Notify is CDP's signed onchain.activity.detected webhook for the first transfer in ref, a deposit's transaction hash
// or a sent user operation's hash.
func (f *Fake) Notify(id, ref string) ([]byte, http.Header) {
	f.mu.Lock()
	rc := f.receipts[ref]
	f.mu.Unlock()
	l := rc.Logs[0]
	value, _ := new(big.Int).SetString(strings.TrimPrefix(l.Data, "0x"), 16)
	body, _ := json.Marshal(map[string]any{"id": id, "type": "onchain.activity.detected", "createdAt": time.Now().UTC().Format(time.RFC3339Nano),
		"data": map[string]any{"subscriptionId": "sub_fake", "networkId": Network, "blockNumber": 12345678, "blockHash": "0xb10c",
			"transactionHash": rc.TransactionHash, "logIndex": 0, "contractAddress": strings.ToLower(l.Address), "eventName": "Transfer",
			"from": "0x" + l.Topics[1][26:], "to": "0x" + l.Topics[2][26:], "value": value.String()}})
	return body, f.Sign(body)
}

// Sign is the X-Webhook-Signature header CDP sends with body, signed with the fake's webhook secret now.
func (f *Fake) Sign(body []byte) http.Header { return f.SignAt(body, time.Now()) }

// SignAt is Sign at at.
func (f *Fake) SignAt(body []byte, at time.Time) http.Header {
	t := strconv.FormatInt(at.Unix(), 10)
	mac := func(s string) string {
		m := hmac.New(sha256.New, []byte(f.WebhookSecret))
		m.Write([]byte(s))
		return hex.EncodeToString(m.Sum(nil))
	}
	h := "content-type"
	return http.Header{"Content-Type": {"application/json"},
		"X-Webhook-Signature": {"t=" + t + ",v0=" + mac(t+"."+string(body)) + ",h=" + h + ",v1=" + mac(t+"."+h+".application/json."+string(body))}}
}

// refuseAll allows and clears no address: agents' keys move no money themselves.
type refuseAll struct{}

func (refuseAll) Approved(context.Context, string, string, string) (bool, error) { return false, nil }
func (refuseAll) Screen(context.Context, string, string) error {
	return fmt.Errorf("%w: the fake clears no address", money.ErrRefused)
}

// Signers is Coinbase's adapter's agents' keys (B48.2), against f.
func (f *Fake) Signers(t *testing.T) money.Signers {
	t.Helper()
	a, err := coinbase.New(coinbase.Config{BaseURL: f.URL, APIKeyID: f.APIKeyID, APIKeySecret: f.APIKeySecret, WalletSecret: f.WalletSecret,
		Network: Network, RPCURL: f.RPCURL, WebhookSecret: f.WebhookSecret, Allowlist: refuseAll{}, Screener: refuseAll{}})
	if err != nil {
		t.Fatal(err)
	}
	return a.Signers
}
