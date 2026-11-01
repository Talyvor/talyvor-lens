// Package coinbase is the money layer's stablecoin adapter for Coinbase Developer Platform's server wallets (B46.6): a
// smart account per agent on Base, its USDC balance and receive address, transfers out to an address the owner approved
// and screening cleared, and deposits credited from CDP's signed onchain-activity webhooks and checked against a Base
// node. It is written from CDP's published v2 API reference (docs.cdp.coinbase.com) and tested against cdpfake. It
// serves nothing until CDP's keys are on the server, it is registered, and its switch is on (internal/money).
package coinbase

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/talyvor/lens/internal/money"
)

// Name is the adapter's name: its switches and its webhook endpoint, POST /v1/money/webhooks/coinbase.
const Name = "coinbase"

// Network is the money layer's name for the chain the adapter's wallets are on.
const Network = "base"

// usdcOn is USDC's contract address on each CDP network the adapter runs on, as Circle publishes it. A token is USDC
// only at this address: anyone can deploy a token whose symbol is "USDC".
var usdcOn = map[string]string{
	"base":         "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
	"base-sepolia": "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
}

// webhookNetwork is the networkId CDP's webhooks name each network by.
var webhookNetwork = map[string]string{"base": "base-mainnet", "base-sepolia": "base-sepolia"}

// transferTopic is the ERC-20 Transfer(address,address,uint256) event's topic.
const transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// Allowlist is the addresses the owner approved money out of a wallet to.
type Allowlist interface {
	Approved(ctx context.Context, wallet, network, address string) (bool, error)
}

// Screener clears an address before money goes to it: nil when it may be paid, an error wrapping money.ErrRefused
// when it may not. CDP screens nothing for us, so there is no default.
type Screener interface {
	Screen(ctx context.Context, network, address string) error
}

// Config is what the adapter needs from CDP's portal.
type Config struct {
	BaseURL       string // https://api.cdp.coinbase.com/platform
	APIKeyID      string
	APIKeySecret  string // the Ed25519 secret API key, base64, as the portal gives it
	WalletSecret  string // the wallet secret, base64 DER PKCS#8, as the portal gives it
	Network       string // base, or base-sepolia on CDP's testnet
	PaymasterURL  string // CDP's paymaster, so agents' smart accounts never need ETH; base-sepolia is sponsored without one
	RPCURL        string // a Base node, such as CDP's: deposits are checked against their receipts
	WebhookSecret string // the onchain-activity subscription's secret
	Allowlist     Allowlist
	Screener      Screener
	HTTP          *http.Client
}

// New is Coinbase's adapter: wallets, transfers and webhooks. It moves only USDC.
func New(cfg Config) (*money.Adapter, error) {
	apiKey, err := base64.StdEncoding.DecodeString(cfg.APIKeySecret)
	if err != nil || len(apiKey) != ed25519.PrivateKeySize {
		return nil, errors.New("coinbase: the API key secret is a base64 Ed25519 key")
	}
	der, err := base64.StdEncoding.DecodeString(cfg.WalletSecret)
	if err != nil {
		return nil, errors.New("coinbase: the wallet secret is base64")
	}
	pk, err := x509.ParsePKCS8PrivateKey(der)
	walletKey, ok := pk.(*ecdsa.PrivateKey)
	if err != nil || !ok {
		return nil, errors.New("coinbase: the wallet secret is a PKCS#8 EC key")
	}
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	switch {
	case err != nil || base.Host == "" || cfg.APIKeyID == "":
		return nil, errors.New("coinbase: the adapter needs CDP's URL and an API key id")
	case usdcOn[cfg.Network] == "":
		return nil, fmt.Errorf("coinbase: the adapter runs on base or base-sepolia, not %q", cfg.Network)
	case cfg.Network == "base" && cfg.PaymasterURL == "":
		return nil, errors.New("coinbase: on base the adapter needs CDP's paymaster, so agents never need ETH")
	case cfg.RPCURL == "":
		return nil, errors.New("coinbase: the adapter needs a Base node to check deposits against")
	case cfg.WebhookSecret == "":
		return nil, errors.New("coinbase: the adapter needs the webhook subscription's secret")
	case cfg.Allowlist == nil:
		return nil, errors.New("coinbase: the adapter needs the owner's allowlist of addresses")
	case cfg.Screener == nil:
		return nil, errors.New("coinbase: the adapter needs a screener for addresses money goes to")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	c := &client{cfg: cfg, apiKey: ed25519.PrivateKey(apiKey), walletKey: walletKey, base: base, usdc: usdcOn[cfg.Network]}
	return &money.Adapter{Name: Name, Wallets: wallets{c}, Transfers: transfers{c}, Webhooks: webhooks{c}, Signers: signers{c}}, nil
}

type client struct {
	cfg       Config
	apiKey    ed25519.PrivateKey
	walletKey *ecdsa.PrivateKey
	base      *url.URL
	usdc      string
}

// apiError is CDP's error body.
type apiError struct {
	ErrorType    string `json:"errorType"`
	ErrorMessage string `json:"errorMessage"`
}

func nonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// encode is v as compact JSON with its keys sorted, the form CDP hashes a body in: bodies are maps, which encoding/json
// writes in key order.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// do calls CDP and reads its answer into out, answering the HTTP status. Every call carries a bearer JWT signed with
// the API key; a POST also carries the wallet JWT, bound to its body, and idem as its idempotency key. CDP's errors are
// the money layer's: not found, a key used for another request, refused, or CDP down.
func (c *client) do(ctx context.Context, method, path, idem string, body map[string]any, out any) (int, error) {
	u := *c.base
	p, q, _ := strings.Cut(path, "?")
	u.Path, u.RawQuery = c.base.Path+p, q
	uri := method + " " + u.Host + u.Path
	now := time.Now()
	bearer := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"sub": c.cfg.APIKeyID, "iss": "cdp", "aud": []string{"cdp_service"},
		"nbf": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "uri": uri})
	bearer.Header["kid"], bearer.Header["nonce"] = c.cfg.APIKeyID, nonce()
	bt, err := bearer.SignedString(c.apiKey)
	if err != nil {
		return 0, fmt.Errorf("%w: signing CDP's bearer token: %v", money.ErrInvalid, err)
	}
	var rd io.Reader
	var walletAuth string
	if method == http.MethodPost {
		raw, err := encode(body)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", money.ErrInvalid, err)
		}
		sum := sha256.Sum256(raw)
		walletAuth, err = jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iat": now.Unix(), "nbf": now.Unix(), "jti": nonce(),
			"uris": []string{uri}, "reqHash": hex.EncodeToString(sum[:])}).SignedString(c.walletKey)
		if err != nil {
			return 0, fmt.Errorf("%w: signing CDP's wallet token: %v", money.ErrInvalid, err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", money.ErrInvalid, err)
	}
	req.Header.Set("Authorization", "Bearer "+bt)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Wallet-Auth", walletAuth)
		req.Header.Set("X-Idempotency-Key", idem)
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: coinbase: %v", money.ErrPartnerDown, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, fmt.Errorf("%w: coinbase: %v", money.ErrPartnerDown, err)
	}
	if resp.StatusCode >= 300 {
		var e apiError
		_ = json.Unmarshal(raw, &e)
		msg := fmt.Sprintf("coinbase %s %s: %d %s %s", method, p, resp.StatusCode, e.ErrorType, e.ErrorMessage)
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return resp.StatusCode, fmt.Errorf("%w: %s", money.ErrNotFound, msg)
		case resp.StatusCode == http.StatusConflict || e.ErrorType == "idempotency_error":
			return resp.StatusCode, fmt.Errorf("%w: %s", money.ErrKeyReused, msg)
		case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusForbidden:
			return resp.StatusCode, fmt.Errorf("%w: %s", money.ErrRefused, msg)
		}
		return resp.StatusCode, fmt.Errorf("%w: %s", money.ErrPartnerDown, msg) // 401, 402, 429, 5xx: ours or CDP's to fix
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%w: coinbase %s %s answered %v", money.ErrPartnerDown, method, p, err)
		}
	}
	return resp.StatusCode, nil
}

// digest is the caller's key, scoped to the adapter and kind, as hex.
func digest(kind, key string) string {
	s := sha256.Sum256([]byte("talyvor-money\x00" + Name + "\x00" + kind + "\x00" + key))
	return hex.EncodeToString(s[:])
}

// idempotencyKey is the UUID CDP is given for the caller's key: the same key is the same UUID, so a retry is the same
// request to CDP, which answers it as it did the first time.
func idempotencyKey(kind, key string) string {
	s, _ := hex.DecodeString(digest(kind, key))
	s[6] = s[6]&0x0f | 0x40
	s[8] = s[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", s[0:4], s[4:6], s[6:8], s[8:10], s[10:16])
}

func usdcOnly(a money.Amount) error {
	if a.Asset != money.USDC {
		return fmt.Errorf("%w: Coinbase moves USDC, not %s", money.ErrInvalid, a.Asset)
	}
	if a.Minor <= 0 {
		return fmt.Errorf("%w: an amount is positive, not %d", money.ErrInvalid, a.Minor)
	}
	return nil
}

var addressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// word is a 0x hex value left-padded to 32 bytes, as an event topic or a call argument is.
func word(hexed string) string {
	return "0x" + strings.Repeat("0", 64-len(hexed)) + strings.ToLower(hexed)
}

// account is CDP's EVM account or smart account object.
type account struct {
	Address string   `json:"address"`
	Name    string   `json:"name"`
	Owners  []string `json:"owners"`
}

// ours is smart account ref, if it is one of the adapter's: CDP answers only for its own project's accounts.
func (c *client) ours(ctx context.Context, ref string) (account, error) {
	if !addressRE.MatchString(ref) {
		return account{}, fmt.Errorf("%w: no wallet %q", money.ErrNotFound, ref)
	}
	var a account
	if _, err := c.do(ctx, http.MethodGet, "/v2/evm/smart-accounts/"+ref, "", nil, &a); err != nil {
		return account{}, err
	}
	return a, nil
}

// balance is the USDC address ref holds, in micro-units.
func (c *client) balance(ctx context.Context, ref string) (int64, error) {
	if !addressRE.MatchString(ref) {
		return 0, fmt.Errorf("%w: no wallet %q", money.ErrNotFound, ref)
	}
	var sum int64
	page := ""
	for range 50 {
		var out struct {
			Balances []struct {
				Amount struct {
					Amount   string `json:"amount"`
					Decimals int    `json:"decimals"`
				} `json:"amount"`
				Token struct {
					Network         string `json:"network"`
					ContractAddress string `json:"contractAddress"`
				} `json:"token"`
			} `json:"balances"`
			NextPageToken string `json:"nextPageToken"`
		}
		path := "/v2/evm/token-balances/" + c.cfg.Network + "/" + ref
		if page != "" {
			path += "?" + url.Values{"pageToken": {page}}.Encode()
		}
		if _, err := c.do(ctx, http.MethodGet, path, "", nil, &out); err != nil {
			return 0, err
		}
		for _, b := range out.Balances {
			if b.Token.Network != c.cfg.Network || !strings.EqualFold(b.Token.ContractAddress, c.usdc) {
				continue
			}
			n, err := strconv.ParseInt(b.Amount.Amount, 10, 64)
			if err != nil || n < 0 || b.Amount.Decimals != 6 {
				return 0, fmt.Errorf("%w: CDP's USDC balance %q (%d decimals) is not micro-units", money.ErrPartnerDown, b.Amount.Amount, b.Amount.Decimals)
			}
			sum += n
		}
		if page = out.NextPageToken; page == "" {
			return sum, nil
		}
	}
	return 0, fmt.Errorf("%w: CDP's balances of %s did not end", money.ErrPartnerDown, ref)
}

// status is a CDP user operation's status as the money layer's.
func status(s string) money.Status {
	switch s {
	case "complete":
		return money.StatusCompleted
	case "failed", "dropped":
		return money.StatusFailed
	}
	return money.StatusPending // pending, signed, broadcast: on its way
}

// send moves amount of USDC out of smart account from to address, once for key: a retry is the same request to CDP,
// which answers the user operation it made, and the same key for another transfer is refused by CDP.
//
// shortcut: CDP keeps an idempotency key for 24 hours; a retry later than that is a new transfer. Upgrade when B46.8
// keeps each payment's partner reference and answers its retries before the adapter is asked.
func (c *client) send(ctx context.Context, kind, key, from, to string, amount money.Amount) (money.Result, error) {
	held, err := c.balance(ctx, from)
	if err != nil {
		return money.Result{}, err
	}
	body := map[string]any{"network": c.cfg.Network, "calls": []map[string]string{{"to": c.usdc, "value": "0",
		"data": "0xa9059cbb" + word(strings.TrimPrefix(to, "0x"))[2:] + word(strconv.FormatInt(amount.Minor, 16))[2:]}}}
	if c.cfg.PaymasterURL != "" {
		body["paymasterUrl"] = c.cfg.PaymasterURL
	}
	var op struct {
		UserOpHash string `json:"userOpHash"`
		Status     string `json:"status"`
	}
	// Asked even when the wallet holds too little: only CDP knows whether this key already sent, and it refuses a
	// transfer the account cannot make when it simulates it.
	if _, err := c.do(ctx, http.MethodPost, "/v2/evm/smart-accounts/"+from+"/user-operations/prepare-and-send", idempotencyKey(kind, key), body, &op); err != nil {
		if held < amount.Minor && errors.Is(err, money.ErrRefused) {
			return money.Result{}, fmt.Errorf("%w: wallet %s holds %d, not %d", money.ErrInsufficientFunds, from, held, amount.Minor)
		}
		return money.Result{}, err
	}
	if op.UserOpHash == "" {
		return money.Result{}, fmt.Errorf("%w: CDP answered no user operation", money.ErrPartnerDown)
	}
	r := money.Result{Ref: op.UserOpHash, Status: status(op.Status)}
	if r.Status == money.StatusFailed {
		return money.Result{}, fmt.Errorf("%w: CDP answered %s", money.ErrRefused, op.Status)
	}
	return r, nil
}

// receipt is a transaction's receipt from the Base node: nil when the node has not seen it mined.
type receipt struct {
	Status string `json:"status"`
	Logs   []struct {
		Address  string   `json:"address"`
		Topics   []string `json:"topics"`
		Data     string   `json:"data"`
		LogIndex string   `json:"logIndex"`
	} `json:"logs"`
}

func (c *client) receipt(ctx context.Context, hash string) (*receipt, error) {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "eth_getTransactionReceipt", "params": []string{hash}})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", money.ErrInvalid, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.RPCURL, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w: the node's URL", money.ErrInvalid) // the URL carries the node's key: never in an error
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: the Base node did not answer", money.ErrPartnerDown)
	}
	defer resp.Body.Close()
	var out struct {
		Result *receipt `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out) != nil || out.Error != nil {
		return nil, fmt.Errorf("%w: the Base node answered %d", money.ErrPartnerDown, resp.StatusCode)
	}
	return out.Result, nil
}

type wallets struct{ c *client }

// Create makes an agent's wallet: an EVM account CDP holds the key of, and the smart account it owns, so CDP's
// paymaster pays its network fees and it never needs ETH. Both are named from the key and the agent, so the same key
// for another agent is another request, which CDP refuses.
//
// shortcut: a create retried more than 24 hours after the first finds its names taken and is answered ErrKeyReused,
// never a second wallet. Upgrade if B46.4 retries a create that late.
func (w wallets) Create(ctx context.Context, req money.CreateWallet) (money.Wallet, error) {
	switch {
	case strings.TrimSpace(req.Key) == "":
		return money.Wallet{}, fmt.Errorf("%w: every call carries the caller's idempotency key", money.ErrInvalid)
	case req.Asset != money.USDC:
		return money.Wallet{}, fmt.Errorf("%w: Coinbase's wallets hold USDC, not %s", money.ErrInvalid, req.Asset)
	case strings.TrimSpace(req.AgentID) == "":
		return money.Wallet{}, fmt.Errorf("%w: a wallet is an agent's", money.ErrInvalid)
	}
	name := digest("wallet", req.Key+"\x00"+req.AgentID)[:32]
	var owner, smart account
	if _, err := w.c.do(ctx, http.MethodPost, "/v2/evm/accounts", idempotencyKey("owner", req.Key), map[string]any{"name": "tlo-" + name}, &owner); err != nil {
		return money.Wallet{}, err
	}
	if !addressRE.MatchString(owner.Address) {
		return money.Wallet{}, fmt.Errorf("%w: CDP made no account", money.ErrPartnerDown)
	}
	if _, err := w.c.do(ctx, http.MethodPost, "/v2/evm/smart-accounts", idempotencyKey("wallet", req.Key),
		map[string]any{"owners": []string{owner.Address}, "name": "tly-" + name}, &smart); err != nil {
		return money.Wallet{}, err
	}
	if !addressRE.MatchString(smart.Address) {
		return money.Wallet{}, fmt.Errorf("%w: CDP made no smart account", money.ErrPartnerDown)
	}
	return money.Wallet{Ref: smart.Address, Asset: money.USDC}, nil
}

func (w wallets) Balance(ctx context.Context, ref string) (money.Amount, error) {
	n, err := w.c.balance(ctx, ref)
	return money.Amount{Minor: n, Asset: money.USDC}, err
}

func (w wallets) Receive(ctx context.Context, ref string) (money.Details, error) {
	a, err := w.c.ours(ctx, ref)
	if err != nil {
		return money.Details{}, err
	}
	return money.Details{Asset: money.USDC, Network: Network, Address: a.Address}, nil
}

type signers struct{ c *client }

// NewSigner makes an agent's key (B48.2): an EVM account whose private key CDP makes and keeps inside its enclave, named
// from the caller's key and the agent, so a retry is the same account and the same key for another agent is refused.
func (s signers) NewSigner(ctx context.Context, key, agentID string) (string, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("%w: an agent's key is made for an agent, under the caller's idempotency key", money.ErrInvalid)
	}
	var a account
	if _, err := s.c.do(ctx, http.MethodPost, "/v2/evm/accounts", idempotencyKey("signer", key),
		map[string]any{"name": "tla-" + digest("signer", key+"\x00"+agentID)[:32]}, &a); err != nil {
		return "", err
	}
	if !addressRE.MatchString(a.Address) {
		return "", fmt.Errorf("%w: CDP made no account", money.ErrPartnerDown)
	}
	return a.Address, nil
}

// NewSolanaWallet makes an agent's Solana wallet: a Solana account whose key CDP makes and keeps inside its enclave,
// named as NewSigner names an agent's EVM key.
func (s signers) NewSolanaWallet(ctx context.Context, key, agentID string) (string, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("%w: an agent's wallet is made for an agent, under the caller's idempotency key", money.ErrInvalid)
	}
	var a account
	if _, err := s.c.do(ctx, http.MethodPost, "/v2/solana/accounts", idempotencyKey("solana", key),
		map[string]any{"name": "tls-" + digest("solana", key+"\x00"+agentID)[:32]}, &a); err != nil {
		return "", err
	}
	if !solanaRE.MatchString(a.Address) {
		return "", fmt.Errorf("%w: CDP made no Solana account", money.ErrPartnerDown)
	}
	return a.Address, nil
}

// solanaRE is a Solana address: an Ed25519 public key in base58.
var solanaRE = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)

// SignHash has CDP sign hash with the agent's key signer, inside its enclave.
func (s signers) SignHash(ctx context.Context, signer string, hash [32]byte) ([]byte, error) {
	if !addressRE.MatchString(signer) {
		return nil, fmt.Errorf("%w: no key %q", money.ErrNotFound, signer)
	}
	h := "0x" + hex.EncodeToString(hash[:])
	var out struct {
		Signature string `json:"signature"`
	}
	if _, err := s.c.do(ctx, http.MethodPost, "/v2/evm/accounts/"+signer+"/sign", idempotencyKey("sign", signer+h),
		map[string]any{"hash": h}, &out); err != nil {
		return nil, err
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(out.Signature, "0x"))
	if err != nil || len(sig) != 65 {
		return nil, fmt.Errorf("%w: CDP's signature is not 65 bytes", money.ErrPartnerDown)
	}
	return sig, nil
}

type transfers struct{ c *client }

// Internal moves USDC between two of the adapter's wallets, on chain.
func (t transfers) Internal(ctx context.Context, req money.Internal) (money.Result, error) {
	if err := usdcOnly(req.Amount); err != nil {
		return money.Result{}, err
	}
	if strings.EqualFold(req.From, req.To) {
		return money.Result{}, fmt.Errorf("%w: a transfer is between two wallets", money.ErrInvalid)
	}
	if _, err := t.c.ours(ctx, req.From); err != nil {
		return money.Result{}, err
	}
	to, err := t.c.ours(ctx, req.To)
	if err != nil {
		return money.Result{}, err
	}
	return t.c.send(ctx, "internal", req.Key, req.From, to.Address, req.Amount)
}

// Outbound pays USDC out to an address on Base: only one the owner approved for this wallet, and only once screening
// cleared it.
func (t transfers) Outbound(ctx context.Context, req money.Outbound) (money.Result, error) {
	if err := usdcOnly(req.Amount); err != nil {
		return money.Result{}, err
	}
	if req.Payee.Network != Network || !addressRE.MatchString(req.Payee.Address) {
		return money.Result{}, fmt.Errorf("%w: Coinbase pays an address on %s, not %q on %q", money.ErrInvalid, Network, req.Payee.Address, req.Payee.Network)
	}
	if _, err := t.c.ours(ctx, req.From); err != nil {
		return money.Result{}, err
	}
	// Money to one of ours is Internal: a webhook does not credit a transfer between two of ours (Verify).
	if _, err := t.c.ours(ctx, req.Payee.Address); !errors.Is(err, money.ErrNotFound) {
		if err != nil {
			return money.Result{}, err
		}
		return money.Result{}, fmt.Errorf("%w: %s is one of Talyvor's wallets: move money to it with Internal", money.ErrInvalid, req.Payee.Address)
	}
	ok, err := t.c.cfg.Allowlist.Approved(ctx, req.From, Network, req.Payee.Address)
	if err != nil {
		return money.Result{}, fmt.Errorf("%w: the owner's allowlist could not be read: %v", money.ErrRefused, err)
	}
	if !ok {
		return money.Result{}, fmt.Errorf("%w: %s is not an address the owner approved", money.ErrRefused, req.Payee.Address)
	}
	if err := t.c.cfg.Screener.Screen(ctx, Network, req.Payee.Address); err != nil {
		if !errors.Is(err, money.ErrRefused) {
			err = fmt.Errorf("%w: %s could not be screened: %v", money.ErrRefused, req.Payee.Address, err)
		}
		return money.Result{}, err
	}
	return t.c.send(ctx, "outbound", req.Key, req.From, req.Payee.Address, req.Amount)
}

// Credit is USDC that came into a wallet, checked against the Base node: PartnerRef is the deposit's transaction hash,
// with ":<log index>" for one transfer in it (as webhooks report it), and the USDC transfers it names into the wallet
// must come to exactly the amount. It reads and changes nothing, so it credits once however often it is asked.
func (t transfers) Credit(ctx context.Context, req money.Credit) (money.Result, error) {
	if err := usdcOnly(req.Amount); err != nil {
		return money.Result{}, err
	}
	to, err := t.c.ours(ctx, req.To)
	if err != nil {
		return money.Result{}, err
	}
	hash, idx, one := strings.Cut(req.PartnerRef, ":")
	if hash == "" {
		return money.Result{}, fmt.Errorf("%w: a credit names its transaction", money.ErrInvalid)
	}
	rc, err := t.c.receipt(ctx, hash)
	if err != nil {
		return money.Result{}, err
	}
	if rc == nil {
		return money.Result{}, fmt.Errorf("%w: the node has no mined transaction %s", money.ErrNotFound, hash)
	}
	if rc.Status != "0x1" {
		return money.Result{}, fmt.Errorf("%w: transaction %s failed", money.ErrRefused, hash)
	}
	var sum int64
	for _, l := range rc.Logs {
		if !strings.EqualFold(l.Address, t.c.usdc) || len(l.Topics) != 3 || l.Topics[0] != transferTopic ||
			!strings.EqualFold(l.Topics[2], word(strings.TrimPrefix(to.Address, "0x"))) {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimPrefix(l.LogIndex, "0x"), 16, 64); one && (err != nil || strconv.FormatInt(n, 10) != idx) {
			continue
		}
		v, ok := new(big.Int).SetString(strings.TrimPrefix(l.Data, "0x"), 16)
		if !ok || !v.IsInt64() || v.Int64() > math.MaxInt64-sum {
			return money.Result{}, fmt.Errorf("%w: transaction %s's amount is out of range", money.ErrRefused, hash)
		}
		sum += v.Int64()
	}
	switch {
	case sum == 0:
		return money.Result{}, fmt.Errorf("%w: %s is not a USDC deposit into %s", money.ErrRefused, req.PartnerRef, req.To)
	case sum != req.Amount.Minor:
		return money.Result{}, fmt.Errorf("%w: %s brought %d, not %d", money.ErrRefused, req.PartnerRef, sum, req.Amount.Minor)
	}
	return money.Result{Ref: req.PartnerRef, Status: money.StatusCompleted}, nil
}

// SignatureHeader carries CDP's webhook signature: "t=<unix seconds>,v0=<hex HMAC-SHA256 of t.body>,h=…,v1=…".
const SignatureHeader = "X-Webhook-Signature"

// signatureTolerance is how old a signed webhook may be.
const signatureTolerance = 5 * time.Minute

type webhooks struct{ c *client }

// Activity is one of CDP's onchain-activity notifications, once its signature has verified.
type Activity struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		NetworkID       string `json:"networkId"`
		TransactionHash string `json:"transactionHash"`
		LogIndex        int64  `json:"logIndex"`
		ContractAddress string `json:"contractAddress"`
		EventName       string `json:"eventName"`
		From            string `json:"from"`
		To              string `json:"to"`
		Value           string `json:"value"`
	} `json:"data"`
}

// VerifyActivity checks CDP's signature on body — v0, an HMAC-SHA256 of the timestamp and the raw body with the
// subscription's secret, no older than five minutes — and reads it: money.ErrBadSignature when it does not verify.
func VerifyActivity(secret string, h http.Header, body []byte, now time.Time) (Activity, error) {
	if secret == "" {
		return Activity{}, fmt.Errorf("%w: no webhook secret is set", money.ErrBadSignature)
	}
	var ts, v0 string
	for _, part := range strings.Split(h.Get(SignatureHeader), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v0":
			v0 = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Activity{}, fmt.Errorf("%w: no CDP signature", money.ErrBadSignature)
	}
	if age := now.Sub(time.Unix(sec, 0)); age > signatureTolerance || age < -signatureTolerance {
		return Activity{}, fmt.Errorf("%w: signed %s from now", money.ErrBadSignature, age.Round(time.Second))
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	if got, err := hex.DecodeString(v0); err != nil || !hmac.Equal(got, m.Sum(nil)) {
		return Activity{}, money.ErrBadSignature
	}
	var n Activity
	if err := json.Unmarshal(body, &n); err != nil {
		return Activity{}, fmt.Errorf("%w: the body is not a notification: %v", money.ErrInvalid, err)
	}
	return n, nil
}

// Verify checks CDP's signature (VerifyActivity) and reads a USDC Transfer from outside into one of the adapter's
// wallets as an Event. Any other genuine notification is money.ErrNotACredit.
func (w webhooks) Verify(h http.Header, body []byte, now time.Time) (money.Event, error) {
	n, err := VerifyActivity(w.c.cfg.WebhookSecret, h, body, now)
	if err != nil {
		return money.Event{}, err
	}
	d := n.Data
	if n.Type != "onchain.activity.detected" || d.EventName != "Transfer" || d.NetworkID != webhookNetwork[w.c.cfg.Network] ||
		!strings.EqualFold(d.ContractAddress, w.c.usdc) {
		return money.Event{}, money.ErrNotACredit
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Money between two of ours is the transfer that moved it, not a deposit: posting it would count it twice.
	if _, err := w.c.ours(ctx, d.From); !errors.Is(err, money.ErrNotFound) {
		if err != nil {
			return money.Event{}, err
		}
		return money.Event{}, money.ErrNotACredit
	}
	to, err := w.c.ours(ctx, d.To)
	if errors.Is(err, money.ErrNotFound) { // money into someone else's
		return money.Event{}, money.ErrNotACredit
	}
	if err != nil {
		return money.Event{}, err
	}
	amount, err := strconv.ParseInt(d.Value, 10, 64)
	if err != nil || amount <= 0 || d.TransactionHash == "" {
		return money.Event{}, fmt.Errorf("%w: the transfer's value %q", money.ErrInvalid, d.Value)
	}
	return money.Event{ID: n.ID, To: to.Address, Amount: money.Amount{Minor: amount, Asset: money.USDC},
		PartnerRef: d.TransactionHash + ":" + strconv.FormatInt(d.LogIndex, 10), Payer: d.From}, nil
}
