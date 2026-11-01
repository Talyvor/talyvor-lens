package economy

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// spending.go — B53.3: EVERY PAYMENT GETS A CATEGORY; ONE CORRECTION TEACHES IT. SPEND TAGS ON POSTINGS.
//
// A payment is money that left one of the workspace's wallets for someone outside: a card charge or a payment out to
// a payee. Its amount is what its postings credited outside (the partner, and any fee). Its category is worked out
// each time it is read, in this order: the owner's correction for its merchant ("always put this merchant in X"), the
// card's merchant category code, the merchant's name, the payee (one that sent the workspace a bill is a bill), and
// otherwise other. So one correction moves that merchant's past payments and its future ones alike. Gambling,
// religious, political and health merchants only ever read as a broad category (B53's rules).
//
// Totals are by wallet — the owner's own (agent_id empty) and each agent's — and currency, then by category and by
// the owner's own tags on each payment. A payment with two tags counts in both tags' totals.

// Spend categories (B53.3), in the order a picker lists them.
const (
	SpendGroceries     = "groceries"
	SpendEatingOut     = "eating_out"
	SpendTransport     = "transport"
	SpendTravel        = "travel"
	SpendBills         = "bills"
	SpendSubscriptions = "subscriptions"
	SpendShopping      = "shopping"
	SpendHealth        = "health"
	SpendEntertainment = "entertainment"
	SpendCash          = "cash"
	SpendTransfers     = "transfers" // to people
	SpendAISoftware    = "ai_software"
	SpendOther         = "other"
)

// SpendCategories is every category, in picker order.
var SpendCategories = []string{SpendGroceries, SpendEatingOut, SpendTransport, SpendTravel, SpendBills, SpendSubscriptions,
	SpendShopping, SpendHealth, SpendEntertainment, SpendCash, SpendTransfers, SpendAISoftware, SpendOther}

// Where a payment's category came from.
const (
	SpendFromCorrection = "correction"
	SpendFromCard       = "card"     // the merchant category code
	SpendFromMerchant   = "merchant" // the merchant's name
	SpendFromPayee      = "payee"
	SpendFromNone       = "none"
)

// ErrSpending is a correction or tag that cannot be saved; ErrSpendEntryNotFound an entry not the workspace's.
var (
	ErrSpending           = errors.New("economy: spending")
	ErrSpendEntryNotFound = errors.New("economy: spending: no such entry")
)

const spendMaxTags = 10

var spendTagShape = regexp.MustCompile(`^[a-z0-9][a-z0-9 _-]{0,39}$`)

// SpendPayment is one payment and its category.
type SpendPayment struct {
	EntryID        string    `json:"entry_id"`
	Kind           string    `json:"kind"` // card_charge or payment_out
	Merchant       string    `json:"merchant"`
	MCC            string    `json:"mcc,omitempty"`
	AgentID        string    `json:"agent_id,omitempty"` // empty: the owner's own wallet
	AmountMinor    int64     `json:"amount_minor"`
	Currency       string    `json:"currency"`
	Category       string    `json:"category"`
	CategorySource string    `json:"category_source"`
	Tags           []string  `json:"tags"`
	CreatedAt      time.Time `json:"created_at"`
}

// SpendTotal is one wallet's spend in one currency.
type SpendTotal struct {
	AgentID    string           `json:"agent_id,omitempty"` // empty: the owner's own wallet
	Currency   string           `json:"currency"`
	TotalMinor int64            `json:"total_minor"`
	ByCategory map[string]int64 `json:"by_category"`
	ByTag      map[string]int64 `json:"by_tag"`
}

// Spending is a workspace's payments in [From, To), newest first, and their totals.
type Spending struct {
	From       time.Time      `json:"from"`
	To         time.Time      `json:"to"`
	Categories []string       `json:"categories"`
	Payments   []SpendPayment `json:"payments"`
	Totals     []SpendTotal   `json:"totals"`
}

// spendMerchantKey is how a merchant is matched to its correction: trimmed and lower-cased, as the query does.
func spendMerchantKey(merchant string) string { return strings.ToLower(strings.TrimSpace(merchant)) }

// mccCategory is the category of a merchant category code (ISO 18245), or "" for one it does not place.
func mccCategory(mcc string) string {
	switch mcc {
	case "5411", "5422", "5441", "5451", "5462", "5499":
		return SpendGroceries
	case "5811", "5812", "5813", "5814":
		return SpendEatingOut
	case "4011", "4111", "4112", "4121", "4131", "4784", "4789", "5541", "5542", "7523", "7531", "7538":
		return SpendTransport
	case "4411", "4511", "4582", "4722", "7011", "7012", "7512", "7519":
		return SpendTravel
	case "4812", "4814", "4816", "4900", "6300", "9311", "9399", "6513":
		return SpendBills
	case "4899", "5815", "5816", "5818", "5968":
		return SpendSubscriptions
	case "5734", "5817", "7372", "7374", "7379", "5045":
		return SpendAISoftware
	case "5912", "5975", "5976", "8011", "8021", "8031", "8041", "8042", "8043", "8049", "8050", "8062", "8071", "8099":
		return SpendHealth
	case "7832", "7841", "7911", "7922", "7929", "7932", "7933", "7941", "7991", "7992", "7993", "7994", "7995", "7996",
		"7997", "7998", "7999", "8661", "8651": // gambling, religious and political merchants read only as this broad one
		return SpendEntertainment
	case "6010", "6011", "6051":
		return SpendCash
	case "4829", "6012":
		return SpendTransfers
	}
	if len(mcc) == 4 && mcc >= "3000" && mcc <= "3999" { // airlines, car hire and hotels by name
		return SpendTravel
	}
	if len(mcc) == 4 && mcc >= "5000" && mcc <= "5999" {
		return SpendShopping
	}
	return ""
}

// spendNames places a merchant by a word in its name when its card said nothing that places it.
var spendNames = []struct{ word, category string }{
	{"openai", SpendAISoftware}, {"anthropic", SpendAISoftware}, {"github", SpendAISoftware}, {"aws", SpendAISoftware},
	{"google cloud", SpendAISoftware}, {"vercel", SpendAISoftware},
	{"tesco", SpendGroceries}, {"sainsbury", SpendGroceries}, {"lidl", SpendGroceries}, {"aldi", SpendGroceries},
	{"pret", SpendEatingOut}, {"deliveroo", SpendEatingOut}, {"uber eats", SpendEatingOut},
	{"uber", SpendTransport}, {"tfl", SpendTransport}, {"trainline", SpendTransport},
	{"netflix", SpendSubscriptions}, {"spotify", SpendSubscriptions},
	{"airways", SpendTravel}, {"airbnb", SpendTravel}, {"hotel", SpendTravel},
}

func nameCategory(merchant string) string {
	m := spendMerchantKey(merchant)
	for _, n := range spendNames {
		if strings.Contains(m, n.word) {
			return n.category
		}
	}
	return ""
}

// spendCategory is a payment's category and where it came from.
func spendCategory(correction, mcc, merchant, kind string, payeeSentBill bool) (string, string) {
	switch {
	case correction != "":
		return correction, SpendFromCorrection
	case mccCategory(mcc) != "":
		return mccCategory(mcc), SpendFromCard
	case nameCategory(merchant) != "":
		return nameCategory(merchant), SpendFromMerchant
	case payeeSentBill:
		return SpendBills, SpendFromPayee
	case kind == "payment_out":
		return SpendTransfers, SpendFromPayee
	}
	return SpendOther, SpendFromNone
}

// Spending is workspaceID's payments in [from, to) and their totals; a zero from or to is this month's start or end.
// shortcut: every payment in the period is read and listed; page it when a month's payments outgrow one answer.
// shortcut: a returned payment out still counts as spent; subtract payment_out_returned when the Coach needs it.
func (s *DualTokenStore) Spending(ctx context.Context, workspaceID string, from, to time.Time) (Spending, error) {
	now := time.Now().UTC()
	if from.IsZero() {
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	if to.IsZero() {
		to = time.Date(from.Year(), from.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	}
	if !to.After(from) {
		return Spending{}, fmt.Errorf("%w: to must be after from", ErrSpending)
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id, e.kind, e.counterparty, COALESCE(e.card_mcc, ''), e.created_at, COALESCE(w.agent_id, ''),
			o.currency, o.amount, COALESCE(cc.category, ''),
			COALESCE(p.payee_id <> '' AND EXISTS (SELECT 1 FROM money_bills b WHERE b.workspace_id = e.workspace_id AND b.payee_id = p.payee_id), false),
			COALESCE((SELECT array_agg(t.tag ORDER BY t.tag) FROM money_entry_tags t WHERE t.workspace_id = e.workspace_id AND t.entry_id = e.id), '{}')
		FROM money_entries e
		LEFT JOIN money_cards c ON e.kind = 'card_charge' AND c.workspace_id = e.workspace_id AND e.idempotency_key = 'card_charge:' || c.id
		LEFT JOIN money_payments p ON e.kind = 'payment_out' AND p.workspace_id = e.workspace_id AND e.idempotency_key = 'payment_out:' || p.idempotency_key
		JOIN money_accounts w ON w.id = COALESCE(c.account_id, p.account_id)
		JOIN LATERAL (SELECT po.currency, SUM(po.amount_minor)::bigint AS amount FROM money_postings po JOIN money_accounts a ON a.id = po.account_id
			WHERE po.entry_id = e.id AND a.purpose IN ('partner', 'revenue') AND po.amount_minor > 0 GROUP BY po.currency) o ON true
		LEFT JOIN spend_category_corrections cc ON cc.workspace_id = e.workspace_id AND cc.merchant_key = lower(btrim(e.counterparty))
		WHERE e.workspace_id = $1 AND e.kind IN ('card_charge', 'payment_out') AND e.created_at >= $2 AND e.created_at < $3
		ORDER BY e.created_at DESC, e.id`, workspaceID, from, to)
	if err != nil {
		return Spending{}, fmt.Errorf("economy: spending: %w", err)
	}
	defer rows.Close()
	out := Spending{From: from, To: to, Categories: SpendCategories, Payments: []SpendPayment{}, Totals: []SpendTotal{}}
	totals := map[[2]string]int{} // agent, currency → index in out.Totals
	for rows.Next() {
		var p SpendPayment
		var correction string
		var sentBill bool
		if err := rows.Scan(&p.EntryID, &p.Kind, &p.Merchant, &p.MCC, &p.CreatedAt, &p.AgentID, &p.Currency, &p.AmountMinor,
			&correction, &sentBill, &p.Tags); err != nil {
			return Spending{}, fmt.Errorf("economy: spending: %w", err)
		}
		p.Category, p.CategorySource = spendCategory(correction, p.MCC, p.Merchant, p.Kind, sentBill)
		out.Payments = append(out.Payments, p)
		i, ok := totals[[2]string{p.AgentID, p.Currency}]
		if !ok {
			i = len(out.Totals)
			totals[[2]string{p.AgentID, p.Currency}] = i
			out.Totals = append(out.Totals, SpendTotal{AgentID: p.AgentID, Currency: p.Currency, ByCategory: map[string]int64{}, ByTag: map[string]int64{}})
		}
		t := &out.Totals[i]
		t.TotalMinor += p.AmountMinor
		t.ByCategory[p.Category] += p.AmountMinor
		for _, tag := range p.Tags {
			t.ByTag[tag] += p.AmountMinor
		}
	}
	if err := rows.Err(); err != nil {
		return Spending{}, fmt.Errorf("economy: spending: %w", err)
	}
	return out, nil
}

// SetSpendCategory teaches workspaceID that merchant's payments, past and future, are category; an empty category
// forgets the correction, so the merchant is placed as it would be without one.
func (s *DualTokenStore) SetSpendCategory(ctx context.Context, workspaceID, merchant, category, by string) error {
	key := spendMerchantKey(merchant)
	if key == "" || len(key) > 200 {
		return fmt.Errorf("%w: name the merchant (at most 200 characters)", ErrSpending)
	}
	if category == "" {
		_, err := s.pool.Exec(ctx, `DELETE FROM spend_category_corrections WHERE workspace_id = $1 AND merchant_key = $2`, workspaceID, key)
		return err
	}
	if !slices.Contains(SpendCategories, category) {
		return fmt.Errorf("%w: category must be one of %s", ErrSpending, strings.Join(SpendCategories, ", "))
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO spend_category_corrections (workspace_id, merchant_key, category, set_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (workspace_id, merchant_key) DO UPDATE SET category = EXCLUDED.category, set_by = EXCLUDED.set_by, updated_at = now()`,
		workspaceID, key, category, by)
	return err
}

// SetEntryTags replaces the tags on workspaceID's money entry entryID with tags (trimmed, lower-cased, each once),
// and answers them in order.
func (s *DualTokenStore) SetEntryTags(ctx context.Context, workspaceID, entryID string, tags []string) ([]string, error) {
	clean := []string{}
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if !spendTagShape.MatchString(t) {
			return nil, fmt.Errorf("%w: a tag is 1 to 40 letters, digits, spaces, - or _, starting with a letter or digit", ErrSpending)
		}
		if !slices.Contains(clean, t) {
			clean = append(clean, t)
		}
	}
	if len(clean) > spendMaxTags {
		return nil, fmt.Errorf("%w: at most %d tags on a payment", ErrSpending, spendMaxTags)
	}
	slices.Sort(clean)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_entries WHERE id = $1 AND workspace_id = $2)`, entryID, workspaceID).
			Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrSpendEntryNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM money_entry_tags WHERE workspace_id = $1 AND entry_id = $2`, workspaceID, entryID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO money_entry_tags (workspace_id, entry_id, tag) SELECT $1, $2, unnest($3::text[])`,
			workspaceID, entryID, clean)
		return err
	})
	if err != nil && !errors.Is(err, ErrSpendEntryNotFound) {
		return nil, fmt.Errorf("economy: spend tags: %w", err)
	}
	return clean, err
}
