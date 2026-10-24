package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// payment_requests.go — B40.3: PREPARE, NEVER PAY.
//
// An assistant or an agent prepares a payment request — one payment to a saved payee, a batch of them, or a purchase
// of a marketplace listing — and nothing here moves money. Payees come only from saved payees (B30.16) and listings;
// bank details in the arguments are refused (RefuseAssistantPayeeDetails). Each item is judged: a payee that cannot be
// paid, or one the agent's rules refuse, is blocked with the reason; one a signed permission mandate (B40.2) allows is
// auto; anything else needs the person's approval — and a request by voice always does, the evaluator says so. The
// canonical payload is frozen and hashed; the person approves that hash (B40.5), which also pays the auto items.
// An Idempotency-Key is required: the same key and arguments give the same request, other arguments are refused.
// Class: rules_approvals_statements_pots, GREEN — it records a request and moves nothing.

var (
	// ErrPaymentRequest: arguments a request cannot have.
	ErrPaymentRequest = errors.New("economy: invalid payment request")
	// ErrPaymentRequestNotFound: no such request in this workspace, or not the caller's.
	ErrPaymentRequestNotFound = errors.New("economy: no such payment request")
	// ErrPaymentRequestConflict: the Idempotency-Key was used before with other arguments.
	ErrPaymentRequestConflict = errors.New("economy: this Idempotency-Key was already used for a different request")
)

// PaymentRequestVersion names the canonical form frozen.
const PaymentRequestVersion = "talyvor.payment_request.v1"

// The approval page and the app's deep link a request names (B40.7 builds the page).
const (
	ApprovalURLBase = "https://app.talyvor.com/approve/"
	ApprovalDeepURL = "talyvor://approve/"
)

// The decisions an item can have.
const (
	ItemAuto          = "auto"
	ItemNeedsApproval = "needs_approval"
	ItemBlocked       = "blocked"
)

// assistantPayeeDetails are the argument names that would carry bank details.
var assistantPayeeDetails = []string{"sort_code", "account_number", "iban", "bic", "swift", "routing_number", "bank_account",
	"bank_details", "account_details", "account_name"}

// RefuseAssistantPayeeDetails refuses arguments that carry bank details anywhere in them: a payee is paid only as
// saved, never as the assistant describes it.
func RefuseAssistantPayeeDetails(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("%w: the arguments are not JSON", ErrPaymentRequest)
	}
	var walk func(any) string
	walk = func(v any) string {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if slices.Contains(assistantPayeeDetails, strings.ToLower(k)) {
					return k
				}
				if f := walk(x); f != "" {
					return f
				}
			}
		case []any:
			for _, x := range t {
				if f := walk(x); f != "" {
					return f
				}
			}
		}
		return ""
	}
	if f := walk(v); f != "" {
		return fmt.Errorf("%w: payee details supplied by the assistant (%s): pay a saved payee by its payee_id", ErrPaymentRequest, f)
	}
	return nil
}

// PaymentRequestItemIn is one item as asked for: a saved payee with an amount, or a marketplace listing at its price.
type PaymentRequestItemIn struct {
	PayeeID     string `json:"payee_id,omitempty"`
	ListingID   string `json:"listing_id,omitempty"`
	Currency    string `json:"currency,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	Reference   string `json:"reference,omitempty"`
	// B41.102: a bill the workspace received, paid to its saved payee at its amount, its invoice number the reference.
	BillID string `json:"bill_id,omitempty"`
}

// PaymentRequestIn is what is prepared, and by whom: Client as the mandates name it ("chat", "voice", "mcp:<id>",
// "key:<api key id>"), AgentID the agent whose key asks ("" for the person).
type PaymentRequestIn struct {
	Client  string                 `json:"client"`
	AgentID string                 `json:"agent_id"`
	Items   []PaymentRequestItemIn `json:"items"`
	// B45.13: a recurring plan's first payment to a payee, which waits for approval even inside the mandate.
	FirstPayment bool `json:"-"`
}

// PaymentRequestItem is one frozen item and how it was judged.
type PaymentRequestItem struct {
	Position    int    `json:"position"`
	PayeeID     string `json:"payee_id"`
	PayeeName   string `json:"payee_name"`
	ListingID   string `json:"listing_id,omitempty"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	Reference   string `json:"reference"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason"`
	Rule        string `json:"rule,omitempty"`
	MandateID   string `json:"mandate_id,omitempty"`
	BillID      string `json:"bill_id,omitempty"` // B41.102
	// B41.103: the risks found on it, for the approval card.
	Flags []ItemFlag `json:"flags"`
	// B40.5: once paying it was tried, the payment out that paid it, where it stands, and under what authority:
	// "approval" or "mandate:<id>".
	PaymentID string `json:"payment_id,omitempty"`
	PayStatus string `json:"pay_status,omitempty"`
	PayDetail string `json:"pay_detail,omitempty"`
	Authority string `json:"authority,omitempty"`
}

// PaymentRequest is a prepared request as kept.
type PaymentRequest struct {
	ID          string               `json:"id"`
	Kind        string               `json:"kind"`
	Status      string               `json:"status"`
	Summary     string               `json:"summary"`
	Client      string               `json:"client"`
	AgentID     string               `json:"agent_id,omitempty"`
	Canonical   string               `json:"canonical"`
	PayloadHash string               `json:"payload_hash"`
	ApprovalURL string               `json:"approval_url"`
	DeepLink    string               `json:"deep_link"`
	Totals      map[string]int64     `json:"totals"`
	Items       []PaymentRequestItem `json:"items"`
	CreatedAt   time.Time            `json:"created_at"`
	CancelledAt *time.Time           `json:"cancelled_at,omitempty"`
}

// PaymentRequestClient is who a request is judged as: an agent's key always as itself, "key:<its id>"; the person as
// the client they name — "chat", "voice" or "mcp:<client id>" — or else as the key they call with.
func PaymentRequestClient(apiKeyID, agentID, named string) (string, error) {
	if agentID != "" {
		return "key:" + apiKeyID, nil
	}
	if named = strings.TrimSpace(named); named == "" && apiKeyID != "" {
		named = "key:" + apiKeyID
	}
	ok := named == ClientChat || named == ClientVoice || ((strings.HasPrefix(named, "mcp:") || strings.HasPrefix(named, "key:")) && len(named) > 4)
	if !ok {
		return "", fmt.Errorf(`%w: name the client the request comes from: "chat", "voice" or "mcp:<client id>"`, ErrPaymentRequest)
	}
	return named, nil
}

// canonicalItem is what is frozen of an item.
type canonicalItem struct {
	PayeeID     string `json:"payee_id"`
	ListingID   string `json:"listing_id"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	Reference   string `json:"reference"`
	BillID      string `json:"bill_id,omitempty"` // omitted when empty, so requests frozen before B41.102 hash as they did
}

type canonicalRequest struct {
	Version     string          `json:"version"`
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	Kind        string          `json:"kind"`
	Items       []canonicalItem `json:"items"`
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// normalise checks the arguments and puts them in the form their hash is taken of.
func (in *PaymentRequestIn) normalise() (kind string, err error) {
	bad := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("%w: %s", ErrPaymentRequest, fmt.Sprintf(format, args...))
	}
	if len(in.Items) == 0 {
		return bad("name at least one item")
	}
	listings := 0
	bills := map[string]bool{}
	for i := range in.Items {
		it := &in.Items[i]
		it.PayeeID, it.ListingID, it.Reference = strings.TrimSpace(it.PayeeID), strings.TrimSpace(it.ListingID), strings.TrimSpace(it.Reference)
		it.BillID = strings.TrimSpace(it.BillID)
		it.Currency = strings.ToUpper(strings.TrimSpace(it.Currency))
		if len(it.Reference) > 140 {
			return bad("item %d: a reference is up to 140 characters", i+1)
		}
		switch {
		case it.BillID != "":
			if in.AgentID != "" {
				return bad("item %d: the workspace's bills are paid by its owner or an admin, not an agent's key", i+1)
			}
			if it.PayeeID != "" || it.ListingID != "" || it.Currency != "" || it.AmountMinor != 0 || it.Reference != "" {
				return bad("item %d: a bill is paid as received: name the bill_id alone", i+1)
			}
			if bills[it.BillID] {
				return bad("item %d: bill %s is named twice", i+1, it.BillID)
			}
			bills[it.BillID] = true
		case it.ListingID != "":
			if it.PayeeID != "" || it.Currency != "" || it.AmountMinor != 0 {
				return bad("item %d: a listing is bought at its own price: name the listing_id alone", i+1)
			}
			listings++
		case it.PayeeID == "":
			return bad("item %d: name a saved payee's payee_id or a listing_id", i+1)
		case it.AmountMinor <= 0:
			return bad("item %d: amount_minor must be more than 0", i+1)
		default:
			if _, ok := MoneyCurrencies[it.Currency]; !ok {
				return bad("item %d: %q is not a currency Talyvor holds", i+1, it.Currency)
			}
		}
	}
	switch {
	case len(in.Items) > 1:
		return "batch", nil
	case listings == 1:
		return "marketplace_purchase", nil
	}
	return "payment", nil
}

// PreparePaymentRequest prepares in for workspaceID under idempotencyKey, by `by`. The same key and arguments return
// the request kept the first time (again = true); the same key with other arguments is ErrPaymentRequestConflict.
func (s *DualTokenStore) PreparePaymentRequest(ctx context.Context, workspaceID, by, idempotencyKey string, in PaymentRequestIn) (r PaymentRequest, again bool, err error) {
	if idempotencyKey = strings.TrimSpace(idempotencyKey); idempotencyKey == "" {
		return r, false, fmt.Errorf("%w: an Idempotency-Key is required", ErrPaymentRequest)
	}
	// B45.13: "schedule:" keys are a recurring plan's ticks', so no caller can take a tick's key before it runs.
	if strings.HasPrefix(idempotencyKey, ClientSchedulePrefix) != strings.HasPrefix(in.Client, ClientSchedulePrefix) {
		return r, false, fmt.Errorf("%w: an Idempotency-Key starting %q is reserved for recurring payments", ErrPaymentRequest, ClientSchedulePrefix)
	}
	kind, err := in.normalise()
	if err != nil {
		return r, false, err
	}
	args, _ := json.Marshal(in)
	argsHash := sha256Hex(string(args))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return r, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One preparer at a time in a workspace: the key is checked, and a mandate's auto spend counted, consistently.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('payment_requests:' || $1))`, workspaceID); err != nil {
		return r, false, fmt.Errorf("economy: payment request lock: %w", err)
	}
	var id, kept string
	err = tx.QueryRow(ctx, `SELECT id, args_hash FROM payment_requests WHERE workspace_id = $1 AND idempotency_key = $2`,
		workspaceID, idempotencyKey).Scan(&id, &kept)
	switch {
	case err == nil && kept != argsHash:
		return r, false, ErrPaymentRequestConflict
	case err == nil:
		r, err = s.PaymentRequest(ctx, workspaceID, "", id)
		return r, true, err
	case !errors.Is(err, pgx.ErrNoRows):
		return r, false, fmt.Errorf("economy: payment request: %w", err)
	}

	id = "preq_" + uuid.NewString()
	items, err := s.judgePaymentItems(ctx, tx, workspaceID, in)
	if err != nil {
		return r, false, err
	}
	c := canonicalRequest{Version: PaymentRequestVersion, ID: id, WorkspaceID: workspaceID, Kind: kind, Items: []canonicalItem{}}
	status := ItemBlocked
	for _, it := range items {
		c.Items = append(c.Items, canonicalItem{it.PayeeID, it.ListingID, it.Currency, it.AmountMinor, it.Reference, it.BillID})
		switch {
		case it.Decision == ItemNeedsApproval:
			status = "pending_approval"
		case it.Decision == ItemAuto && status == ItemBlocked:
			status = ItemAuto
		}
	}
	canonical, _ := json.Marshal(c)
	if _, err := tx.Exec(ctx, `INSERT INTO payment_requests (id, workspace_id, idempotency_key, args_hash, kind, client, agent_id,
		canonical, payload_hash, status, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		id, workspaceID, idempotencyKey, argsHash, kind, in.Client, in.AgentID, string(canonical), sha256Hex(string(canonical)), status, by); err != nil {
		return r, false, fmt.Errorf("economy: payment request: %w", err)
	}
	for _, it := range items {
		flags, _ := json.Marshal(it.Flags)
		if _, err := tx.Exec(ctx, `INSERT INTO payment_request_items (request_id, position, payee_id, payee_name, listing_id, currency,
			amount_minor, reference, decision, reason, rule, mandate_id, bill_id, flags) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::jsonb)`,
			id, it.Position, it.PayeeID, it.PayeeName, it.ListingID, it.Currency, it.AmountMinor, it.Reference, it.Decision, it.Reason, it.Rule, it.MandateID, it.BillID, string(flags)); err != nil {
			return r, false, fmt.Errorf("economy: payment request item: %w", err)
		}
	}
	// B40.5: the request and how each item was judged are the first links of its record.
	if err := appendAudit(ctx, tx, workspaceID, id, "request", map[string]any{"by": by, "client": in.Client, "agent_id": in.AgentID,
		"kind": kind, "canonical": string(canonical), "payload_hash": sha256Hex(string(canonical))}); err != nil {
		return r, false, err
	}
	decisions := []map[string]any{}
	for _, it := range items {
		decisions = append(decisions, map[string]any{"position": it.Position, "decision": it.Decision, "reason": it.Reason, "rule": it.Rule, "mandate_id": it.MandateID,
			"flags": it.Flags})
	}
	if err := appendAudit(ctx, tx, workspaceID, id, "decision", map[string]any{"items": decisions}); err != nil {
		return r, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, false, err
	}
	r, err = s.PaymentRequest(ctx, workspaceID, "", id)
	if err == nil {
		s.notifyPaymentRequest(workspaceID, r) // B40.15: a retry with the same key returned above and tells nobody again
	}
	return r, false, err
}

// judgePaymentItems resolves each item's payee and judges it, in order: blocked when it cannot be paid or the agent's
// rules refuse it, auto when a mandate allows it, otherwise needs approval with the closest mandate's reason.
func (s *DualTokenStore) judgePaymentItems(ctx context.Context, tx pgx.Tx, workspaceID string, in PaymentRequestIn) ([]PaymentRequestItem, error) {
	mandates, err := s.PermissionMandates(ctx, workspaceID, in.AgentID)
	if err != nil {
		return nil, err
	}
	var rules *AgentRules
	paused := ""
	if in.AgentID != "" {
		if err := refuseIfPaused(ctx, tx, in.AgentID); err != nil {
			paused = err.Error()
		}
		r, err := agentRulesInForce(ctx, tx, in.AgentID, time.Now())
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		rules = &r
	}
	batchTotal := map[string]int64{}
	out := make([]PaymentRequestItem, len(in.Items))
	resolved := make([]PreparedPayment, len(in.Items))
	for i, x := range in.Items {
		it := PaymentRequestItem{Position: i + 1, PayeeID: x.PayeeID, ListingID: x.ListingID, Currency: x.Currency, AmountMinor: x.AmountMinor,
			Reference: x.Reference, BillID: x.BillID, Flags: []ItemFlag{}}
		p := PreparedPayment{Client: in.Client, AgentID: in.AgentID}
		var blocked string
		if x.BillID != "" {
			blocked, err = s.resolveBill(ctx, tx, workspaceID, &it, &p)
		} else if x.ListingID != "" {
			blocked, err = s.resolveListing(ctx, workspaceID, &it, &p)
		} else if blocked, err = s.resolvePayee(ctx, workspaceID, &it, &p); err == nil && blocked == "" {
			err = s.riskFlags(ctx, workspaceID, &it, it.Reference, p.NewPayee)
		}
		if err != nil {
			return nil, err
		}
		if blocked == "" && paused != "" {
			blocked = paused
		}
		if blocked == "" && rules != nil {
			ids := []string{it.PayeeID, it.ListingID}
			switch {
			case slices.ContainsFunc(ids, func(id string) bool { return id != "" && slices.Contains(rules.BlockedPayees, id) }):
				blocked = "the agent's rules block this payee"
			case len(rules.AllowedPayees) > 0 && !slices.ContainsFunc(ids, func(id string) bool { return id != "" && slices.Contains(rules.AllowedPayees, id) }):
				blocked = "the agent's rules do not name this payee among those it may pay"
			}
		}
		if blocked != "" {
			it.Decision, it.Reason, it.Rule = ItemBlocked, blocked, "payee"
		} else {
			batchTotal[it.Currency] += it.AmountMinor
		}
		out[i], resolved[i] = it, p
	}
	// B41.103: an item repeating an earlier one of this request — its payee's invoice number or amount — is a duplicate.
	for i := range out {
		it := &out[i]
		if it.Decision == ItemBlocked || it.ListingID != "" || slices.ContainsFunc(it.Flags, func(f ItemFlag) bool { return f.Code == FlagDuplicate }) {
			continue
		}
		for _, e := range out[:i] {
			if e.Decision != ItemBlocked && e.ListingID == "" && e.PayeeID == it.PayeeID && ((it.Reference != "" && strings.EqualFold(e.Reference, it.Reference)) ||
				(e.AmountMinor == it.AmountMinor && e.Currency == it.Currency)) {
				it.Flags = append(it.Flags, ItemFlag{FlagDuplicate, fmt.Sprintf("item %d of this request pays %s %s too (reference %q)",
					e.Position, it.PayeeName, formatMinor(e.Currency, e.AmountMinor), e.Reference)})
				break
			}
		}
	}

	// Auto items so far count towards each mandate's limits, as earlier requests' auto items do.
	type spentKey struct{ mandate, currency string }
	type spent struct {
		day, month int64
		lastHour   int
	}
	counted := map[spentKey]*spent{}
	spentUnder := func(mandateID, currency string) (*spent, error) {
		k := spentKey{mandateID, currency}
		if sp, ok := counted[k]; ok {
			return sp, nil
		}
		sp := &spent{}
		err := tx.QueryRow(ctx, `SELECT
			COALESCE(sum(i.amount_minor) FILTER (WHERE i.created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'), 0),
			COALESCE(sum(i.amount_minor) FILTER (WHERE i.created_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'), 0),
			count(*) FILTER (WHERE i.created_at >= now() - interval '1 hour')
			FROM payment_request_items i JOIN payment_requests r ON r.id = i.request_id
			WHERE i.mandate_id = $1 AND i.currency = $2 AND i.decision = 'auto' AND r.workspace_id = $3 AND r.status <> 'cancelled'
			AND i.created_at >= now() - interval '32 days'`,
			mandateID, currency, workspaceID).Scan(&sp.day, &sp.month, &sp.lastHour)
		if err != nil {
			return nil, fmt.Errorf("economy: spent under mandate: %w", err)
		}
		counted[k] = sp
		return sp, nil
	}
	for i := range out {
		it := &out[i]
		if it.Decision == ItemBlocked {
			continue
		}
		// B41.103: a duplicate or a changed bank detail is never paid under a mandate.
		if codes := reasonFlags(it.Flags); len(codes) > 0 {
			it.Decision, it.Reason, it.Rule = ItemNeedsApproval, "flagged "+strings.Join(codes, " and ")+": a person approves it with a passkey and a reason", "risk"
			continue
		}
		p := resolved[i]
		p.Currency, p.AmountMinor, p.BatchTotalMinor = it.Currency, it.AmountMinor, batchTotal[it.Currency]
		var closest *MandateDecision
		for _, m := range mandates {
			if m.Status != "active" {
				continue
			}
			sp, err := spentUnder(m.ID, it.Currency)
			if err != nil {
				return nil, err
			}
			p.SpentTodayMinor, p.SpentMonthMinor, p.PaymentsLastHour = sp.day, sp.month, sp.lastHour
			d, err := s.EvaluateMandatePayment(ctx, workspaceID, m.ID, p)
			if err != nil {
				return nil, err
			}
			if d.Decision == MandateAllow {
				sp.day, sp.month, sp.lastHour = sp.day+it.AmountMinor, sp.month+it.AmountMinor, sp.lastHour+1
				closest = &d
				break
			}
			if closest == nil || (closest.Decision == MandateBlock && d.Decision == MandateNeedsApproval) {
				closest = &d
			}
		}
		switch {
		case closest == nil:
			it.Decision, it.Reason = ItemNeedsApproval, "no permission mandate the person signed covers this payment"
		case closest.Decision == MandateAllow && in.FirstPayment:
			it.Decision, it.Reason, it.Rule, it.MandateID = ItemNeedsApproval, "the first payment of a recurring payment waits for approval", "first_payment", closest.MandateID
		case closest.Decision == MandateAllow:
			it.Decision, it.Reason, it.Rule, it.MandateID = ItemAuto, "inside the mandate", "", closest.MandateID
		default:
			it.Decision, it.Reason, it.Rule, it.MandateID = ItemNeedsApproval, closest.Reason, closest.Rule, closest.MandateID
		}
	}
	return out, nil
}

// resolvePayee fills in a saved payee: blocked (with why) when it is not the workspace's, its name did not match and
// was not confirmed, or screening does not let it through.
func (s *DualTokenStore) resolvePayee(ctx context.Context, workspaceID string, it *PaymentRequestItem, p *PreparedPayment) (string, error) {
	py, err := s.outsidePayee(ctx, s.pool, workspaceID, it.PayeeID, "")
	switch {
	case errors.Is(err, ErrPayeeNotFound):
		return "no saved payee of this workspace has this id", nil
	case err != nil:
		return "", err
	}
	it.PayeeName = py.Name
	if py.NeedsConfirmation {
		return strings.TrimPrefix(ErrPayeeUnconfirmed.Error(), "economy: "), nil
	}
	if s.payeeScreener == nil {
		return "screening is unavailable, so no payee is paid", nil
	}
	if err := s.payeeScreener.ScreenPayee(ctx, workspaceID, py.ID, py.Name, CapabilityPaymentsOut); err != nil {
		return err.Error(), nil
	}
	var paidBefore bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_payments WHERE workspace_id = $1 AND payee_id = $2)`,
		workspaceID, py.ID).Scan(&paidBefore); err != nil {
		return "", fmt.Errorf("economy: payee paid before: %w", err)
	}
	added := py.CreatedAt
	p.PayeeID, p.NewPayee, p.PayeeConfirmed, p.PayeeAddedAt = py.ID, !paidBefore, py.Check == "exact_match" || py.ConfirmedAt != nil, &added
	return "", nil
}

// resolveListing fills in a marketplace listing at its per-use price, the seller being its workspace.
func (s *DualTokenStore) resolveListing(ctx context.Context, workspaceID string, it *PaymentRequestItem, p *PreparedPayment) (string, error) {
	var seller, title, visibility, review string
	var room *string
	var ulxc int64
	err := s.pool.QueryRow(ctx, `SELECT workspace_id, title, visibility, review_status, room_id, price_per_use_ulxc
		FROM market_listings WHERE id = $1`, it.ListingID).Scan(&seller, &title, &visibility, &review, &room, &ulxc)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && visibility == "private" && seller != workspaceID) {
		return "", fmt.Errorf("%w: item %d: no listing the workspace can see has the id %q", ErrPaymentRequest, it.Position, it.ListingID)
	}
	if err != nil {
		return "", fmt.Errorf("economy: listing: %w", err)
	}
	// A listing is priced in µLXC; the request holds US cents, rounded up so a limit is never under-counted.
	cents := (ulxc/ULXCPerUSDMicro + 9_999) / 10_000
	it.PayeeID, it.PayeeName, it.Currency, it.AmountMinor = seller, title, CurrencyUSD, max(cents, 1)
	if room != nil {
		p.RoomID = *room
	}
	p.SellerID = seller
	switch {
	case seller == workspaceID:
		return "a workspace does not buy its own listing", nil
	case review != "approved":
		return "the listing is not approved for sale", nil
	case ulxc == 0:
		return "the listing is free: there is nothing to pay", nil
	}
	return "", nil
}

const paymentRequestCols = `id, kind, status, client, agent_id, canonical, payload_hash, created_at, cancelled_at`

// PaymentRequest reads one of workspaceID's requests; agentID, when not "", only one that agent prepared.
func (s *DualTokenStore) PaymentRequest(ctx context.Context, workspaceID, agentID, id string) (PaymentRequest, error) {
	var r PaymentRequest
	err := s.pool.QueryRow(ctx, `SELECT `+paymentRequestCols+` FROM payment_requests WHERE id = $1 AND workspace_id = $2
		AND ($3 = '' OR agent_id = $3)`, id, workspaceID, agentID).
		Scan(&r.ID, &r.Kind, &r.Status, &r.Client, &r.AgentID, &r.Canonical, &r.PayloadHash, &r.CreatedAt, &r.CancelledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrPaymentRequestNotFound
	}
	if err != nil {
		return r, fmt.Errorf("economy: payment request: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT position, payee_id, payee_name, listing_id, currency, amount_minor, reference, decision,
		reason, rule, mandate_id, payment_id, pay_status, pay_detail, authority, bill_id, flags FROM payment_request_items WHERE request_id = $1 ORDER BY position`, id)
	if err != nil {
		return r, fmt.Errorf("economy: payment request items: %w", err)
	}
	defer rows.Close()
	r.Items, r.Totals = []PaymentRequestItem{}, map[string]int64{}
	counts := map[string]int{}
	for rows.Next() {
		var it PaymentRequestItem
		if err := rows.Scan(&it.Position, &it.PayeeID, &it.PayeeName, &it.ListingID, &it.Currency, &it.AmountMinor, &it.Reference,
			&it.Decision, &it.Reason, &it.Rule, &it.MandateID, &it.PaymentID, &it.PayStatus, &it.PayDetail, &it.Authority, &it.BillID, &it.Flags); err != nil {
			return r, err
		}
		if it.Decision != ItemBlocked {
			r.Totals[it.Currency] += it.AmountMinor
		}
		counts[it.Decision]++
		r.Items = append(r.Items, it)
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	r.ApprovalURL, r.DeepLink = ApprovalURLBase+r.ID, ApprovalDeepURL+r.ID
	totals := []string{}
	for _, c := range slices.Sorted(maps.Keys(r.Totals)) {
		totals = append(totals, formatMinor(c, r.Totals[c]))
	}
	if len(totals) == 0 {
		totals = []string{"nothing payable"}
	}
	r.Summary = fmt.Sprintf("%d item(s), %s: %d need approval, %d auto, %d blocked", len(r.Items), strings.Join(totals, " + "),
		counts[ItemNeedsApproval], counts[ItemAuto], counts[ItemBlocked])
	return r, nil
}

// PendingPaymentRequests lists workspaceID's requests waiting for approval, newest first, each as PaymentRequest reads
// it; agentID, when not "", only that agent's (B40.38).
func (s *DualTokenStore) PendingPaymentRequests(ctx context.Context, workspaceID, agentID string) ([]PaymentRequest, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM payment_requests WHERE workspace_id = $1 AND status = 'pending_approval'
		AND ($2 = '' OR agent_id = $2) ORDER BY created_at DESC, id DESC`, workspaceID, agentID)
	if err != nil {
		return nil, fmt.Errorf("economy: pending payment requests: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("economy: pending payment requests: %w", err)
	}
	out := []PaymentRequest{}
	for _, id := range ids {
		r, err := s.PaymentRequest(ctx, workspaceID, agentID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// formatMinor writes minor units as the currency's decimal amount: "GBP 120.50".
func formatMinor(currency string, minor int64) string {
	d := MoneyCurrencies[currency]
	if d == 0 {
		return fmt.Sprintf("%s %d", currency, minor)
	}
	scale := int64(1)
	for range d {
		scale *= 10
	}
	return fmt.Sprintf("%s %d.%0*d", currency, minor/scale, d, minor%scale)
}

// CancelPaymentRequest cancels a request not yet cancelled: its auto items stop counting towards the mandates' limits.
// agentID, when not "", cancels only a request that agent prepared. One approved, or with an item paid, stays (B40.5).
func (s *DualTokenStore) CancelPaymentRequest(ctx context.Context, workspaceID, agentID, id, by string) (PaymentRequest, error) {
	if _, err := s.PaymentRequest(ctx, workspaceID, agentID, id); err != nil {
		return PaymentRequest{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaymentRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Held as PayPaymentRequest holds it: a request is cancelled before any item is paid, or not at all.
	var status string
	var paid bool
	if err := tx.QueryRow(ctx, `SELECT status FROM payment_requests WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, id, workspaceID).Scan(&status); err != nil {
		return PaymentRequest{}, fmt.Errorf("economy: cancel payment request: %w", err)
	}
	// Read after the lock, so a payer that held it is seen.
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_request_items WHERE request_id = $1 AND payment_id <> '')`, id).Scan(&paid); err != nil {
		return PaymentRequest{}, fmt.Errorf("economy: cancel payment request: %w", err)
	}
	if status == "approved" || paid {
		return PaymentRequest{}, fmt.Errorf("%w: the request was approved or paid already: it cannot be cancelled", ErrPaymentRequest)
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_requests SET status = 'cancelled', cancelled_at = now(), cancelled_by = $3
		WHERE id = $1 AND workspace_id = $2 AND status <> 'cancelled'`, id, workspaceID, by); err != nil {
		return PaymentRequest{}, fmt.Errorf("economy: cancel payment request: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PaymentRequest{}, err
	}
	return s.PaymentRequest(ctx, workspaceID, agentID, id)
}
