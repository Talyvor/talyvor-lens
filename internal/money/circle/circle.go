// Package circle is the money layer's stablecoin adapter for Circle's developer-controlled wallets (B46.5): a wallet
// per agent on Base, its USDC balance and receive address, transfers out to an address the owner approved and screening
// cleared, and deposits credited from Circle's signed notifications. It is written from Circle's published API
// reference (developers.circle.com) and tested against circlefake. It serves nothing until Circle's keys are on the
// server, it is registered, and its switch is on (internal/money).
package circle

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/money"
)

// Name is the adapter's name: its switches and its webhook endpoint, POST /v1/money/webhooks/circle.
const Name = "circle"

// Network is the money layer's name for the chain Circle's wallets are on.
const Network = "base"

// usdcOn is USDC's contract address on each chain the adapter runs on, as Circle publishes it. A token is USDC only at
// this address: anyone can deploy a token whose symbol is "USDC".
var usdcOn = map[string]string{
	"BASE":         "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913",
	"BASE-SEPOLIA": "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
}

// Allowlist is the addresses the owner approved money out of a wallet to.
type Allowlist interface {
	Approved(ctx context.Context, wallet, network, address string) (bool, error)
}

// Screener clears an address before money goes to it: nil when it may be paid, an error wrapping money.ErrRefused
// when it may not.
type Screener interface {
	Screen(ctx context.Context, network, address string) error
}

// Config is what the adapter needs from Circle's console. Screener nil is Circle's own address screening.
type Config struct {
	BaseURL      string // https://api.circle.com
	APIKey       string
	EntitySecret string // the 32-byte entity secret registered with Circle, in hex
	WalletSetID  string // the wallet set agents' wallets are made in
	Blockchain   string // BASE, or BASE-SEPOLIA on Circle's testnet
	Allowlist    Allowlist
	Screener     Screener
	HTTP         *http.Client
}

// New is Circle's adapter: wallets, transfers and webhooks. It moves only USDC.
func New(cfg Config) (*money.Adapter, error) {
	secret, err := hex.DecodeString(cfg.EntitySecret)
	switch {
	case cfg.BaseURL == "" || cfg.APIKey == "":
		return nil, errors.New("circle: the adapter needs Circle's URL and an API key")
	case err != nil || len(secret) != 32:
		return nil, errors.New("circle: the entity secret is 32 bytes in hex")
	case cfg.WalletSetID == "":
		return nil, errors.New("circle: the adapter needs the wallet set agents' wallets are made in")
	case usdcOn[cfg.Blockchain] == "":
		return nil, fmt.Errorf("circle: the adapter runs on BASE or BASE-SEPOLIA, not %q", cfg.Blockchain)
	case cfg.Allowlist == nil:
		return nil, errors.New("circle: the adapter needs the owner's allowlist of addresses")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	c := &client{cfg: cfg, secret: secret, base: strings.TrimRight(cfg.BaseURL, "/"), usdc: usdcOn[cfg.Blockchain],
		notifyKeys: map[string]*ecdsa.PublicKey{}, tokens: map[string]bool{}}
	if cfg.Screener == nil {
		c.cfg.Screener = circleScreening{c}
	}
	return &money.Adapter{Name: Name, Wallets: wallets{c}, Transfers: transfers{c}, Webhooks: webhooks{c}}, nil
}

type client struct {
	cfg    Config
	secret []byte
	base   string
	usdc   string

	mu         sync.Mutex
	entityKey  *rsa.PublicKey
	notifyKeys map[string]*ecdsa.PublicKey // Circle's notification keys by id: a key id's key never changes
	tokens     map[string]bool             // whether a Circle token id is USDC on the chain
}

// apiError is Circle's error body.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// do calls Circle and reads its {"data": …} into out, answering the HTTP status. Circle's errors are the money
// layer's: not found, a key used for another request, refused, or Circle down.
func (c *client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", money.ErrInvalid, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", money.ErrInvalid, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: circle: %v", money.ErrPartnerDown, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, fmt.Errorf("%w: circle: %v", money.ErrPartnerDown, err)
	}
	if resp.StatusCode >= 300 {
		var e apiError
		_ = json.Unmarshal(raw, &e)
		msg := fmt.Sprintf("circle %s %s: %d %d %s", method, path, resp.StatusCode, e.Code, e.Message)
		switch resp.StatusCode {
		case http.StatusNotFound:
			return resp.StatusCode, fmt.Errorf("%w: %s", money.ErrNotFound, msg)
		case http.StatusConflict:
			return resp.StatusCode, fmt.Errorf("%w: %s", money.ErrKeyReused, msg)
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return resp.StatusCode, fmt.Errorf("%w: %s", money.ErrRefused, msg)
		}
		return resp.StatusCode, fmt.Errorf("%w: %s", money.ErrPartnerDown, msg) // 401, 403, 429, 5xx: ours or Circle's to fix
	}
	if out != nil {
		if err := json.Unmarshal(raw, &struct{ Data any }{out}); err != nil {
			return resp.StatusCode, fmt.Errorf("%w: circle %s %s answered %v", money.ErrPartnerDown, method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// ciphertext is the entity secret encrypted with the entity's public key (RSA-OAEP, SHA-256), new for every request,
// as Circle asks of every call that moves money or makes a wallet.
func (c *client) ciphertext(ctx context.Context) (string, error) {
	c.mu.Lock()
	key := c.entityKey
	c.mu.Unlock()
	if key == nil {
		var out struct {
			PublicKey string `json:"publicKey"`
		}
		if _, err := c.do(ctx, http.MethodGet, "/v1/w3s/config/entity/publicKey", nil, &out); err != nil {
			return "", err
		}
		blk, _ := pem.Decode([]byte(out.PublicKey))
		if blk == nil {
			return "", fmt.Errorf("%w: circle's entity public key is not PEM", money.ErrPartnerDown)
		}
		var pub any
		pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
		if err != nil {
			pub, err = x509.ParsePKCS1PublicKey(blk.Bytes)
		}
		rk, ok := pub.(*rsa.PublicKey)
		if err != nil || !ok {
			return "", fmt.Errorf("%w: circle's entity public key is not RSA", money.ErrPartnerDown)
		}
		c.mu.Lock()
		c.entityKey, key = rk, rk
		c.mu.Unlock()
	}
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, key, c.secret, nil)
	if err != nil {
		return "", fmt.Errorf("%w: encrypting the entity secret: %v", money.ErrInvalid, err)
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

// idempotencyKey is the UUID Circle is given for the caller's key: the same key is the same UUID, so a retry is the
// same request to Circle.
func idempotencyKey(kind, key string) string {
	s := sha256.Sum256([]byte("talyvor-money\x00" + Name + "\x00" + kind + "\x00" + key))
	s[6] = s[6]&0x0f | 0x40
	s[8] = s[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", s[0:4], s[4:6], s[6:8], s[8:10], s[10:16])
}

// decimal is micro-units of USDC as the decimal string Circle reads.
func decimal(minor int64) string {
	s := fmt.Sprintf("%d.%06d", minor/1_000_000, minor%1_000_000)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

var decimalRE = regexp.MustCompile(`^[0-9]{1,13}(\.[0-9]+)?$`)

// micro is Circle's decimal string of USDC in micro-units. More than six places is refused unless they are zeros.
func micro(s string) (int64, error) {
	if !decimalRE.MatchString(s) {
		return 0, fmt.Errorf("%w: circle's amount %q is not a decimal", money.ErrPartnerDown, s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 6 {
		if strings.Trim(frac[6:], "0") != "" {
			return 0, fmt.Errorf("%w: %q is finer than a millionth of a USDC", money.ErrRefused, s)
		}
		frac = frac[:6]
	}
	return strconv.ParseInt(whole+frac+strings.Repeat("0", 6-len(frac)), 10, 64)
}

func usdcOnly(a money.Amount) error {
	if a.Asset != money.USDC {
		return fmt.Errorf("%w: Circle moves USDC, not %s", money.ErrInvalid, a.Asset)
	}
	if a.Minor <= 0 {
		return fmt.Errorf("%w: an amount is positive, not %d", money.ErrInvalid, a.Minor)
	}
	return nil
}

var addressRE = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// wallet is Circle's wallet object.
type wallet struct {
	ID          string `json:"id"`
	Address     string `json:"address"`
	Blockchain  string `json:"blockchain"`
	WalletSetID string `json:"walletSetId"`
	State       string `json:"state"`
	RefID       string `json:"refId"`
}

// ours is wallet ref, if it is one of the adapter's: in its wallet set, on its chain.
func (c *client) ours(ctx context.Context, ref string) (wallet, error) {
	if ref == "" || strings.ContainsAny(ref, "/?#") {
		return wallet{}, fmt.Errorf("%w: no wallet %q", money.ErrNotFound, ref)
	}
	var out struct {
		Wallet wallet `json:"wallet"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/v1/w3s/wallets/"+url.PathEscape(ref), nil, &out); err != nil {
		return wallet{}, err
	}
	if out.Wallet.WalletSetID != c.cfg.WalletSetID || out.Wallet.Blockchain != c.cfg.Blockchain {
		return wallet{}, fmt.Errorf("%w: wallet %q is not one of Talyvor's on %s", money.ErrNotFound, ref, c.cfg.Blockchain)
	}
	return out.Wallet, nil
}

// token is Circle's token object.
type token struct {
	ID           string `json:"id"`
	Blockchain   string `json:"blockchain"`
	TokenAddress string `json:"tokenAddress"`
}

func (c *client) isUSDC(t token) bool {
	return t.Blockchain == c.cfg.Blockchain && strings.EqualFold(t.TokenAddress, c.usdc)
}

// tokenIsUSDC is whether Circle's token id is USDC on the adapter's chain.
func (c *client) tokenIsUSDC(ctx context.Context, id string) (bool, error) {
	c.mu.Lock()
	known, ok := c.tokens[id]
	c.mu.Unlock()
	if ok {
		return known, nil
	}
	if id == "" || strings.ContainsAny(id, "/?#") {
		return false, nil
	}
	var out struct {
		Token token `json:"token"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/v1/w3s/tokens/"+url.PathEscape(id), nil, &out); err != nil {
		return false, err
	}
	is := c.isUSDC(out.Token)
	c.mu.Lock()
	c.tokens[id] = is
	c.mu.Unlock()
	return is, nil
}

// balance is the USDC wallet ref holds, in micro-units.
func (c *client) balance(ctx context.Context, ref string) (int64, error) {
	if ref == "" || strings.ContainsAny(ref, "/?#") {
		return 0, fmt.Errorf("%w: no wallet %q", money.ErrNotFound, ref)
	}
	var out struct {
		TokenBalances []struct {
			Token  token  `json:"token"`
			Amount string `json:"amount"`
		} `json:"tokenBalances"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/v1/w3s/wallets/"+url.PathEscape(ref)+"/balances", nil, &out); err != nil {
		return 0, err
	}
	var sum int64
	for _, tb := range out.TokenBalances {
		if !c.isUSDC(tb.Token) {
			continue
		}
		n, err := micro(tb.Amount)
		if err != nil {
			return 0, err
		}
		sum += n
	}
	return sum, nil
}

// transaction is Circle's transaction object.
type transaction struct {
	ID                 string   `json:"id"`
	State              string   `json:"state"`
	Blockchain         string   `json:"blockchain"`
	TransactionType    string   `json:"transactionType"`
	WalletID           string   `json:"walletId"`
	SourceAddress      string   `json:"sourceAddress"`
	DestinationAddress string   `json:"destinationAddress"`
	Amounts            []string `json:"amounts"`
	TokenID            string   `json:"tokenId"`
	RefID              string   `json:"refId"`
}

// status is a Circle transaction state as the money layer's.
func status(state string) money.Status {
	switch state {
	case "COMPLETE":
		return money.StatusCompleted
	case "FAILED", "DENIED", "CANCELLED":
		return money.StatusFailed
	}
	return money.StatusPending // INITIATED, CLEARED, QUEUED, SENT, STUCK, CONFIRMED: on its way
}

func (t transaction) amount() (int64, error) {
	var sum int64
	for _, a := range t.Amounts {
		n, err := micro(a)
		if err != nil {
			return 0, err
		}
		sum += n
	}
	return sum, nil
}

func (c *client) transaction(ctx context.Context, id string) (transaction, error) {
	if id == "" || strings.ContainsAny(id, "/?#") {
		return transaction{}, fmt.Errorf("%w: no transaction %q", money.ErrNotFound, id)
	}
	var out struct {
		Transaction transaction `json:"transaction"`
	}
	_, err := c.do(ctx, http.MethodGet, "/v1/w3s/transactions/"+url.PathEscape(id), nil, &out)
	return out.Transaction, err
}

// made is the transfer out of from that Circle was asked for under idem, if there is one.
//
// shortcut: reads the wallet's 50 latest transfers out; a retry made after 50 more is answered insufficient funds.
// Upgrade when Circle filters transactions by refId.
func (c *client) made(ctx context.Context, from, idem string) (money.Result, bool, error) {
	var out struct {
		Transactions []transaction `json:"transactions"`
	}
	q := url.Values{"walletIds": {from}, "txType": {"OUTBOUND"}, "pageSize": {"50"}}
	if _, err := c.do(ctx, http.MethodGet, "/v1/w3s/transactions?"+q.Encode(), nil, &out); err != nil {
		return money.Result{}, false, err
	}
	for _, t := range out.Transactions {
		if t.RefID == idem && t.WalletID == from {
			return money.Result{Ref: t.ID, Status: status(t.State)}, true, nil
		}
	}
	return money.Result{}, false, nil
}

// send moves amount of USDC out of wallet from to address, once for key: a retry is the same request to Circle and
// answers the transfer it made.
func (c *client) send(ctx context.Context, kind, key, from, to string, amount money.Amount) (money.Result, error) {
	idem := idempotencyKey(kind, key)
	held, err := c.balance(ctx, from)
	if err != nil {
		return money.Result{}, err
	}
	if held < amount.Minor {
		if r, ok, err := c.made(ctx, from, idem); err != nil || ok {
			return r, err
		}
		return money.Result{}, fmt.Errorf("%w: wallet %s holds %d, not %d", money.ErrInsufficientFunds, from, held, amount.Minor)
	}
	ct, err := c.ciphertext(ctx)
	if err != nil {
		return money.Result{}, err
	}
	var out struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	code, err := c.do(ctx, http.MethodPost, "/v1/w3s/developer/transactions/transfer", map[string]any{
		"idempotencyKey": idem, "entitySecretCiphertext": ct, "walletId": from, "destinationAddress": to,
		"amounts": []string{decimal(amount.Minor)}, "tokenAddress": c.usdc, "blockchain": c.cfg.Blockchain,
		"feeLevel": "MEDIUM", "refId": idem}, &out)
	if err != nil {
		return money.Result{}, err
	}
	if code == http.StatusOK { // Circle answered a transfer it made before for this key: it must be this one
		t, err := c.transaction(ctx, out.ID)
		if err != nil {
			return money.Result{}, err
		}
		if n, err := t.amount(); err != nil || n != amount.Minor || t.WalletID != from || !strings.EqualFold(t.DestinationAddress, to) {
			return money.Result{}, fmt.Errorf("%w: transfer %q", money.ErrKeyReused, key)
		}
	}
	r := money.Result{Ref: out.ID, Status: status(out.State)}
	if r.Status == money.StatusFailed {
		return money.Result{}, fmt.Errorf("%w: circle answered %s", money.ErrRefused, out.State)
	}
	return r, nil
}

type wallets struct{ c *client }

// Create makes an agent's wallet in the adapter's wallet set: a smart-contract account, so Circle's gas station pays
// its network fees and it never needs ETH.
func (w wallets) Create(ctx context.Context, req money.CreateWallet) (money.Wallet, error) {
	switch {
	case strings.TrimSpace(req.Key) == "":
		return money.Wallet{}, fmt.Errorf("%w: every call carries the caller's idempotency key", money.ErrInvalid)
	case req.Asset != money.USDC:
		return money.Wallet{}, fmt.Errorf("%w: Circle's wallets hold USDC, not %s", money.ErrInvalid, req.Asset)
	case strings.TrimSpace(req.AgentID) == "":
		return money.Wallet{}, fmt.Errorf("%w: a wallet is an agent's", money.ErrInvalid)
	}
	ct, err := w.c.ciphertext(ctx)
	if err != nil {
		return money.Wallet{}, err
	}
	var out struct {
		Wallets []wallet `json:"wallets"`
	}
	if _, err := w.c.do(ctx, http.MethodPost, "/v1/w3s/developer/wallets", map[string]any{
		"idempotencyKey": idempotencyKey("wallet", req.Key), "entitySecretCiphertext": ct, "walletSetId": w.c.cfg.WalletSetID,
		"blockchains": []string{w.c.cfg.Blockchain}, "accountType": "SCA", "count": 1,
		"metadata": []map[string]string{{"name": "agent " + req.AgentID, "refId": req.AgentID}}}, &out); err != nil {
		return money.Wallet{}, err
	}
	if len(out.Wallets) != 1 || out.Wallets[0].ID == "" {
		return money.Wallet{}, fmt.Errorf("%w: circle made %d wallets", money.ErrPartnerDown, len(out.Wallets))
	}
	if out.Wallets[0].RefID != req.AgentID { // the key made another agent's wallet before
		return money.Wallet{}, fmt.Errorf("%w: wallet %q", money.ErrKeyReused, req.Key)
	}
	return money.Wallet{Ref: out.Wallets[0].ID, Asset: money.USDC}, nil
}

func (w wallets) Balance(ctx context.Context, ref string) (money.Amount, error) {
	n, err := w.c.balance(ctx, ref)
	return money.Amount{Minor: n, Asset: money.USDC}, err
}

func (w wallets) Receive(ctx context.Context, ref string) (money.Details, error) {
	wl, err := w.c.ours(ctx, ref)
	if err != nil {
		return money.Details{}, err
	}
	return money.Details{Asset: money.USDC, Holder: wl.RefID, Network: Network, Address: wl.Address}, nil
}

type transfers struct{ c *client }

// Internal moves USDC between two of the adapter's wallets, on chain.
func (t transfers) Internal(ctx context.Context, req money.Internal) (money.Result, error) {
	if err := usdcOnly(req.Amount); err != nil {
		return money.Result{}, err
	}
	if req.From == req.To {
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
		return money.Result{}, fmt.Errorf("%w: Circle pays an address on %s, not %q on %q", money.ErrInvalid, Network, req.Payee.Address, req.Payee.Network)
	}
	if _, err := t.c.ours(ctx, req.From); err != nil {
		return money.Result{}, err
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

// Credit is USDC that came into a wallet, as Circle reports it: transaction PartnerRef must be a deposit of exactly
// that much USDC into that wallet. It asks Circle and changes nothing, so it credits once however often it is asked.
func (t transfers) Credit(ctx context.Context, req money.Credit) (money.Result, error) {
	if err := usdcOnly(req.Amount); err != nil {
		return money.Result{}, err
	}
	tx, err := t.c.transaction(ctx, req.PartnerRef)
	if err != nil {
		return money.Result{}, err
	}
	usdc, err := t.c.tokenIsUSDC(ctx, tx.TokenID)
	if err != nil {
		return money.Result{}, err
	}
	n, err := tx.amount()
	switch {
	case err != nil:
		return money.Result{}, err
	case tx.TransactionType != "INBOUND" || tx.WalletID != req.To || tx.Blockchain != t.c.cfg.Blockchain || !usdc:
		return money.Result{}, fmt.Errorf("%w: %s is not a USDC deposit into %s", money.ErrRefused, req.PartnerRef, req.To)
	case n != req.Amount.Minor:
		return money.Result{}, fmt.Errorf("%w: %s brought %d, not %d", money.ErrRefused, req.PartnerRef, n, req.Amount.Minor)
	}
	r := money.Result{Ref: tx.ID, Status: status(tx.State)}
	if r.Status == money.StatusFailed {
		return money.Result{}, fmt.Errorf("%w: the deposit %s", money.ErrRefused, strings.ToLower(tx.State))
	}
	return r, nil
}

// circleScreening is Circle's compliance engine screening an address: only APPROVED clears it, and a sanctions signal
// refuses it whatever the rule decided. The rules — sanctions lists, Russia- and Belarus-linked addresses — are set in
// Circle's console.
type circleScreening struct{ c *client }

func (s circleScreening) Screen(ctx context.Context, _, address string) error {
	var out struct {
		Result   string `json:"result"`
		Decision struct {
			RuleName string `json:"ruleName"`
			Reasons  []struct {
				RiskScore      string   `json:"riskScore"`
				RiskCategories []string `json:"riskCategories"`
			} `json:"reasons"`
		} `json:"decision"`
	}
	day := time.Now().UTC().Format("2006-01-02") // one screening per address a day: a retry today is the same request
	if _, err := s.c.do(ctx, http.MethodPost, "/v1/w3s/compliance/screening/addresses", map[string]any{
		"idempotencyKey": idempotencyKey("screen", strings.ToLower(address)+"@"+day), "address": address, "chain": s.c.cfg.Blockchain}, &out); err != nil {
		return err
	}
	for _, r := range out.Decision.Reasons {
		for _, cat := range r.RiskCategories {
			if cat == "SANCTIONS" || cat == "TERRORIST_FINANCING" || r.RiskScore == "BLOCKLIST" {
				return fmt.Errorf("%w: screening found %s (%s) on %s", money.ErrRefused, cat, r.RiskScore, address)
			}
		}
	}
	if out.Result != "APPROVED" {
		return fmt.Errorf("%w: screening answered %q for %s (rule %q)", money.ErrRefused, out.Result, address, out.Decision.RuleName)
	}
	return nil
}

// Circle's notification headers.
const (
	SignatureHeader = "X-Circle-Signature"
	KeyIDHeader     = "X-Circle-Key-Id"
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type webhooks struct{ c *client }

// notifyKey is Circle's notification key id, kept once read.
func (w webhooks) notifyKey(id string) (*ecdsa.PublicKey, error) {
	w.c.mu.Lock()
	k := w.c.notifyKeys[id]
	w.c.mu.Unlock()
	if k != nil {
		return k, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out struct {
		Algorithm string `json:"algorithm"`
		PublicKey string `json:"publicKey"`
	}
	if _, err := w.c.do(ctx, http.MethodGet, "/v2/notifications/publicKey/"+id, nil, &out); err != nil {
		if errors.Is(err, money.ErrNotFound) {
			return nil, fmt.Errorf("%w: circle has no key %q", money.ErrBadSignature, id)
		}
		return nil, err
	}
	der, err := base64.StdEncoding.DecodeString(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("%w: circle's key %q is not base64", money.ErrPartnerDown, id)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	k, ok := pub.(*ecdsa.PublicKey)
	if err != nil || !ok || out.Algorithm != "ECDSA_SHA_256" {
		return nil, fmt.Errorf("%w: circle's key %q is not an ECDSA SHA-256 key", money.ErrPartnerDown, id)
	}
	w.c.mu.Lock()
	w.c.notifyKeys[id] = k
	w.c.mu.Unlock()
	return k, nil
}

// Verify checks Circle's signature — ECDSA SHA-256 over the raw body, by the key its id names — and reads a completed
// USDC deposit into one of the adapter's wallets as an Event. Any other genuine notification is money.ErrNotACredit.
// Circle's signature covers no timestamp: a notification is taken once by its id (ServeWebhook's replay check).
func (w webhooks) Verify(h http.Header, body []byte, _ time.Time) (money.Event, error) {
	id := h.Get(KeyIDHeader)
	sig, err := base64.StdEncoding.DecodeString(h.Get(SignatureHeader))
	if !uuidRE.MatchString(id) || err != nil || len(sig) == 0 {
		return money.Event{}, fmt.Errorf("%w: no Circle signature", money.ErrBadSignature)
	}
	key, err := w.notifyKey(id)
	if err != nil {
		return money.Event{}, err
	}
	sum := sha256.Sum256(body)
	if !ecdsa.VerifyASN1(key, sum[:], sig) {
		return money.Event{}, money.ErrBadSignature
	}
	var n struct {
		NotificationID   string      `json:"notificationId"`
		NotificationType string      `json:"notificationType"`
		Notification     transaction `json:"notification"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return money.Event{}, fmt.Errorf("%w: the body is not a notification: %v", money.ErrInvalid, err)
	}
	tx := n.Notification
	if n.NotificationType != "transactions.inbound" || tx.TransactionType != "INBOUND" || tx.State != "COMPLETE" || tx.Blockchain != w.c.cfg.Blockchain {
		return money.Event{}, money.ErrNotACredit
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	usdc, err := w.c.tokenIsUSDC(ctx, tx.TokenID)
	if err != nil {
		return money.Event{}, err
	}
	if !usdc {
		return money.Event{}, money.ErrNotACredit
	}
	amount, err := tx.amount()
	if err != nil {
		return money.Event{}, err
	}
	return money.Event{ID: n.NotificationID, To: tx.WalletID, Amount: money.Amount{Minor: amount, Asset: money.USDC},
		PartnerRef: tx.ID, Payer: tx.SourceAddress}, nil
}
