// Package swan is the money layer's EU e-money adapter for Swan (B46.7): euro accounts with IBANs for a workspace and,
// where Swan's account limit allows, one per agent; their balances; SEPA credit transfers out (SEPA Instant with
// fallback when asked); incoming SEPA credits from Swan's webhooks, checked against Swan's API; and KYB and KYC handed to
// Swan's hosted onboarding. It is written from Swan's published API reference (docs.swan.io) and tested against
// swanfake. It serves nothing until Swan's keys are on the server, it is registered, and its switch is on
// (internal/money).
package swan

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/talyvor/lens/internal/money"
)

// Name is the adapter's name: its switches and its webhook endpoint, POST /v1/money/webhooks/swan.
const Name = "swan"

// SecretHeader carries the webhook secret Swan was given: Swan's webhooks are not signed otherwise.
const SecretHeader = "X-Swan-Secret"

// accountCountry is Swan's country for an account's IBAN, from the holder's ISO 3166-1 alpha-2 country.
var accountCountry = map[string]string{"FR": "FRA", "DE": "DEU", "ES": "ESP", "NL": "NLD", "IT": "ITA", "BE": "BEL"}

// Holders answers the Swan account holder that accounts are opened under: for OpenAccount.Holder, and for
// CreateWallet.AgentID (the account holder of the agent's workspace). Lens records it when Swan's verification of the
// workspace completes (KYC.Status's Detail). ErrNotFound when there is none yet.
type Holders interface {
	AccountHolder(ctx context.Context, of string) (string, error)
}

// Config is what the adapter needs from Swan's dashboard.
type Config struct {
	APIURL             string // https://api.swan.io/live-partner/graphql, or sandbox-partner
	TokenURL           string // https://oauth.swan.io/oauth2/token
	ClientID           string
	ClientSecret       string
	ProjectID          string // webhooks for any other project are refused
	ConsentKey         string // the server-consent private key, PEM (PKCS#8 or SEC 1, P-256), whose public key Swan holds
	ConsentRedirectURL string
	WebhookSecret      string
	Instant            bool // SEPA Instant where the payee's bank takes it, regular SEPA otherwise
	Holders            Holders
	HTTP               *http.Client
}

// New is Swan's adapter: accounts, wallets, transfers, verification and webhooks. It holds only euros.
func New(cfg Config) (*money.Adapter, error) {
	block, _ := pem.Decode([]byte(cfg.ConsentKey))
	if block == nil {
		return nil, errors.New("swan: the server-consent key is a PEM P-256 private key")
	}
	key, err := parseKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	for _, u := range []string{cfg.APIURL, cfg.TokenURL, cfg.ConsentRedirectURL} {
		if p, err := url.Parse(u); err != nil || p.Host == "" {
			return nil, errors.New("swan: the adapter needs Swan's API and token URLs and a consent redirect URL")
		}
	}
	switch {
	case cfg.ClientID == "" || cfg.ClientSecret == "":
		return nil, errors.New("swan: the adapter needs the project's client id and secret")
	case cfg.ProjectID == "":
		return nil, errors.New("swan: the adapter needs the project's id")
	case cfg.WebhookSecret == "":
		return nil, errors.New("swan: the adapter needs the webhook subscription's secret")
	case cfg.Holders == nil:
		return nil, errors.New("swan: the adapter needs the account holders accounts are opened under")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	c := &client{cfg: cfg, consent: key}
	return &money.Adapter{Name: Name, Accounts: accounts{c}, Wallets: wallets{c}, Transfers: transfers{c}, KYC: kyc{c}, Webhooks: webhooks{c}}, nil
}

func parseKey(der []byte) (*ecdsa.PrivateKey, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if ec, ok := k.(*ecdsa.PrivateKey); ok && ec.Curve.Params().Name == "P-256" {
			return ec, nil
		}
		return nil, errors.New("swan: the server-consent key is a P-256 key")
	}
	k, err := x509.ParseECPrivateKey(der)
	if err != nil || k.Curve.Params().Name != "P-256" {
		return nil, errors.New("swan: the server-consent key is a P-256 key")
	}
	return k, nil
}

type client struct {
	cfg     Config
	consent *ecdsa.PrivateKey

	mu      sync.Mutex
	token   string
	expires time.Time
}

// accessToken is a project access token from Swan's OAuth server, kept until a minute before it expires.
func (c *client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.cfg.ClientID}, "client_secret": {c.cfg.ClientSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: the token URL", money.ErrInvalid)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: Swan's token server did not answer", money.ErrPartnerDown)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.AccessToken == "" {
		return "", fmt.Errorf("%w: Swan's token server answered %d", money.ErrPartnerDown, resp.StatusCode)
	}
	c.token, c.expires = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second-time.Minute)
	return c.token, nil
}

// do runs one GraphQL operation and reads its data into out. Transport failures, Swan's errors and its answers it
// cannot read are money.ErrPartnerDown.
func (c *client) do(ctx context.Context, op, query string, vars map[string]any, out any) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]any{"operationName": op, "query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("%w: %v", money.ErrInvalid, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%w: the API URL", money.ErrInvalid)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: swan %s: %v", money.ErrPartnerDown, op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
	}
	var body struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: swan %s answered %d", money.ErrPartnerDown, op, resp.StatusCode)
	}
	if len(body.Errors) > 0 {
		return fmt.Errorf("%w: swan %s: %s", money.ErrPartnerDown, op, body.Errors[0].Message)
	}
	if err := json.Unmarshal(body.Data, out); err != nil {
		return fmt.Errorf("%w: swan %s answered %v", money.ErrPartnerDown, op, err)
	}
	return nil
}

// rejection is the money layer's error for one of Swan's rejection payloads, or nil when typename is a success.
func rejection(typename, message string) error {
	switch {
	case !strings.HasSuffix(typename, "Rejection"):
		return nil
	case strings.HasSuffix(typename, "NotFoundRejection"):
		return fmt.Errorf("%w: swan: %s %s", money.ErrNotFound, typename, message)
	case typename == "ValidationRejection":
		return fmt.Errorf("%w: swan: %s", money.ErrInvalid, message)
	case typename == "InternalErrorRejection":
		return fmt.Errorf("%w: swan: %s", money.ErrPartnerDown, message)
	}
	return fmt.Errorf("%w: swan: %s %s", money.ErrRefused, typename, message)
}

// digest is the caller's key, scoped to the adapter and kind, as hex.
func digest(kind, key string) string {
	s := sha256.Sum256([]byte("talyvor-money\x00" + Name + "\x00" + kind + "\x00" + key))
	return hex.EncodeToString(s[:])
}

func eurOnly(a money.Amount) error {
	if a.Asset != money.EUR {
		return fmt.Errorf("%w: Swan holds euros, not %s", money.ErrInvalid, a.Asset)
	}
	if a.Minor <= 0 {
		return fmt.Errorf("%w: an amount is positive, not %d", money.ErrInvalid, a.Minor)
	}
	return nil
}

// decimal is cents as Swan writes an amount: "12.30".
func decimal(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

// cents is Swan's amount in euro cents: it never has more than two decimals, and neither does a cent.
func cents(v string) (int64, error) {
	neg := strings.HasPrefix(v, "-")
	whole, frac, _ := strings.Cut(strings.TrimPrefix(v, "-"), ".")
	if whole == "" || len(frac) > 2 || strings.ContainsAny(whole+frac, "+-") {
		return 0, fmt.Errorf("%w: Swan's amount %q", money.ErrPartnerDown, v)
	}
	frac += strings.Repeat("0", 2-len(frac))
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: Swan's amount %q", money.ErrPartnerDown, v)
	}
	if neg {
		n = -n
	}
	return n, nil
}

// validIBAN is whether iban's check digits verify (ISO 13616: mod 97 is 1).
func validIBAN(iban string) bool {
	if len(iban) < 15 || len(iban) > 34 {
		return false
	}
	var b strings.Builder
	for _, r := range iban[4:] + iban[:4] {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteString(strconv.Itoa(int(r-'A') + 10))
		default:
			return false
		}
	}
	n, _ := new(big.Int).SetString(b.String(), 10)
	return n.Mod(n, big.NewInt(97)).Int64() == 1
}

func normIBAN(s string) string { return strings.ToUpper(strings.ReplaceAll(s, " ", "")) }

// account is Swan's account object.
type account struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IBAN       string `json:"IBAN"`
	BIC        string `json:"BIC"`
	Currency   string `json:"currency"`
	StatusInfo struct {
		Status string `json:"status"`
	} `json:"statusInfo"`
	Holder struct {
		ID   string `json:"id"`
		Info struct {
			Name string `json:"name"`
		} `json:"info"`
	} `json:"holder"`
	Balances struct {
		Available struct {
			Value    string `json:"value"`
			Currency string `json:"currency"`
		} `json:"available"`
	} `json:"balances"`
}

func (a account) open() bool { return a.StatusInfo.Status == "Opened" }

const accountFields = `id name IBAN BIC currency statusInfo { status } holder { id info { name } } balances { available { value currency } }`

// ours is account ref, if Swan holds it for the project and it is not closed.
func (c *client) ours(ctx context.Context, ref string) (account, error) {
	if strings.TrimSpace(ref) == "" {
		return account{}, fmt.Errorf("%w: no account %q", money.ErrNotFound, ref)
	}
	var out struct {
		Account *account `json:"account"`
	}
	if err := c.do(ctx, "Account", `query Account($id: ID!) { account(accountId: $id) { `+accountFields+` } }`, map[string]any{"id": ref}, &out); err != nil {
		return account{}, err
	}
	if out.Account == nil || out.Account.StatusInfo.Status == "Closing" || out.Account.StatusInfo.Status == "Closed" {
		return account{}, fmt.Errorf("%w: no open account %q", money.ErrNotFound, ref)
	}
	return *out.Account, nil
}

// holderAccounts is every account of Swan's account holder holder.
func (c *client) holderAccounts(ctx context.Context, holder string) ([]account, error) {
	var all []account
	after := ""
	for range 50 {
		var out struct {
			AccountHolder *struct {
				Accounts struct {
					Edges []struct {
						Node account `json:"node"`
					} `json:"edges"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"accounts"`
			} `json:"accountHolder"`
		}
		vars := map[string]any{"id": holder, "after": nil}
		if after != "" {
			vars["after"] = after
		}
		if err := c.do(ctx, "AccountHolderAccounts", `query AccountHolderAccounts($id: ID!, $after: String) { accountHolder(id: $id) {
			accounts(first: 100, after: $after) { edges { node { `+accountFields+` } } pageInfo { hasNextPage endCursor } } } }`, vars, &out); err != nil {
			return nil, err
		}
		if out.AccountHolder == nil {
			return nil, fmt.Errorf("%w: no account holder %q", money.ErrNotFound, holder)
		}
		for _, e := range out.AccountHolder.Accounts.Edges {
			all = append(all, e.Node)
		}
		if !out.AccountHolder.Accounts.PageInfo.HasNextPage {
			return all, nil
		}
		after = out.AccountHolder.Accounts.PageInfo.EndCursor
	}
	return nil, fmt.Errorf("%w: the accounts of %s did not end", money.ErrPartnerDown, holder)
}

// openFor opens a euro account under the account holder of of, once for key: the account is named from the key, and
// from the request, so a retry finds the account it opened and the same key for another request is refused. Swan's
// openAccount takes no idempotency key.
func (c *client) openFor(ctx context.Context, kind, key, of string) (account, error) {
	switch {
	case strings.TrimSpace(key) == "":
		return account{}, fmt.Errorf("%w: every call carries the caller's idempotency key", money.ErrInvalid)
	case strings.TrimSpace(of) == "":
		return account{}, fmt.Errorf("%w: an account has a holder", money.ErrInvalid)
	}
	holder, err := c.cfg.Holders.AccountHolder(ctx, of)
	if err != nil {
		return account{}, err
	}
	prefix := "tly-" + digest(kind, key)[:20] + "."
	name := prefix + digest(kind+"-request", key+"\x00"+of)[:8]
	existing, err := c.holderAccounts(ctx, holder)
	if err != nil {
		return account{}, err
	}
	for _, a := range existing {
		if a.Name == name {
			return a, nil
		}
		if strings.HasPrefix(a.Name, prefix) {
			return account{}, fmt.Errorf("%w: key %q opened an account for another request", money.ErrKeyReused, key)
		}
	}
	var out struct {
		OpenAccount struct {
			Typename string  `json:"__typename"`
			Message  string  `json:"message"`
			Account  account `json:"account"`
		} `json:"openAccount"`
	}
	if err := c.do(ctx, "OpenAccount", `mutation OpenAccount($input: OpenAccountInput!) { openAccount(input: $input) { __typename
		... on OpenAccountSuccessPayload { account { `+accountFields+` } } ... on Rejection { message } } }`,
		map[string]any{"input": map[string]any{"accountHolderId": holder, "name": name, "language": "en"}}, &out); err != nil {
		return account{}, err
	}
	if err := rejection(out.OpenAccount.Typename, out.OpenAccount.Message); err != nil {
		return account{}, err
	}
	if out.OpenAccount.Account.ID == "" {
		return account{}, fmt.Errorf("%w: Swan opened no account", money.ErrPartnerDown)
	}
	return out.OpenAccount.Account, nil
}

func details(a account) money.Details {
	return money.Details{Asset: money.EUR, Holder: a.Holder.Info.Name, IBAN: a.IBAN, BIC: a.BIC}
}

type accounts struct{ c *client }

// Open opens a euro account with its own IBAN under the holder's verified Swan account holder.
func (a accounts) Open(ctx context.Context, req money.OpenAccount) (money.Account, error) {
	if req.Asset != money.EUR {
		return money.Account{}, fmt.Errorf("%w: Swan's accounts hold euros, not %s", money.ErrInvalid, req.Asset)
	}
	acc, err := a.c.openFor(ctx, "account", req.Key, req.Holder)
	if err != nil {
		return money.Account{}, err
	}
	st := money.StatusPending
	if acc.open() {
		st = money.StatusCompleted
	}
	return money.Account{Result: money.Result{Ref: acc.ID, Status: st}, Asset: money.EUR}, nil
}

// Close asks Swan to close the account; closing it again answers as the first time did.
func (a accounts) Close(ctx context.Context, key, ref string) (money.Result, error) {
	if strings.TrimSpace(key) == "" {
		return money.Result{}, fmt.Errorf("%w: every call carries the caller's idempotency key", money.ErrInvalid)
	}
	var out struct {
		CloseAccount struct {
			Typename string `json:"__typename"`
			Message  string `json:"message"`
		} `json:"closeAccount"`
	}
	if err := a.c.do(ctx, "CloseAccount", `mutation CloseAccount($input: CloseAccountInput!) { closeAccount(input: $input) { __typename
		... on Rejection { message } } }`, map[string]any{"input": map[string]any{"accountId": ref}}, &out); err != nil {
		return money.Result{}, err
	}
	if err := rejection(out.CloseAccount.Typename, out.CloseAccount.Message); err != nil {
		return money.Result{}, err
	}
	return money.Result{Ref: ref, Status: money.StatusCompleted}, nil
}

func (a accounts) Details(ctx context.Context, ref string) (money.Details, error) {
	acc, err := a.c.ours(ctx, ref)
	if err != nil {
		return money.Details{}, err
	}
	return details(acc), nil
}

type wallets struct{ c *client }

// Create opens an agent's own euro account, with its own IBAN, under its workspace's Swan account holder. Swan caps how
// many accounts one holder opens: past the cap it is refused (money.ErrRefused).
func (w wallets) Create(ctx context.Context, req money.CreateWallet) (money.Wallet, error) {
	if req.Asset != money.EUR {
		return money.Wallet{}, fmt.Errorf("%w: Swan's accounts hold euros, not %s", money.ErrInvalid, req.Asset)
	}
	acc, err := w.c.openFor(ctx, "wallet", req.Key, req.AgentID)
	if err != nil {
		return money.Wallet{}, err
	}
	return money.Wallet{Ref: acc.ID, Asset: money.EUR}, nil
}

func (w wallets) Balance(ctx context.Context, ref string) (money.Amount, error) {
	acc, err := w.c.ours(ctx, ref)
	if err != nil {
		return money.Amount{}, err
	}
	if acc.Balances.Available.Currency != money.EUR {
		return money.Amount{}, fmt.Errorf("%w: Swan's balance is in %q", money.ErrPartnerDown, acc.Balances.Available.Currency)
	}
	n, err := cents(acc.Balances.Available.Value)
	return money.Amount{Minor: n, Asset: money.EUR}, err
}

func (w wallets) Receive(ctx context.Context, ref string) (money.Details, error) {
	acc, err := w.c.ours(ctx, ref)
	if err != nil {
		return money.Details{}, err
	}
	return details(acc), nil
}

// paymentStatus is Swan's payment, as initiateCreditTransfers and payment answer it.
type paymentStatus struct {
	ID         string `json:"id"`
	StatusInfo struct {
		Typename string `json:"__typename"`
		Status   string `json:"status"`
		Reason   string `json:"reason"`
		Consent  struct {
			ID        string `json:"id"`
			Challenge string `json:"challenge"`
		} `json:"consent"`
	} `json:"statusInfo"`
}

const paymentFields = `id statusInfo { __typename status ... on PaymentConsentPending { consent { id challenge } } ... on PaymentRejected { reason } }`

// send pays amount out of account from to the IBAN to, once for key: Swan answers a retry with the payment it made,
// and the payment is consented to with the server-consent key, the owner having approved it before the adapter was
// asked (money.Limits).
//
// shortcut: Swan keeps an idempotency key for 24 hours; a retry later than that is a new payment. Upgrade when B46.8
// keeps each payment's partner reference and answers its retries before the adapter is asked.
func (c *client) send(ctx context.Context, kind, key string, from account, to money.Payee, own bool, amount money.Amount, label string) (money.Result, error) {
	if strings.TrimSpace(key) == "" {
		return money.Result{}, fmt.Errorf("%w: every call carries the caller's idempotency key", money.ErrInvalid)
	}
	held, err := cents(from.Balances.Available.Value)
	if err != nil {
		return money.Result{}, err
	}
	mode := "Regular"
	if c.cfg.Instant {
		mode = "InstantWithFallback"
	}
	ct := map[string]any{"amount": map[string]any{"value": decimal(amount.Minor), "currency": money.EUR},
		"sepaBeneficiary": map[string]any{"iban": normIBAN(to.IBAN), "name": to.Name, "isMyOwnIban": own, "save": false}, "mode": mode}
	if label != "" {
		ct["label"] = label
	}
	var out struct {
		Initiate struct {
			Typename string        `json:"__typename"`
			Message  string        `json:"message"`
			Payment  paymentStatus `json:"payment"`
		} `json:"initiateCreditTransfers"`
	}
	// Asked even when the account holds too little: only Swan knows whether this key already paid.
	if err := c.do(ctx, "InitiateCreditTransfers", `mutation InitiateCreditTransfers($input: InitiateCreditTransfersInput!) {
		initiateCreditTransfers(input: $input) { __typename ... on InitiateCreditTransfersSuccessPayload { payment { `+paymentFields+` } }
		... on Rejection { message } } }`, map[string]any{"input": map[string]any{"idempotencyKey": digest(kind, key)[:32], "accountId": from.ID,
		"consentRedirectUrl": c.cfg.ConsentRedirectURL, "creditTransfers": []any{ct}}}, &out); err != nil {
		return money.Result{}, err
	}
	if err := rejection(out.Initiate.Typename, out.Initiate.Message); err != nil {
		return money.Result{}, err
	}
	p := out.Initiate.Payment
	if p.ID == "" {
		return money.Result{}, fmt.Errorf("%w: Swan answered no payment", money.ErrPartnerDown)
	}
	if p.StatusInfo.Status == "ConsentPending" {
		if p, err = c.grant(ctx, p); err != nil {
			return money.Result{}, err
		}
	}
	switch p.StatusInfo.Status {
	case "Initiated":
		return money.Result{Ref: p.ID, Status: money.StatusPending}, nil // booked when SEPA settles it
	case "Rejected":
		if held < amount.Minor {
			return money.Result{}, fmt.Errorf("%w: account %s holds %d, not %d", money.ErrInsufficientFunds, from.ID, held, amount.Minor)
		}
		return money.Result{}, fmt.Errorf("%w: Swan rejected the payment: %s", money.ErrRefused, p.StatusInfo.Reason)
	}
	return money.Result{}, fmt.Errorf("%w: Swan's payment %s is %q", money.ErrPartnerDown, p.ID, p.StatusInfo.Status)
}

// grant consents to p's consent with the server-consent key, signing its challenge as Swan's server-to-server consent
// asks (an ES256 JWT of {"challenge": …}), and answers the payment after it.
func (c *client) grant(ctx context.Context, p paymentStatus) (paymentStatus, error) {
	if p.StatusInfo.Consent.ID == "" || p.StatusInfo.Consent.Challenge == "" {
		return p, fmt.Errorf("%w: Swan's payment %s waits for a consent it did not name", money.ErrPartnerDown, p.ID)
	}
	sig, err := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"challenge": p.StatusInfo.Consent.Challenge}).SignedString(c.consent)
	if err != nil {
		return p, fmt.Errorf("%w: signing Swan's consent: %v", money.ErrInvalid, err)
	}
	var out struct {
		Grant struct {
			Typename string `json:"__typename"`
			Message  string `json:"message"`
		} `json:"grantConsentWithServerSignature"`
	}
	if err := c.do(ctx, "GrantConsentWithServerSignature", `mutation GrantConsentWithServerSignature($input: GrantConsentWithServerSignatureInput!) {
		grantConsentWithServerSignature(input: $input) { __typename ... on Rejection { message } } }`,
		map[string]any{"input": map[string]any{"consentId": p.StatusInfo.Consent.ID, "signature": sig}}, &out); err != nil {
		return p, err
	}
	if err := rejection(out.Grant.Typename, out.Grant.Message); err != nil {
		return p, err
	}
	var after struct {
		Payment *paymentStatus `json:"payment"`
	}
	if err := c.do(ctx, "Payment", `query Payment($id: ID!) { payment(id: $id) { `+paymentFields+` } }`, map[string]any{"id": p.ID}, &after); err != nil {
		return p, err
	}
	if after.Payment == nil {
		return p, fmt.Errorf("%w: Swan has no payment %s after its consent", money.ErrPartnerDown, p.ID)
	}
	return *after.Payment, nil
}

type transfers struct{ c *client }

// Internal moves euros between two accounts of the same Swan account holder: a workspace's and its agents'.
func (t transfers) Internal(ctx context.Context, req money.Internal) (money.Result, error) {
	if err := eurOnly(req.Amount); err != nil {
		return money.Result{}, err
	}
	if req.From == req.To {
		return money.Result{}, fmt.Errorf("%w: a transfer is between two accounts", money.ErrInvalid)
	}
	from, err := t.c.ours(ctx, req.From)
	if err != nil {
		return money.Result{}, err
	}
	to, err := t.c.ours(ctx, req.To)
	if err != nil {
		return money.Result{}, err
	}
	if from.Holder.ID != to.Holder.ID {
		return money.Result{}, fmt.Errorf("%w: an internal transfer stays with one account holder; pay another with Outbound", money.ErrInvalid)
	}
	return t.c.send(ctx, "internal", req.Key, from, money.Payee{Name: to.Holder.Info.Name, IBAN: to.IBAN}, true, req.Amount, "")
}

// Outbound pays euros out by SEPA credit transfer to the payee's IBAN.
func (t transfers) Outbound(ctx context.Context, req money.Outbound) (money.Result, error) {
	if err := eurOnly(req.Amount); err != nil {
		return money.Result{}, err
	}
	iban := normIBAN(req.Payee.IBAN)
	if !validIBAN(iban) || strings.TrimSpace(req.Payee.Name) == "" {
		return money.Result{}, fmt.Errorf("%w: a SEPA payee has a name and a valid IBAN, not %q", money.ErrInvalid, req.Payee.IBAN)
	}
	from, err := t.c.ours(ctx, req.From)
	if err != nil {
		return money.Result{}, err
	}
	// Money to the holder's own account is Internal: a webhook does not credit it (Verify).
	own, err := t.c.holderAccounts(ctx, from.Holder.ID)
	if err != nil {
		return money.Result{}, err
	}
	for _, a := range own {
		if normIBAN(a.IBAN) == iban {
			return money.Result{}, fmt.Errorf("%w: %s is one of the holder's own accounts: move money to it with Internal", money.ErrInvalid, iban)
		}
	}
	return t.c.send(ctx, "outbound", req.Key, from, req.Payee, false, req.Amount, req.Reference)
}

// transaction is Swan's transaction object.
type transaction struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Amount struct {
		Value    string `json:"value"`
		Currency string `json:"currency"`
	} `json:"amount"`
	StatusInfo struct {
		Status string `json:"status"`
	} `json:"statusInfo"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
	Counterparty string `json:"counterparty"`
	Label        string `json:"label"`
	Debtor       struct {
		IBAN string `json:"IBAN"`
	} `json:"debtor"`
}

// incoming is the SEPA credits Swan books into an account from outside.
var incoming = map[string]bool{"SepaCreditTransferIn": true, "SepaInstantCreditTransferIn": true}

func (c *client) transaction(ctx context.Context, id string) (transaction, error) {
	var out struct {
		Transaction *transaction `json:"transaction"`
	}
	if err := c.do(ctx, "Transaction", `query Transaction($id: ID!) { transaction(id: $id) { id type amount { value currency } statusInfo { status }
		account { id } counterparty label ... on SEPACreditTransferTransaction { debtor { IBAN } } } }`, map[string]any{"id": id}, &out); err != nil {
		return transaction{}, err
	}
	if out.Transaction == nil {
		return transaction{}, fmt.Errorf("%w: Swan has no transaction %q", money.ErrNotFound, id)
	}
	return *out.Transaction, nil
}

// Credit is euros that came into an account, checked against Swan: PartnerRef is Swan's transaction id, and it must be
// a booked SEPA credit into the account of exactly the amount. It reads and changes nothing, so it credits once however
// often it is asked.
func (t transfers) Credit(ctx context.Context, req money.Credit) (money.Result, error) {
	if err := eurOnly(req.Amount); err != nil {
		return money.Result{}, err
	}
	if _, err := t.c.ours(ctx, req.To); err != nil {
		return money.Result{}, err
	}
	if strings.TrimSpace(req.PartnerRef) == "" {
		return money.Result{}, fmt.Errorf("%w: a credit names its transaction", money.ErrInvalid)
	}
	tx, err := t.c.transaction(ctx, req.PartnerRef)
	if err != nil {
		return money.Result{}, err
	}
	n, err := cents(tx.Amount.Value)
	switch {
	case err != nil:
		return money.Result{}, err
	case !incoming[tx.Type] || tx.Account.ID != req.To || tx.Amount.Currency != money.EUR:
		return money.Result{}, fmt.Errorf("%w: %s is not a SEPA credit into %s", money.ErrRefused, req.PartnerRef, req.To)
	case tx.StatusInfo.Status != "Booked":
		return money.Result{}, fmt.Errorf("%w: %s is %s, not booked", money.ErrRefused, req.PartnerRef, tx.StatusInfo.Status)
	case n != req.Amount.Minor:
		return money.Result{}, fmt.Errorf("%w: %s brought %d, not %d", money.ErrRefused, req.PartnerRef, n, req.Amount.Minor)
	}
	return money.Result{Ref: tx.ID, Status: money.StatusCompleted}, nil
}

type kyc struct{ c *client }

// Start hands a company (KYB) or a person (KYC) to Swan's hosted onboarding: Link is where they complete it.
//
// shortcut: Swan's onboarding takes no idempotency key, so a retried Start opens a second onboarding; an unfinished
// one holds no money and opens nothing. Upgrade when B48.1 keeps the verification's ref and answers retries itself.
func (k kyc) Start(ctx context.Context, req money.StartKYC) (money.Verification, error) {
	country := accountCountry[strings.ToUpper(req.Country)]
	switch {
	case strings.TrimSpace(req.Key) == "":
		return money.Verification{}, fmt.Errorf("%w: every call carries the caller's idempotency key", money.ErrInvalid)
	case strings.TrimSpace(req.Name) == "":
		return money.Verification{}, fmt.Errorf("%w: a verification names who it is of", money.ErrInvalid)
	case req.Subject != money.KYCPerson && req.Subject != money.KYCCompany:
		return money.Verification{}, fmt.Errorf("%w: a verification is of a person or a company, not %q", money.ErrInvalid, req.Subject)
	case country == "":
		return money.Verification{}, fmt.Errorf("%w: Swan opens accounts in FR, DE, ES, NL, IT and BE, not %q", money.ErrInvalid, req.Country)
	}
	op, input := "CreateIndividualAccountHolderOnboarding", map[string]any{"accountInfo": map[string]any{"country": country, "name": req.Name}}
	if req.Subject == money.KYCCompany {
		op, input["company"] = "CreateCompanyAccountHolderOnboarding", map[string]any{"name": req.Name}
	}
	field := strings.ToLower(op[:1]) + op[1:]
	var out map[string]struct {
		Typename   string     `json:"__typename"`
		Message    string     `json:"message"`
		Onboarding onboarding `json:"onboarding"`
	}
	if err := k.c.do(ctx, op, `mutation `+op+`($input: `+op+`Input!) { `+field+`(input: $input) { __typename
		... on `+op+`SuccessPayload { onboarding { `+onboardingFields+` } } ... on Rejection { message } } }`, map[string]any{"input": input}, &out); err != nil {
		return money.Verification{}, err
	}
	p := out[field]
	if err := rejection(p.Typename, p.Message); err != nil {
		return money.Verification{}, err
	}
	if p.Onboarding.ID == "" {
		return money.Verification{}, fmt.Errorf("%w: Swan started no onboarding", money.ErrPartnerDown)
	}
	return p.Onboarding.verification(), nil
}

// Status is where the onboarding stands: completed once Swan verified the account holder, whose id is its Detail.
func (k kyc) Status(ctx context.Context, ref string) (money.Verification, error) {
	var out struct {
		Onboarding *onboarding `json:"accountHolderOnboarding"`
	}
	if err := k.c.do(ctx, "AccountHolderOnboarding", `query AccountHolderOnboarding($id: ID!) { accountHolderOnboarding(id: $id) { `+onboardingFields+` } }`,
		map[string]any{"id": ref}, &out); err != nil {
		return money.Verification{}, err
	}
	if out.Onboarding == nil {
		return money.Verification{}, fmt.Errorf("%w: Swan has no onboarding %q", money.ErrNotFound, ref)
	}
	return out.Onboarding.verification(), nil
}

// onboarding is Swan's account holder onboarding.
type onboarding struct {
	ID            string `json:"id"`
	OnboardingURL string `json:"onboardingUrl"`
	AccountHolder *struct {
		ID                 string `json:"id"`
		VerificationStatus string `json:"verificationStatus"`
	} `json:"accountHolder"`
}

const onboardingFields = `id onboardingUrl accountHolder { id verificationStatus }`

func (o onboarding) verification() money.Verification {
	v := money.Verification{Result: money.Result{Ref: o.ID, Status: money.StatusPending}, Link: o.OnboardingURL}
	if h := o.AccountHolder; h != nil {
		v.Detail = h.ID
		switch h.VerificationStatus {
		case "Verified":
			v.Status = money.StatusCompleted
		case "Refused":
			v.Status = money.StatusFailed
		}
	}
	return v
}

type webhooks struct{ c *client }

// Verify checks the secret Swan sends with every webhook, and reads a booked SEPA credit from outside into one of the
// project's accounts as an Event, from Swan's API: the notification itself carries only the transaction's id. Any other
// genuine notification is money.ErrNotACredit.
func (w webhooks) Verify(h http.Header, body []byte, _ time.Time) (money.Event, error) {
	if got := h.Get(SecretHeader); got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(w.c.cfg.WebhookSecret)) != 1 {
		return money.Event{}, money.ErrBadSignature
	}
	var n struct {
		EventType  string `json:"eventType"`
		EventID    string `json:"eventId"`
		ProjectID  string `json:"projectId"`
		ResourceID string `json:"resourceId"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return money.Event{}, fmt.Errorf("%w: the body is not a notification: %v", money.ErrInvalid, err)
	}
	if n.ProjectID != w.c.cfg.ProjectID {
		return money.Event{}, fmt.Errorf("%w: a notification for project %q", money.ErrBadSignature, n.ProjectID)
	}
	if n.EventType != "Transaction.Booked" {
		return money.Event{}, money.ErrNotACredit
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second) // Swan waits 10 seconds for the answer
	defer cancel()
	tx, err := w.c.transaction(ctx, n.ResourceID)
	if err != nil {
		return money.Event{}, err
	}
	if !incoming[tx.Type] || tx.StatusInfo.Status != "Booked" || tx.Amount.Currency != money.EUR {
		return money.Event{}, money.ErrNotACredit
	}
	to, err := w.c.ours(ctx, tx.Account.ID)
	if errors.Is(err, money.ErrNotFound) {
		return money.Event{}, money.ErrNotACredit
	}
	if err != nil {
		return money.Event{}, err
	}
	// Money from another of the holder's accounts is the Internal transfer that moved it: posting it would count it twice.
	if tx.Debtor.IBAN != "" {
		own, err := w.c.holderAccounts(ctx, to.Holder.ID)
		if err != nil {
			return money.Event{}, err
		}
		for _, a := range own {
			if normIBAN(a.IBAN) == normIBAN(tx.Debtor.IBAN) {
				return money.Event{}, money.ErrNotACredit
			}
		}
	}
	amount, err := cents(tx.Amount.Value)
	if err != nil || amount <= 0 {
		return money.Event{}, fmt.Errorf("%w: the credit's amount %q", money.ErrInvalid, tx.Amount.Value)
	}
	return money.Event{ID: n.EventID, To: to.ID, Amount: money.Amount{Minor: amount, Asset: money.EUR}, PartnerRef: tx.ID,
		Payer: tx.Counterparty, Reference: tx.Label}, nil
}
