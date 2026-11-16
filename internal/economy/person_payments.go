package economy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/passkey"
	"github.com/talyvor/lens/internal/screening"
	"github.com/talyvor/lens/internal/workspace"
)

// person_payments.go — B61.32: PAY A PERSON ON TALYVOR BY THEIR NAME, PHONE NUMBER OR EMAIL.
//
// An account pays a person it finds (people_found.go) from one of its currency accounts — its owner or an admin from
// the company's account, signed as B54.33 signs a person's payment, or an agent from its own, judged by its rules
// (B61.12) — in that currency, and it arrives at once in the person's account in the same currency: the account they
// named to receive into, else their first Personal account, else their first, among those holding the currency. A
// person who holds no account in it is refused in words before anything moves. It is one money entry of two postings
// in one transaction, the sender's account −amount and the person's +amount, in one currency, so nothing is converted
// or rounded. It is made once under the sender's idempotency key: the same key again answers the payment it made, and
// another payment under it is ErrIdempotencyKeyReused.
//
// The first payment from a sender to a person is a new payee: an owner's is paid only signed by one of the account's
// keys, which it must have, and an agent's waits for the owner's approval whatever its rules allow. It is screened by
// the person's verified name, frozen accounts on either side are refused (postMoneyTx), and a test account pays only
// test accounts (B25.1). Each side's statement shows the other by their shown name, and the note; the sender reads
// nothing else of the person, before or after. There is no fee in this first version.

var (
	// ErrPersonPayment: a payment to a person without a name, number or email, a positive amount in GBP, EUR, USD or
	// USDC, or an idempotency key.
	ErrPersonPayment = errors.New("economy: invalid payment to a person")
	// ErrPersonCurrency: the person paid holds no account in the payment's currency.
	ErrPersonCurrency = errors.New("economy: the person paid has no account in this currency")
	// ErrPayYourself: the person found receives into the very account paying.
	ErrPayYourself = errors.New("economy: this is the account paying: a person does not pay their own account")
	// ErrPersonFirstPaymentKey: the first payment to a person is signed with one of the account's keys, and it has none.
	ErrPersonFirstPaymentKey = errors.New("economy: the first payment to a person is signed with your passkey or the phone app's key. Add one first")
	// ErrPersonCannotBePaid: the person's account is frozen, which the sender is not told more of.
	ErrPersonCannotBePaid = errors.New("economy: this person cannot be paid just now")
	// ErrPersonPhoneSandbox: live money to a person found by a number confirmed only in Sandbox.
	ErrPersonPhoneSandbox = errors.New("economy: this person's number was confirmed only in Sandbox, so it is not paid live money until they confirm it again")
)

// PersonPayment is one payment to a person on Talyvor, as the sender sees it.
type PersonPayment struct {
	ID             string    `json:"id"` // its money entry
	ToShownName    string    `json:"to_shown_name"`
	FromAccountID  string    `json:"from_account_id"`
	AgentID        string    `json:"agent_id,omitempty"`
	AmountMinor    int64     `json:"amount_minor"`
	Currency       string    `json:"currency"`
	Funding        string    `json:"funding"`
	Note           string    `json:"note,omitempty"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`

	// To is the name, phone number or email the sender typed; AccountID the account it pays from, "" for the
	// company's in the currency. Neither is answered back.
	To        string `json:"-"`
	AccountID string `json:"-"`

	signed    bool
	assertion *passkey.Assertion
}

func (p *PersonPayment) normalise() error {
	p.To = strings.TrimSpace(p.To)
	p.AccountID = strings.TrimSpace(p.AccountID)
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	p.Note = strings.Join(strings.Fields(p.Note), " ")
	p.IdempotencyKey = strings.TrimSpace(p.IdempotencyKey)
	if p.Funding == "" {
		p.Funding = FundingTest
	}
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrPersonPayment, msg) }
	_, known := MoneyCurrencies[p.Currency]
	switch {
	case p.To == "":
		return bad("a payment names the person it pays: their Talyvor name, phone number or email (to)")
	case !known:
		return bad(fmt.Sprintf("a payment is in GBP, EUR, USD or USDC, not %q", p.Currency))
	case p.AmountMinor <= 0:
		return bad("a payment's amount_minor is a positive number of pence, cents or millionths of a USDC")
	case p.Funding != FundingTest && p.Funding != FundingLive:
		return bad(fmt.Sprintf("funding is test or live, not %q", p.Funding))
	case p.IdempotencyKey == "" || len(p.IdempotencyKey) > 128:
		return bad("a payment carries an idempotency key (the Idempotency-Key header), up to 128 characters")
	case len(p.Note) > 140:
		return bad("a payment's note is up to 140 characters")
	}
	return nil
}

// entryKey is the idempotency key of p's money entry: the sender's — the agent's, or the owner's — so two senders'
// keys never meet.
func (p PersonPayment) entryKey() string {
	return "person_pay:" + cmp.Or(p.AgentID, "owner") + ":" + p.IdempotencyKey
}

// toHash is what the sender typed, kept only as a hash, so a retry finding someone else is another payment.
func (p PersonPayment) toHash() string {
	kind, value := personQuery(p.To)
	h := sha256.Sum256([]byte(kind + "\x00" + cmp.Or(value, p.To)))
	return hex.EncodeToString(h[:])
}

// personPaymentPurpose binds a challenge to one payment exactly as asked, so a signature for it pays no other.
func personPaymentPurpose(p PersonPayment) string {
	h := sha256.Sum256([]byte(strings.Join([]string{p.AccountID, p.toHash(), strconv.FormatInt(p.AmountMinor, 10), p.Currency,
		p.Funding, p.Note, p.IdempotencyKey}, "\x00")))
	return "person_pay:" + hex.EncodeToString(h[:])
}

// PersonPaymentChallenge issues the challenge an owner's payment in is signed over, and the workspace's passkeys that
// may sign it; its device keys sign it too.
func (s *DualTokenStore) PersonPaymentChallenge(ctx context.Context, workspaceID string, in PersonPayment) (challenge string, credentialIDs []string, err error) {
	if err := in.normalise(); err != nil {
		return "", nil, err
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, personPaymentPurpose(in))
	return c, credentialIDs, err
}

// PayPersonSigned is PayPerson for the owner or an admin paying from the company's account, with a, the signature over
// PersonPaymentChallenge's challenge for in.
func (s *DualTokenStore) PayPersonSigned(ctx context.Context, workspaceID string, in PersonPayment, a *passkey.Assertion) (PersonPayment, error) {
	in.AgentID, in.signed, in.assertion = "", true, a
	return s.PayPerson(ctx, workspaceID, in)
}

// replayedPersonPayment is the payment workspaceID made under p's key, when it made one: p as made, or
// ErrIdempotencyKeyReused when it was another payment.
func replayedPersonPayment(ctx context.Context, q pgxDB, workspaceID string, p PersonPayment) (PersonPayment, bool, error) {
	var made PersonPayment
	var hash string
	err := q.QueryRow(ctx, `SELECT id, to_shown_name, from_account_id, from_agent_id, amount_minor, currency, funding, note, to_hash, created_at
		FROM person_payments WHERE from_workspace_id = $1 AND entry_key = $2`, workspaceID, p.entryKey()).
		Scan(&made.ID, &made.ToShownName, &made.FromAccountID, &made.AgentID, &made.AmountMinor, &made.Currency, &made.Funding,
			&made.Note, &hash, &made.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersonPayment{}, false, nil
	}
	if err != nil {
		return PersonPayment{}, false, fmt.Errorf("economy: pay a person: %w", err)
	}
	if made.AmountMinor != p.AmountMinor || made.Currency != p.Currency || made.Funding != p.Funding || made.Note != p.Note ||
		hash != p.toHash() || (p.AccountID != "" && made.FromAccountID != p.AccountID) {
		return PersonPayment{}, true, ErrIdempotencyKeyReused
	}
	made.IdempotencyKey = p.IdempotencyKey
	return made, true, nil
}

// PayPerson pays in.AmountMinor of in.Currency from workspaceID's account in that currency — in.AgentID's own, judged
// by its rules, when it is set — to the person in.To finds, under in.IdempotencyKey.
func (s *DualTokenStore) PayPerson(ctx context.Context, workspaceID string, in PersonPayment) (PersonPayment, error) {
	if err := in.normalise(); err != nil {
		return PersonPayment{}, err
	}
	p := in
	// The same key again answers the payment it made, and finds and judges nothing again.
	if made, ok, err := replayedPersonPayment(ctx, s.pool, workspaceID, p); ok || err != nil {
		return made, err
	}
	keys, err := workspaceKeys(ctx, s.pool, workspaceID)
	if err != nil {
		return PersonPayment{}, err
	}
	if p.AgentID == "" && keys > 0 && p.assertion == nil { // B54.33: refused before anything is looked up or screened
		return PersonPayment{}, ErrPaymentUnsigned
	}
	to, err := s.lookUpPerson(ctx, workspaceID, p.To)
	if err != nil {
		return PersonPayment{}, err
	}
	p.ToShownName = to.ShownName
	if p.Funding == FundingLive && to.byPhone && to.phoneSandbox {
		return PersonPayment{}, ErrPersonPhoneSandbox
	}
	var toWorkspaceID, toAccountID string
	err = s.pool.QueryRow(ctx, `SELECT w.id, a.id FROM workspaces w
		JOIN money_accounts a ON a.workspace_id = w.id AND a.purpose = 'company' AND a.currency = $4 AND a.status = 'open'
		WHERE w.id = $1 OR w.id IN (SELECT workspace_id FROM person_accounts WHERE person_key = $2)
		ORDER BY (w.id = $3) DESC, (w.account_type = 'personal') DESC, (w.id = $1) DESC, w.created_at, w.id, a.created_at
		LIMIT 1`, to.firstID, to.personKey, to.receiveID, p.Currency).Scan(&toWorkspaceID, &toAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersonPayment{}, fmt.Errorf("%w: %s has no %s account, so they cannot be paid in %s. Pay them in a currency they hold",
			ErrPersonCurrency, to.ShownName, p.Currency, p.Currency)
	}
	if err != nil {
		return PersonPayment{}, fmt.Errorf("economy: pay a person: %w", err)
	}
	if toWorkspaceID == workspaceID {
		return PersonPayment{}, ErrPayYourself
	}
	// B25.1: test money moves only between test workspaces, real money only between real ones.
	if err := workspace.CheckMoneyWall(ctx, s.pool, workspaceID, toWorkspaceID); err != nil {
		return PersonPayment{}, err
	}
	if err := checkMoneySide(ctx, s.pool, workspaceID, "currency", moneyKindOf(p.Currency)); err != nil {
		return PersonPayment{}, err
	}
	if err := checkMoneySide(ctx, s.pool, toWorkspaceID, "to", moneyKindOf(p.Currency)); err != nil {
		return PersonPayment{}, err
	}
	if p.AgentID != "" {
		p.FromAccountID, err = openAgentAccount(ctx, s.pool, workspaceID, p.AgentID, p.Currency)
	} else {
		err = s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT id FROM money_accounts WHERE workspace_id = $1 AND purpose = 'company'
			AND currency = $2 AND status = 'open' AND ($3 = '' OR id = $3) ORDER BY created_at LIMIT 1), '')`,
			workspaceID, p.Currency, p.AccountID).Scan(&p.FromAccountID)
	}
	if err != nil {
		return PersonPayment{}, fmt.Errorf("economy: pay a person: %w", err)
	}
	if p.FromAccountID == "" {
		return PersonPayment{}, fmt.Errorf("%w: no open %s account to pay from", ErrMoneyAccountNotFound, p.Currency)
	}
	fromName, err := s.accountShownName(ctx, workspaceID)
	if err != nil {
		return PersonPayment{}, err
	}
	var paidBefore bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM person_payments WHERE from_workspace_id = $1 AND from_agent_id = $2
		AND to_person_id = $3)`, workspaceID, p.AgentID, to.personID).Scan(&paidBefore); err != nil {
		return PersonPayment{}, fmt.Errorf("economy: pay a person: %w", err)
	}
	if p.AgentID == "" && !paidBefore && keys == 0 {
		return PersonPayment{}, ErrPersonFirstPaymentKey
	}

	var value int64
	if p.AgentID != "" {
		if s.accountPartners == nil && !dollarLike(p.Currency) {
			return PersonPayment{}, fmt.Errorf("%w: no account partner is set to value the payment", ErrAccountNotOpened)
		}
		if value, err = s.paymentValue(ctx, OutsidePayment{WorkspaceID: workspaceID, IdempotencyKey: p.entryKey(), AmountMinor: p.AmountMinor,
			Currency: p.Currency}); err != nil {
			return PersonPayment{}, err
		}
		fp := sha256.Sum256([]byte("person_pay\x00" + p.AgentID + "\x00" + p.FromAccountID + "\x00" + to.personID + "\x00" +
			strconv.FormatInt(p.AmountMinor, 10) + "\x00" + p.Currency + "\x00" + p.Funding + "\x00" + p.Note))
		ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]),
			Payee:    Payee{Kind: "person", ID: personPayeeID(to.personID), Name: to.ShownName},
			Memo:     strings.TrimSuffix(Money(p.AmountMinor, p.Currency)+" to "+to.ShownName+" — "+p.Note, " — "),
			PaidFrom: PaidFrom{Kind: "account", AccountID: p.FromAccountID, Currency: p.Currency, AmountMinor: p.AmountMinor, funding: p.Funding}})
		if !paidBefore { // a new payee waits for the owner's approval, whatever the rules allow
			ctx = WithConnectPrepareOnly(ctx)
		}
	}

	// B30.6: screened by the person's verified name before the locks, as a payment out is.
	if s.screener == nil {
		return PersonPayment{}, fmt.Errorf("%w: no screener is set, so no money moves to a person", partners.ErrScreeningUnavailable)
	}
	if err := s.screener.ScreenPayment(ctx, screening.Payment{WorkspaceID: workspaceID, ID: p.entryKey(), Capability: CapabilityCurrencyAccounts,
		Direction: "out", Counterparty: to.fullName, AmountMinor: p.AmountMinor, Currency: p.Currency, Funding: p.Funding}); err != nil {
		return PersonPayment{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PersonPayment{}, fmt.Errorf("economy: pay a person: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if p.AgentID != "" {
		if err := lockAgent(ctx, tx, workspaceID, p.AgentID); err != nil {
			return PersonPayment{}, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, p.FromAccountID); err != nil {
		return PersonPayment{}, fmt.Errorf("economy: pay a person: %w", err)
	}
	// Read again under the locks: the same key paid twice at once pays once, and the second answers the first.
	if made, ok, err := replayedPersonPayment(ctx, tx, workspaceID, p); ok || err != nil {
		return made, err
	}
	if p.AgentID == "" { // B54.33: the challenge is spent only with the payment it pays
		if err := liveKeyFirst(ctx, tx, workspaceID, p.Funding); err != nil {
			return PersonPayment{}, err
		}
		if err := s.authorizeSignedTx(ctx, tx, workspaceID, personPaymentPurpose(p), p.assertion, ErrPaymentUnsigned); err != nil {
			return PersonPayment{}, err
		}
	}
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		p.FromAccountID, p.Funding).Scan(&held); err != nil {
		return PersonPayment{}, fmt.Errorf("economy: pay a person: %w", err)
	}
	if held < p.AmountMinor {
		return PersonPayment{}, fmt.Errorf("%w: the account holds %s of %s money, and this payment is %s", ErrAgentFunds,
			Money(held, p.Currency), p.Funding, Money(p.AmountMinor, p.Currency))
	}
	if p.AgentID != "" {
		if err := s.enforceAgentRules(ctx, tx, workspaceID, p.AgentID, value, p.entryKey()); err != nil {
			return PersonPayment{}, s.refusedMovement(ctx, tx, err)
		}
	}
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityCurrencyAccounts, Kind: "person_payment",
		IdempotencyKey: p.entryKey(), Funding: p.Funding, Memo: p.Note, payeeWorkspaceID: toWorkspaceID, Postings: []MoneyPosting{
			{AccountID: p.FromAccountID, AmountMinor: -p.AmountMinor, Currency: p.Currency},
			{AccountID: toAccountID, AmountMinor: p.AmountMinor, Currency: p.Currency}}}, s.screener)
	var refusal *CapabilityRefusal
	if errors.As(err, &refusal) && refusal.Freeze != nil && refusal.Freeze.WorkspaceID == toWorkspaceID {
		return PersonPayment{}, ErrPersonCannotBePaid // the sender reads nothing of the person's case
	}
	if err != nil {
		return PersonPayment{}, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO person_payments (id, entry_key, from_workspace_id, from_agent_id, from_account_id, from_shown_name,
		to_person_id, to_workspace_id, to_account_id, to_shown_name, to_hash, amount_minor, currency, funding, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING created_at`,
		e.ID, p.entryKey(), workspaceID, p.AgentID, p.FromAccountID, fromName, to.personID, toWorkspaceID, toAccountID, to.ShownName,
		p.toHash(), p.AmountMinor, p.Currency, p.Funding, p.Note).Scan(&p.CreatedAt); err != nil {
		return PersonPayment{}, fmt.Errorf("economy: pay a person: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return PersonPayment{}, fmt.Errorf("%w: %s", ErrMoneyUnbalanced, pgErr.Message)
		}
		return PersonPayment{}, fmt.Errorf("economy: pay a person: %w", err)
	}
	p.ID = e.ID
	return p, nil
}

// personPayeeID is the person as an agent's rules and approvals name them: a hash, so the owner reads no id of theirs.
func personPayeeID(personID string) string {
	h := sha256.Sum256([]byte("talyvor-person-payee\x00" + personID))
	return "person_" + hex.EncodeToString(h[:12])
}

// workspaceKeys is how many passkeys and device keys workspaceID has, revoked ones included, as B54.33 counts them.
func workspaceKeys(ctx context.Context, q pgxDB, workspaceID string) (int, error) {
	var n int
	if err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_passkeys WHERE workspace_id = $1)
		+ (SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1)`, workspaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("economy: passkeys: %w", err)
	}
	return n, nil
}

// accountShownName is what a person paid sees of workspaceID: its person's shown name from the identity check it
// passed, else the company its company check confirmed, else the account's name.
func (s *DualTokenStore) accountShownName(ctx context.Context, workspaceID string) (string, error) {
	full, err := verifiedPersonName(ctx, s.pool, `v.workspace_id = $1`, workspaceID)
	if err != nil {
		return "", err
	}
	if full != "" {
		return ShownName(full), nil
	}
	var name string
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(
		(SELECT v.verified_name FROM workspace_verifications v WHERE v.workspace_id = $1 AND v.level = 3 AND v.subject = 'company'
		   AND v.status = 'completed' AND v.verified_name <> ''
		   AND v.id = (SELECT max(w.id) FROM workspace_verifications w WHERE w.workspace_id = v.workspace_id AND w.evidence_ref = v.evidence_ref)
		 ORDER BY v.id DESC LIMIT 1),
		(SELECT NULLIF(name, '') FROM workspaces WHERE id = $1), '')`, workspaceID).Scan(&name); err != nil {
		return "", fmt.Errorf("economy: the account's name: %w", err)
	}
	return cmp.Or(strings.TrimSpace(name), "Talyvor account"), nil
}
