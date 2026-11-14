package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/money"
)

// instant_transfer_send.go — B59.15: INSTANT TRANSFER TO A TALYVOR ACCOUNT: ONE ID, THREE LEGS, ALL OR NOTHING
// (launch pack 4.1, 4.2 (1), 4.3).
//
// An agent of a hybrid workspace sends against a live quote (B59.14) under an idempotency key, from its account in the
// currency the quote sends to an account in Talyvor in the currency it delivers. The transfer has one id and three legs,
// each a money entry in ledger order (B58.4) and a call to the money layer's provider for it (B59.13):
//
//	convert — the agent's money leaves its account: the fee to Talyvor's revenue, the rest converted to USDC, which
//	          Talyvor holds for the transfer (Sent, then Converted)
//	move    — the USDC goes on chain to the beneficiary (On its way)
//	credit  — it lands in the beneficiary's account: converted to their currency on arrival, or kept as USDC by an
//	          account that holds USDC (Received)
//
// Each leg moves exactly what the quote showed, so what arrives is the quote's "they get". If a leg fails, the legs
// before it are reversed at the figures they moved at — the fee too — so the agent has exactly what it had, and the
// transfer ends reversed: no transfer ends half done. A leg whose provider did not answer, or whose answer was not
// kept, is neither: the transfer stays where it was, and sending again with the same key finishes it. The same key
// again never moves money twice; it answers the transfer as it stands.
//
// The agent's rule "Can use Instant transfer" (may_instant_transfer) is off until its owner turns it on, and an agent
// without it is refused with that field. Its other rules judge the transfer as a payment, at what the amount sent is
// worth: a limit refuses it, and above its approval amount — or to an account it has never sent an Instant transfer to
// — it files an approval and waits for the owner to sign it; sending again after that goes through once. A Business
// account's second-approver line will apply once it exists (B28.40); until then the owner's approval stands in.
//
// B59.16 — to an outside wallet (launch pack 4.2 (2)): the agent names one of its owner's saved payees at a USDC address
// (to_payee_id, a payee the owner approved) instead of a Talyvor account, at a quote for a wallet. The address and the
// payee are screened through the money layer's transfer hooks before any leg moves, so a screened-out address is
// refused with nothing moved; the move leg then sends the USDC to the address through OnchainProvider with the
// originator's and the beneficiary's details attached, and that is the transfer received: the beneficiary converts it
// themselves, so there is no credit leg. A failed move reverses the conversion, as for an account. A payee the agent has
// never sent an Instant transfer to — counted by the address, as it was paid — waits for the owner's approval.
//
// B59.17 — to an outside bank account (launch pack 4.2 (3)): the agent names a saved payee at a bank account (to_payee_id,
// no address), at a quote for a bank account in the payee's country. The beneficiary is screened through the transfer
// hooks before any leg moves. The third leg is a payout instead of a credit: the USDC is converted to the currency the
// bank is paid in, at the quote the transfer locked, and the payout provider pays it into the account. A rail that does
// not land in seconds carries its real time in the quote, in the approval the owner confirms and in the transfer. A
// failed payout reverses the move and the conversion, so the agent has exactly what it had. A bank account paid in the
// currency sent at a direct quote is paid straight: one leg, the payout, its money entry posted with the transfer and
// reversed if the payout fails. A payee is new until the agent has sent it an Instant transfer since its bank details
// last changed.
//
// shortcut: the provider's side of a transfer is held in accounts opened at the provider for it — one for the currency
// sent, credited with what the convert leg sells, and one for each side's USDC and the currency delivered — and a
// reversal is the ledger's alone, at the locked figures: what the provider holds for a reversed transfer stays there.
// Upgrade when a live provider's accounts and its reverse conversion are known (B59.16, B59.17 build on this).

// The states of an Instant transfer, as the owner sees them: Sent, Converted, On its way, Received; or reversed.
const (
	InstantSent      = "sent"
	InstantConverted = "converted"
	InstantOnItsWay  = "on_its_way"
	InstantReceived  = "received"
	InstantReversed  = "reversed"
)

// The legs of an Instant transfer, in ledger order.
const (
	InstantLegConvert = "convert"
	InstantLegMove    = "move"
	InstantLegCredit  = "credit"
	InstantLegPayout  = "payout" // to an outside bank account, in place of the credit
)

// FieldMayInstantTransfer is the agent rule an Instant transfer needs: "Can use Instant transfer".
const FieldMayInstantTransfer = "may_instant_transfer"

// InstantRuleRefusal is an Instant transfer the agent's rules do not let it send, named by the rule's field.
type InstantRuleRefusal struct {
	Field string `json:"field"`
}

func (r *InstantRuleRefusal) Error() string {
	return "economy: this agent's rules do not let it use Instant transfer (" + r.Field + ")"
}

func (r *InstantRuleRefusal) Unwrap() error { return ErrAgentRule }

// InstantSend is a send against quote QuoteID to the Talyvor account ToAccountID, or to the outside wallet of the saved
// payee ToPayeeID, under IdempotencyKey; Funding is test ("" too) or live.
type InstantSend struct {
	QuoteID        string `json:"quote_id"`
	ToAccountID    string `json:"to_account_id,omitempty"`
	ToPayeeID      string `json:"to_payee_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	Funding        string `json:"funding"`
}

// InstantAmount is an amount of one currency.
type InstantAmount struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

// InstantLeg is one leg of an Instant transfer: what it sold and bought (the same for the move), its money entry and the
// provider's reference — both conversions and the on-chain reference are the receipt's.
type InstantLeg struct {
	ID         string `json:"id"` // <transfer id>:<leg>
	Leg        string `json:"leg"`
	Done       bool   `json:"done"`
	EntryID    string `json:"entry_id,omitempty"`
	PartnerRef string `json:"partner_ref,omitempty"`
	// ConversionRef is a payout's conversion out of USDC, when it made one: the receipt shows both conversions.
	ConversionRef string        `json:"conversion_ref,omitempty"`
	Sell          InstantAmount `json:"sell"`
	Buy           InstantAmount `json:"buy"`
}

// InstantTransfer is an Instant transfer and where it stands.
type InstantTransfer struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	AgentID         string `json:"agent_id"`
	QuoteID         string `json:"quote_id"`
	IdempotencyKey  string `json:"idempotency_key"`
	Funding         string `json:"funding"`
	FromAccountID   string `json:"from_account_id"`
	ToAccountID     string `json:"to_account_id,omitempty"`
	ToPayeeID       string `json:"to_payee_id,omitempty"` // an outside wallet: the payee, and the address it was paid at
	ToNetwork       string `json:"to_network,omitempty"`
	ToAddress       string `json:"to_address,omitempty"`
	BeneficiaryName string `json:"beneficiary_name"`
	SendMinor       int64  `json:"send_minor"`
	SendCurrency    string `json:"send_currency"`
	FeeMinor        int64  `json:"fee_minor"`
	ReceiveMinor    int64  `json:"receive_minor"`
	ReceiveCurrency string `json:"receive_currency"`
	Rate            string `json:"rate"`
	// ArrivesInSeconds is true when the money lands in seconds; otherwise ArrivesWithinSeconds is the bank rail's
	// longest time, as the quote showed it.
	ArrivesInSeconds     bool         `json:"arrives_in_seconds"`
	ArrivesWithinSeconds int64        `json:"arrives_within_seconds,omitempty"`
	Status               string       `json:"status"`
	Legs                 []InstantLeg `json:"legs"`
	FailedLeg            string       `json:"failed_leg,omitempty"`
	Failure              string       `json:"failure,omitempty"`
	ReversalEntries      []string     `json:"reversal_entry_ids"`
	CreatedAt            time.Time    `json:"created_at"`
	UpdatedAt            time.Time    `json:"updated_at"`

	toWorkspaceID      string
	beneficiaryCountry string
	onchainMinor       int64
	convertEntry       string
	moveEntry          string
	creditEntry        string
	convertRef         string
	onchainRef         string
	creditRef          string
	payoutEntry        string
	payoutConvertRef   string
	payoutRef          string
}

const instantTransferCols = `id, workspace_id, agent_id, quote_id, idempotency_key, funding, from_account_id, to_workspace_id, to_account_id,
	beneficiary_name, send_minor, send_currency, fee_minor, onchain_minor, receive_minor, receive_currency, rate, status, convert_entry_id,
	move_entry_id, credit_entry_id, convert_ref, onchain_ref, credit_ref, failed_leg, failure, reversal_entry_ids, created_at, updated_at,
	to_payee_id, to_network, to_address, beneficiary_country, payout_entry_id, payout_convert_ref, payout_ref, arrives_within_seconds`

func scanInstantTransfer(row pgx.Row) (InstantTransfer, error) {
	var t InstantTransfer
	err := row.Scan(&t.ID, &t.WorkspaceID, &t.AgentID, &t.QuoteID, &t.IdempotencyKey, &t.Funding, &t.FromAccountID, &t.toWorkspaceID,
		&t.ToAccountID, &t.BeneficiaryName, &t.SendMinor, &t.SendCurrency, &t.FeeMinor, &t.onchainMinor, &t.ReceiveMinor, &t.ReceiveCurrency,
		&t.Rate, &t.Status, &t.convertEntry, &t.moveEntry, &t.creditEntry, &t.convertRef, &t.onchainRef, &t.creditRef, &t.FailedLeg,
		&t.Failure, &t.ReversalEntries, &t.CreatedAt, &t.UpdatedAt, &t.ToPayeeID, &t.ToNetwork, &t.ToAddress, &t.beneficiaryCountry,
		&t.payoutEntry, &t.payoutConvertRef, &t.payoutRef, &t.ArrivesWithinSeconds)
	if err != nil {
		return t, err
	}
	t.Legs = []InstantLeg{
		{ID: t.ID + ":" + InstantLegConvert, Leg: InstantLegConvert, Done: t.convertRef != "" && t.Status != InstantReversed, EntryID: t.convertEntry,
			PartnerRef: t.convertRef, Sell: InstantAmount{t.SendMinor, t.SendCurrency}, Buy: InstantAmount{t.onchainMinor, CurrencyUSDC}},
		{ID: t.ID + ":" + InstantLegMove, Leg: InstantLegMove, Done: t.onchainRef != "" && t.Status != InstantReversed, EntryID: t.moveEntry,
			PartnerRef: t.onchainRef, Sell: InstantAmount{t.onchainMinor, CurrencyUSDC}, Buy: InstantAmount{t.onchainMinor, CurrencyUSDC}},
	}
	t.ArrivesInSeconds = t.ArrivesWithinSeconds == 0
	payout := InstantLeg{ID: t.ID + ":" + InstantLegPayout, Leg: InstantLegPayout, Done: t.Status == InstantReceived, EntryID: t.payoutEntry,
		PartnerRef: t.payoutRef, ConversionRef: t.payoutConvertRef, Sell: InstantAmount{t.onchainMinor, CurrencyUSDC}, Buy: InstantAmount{t.ReceiveMinor, t.ReceiveCurrency}}
	switch {
	case t.direct(): // paid straight from the currency sent: the payout is the only leg
		payout.Sell = InstantAmount{t.SendMinor, t.SendCurrency}
		t.Legs = []InstantLeg{payout}
	case t.toBank():
		t.Legs = append(t.Legs, payout)
	case !t.toWallet(): // an outside wallet's beneficiary converts the USDC themselves
		t.Legs = append(t.Legs, InstantLeg{ID: t.ID + ":" + InstantLegCredit, Leg: InstantLegCredit, Done: t.Status == InstantReceived, EntryID: t.creditEntry,
			PartnerRef: t.creditRef, Sell: InstantAmount{t.onchainMinor, CurrencyUSDC}, Buy: InstantAmount{t.ReceiveMinor, t.ReceiveCurrency}})
	}
	return t, nil
}

// toWallet is whether t pays an outside wallet rather than a Talyvor account.
func (t InstantTransfer) toWallet() bool { return t.ToAddress != "" }

// toBank is whether t pays a saved payee's outside bank account.
func (t InstantTransfer) toBank() bool { return t.ToPayeeID != "" && t.ToAddress == "" }

// direct is whether t pays a bank account straight from the currency sent, with nothing converted or moved on chain.
func (t InstantTransfer) direct() bool { return t.toBank() && t.onchainMinor == 0 }

// InstantTransfer is workspaceID's Instant transfer id, its receipt; ErrInstantTransfer when there is none.
func (s *DualTokenStore) InstantTransfer(ctx context.Context, workspaceID, id string) (InstantTransfer, error) {
	t, err := scanInstantTransfer(s.pool.QueryRow(ctx, `SELECT `+instantTransferCols+` FROM instant_transfers WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, fmt.Errorf("%w: no Instant transfer %q", ErrMoneyAccountNotFound, id)
	}
	if err != nil {
		return t, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	return t, nil
}

// SendInstantTransfer sends in from agentID's account, all or nothing, and answers the transfer as it ended.
func (s *DualTokenStore) SendInstantTransfer(ctx context.Context, workspaceID, agentID string, in InstantSend) (InstantTransfer, error) {
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || len(key) > 200 {
		return InstantTransfer{}, fmt.Errorf("%w: a send carries an idempotency key (the Idempotency-Key header, or idempotency_key), up to 200 characters", ErrInstantTransfer)
	}
	if in.Funding == "" {
		in.Funding = FundingTest
	}
	in.ToAccountID, in.ToPayeeID = strings.TrimSpace(in.ToAccountID), strings.TrimSpace(in.ToPayeeID)
	if (in.ToAccountID == "") == (in.ToPayeeID == "") {
		return InstantTransfer{}, fmt.Errorf("%w: a send names to_account_id, a Talyvor account, or to_payee_id, a saved payee at a wallet address", ErrInstantTransfer)
	}
	if t, found, err := s.instantTransferByKey(ctx, s.pool, workspaceID, agentID, key, in); found || err != nil {
		if err != nil {
			return InstantTransfer{}, err
		}
		return s.driveInstantTransfer(ctx, t)
	}
	kind, err := workspaceMoneyKind(ctx, s.pool, workspaceID)
	if err != nil {
		return InstantTransfer{}, err
	}
	if kind != MoneyKindHybrid {
		return InstantTransfer{}, &MoneyKindRefusal{Field: "instant_transfer", MoneyKind: kind}
	}
	switch {
	case in.Funding != FundingTest && in.Funding != FundingLive:
		return InstantTransfer{}, fmt.Errorf("%w: funding is test or live, not %q", ErrInstantTransfer, in.Funding)
	case s.moneyLayer == nil:
		return InstantTransfer{}, fmt.Errorf("%w: no money layer is set", ErrAccountNotOpened)
	}
	rules, err := getAgentRules(ctx, s.pool, workspaceID, agentID)
	if err != nil {
		return InstantTransfer{}, err
	}
	if rules.MayInstantTransfer == nil || !*rules.MayInstantTransfer {
		return InstantTransfer{}, &InstantRuleRefusal{Field: FieldMayInstantTransfer}
	}
	q, err := s.InstantTransferQuote(ctx, workspaceID, in.QuoteID)
	if err != nil {
		return InstantTransfer{}, err
	}
	switch {
	case (in.ToPayeeID == "") != (q.To == InstantToAccount):
		return InstantTransfer{}, fmt.Errorf("%w: quote %s is for a %s: send it with to_account_id for a Talyvor account, to_payee_id for a saved payee", ErrInstantTransfer, q.ID, q.To)
	case !instantSendable(q):
		return InstantTransfer{}, fmt.Errorf("%w: quote %s is not for an Instant transfer", ErrInstantTransfer, q.ID)
	}
	direct := q.route == directRoute
	if err := q.Live(time.Now()); err != nil {
		return InstantTransfer{}, err
	}
	from, err := s.agentCurrencyAccount(ctx, workspaceID, agentID, q.SendCurrency)
	if err != nil {
		return InstantTransfer{}, err
	}
	var to instantDestination
	switch q.To {
	case InstantToWallet:
		to, err = s.instantWallet(ctx, workspaceID, in.ToPayeeID)
	case InstantToBank:
		to, err = s.instantBank(ctx, workspaceID, in.ToPayeeID, q.Country)
	default:
		to, err = s.instantAccount(ctx, workspaceID, in.ToAccountID, q.ReceiveCurrency)
	}
	if err != nil {
		return InstantTransfer{}, err
	}
	beneficiary := to.name
	value, err := s.paymentValue(ctx, OutsidePayment{WorkspaceID: workspaceID, IdempotencyKey: "instant:" + q.ID, AmountMinor: q.SendMinor, Currency: q.SendCurrency})
	if err != nil {
		return InstantTransfer{}, err
	}
	// A bank payee is new again once its bank details change: it counts only what was sent to it since.
	var paidBefore bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instant_transfers t WHERE t.agent_id = $1 AND t.status = 'received'
		AND (($2 <> '' AND t.to_payee_id = '' AND t.to_account_id = $2) OR (t.to_address <> '' AND t.to_address = $3)
		  OR ($3 = '' AND $4 <> '' AND t.to_payee_id = $4 AND t.to_address = '' AND t.created_at > COALESCE((SELECT p.details_changed_at FROM money_payees p
		    WHERE p.id = $4), '-infinity'))))`,
		agentID, in.ToAccountID, to.address, in.ToPayeeID).Scan(&paidBefore); err != nil {
		return InstantTransfer{}, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	// An approval is for the transfer, not the quote: sent again after it, at a new quote, it finds it.
	fp := sha256.Sum256([]byte("instant\x00" + agentID + "\x00" + from + "\x00" + in.ToAccountID + in.ToPayeeID + "\x00" + strconv.FormatInt(q.SendMinor, 10) +
		"\x00" + q.SendCurrency + "\x00" + q.ReceiveCurrency + "\x00" + in.Funding))
	judged := WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: hex.EncodeToString(fp[:]),
		Payee:    to.payee,
		Memo:     "Instant transfer of " + Money(q.SendMinor, q.SendCurrency) + " to " + beneficiary + arrivesWithin(q.ArrivesWithinSeconds),
		PaidFrom: PaidFrom{Kind: "account", AccountID: from, Currency: q.SendCurrency, AmountMinor: q.SendMinor, funding: in.Funding}})
	if !paidBefore { // a new payee waits for the owner's approval, whatever the rules allow
		judged = WithConnectPrepareOnly(judged)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InstantTransfer{}, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return InstantTransfer{}, err
	}
	// Read again under the agent's lock: the same key sent twice at once sends once, and the second answers the first.
	if t, found, err := s.instantTransferByKey(ctx, tx, workspaceID, agentID, key, in); found || err != nil {
		if err != nil {
			return InstantTransfer{}, err
		}
		_ = tx.Rollback(ctx)
		return s.driveInstantTransfer(ctx, t)
	}
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instant_transfers WHERE quote_id = $1)`, q.ID).Scan(&used); err != nil {
		return InstantTransfer{}, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	if used {
		return InstantTransfer{}, fmt.Errorf("%w: quote %s has been sent already, under another key", ErrQuote, q.ID)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM money_accounts WHERE id = $1 FOR UPDATE`, from); err != nil {
		return InstantTransfer{}, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	var balance int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(balance_minor), 0)::bigint FROM money_account_balances WHERE account_id = $1 AND funding = $2`,
		from, in.Funding).Scan(&balance); err != nil {
		return InstantTransfer{}, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	if balance < q.SendMinor {
		return InstantTransfer{}, fmt.Errorf("%w: the account holds %s of %s money, and this transfer sends %s", ErrAgentFunds,
			Money(balance, q.SendCurrency), in.Funding, Money(q.SendMinor, q.SendCurrency))
	}
	id := "itr_" + uuid.NewString()
	if err := enforceAgentRules(judged, tx, workspaceID, agentID, value, id); err != nil {
		return InstantTransfer{}, s.refusedMovement(ctx, tx, err)
	}
	revenue, err := routeAccount(ctx, tx, workspaceID, q.SendCurrency, MoneyRevenue, "", "Talyvor fees")
	if err != nil {
		return InstantTransfer{}, err
	}
	sold, err := routeAccount(ctx, tx, workspaceID, q.SendCurrency, MoneyPartner, "instant:convert", "Instant transfer conversions")
	if err != nil {
		return InstantTransfer{}, err
	}
	bought, err := routeAccount(ctx, tx, workspaceID, CurrencyUSDC, MoneyPartner, "instant:convert", "Instant transfer conversions")
	if err != nil {
		return InstantTransfer{}, err
	}
	held, err := routeAccount(ctx, tx, workspaceID, CurrencyUSDC, MoneyHold, "instant:transfer", "Instant transfers on their way")
	if err != nil {
		return InstantTransfer{}, err
	}
	var e MoneyEntry
	var convertEntry, payoutEntry string
	if direct {
		// Paid straight: the payout's entry is posted before the provider is asked, and reversed if it fails.
		paid, err := routeAccount(ctx, tx, workspaceID, q.SendCurrency, MoneyPartner, "instant:payout", "Instant transfer payouts")
		if err != nil {
			return InstantTransfer{}, err
		}
		if e, err = postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityPaymentsOut, Kind: "instant_transfer_payout",
			IdempotencyKey: id + ":" + InstantLegPayout, Funding: in.Funding, Counterparty: beneficiary,
			Memo: "Instant transfer " + id + " to " + beneficiary + ": " + Money(q.SendMinor, q.SendCurrency) + " paid out", Postings: []MoneyPosting{
				{AccountID: from, AmountMinor: -q.SendMinor},
				{AccountID: paid, AmountMinor: q.SendMinor}}}, s.screener); err != nil {
			return InstantTransfer{}, err
		}
		payoutEntry = e.ID
	} else {
		partner, err := s.moneyLayer.Partner(ctx, money.CapConvert, s.instantRoute(workspaceID, agentID, CapabilityFX))
		if err != nil {
			return InstantTransfer{}, fmt.Errorf("economy: Instant transfer: %w", err)
		}
		// The convert leg is posted before the provider is asked: one the ledger refuses never reaches it.
		if e, err = postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: CapabilityFX, Kind: "instant_transfer_convert",
			IdempotencyKey: id + ":" + InstantLegConvert, Funding: in.Funding, Counterparty: partner,
			Memo: "Instant transfer " + id + " to " + beneficiary + ": " + Money(q.SendMinor, q.SendCurrency) + " sent", Postings: []MoneyPosting{
				{AccountID: from, AmountMinor: -q.SendMinor},
				{AccountID: revenue, AmountMinor: q.FeeMinor},
				{AccountID: sold, AmountMinor: q.SendMinor - q.FeeMinor},
				{AccountID: bought, AmountMinor: -q.onchainMinor, Currency: CurrencyUSDC},
				{AccountID: held, AmountMinor: q.onchainMinor, Currency: CurrencyUSDC}}}, s.screener); err != nil {
			return InstantTransfer{}, err
		}
		convertEntry = e.ID
	}
	t, err := scanInstantTransfer(tx.QueryRow(ctx, `INSERT INTO instant_transfers (id, workspace_id, agent_id, quote_id, idempotency_key, funding,
		from_account_id, to_workspace_id, to_account_id, beneficiary_name, send_minor, send_currency, fee_minor, onchain_minor, receive_minor,
		receive_currency, rate, status, convert_entry_id, to_payee_id, to_network, to_address, beneficiary_country, payout_entry_id, arrives_within_seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
		RETURNING `+instantTransferCols, id, workspaceID, agentID, q.ID, key, in.Funding, from, to.workspace, in.ToAccountID, beneficiary, q.SendMinor,
		q.SendCurrency, q.FeeMinor, q.onchainMinor, q.ReceiveMinor, q.ReceiveCurrency, q.Rate, InstantSent, convertEntry, in.ToPayeeID, to.network, to.address,
		to.country, payoutEntry, q.ArrivesWithinSeconds))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // the key or the quote, used at the same moment by another send
			return InstantTransfer{}, ErrIdempotencyKeyReused
		}
		return InstantTransfer{}, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return InstantTransfer{}, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	s.monitorMoved(ctx, e)
	return s.driveInstantTransfer(ctx, t)
}

// instantSendable is whether q can be sent as an Instant transfer: converted and moved on chain, or — to a bank account
// in the currency sent — paid straight.
func instantSendable(q InstantQuote) bool {
	return (q.route == instantRoute && q.onchainMinor > 0) || (q.route == directRoute && q.To == InstantToBank)
}

// instantDestination is who an Instant transfer pays: a Talyvor account in workspace, or an outside wallet at address on
// network; payee is how the agent's rules judge it.
type instantDestination struct {
	workspace, name, country, network, address string
	payee                                      Payee
}

// instantAccount is the open Talyvor account accountID, which must hold currency: a fiat or hybrid beneficiary is paid
// in its currency, converted on arrival, and a stablecoin one keeps the USDC.
func (s *DualTokenStore) instantAccount(ctx context.Context, workspaceID, accountID, currency string) (instantDestination, error) {
	var d instantDestination
	var toCurrency string
	err := s.pool.QueryRow(ctx, `SELECT a.workspace_id, a.currency, COALESCE(NULLIF(w.name, ''), a.name) FROM money_accounts a
		LEFT JOIN workspaces w ON w.id = a.workspace_id WHERE a.id = $1 AND a.purpose IN ('company', 'agent') AND a.status = 'open'`, accountID).
		Scan(&d.workspace, &toCurrency, &d.name)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, fmt.Errorf("%w: no open Talyvor account %q to send to", ErrMoneyAccountNotFound, accountID)
	}
	if err != nil {
		return d, fmt.Errorf("economy: Instant transfer: %w", err)
	}
	if toCurrency != currency {
		return d, fmt.Errorf("%w: the account holds %s and the quote delivers %s", ErrInstantTransfer, toCurrency, currency)
	}
	if err := checkMoneySide(ctx, s.pool, d.workspace, "to_account_id", moneyKindOf(toCurrency)); err != nil {
		return d, err
	}
	d.payee = Payee{Kind: "company", ID: accountID, Name: d.name} // a Talyvor account, a company's or its agent's
	return d, nil
}

// ErrAddressScreened: an Instant transfer to a wallet address, or a beneficiary, that screening does not let through.
var ErrAddressScreened = errors.New("economy: the wallet address or its beneficiary did not pass screening, so nothing was sent")

// instantWallet is workspaceID's saved payee payeeID at a USDC address, payable and screened through the money layer's
// transfer hooks as the move leg will screen it — so a screened-out address is refused before any leg moves.
func (s *DualTokenStore) instantWallet(ctx context.Context, workspaceID, payeeID string) (instantDestination, error) {
	p, err := s.PayableOutsidePayee(ctx, workspaceID, payeeID)
	if err != nil {
		return instantDestination{}, err
	}
	if p.Address == "" {
		return instantDestination{}, fmt.Errorf("%w: %s is paid at a bank, not at a wallet address", ErrInstantTransfer, p.Name)
	}
	d := instantDestination{name: p.Name, country: p.Country, network: p.Network, address: p.Address,
		payee: Payee{Kind: "outside", ID: p.ID, Name: p.Name}}
	if err := s.moneyLayer.ScreenOnchainPayee(ctx, d.network, d.address, money.Party{Name: d.name, Country: d.country}); err != nil {
		if errors.Is(err, money.ErrRefused) || errors.Is(err, money.ErrHeld) {
			return d, fmt.Errorf("%w: %v", ErrAddressScreened, err)
		}
		return d, fmt.Errorf("economy: Instant transfer: screening %s: %w", d.address, err)
	}
	return d, nil
}

// instantBank is workspaceID's saved payee payeeID at a bank account in country, the country its quote priced the rail
// for, payable and its beneficiary screened through the money layer's transfer hooks as the payout leg will screen it —
// so a screened-out beneficiary is refused before any leg moves.
func (s *DualTokenStore) instantBank(ctx context.Context, workspaceID, payeeID, country string) (instantDestination, error) {
	p, err := s.PayableOutsidePayee(ctx, workspaceID, payeeID)
	if err != nil {
		return instantDestination{}, err
	}
	switch {
	case p.Address != "":
		return instantDestination{}, fmt.Errorf("%w: %s is paid at a wallet address, not at a bank", ErrInstantTransfer, p.Name)
	case !strings.EqualFold(p.Country, country):
		return instantDestination{}, fmt.Errorf("%w: %s banks in %s and the quote is for a bank account in %s", ErrInstantTransfer, p.Name, p.Country, country)
	}
	d := instantDestination{name: p.Name, country: strings.ToUpper(p.Country), payee: Payee{Kind: "outside", ID: p.ID, Name: p.Name}}
	if err := s.moneyLayer.ScreenPayoutPayee(ctx, money.Party{Name: d.name, Country: d.country}); err != nil {
		if errors.Is(err, money.ErrRefused) || errors.Is(err, money.ErrHeld) {
			return d, fmt.Errorf("%w: %v", ErrAddressScreened, err)
		}
		return d, fmt.Errorf("economy: Instant transfer: screening %s: %w", d.name, err)
	}
	return d, nil
}

// arrivesWithin is how a quote's arrival reads in the approval the owner confirms: nothing when it lands in seconds,
// otherwise the bank rail's real time.
func arrivesWithin(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds < 60:
		return fmt.Sprintf(", arrives within %d seconds", seconds)
	case seconds < 60*60:
		return fmt.Sprintf(", arrives within %d minutes", (seconds+59)/60)
	case seconds == 60*60:
		return ", arrives within 1 hour"
	default:
		return fmt.Sprintf(", arrives within %d hours", (seconds+60*60-1)/(60*60))
	}
}

// instantTransferByKey is workspaceID's transfer sent under key, if there is one: ErrIdempotencyKeyReused when it is not
// the same send.
func (s *DualTokenStore) instantTransferByKey(ctx context.Context, q pgxDB, workspaceID, agentID, key string, in InstantSend) (InstantTransfer, bool, error) {
	t, err := scanInstantTransfer(q.QueryRow(ctx, `SELECT `+instantTransferCols+` FROM instant_transfers WHERE workspace_id = $1 AND idempotency_key = $2`,
		workspaceID, key))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return t, false, nil
	case err != nil:
		return t, false, fmt.Errorf("economy: Instant transfer: %w", err)
	case t.AgentID != agentID || t.QuoteID != in.QuoteID || t.ToAccountID != in.ToAccountID || t.ToPayeeID != in.ToPayeeID || t.Funding != in.Funding:
		return t, false, ErrIdempotencyKeyReused
	}
	return t, true, nil
}

func (s *DualTokenStore) instantRoute(workspaceID, agentID, capability string) money.Route {
	return money.Route{Workspace: workspaceID, Agent: agentID, Capability: capability}
}

// errLegUnanswered is a leg whose outcome is not known: the transfer stays where it was, to be finished by a send again.
var errLegUnanswered = errors.New("economy: an Instant transfer leg was not answered")

// driveInstantTransfer takes t through its remaining legs, and reverses it when one fails.
func (s *DualTokenStore) driveInstantTransfer(ctx context.Context, t InstantTransfer) (InstantTransfer, error) {
	for {
		var step func(context.Context, InstantTransfer) (string, error)
		leg := ""
		switch t.Status {
		case InstantSent:
			step, leg = s.instantConvert, InstantLegConvert
			if t.direct() {
				step, leg = s.instantPayout, InstantLegPayout
			}
		case InstantConverted:
			step, leg = s.instantMove, InstantLegMove
		case InstantOnItsWay:
			step, leg = s.instantCredit, InstantLegCredit
			if t.toBank() {
				step, leg = s.instantPayout, InstantLegPayout
			}
		default:
			return t, nil
		}
		_, err := step(ctx, t)
		switch {
		case errors.Is(err, errLegUnanswered):
			return t, fmt.Errorf("%w (%s, the %s leg). Send again with the same key to finish it", err, t.ID, leg)
		case err != nil:
			if t, err = s.reverseInstantTransfer(ctx, t.ID, leg, err); err != nil {
				return t, err
			}
			return t, nil
		}
		// Read again: the leg moved it on, or another send of it did.
		if t, err = s.InstantTransfer(ctx, t.WorkspaceID, t.ID); err != nil {
			return t, err
		}
	}
}

// providerUnanswered sorts a provider's error: a refusal moved nothing, and anything else may have moved money.
func providerUnanswered(err error) error {
	for _, refused := range []error{money.ErrRefused, money.ErrInvalid, money.ErrNotFound, money.ErrKeyReused, money.ErrInsufficientFunds,
		money.ErrLimit, money.ErrApprovalNeeded, money.ErrHeld} {
		if errors.Is(err, refused) {
			return err
		}
	}
	return fmt.Errorf("%w: %v", errLegUnanswered, err)
}

// lockInstantStep locks t in tx when it is still at status; ok false when another send has moved it on, and the leg
// does nothing.
func lockInstantStep(ctx context.Context, tx pgx.Tx, t InstantTransfer, status string) (bool, error) {
	var now string
	if err := tx.QueryRow(ctx, `SELECT status FROM instant_transfers WHERE id = $1 FOR UPDATE`, t.ID).Scan(&now); err != nil {
		return false, fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	return now == status, nil
}

// instantConvert is the convert leg at the provider: what it sells is put in the transfer's account there and
// converted to USDC at the quote it locked. Its money entry was posted with the transfer.
func (s *DualTokenStore) instantConvert(ctx context.Context, t InstantTransfer) (string, error) {
	q, err := s.InstantTransferQuote(ctx, t.WorkspaceID, t.QuoteID)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if ok, err := lockInstantStep(ctx, tx, t, InstantSent); !ok || err != nil {
		return t.Status, err
	}
	rt := s.instantRoute(t.WorkspaceID, t.AgentID, CapabilityFX)
	sell := money.Amount{Minor: t.SendMinor - t.FeeMinor, Asset: t.SendCurrency}
	sent, err := s.instantHolding(ctx, rt, t.ID+":send", t.WorkspaceID, sell.Asset)
	if err != nil {
		return "", err
	}
	out, err := s.instantHolding(ctx, rt, t.ID+":usdc-out", t.WorkspaceID, money.USDC)
	if err != nil {
		return "", err
	}
	transfers, err := s.moneyLayer.Transfers(ctx, rt)
	if err != nil {
		return "", providerUnanswered(err)
	}
	if _, err := transfers.Credit(ctx, money.Credit{Key: t.ID + ":send", To: sent, Amount: sell, PartnerRef: t.ID}); err != nil {
		return "", providerUnanswered(err)
	}
	conv, err := s.moneyLayer.Convert(ctx, rt)
	if err != nil {
		return "", providerUnanswered(err)
	}
	r, err := conv.Convert(ctx, money.Conversion{Key: t.ID + ":" + InstantLegConvert, Transfer: t.ID, Quote: q.sellRef, From: sent, To: out})
	if err != nil {
		return "", providerUnanswered(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE instant_transfers SET status = $2, convert_ref = $3, updated_at = now() WHERE id = $1`,
		t.ID, InstantConverted, r.Ref); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	return InstantConverted, nil
}

// instantMove is the move leg: the USDC Talyvor holds for t goes on chain to the beneficiary.
func (s *DualTokenStore) instantMove(ctx context.Context, t InstantTransfer) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if ok, err := lockInstantStep(ctx, tx, t, InstantConverted); !ok || err != nil {
		return t.Status, err
	}
	held, err := routeAccount(ctx, tx, t.WorkspaceID, CurrencyUSDC, MoneyHold, "instant:transfer", "Instant transfers on their way")
	if err != nil {
		return "", err
	}
	chain, err := routeAccount(ctx, tx, t.WorkspaceID, CurrencyUSDC, MoneyPartner, "instant:onchain", "Instant transfers on chain")
	if err != nil {
		return "", err
	}
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: t.WorkspaceID, Capability: CapabilityStablecoins, Kind: "instant_transfer_move",
		IdempotencyKey: t.ID + ":" + InstantLegMove, Funding: t.Funding, Counterparty: t.BeneficiaryName,
		Memo: "Instant transfer " + t.ID + " to " + t.BeneficiaryName + ": on its way", Postings: []MoneyPosting{
			{AccountID: held, AmountMinor: -t.onchainMinor},
			{AccountID: chain, AmountMinor: t.onchainMinor}}}, s.screener)
	if err != nil {
		return "", err
	}
	var sender string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF((SELECT name FROM workspaces WHERE id = $1), ''), $1)`, t.WorkspaceID).Scan(&sender); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	rt := s.instantRoute(t.WorkspaceID, t.AgentID, CapabilityStablecoins)
	out, err := s.instantHolding(ctx, rt, t.ID+":usdc-out", t.WorkspaceID, money.USDC)
	if err != nil {
		return "", err
	}
	send := money.OnchainSend{Key: t.ID + ":" + InstantLegMove, Transfer: t.ID, From: out, Amount: money.Amount{Minor: t.onchainMinor, Asset: money.USDC},
		Originator: money.Party{Name: sender}, Beneficiary: money.Party{Name: t.BeneficiaryName, Country: t.beneficiaryCountry}}
	// To a Talyvor account the USDC goes to the beneficiary's side at the provider, and the credit leg lands it (to a bank
	// account, the payout leg pays it out); to an outside wallet it goes to the address, with both sides' details
	// attached, and that is it received.
	next := InstantOnItsWay
	if t.toWallet() {
		send.Network, send.Address, next = t.ToNetwork, t.ToAddress, InstantReceived
	} else if send.To, err = s.instantHolding(ctx, rt, t.ID+":usdc-in", t.BeneficiaryName, money.USDC); err != nil {
		return "", err
	}
	chainLeg, err := s.moneyLayer.Onchain(ctx, rt)
	if err != nil {
		return "", providerUnanswered(err)
	}
	r, err := chainLeg.Send(ctx, send)
	if err != nil {
		return "", providerUnanswered(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE instant_transfers SET status = $2, move_entry_id = $3, onchain_ref = $4, updated_at = now() WHERE id = $1`,
		t.ID, next, e.ID, r.Ref); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	s.monitorMoved(ctx, e)
	return next, nil
}

// instantCredit is the credit leg, in the beneficiary's workspace: the USDC arrives, and is converted to the account's
// currency at the quote t locked, or kept as USDC by an account that holds it.
func (s *DualTokenStore) instantCredit(ctx context.Context, t InstantTransfer) (string, error) {
	q, err := s.InstantTransferQuote(ctx, t.WorkspaceID, t.QuoteID)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if ok, err := lockInstantStep(ctx, tx, t, InstantOnItsWay); !ok || err != nil {
		return t.Status, err
	}
	var sender string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF((SELECT name FROM workspaces WHERE id = $1), ''), $1)`, t.WorkspaceID).Scan(&sender); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	chain, err := routeAccount(ctx, tx, t.toWorkspaceID, CurrencyUSDC, MoneyPartner, "instant:onchain", "Instant transfers on chain")
	if err != nil {
		return "", err
	}
	capability, postings := CapabilityStablecoins, []MoneyPosting{{AccountID: chain, AmountMinor: -t.onchainMinor}}
	if t.ReceiveCurrency == CurrencyUSDC {
		postings = append(postings, MoneyPosting{AccountID: t.ToAccountID, AmountMinor: t.onchainMinor, Currency: CurrencyUSDC})
	} else {
		sold, err := routeAccount(ctx, tx, t.toWorkspaceID, CurrencyUSDC, MoneyPartner, "instant:convert", "Instant transfer conversions")
		if err != nil {
			return "", err
		}
		bought, err := routeAccount(ctx, tx, t.toWorkspaceID, t.ReceiveCurrency, MoneyPartner, "instant:convert", "Instant transfer conversions")
		if err != nil {
			return "", err
		}
		capability, postings = CapabilityFX, append(postings,
			MoneyPosting{AccountID: sold, AmountMinor: t.onchainMinor, Currency: CurrencyUSDC},
			MoneyPosting{AccountID: bought, AmountMinor: -t.ReceiveMinor, Currency: t.ReceiveCurrency},
			MoneyPosting{AccountID: t.ToAccountID, AmountMinor: t.ReceiveMinor, Currency: t.ReceiveCurrency})
	}
	e, err := postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: t.toWorkspaceID, Capability: capability, Kind: "instant_transfer_credit",
		IdempotencyKey: t.ID + ":" + InstantLegCredit, Funding: t.Funding, Counterparty: sender,
		Memo: "Instant transfer from " + sender + ": " + Money(t.ReceiveMinor, t.ReceiveCurrency) + " received", Postings: postings}, s.screener)
	if err != nil {
		return "", err
	}
	ref := ""
	if t.ReceiveCurrency != CurrencyUSDC {
		rt := s.instantRoute(t.WorkspaceID, t.AgentID, CapabilityFX)
		in, err := s.instantHolding(ctx, rt, t.ID+":usdc-in", t.BeneficiaryName, money.USDC)
		if err != nil {
			return "", err
		}
		got, err := s.instantHolding(ctx, rt, t.ID+":receive", t.BeneficiaryName, t.ReceiveCurrency)
		if err != nil {
			return "", err
		}
		conv, err := s.moneyLayer.Convert(ctx, rt)
		if err != nil {
			return "", providerUnanswered(err)
		}
		r, err := conv.Convert(ctx, money.Conversion{Key: t.ID + ":" + InstantLegCredit, Transfer: t.ID, Quote: q.buyRef, From: in, To: got})
		if err != nil {
			return "", providerUnanswered(err)
		}
		ref = r.Ref
	}
	if _, err := tx.Exec(ctx, `UPDATE instant_transfers SET status = $2, credit_entry_id = $3, credit_ref = $4, updated_at = now() WHERE id = $1`,
		t.ID, InstantReceived, e.ID, ref); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	s.monitorMoved(ctx, e)
	return InstantReceived, nil
}

// instantPayout is the payout leg, to a saved payee's bank account: the USDC that arrived at the payout's side is
// converted to the currency the bank is paid in, at the quote t locked, and the payout provider pays it in. A transfer
// paid straight pays what was sent, its entry posted with the transfer. A payee that can no longer be paid — its
// details changed, or no longer confirmed — refuses the leg, and the transfer is reversed.
func (s *DualTokenStore) instantPayout(ctx context.Context, t InstantTransfer) (string, error) {
	at := InstantOnItsWay
	if t.direct() {
		at = InstantSent
	}
	payee, err := s.PayableOutsidePayee(ctx, t.WorkspaceID, t.ToPayeeID)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if ok, err := lockInstantStep(ctx, tx, t, at); !ok || err != nil {
		return t.Status, err
	}
	var sender string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF((SELECT name FROM workspaces WHERE id = $1), ''), $1)`, t.WorkspaceID).Scan(&sender); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	rt := money.Route{Workspace: t.WorkspaceID, Agent: t.AgentID, Country: t.beneficiaryCountry, Capability: CapabilityPaymentsOut}
	var e MoneyEntry
	var from, convertRef string
	if t.direct() {
		if from, err = s.instantHolding(ctx, rt, t.ID+":send", t.WorkspaceID, t.SendCurrency); err != nil {
			return "", err
		}
		transfers, err := s.moneyLayer.Transfers(ctx, rt)
		if err != nil {
			return "", providerUnanswered(err)
		}
		if _, err := transfers.Credit(ctx, money.Credit{Key: t.ID + ":send", To: from, Amount: money.Amount{Minor: t.SendMinor, Asset: t.SendCurrency}, PartnerRef: t.ID}); err != nil {
			return "", providerUnanswered(err)
		}
	} else {
		q, err := s.InstantTransferQuote(ctx, t.WorkspaceID, t.QuoteID)
		if err != nil {
			return "", err
		}
		chain, err := routeAccount(ctx, tx, t.WorkspaceID, CurrencyUSDC, MoneyPartner, "instant:onchain", "Instant transfers on chain")
		if err != nil {
			return "", err
		}
		sold, err := routeAccount(ctx, tx, t.WorkspaceID, CurrencyUSDC, MoneyPartner, "instant:convert", "Instant transfer conversions")
		if err != nil {
			return "", err
		}
		bought, err := routeAccount(ctx, tx, t.WorkspaceID, t.ReceiveCurrency, MoneyPartner, "instant:convert", "Instant transfer conversions")
		if err != nil {
			return "", err
		}
		paid, err := routeAccount(ctx, tx, t.WorkspaceID, t.ReceiveCurrency, MoneyPartner, "instant:payout", "Instant transfer payouts")
		if err != nil {
			return "", err
		}
		if e, err = postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: t.WorkspaceID, Capability: CapabilityPaymentsOut, Kind: "instant_transfer_payout",
			IdempotencyKey: t.ID + ":" + InstantLegPayout, Funding: t.Funding, Counterparty: t.BeneficiaryName,
			Memo: "Instant transfer " + t.ID + " to " + t.BeneficiaryName + ": " + Money(t.ReceiveMinor, t.ReceiveCurrency) + " paid out", Postings: []MoneyPosting{
				{AccountID: chain, AmountMinor: -t.onchainMinor, Currency: CurrencyUSDC},
				{AccountID: sold, AmountMinor: t.onchainMinor, Currency: CurrencyUSDC},
				{AccountID: bought, AmountMinor: -t.ReceiveMinor, Currency: t.ReceiveCurrency},
				{AccountID: paid, AmountMinor: t.ReceiveMinor, Currency: t.ReceiveCurrency}}}, s.screener); err != nil {
			return "", err
		}
		in, err := s.instantHolding(ctx, rt, t.ID+":usdc-in", t.BeneficiaryName, money.USDC)
		if err != nil {
			return "", err
		}
		if from, err = s.instantHolding(ctx, rt, t.ID+":receive", t.BeneficiaryName, t.ReceiveCurrency); err != nil {
			return "", err
		}
		conv, err := s.moneyLayer.Convert(ctx, rt)
		if err != nil {
			return "", providerUnanswered(err)
		}
		r, err := conv.Convert(ctx, money.Conversion{Key: t.ID + ":" + InstantLegPayout + ":convert", Transfer: t.ID, Quote: q.buyRef, From: in, To: from})
		if err != nil {
			return "", providerUnanswered(err)
		}
		convertRef = r.Ref
	}
	payouts, err := s.moneyLayer.BankPayouts(ctx, rt)
	if err != nil {
		return "", providerUnanswered(err)
	}
	r, err := payouts.Pay(ctx, money.BankPayout{Key: t.ID + ":" + InstantLegPayout, Transfer: t.ID, From: from, Country: strings.ToUpper(payee.Country),
		Amount: money.Amount{Minor: t.ReceiveMinor, Asset: t.ReceiveCurrency}, Reference: sender,
		To: money.Payee{Name: t.BeneficiaryName, IBAN: payee.IBAN, BIC: payee.BIC, SortCode: payee.SortCode, AccountNumber: payee.AccountNumber,
			RoutingNumber: payee.RoutingNumber}})
	if err != nil {
		return "", providerUnanswered(err)
	}
	entry := t.payoutEntry
	if e.ID != "" {
		entry = e.ID
	}
	if _, err := tx.Exec(ctx, `UPDATE instant_transfers SET status = $2, payout_entry_id = $3, payout_convert_ref = $4, payout_ref = $5,
		updated_at = now() WHERE id = $1`, t.ID, InstantReceived, entry, convertRef, r.Ref); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("%w: %v", errLegUnanswered, err)
	}
	if e.ID != "" {
		s.monitorMoved(ctx, e)
	}
	return InstantReceived, nil
}

// instantHolding is the provider's account for one side of a transfer, opened once under key.
func (s *DualTokenStore) instantHolding(ctx context.Context, rt money.Route, key, holder, asset string) (string, error) {
	accounts, err := s.moneyLayer.Accounts(ctx, rt)
	if err != nil {
		return "", providerUnanswered(err)
	}
	a, err := accounts.Open(ctx, money.OpenAccount{Key: key, Holder: holder, Asset: asset})
	if err != nil {
		return "", providerUnanswered(err)
	}
	return a.Ref, nil
}

// reverseInstantTransfer reverses every leg of transfer id that moved money, latest first, at the figures it moved at,
// and ends it reversed because its leg failed with cause.
func (s *DualTokenStore) reverseInstantTransfer(ctx context.Context, id, leg string, cause error) (InstantTransfer, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InstantTransfer{}, fmt.Errorf("economy: reverse Instant transfer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := scanInstantTransfer(tx.QueryRow(ctx, `SELECT `+instantTransferCols+` FROM instant_transfers WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return t, fmt.Errorf("economy: reverse Instant transfer: %w", err)
	}
	if t.Status == InstantReversed || t.Status == InstantReceived {
		return t, nil
	}
	var reversals []string
	for _, l := range []struct{ leg, entry string }{{InstantLegPayout, t.payoutEntry}, {InstantLegMove, t.moveEntry}, {InstantLegConvert, t.convertEntry}} {
		if l.entry == "" {
			continue
		}
		e, err := reverseMoneyEntry(ctx, tx, t.WorkspaceID, l.entry, t.ID+":"+l.leg+":reversal", "Instant transfer "+t.ID+" reversed: the "+leg+" leg failed")
		if err != nil {
			return t, err
		}
		reversals = append(reversals, e.ID)
	}
	t, err = scanInstantTransfer(tx.QueryRow(ctx, `UPDATE instant_transfers SET status = $2, failed_leg = $3, failure = $4, reversal_entry_ids = $5,
		updated_at = now() WHERE id = $1 RETURNING `+instantTransferCols, id, InstantReversed, leg, cause.Error(), reversals))
	if err != nil {
		return t, fmt.Errorf("economy: reverse Instant transfer: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return t, fmt.Errorf("economy: reverse Instant transfer: %w", err)
	}
	return t, nil
}

// reverseMoneyEntry posts entryID's postings back, each the negative of what it moved, under key.
func reverseMoneyEntry(ctx context.Context, tx pgx.Tx, workspaceID, entryID, key, memo string) (MoneyEntry, error) {
	e, err := readMoneyEntry(ctx, tx, `e.id = $1 AND e.workspace_id = $2`, entryID, workspaceID)
	if err != nil {
		return MoneyEntry{}, fmt.Errorf("economy: reverse %s: %w", entryID, err)
	}
	back := make([]MoneyPosting, 0, len(e.Postings))
	for _, p := range e.Postings {
		back = append(back, MoneyPosting{AccountID: p.AccountID, AmountMinor: -p.AmountMinor, Currency: p.Currency})
	}
	// Screened as the entry was, the other way: the money comes back from whom it went to.
	return postMoneyTx(ctx, tx, MoneyEntry{WorkspaceID: workspaceID, Capability: e.Capability, Kind: e.Kind + "_reversal", IdempotencyKey: key,
		Funding: e.Funding, Counterparty: e.Counterparty, PartnerRef: e.PartnerRef, Memo: memo, Postings: back}, screenedBeforeLocks{})
}

// heldInstantTransfer is whether op is the money layer moving an Instant transfer's USDC on chain that
// SendInstantTransfer has already judged by the agent's rules and the owner's approval, and whose money it holds: the
// move leg of a converted transfer, of exactly what it carries, from that agent, to its own beneficiary.
func (s *DualTokenStore) heldInstantTransfer(ctx context.Context, op money.Op) (bool, error) {
	if op.Capability == money.CapBankPayout {
		return s.heldInstantPayout(ctx, op)
	}
	id, ok := strings.CutSuffix(op.Key, ":"+InstantLegMove)
	if op.Capability != money.CapOnchain || op.Method != "send" || !ok || !strings.HasPrefix(id, "itr_") || op.Amount.Asset != money.USDC {
		return false, nil
	}
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instant_transfers WHERE id = $1 AND workspace_id = $2 AND agent_id = $3
		AND status = $4 AND onchain_minor = $5 AND beneficiary_name = $6)`,
		id, op.Workspace, op.Agent, InstantConverted, op.Amount.Minor, op.Payee).Scan(&held)
	return held, err
}

// heldInstantPayout is whether op is the money layer paying an Instant transfer into its saved payee's bank account
// (B59.17), judged when it was sent: the payout leg of a transfer that has reached it — on its way, or sent when it is
// paid straight — of exactly what it delivers, from that agent, to its own beneficiary.
func (s *DualTokenStore) heldInstantPayout(ctx context.Context, op money.Op) (bool, error) {
	id, ok := strings.CutSuffix(op.Key, ":"+InstantLegPayout)
	if op.Method != "pay" || !ok || !strings.HasPrefix(id, "itr_") {
		return false, nil
	}
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instant_transfers WHERE id = $1 AND workspace_id = $2 AND agent_id = $3
		AND to_payee_id <> '' AND to_address = '' AND status = CASE WHEN onchain_minor = 0 THEN $4 ELSE $5 END
		AND receive_minor = $6 AND receive_currency = $7 AND beneficiary_name = $8)`,
		id, op.Workspace, op.Agent, InstantSent, InstantOnItsWay, op.Amount.Minor, op.Amount.Asset, op.Payee).Scan(&held)
	return held, err
}
