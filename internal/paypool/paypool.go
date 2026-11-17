// Package paypool is B63.1: the payments pool, measured only.
//
// Nick's idea (15 Nov 2026): royalties when a payment repeats, like the shared answers pool but for payments. Its
// one rule: a royalty is a share of a cost that did not have to be paid twice; no saving, no royalty. This first
// step only measures. For each settled Sandbox payment out and payment to a person it writes one row saying which it
// was — a first, a repeat of the owner's own, a repeat of another owner's first (with 3 or more owners paying that
// payee), or a payment inside Talyvor — with the ASSUMED cost a first would have paid and this did not, and the
// royalty a rule would have given the first payer. No money moves and no royalty is owed: every figure says "would
// have". Two accounts of one owner, by the identity graph the answers pool uses, never earn from each other.
//
// It is told of a payment after the payment settles, never in its transaction (economy.PaymentsPool), and it changes
// no payment, balance, fee or decision. Its read gives an operator counts and sums by week, and nothing of one payment.
package paypool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// OwnersFloor is how many separate owners must have paid a payee before another owner's first earns from a repeat.
const OwnersFloor = 3

// The kinds of payment a measure says it was.
const (
	KindFirst            = "first"
	KindOwnRepeat        = "own_repeat"
	KindOthersRepeat     = "others_repeat"
	KindOthersBelowFloor = "others_below_floor"
	KindInside           = "inside"
)

// DB is the database the pool reads and writes.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Pool measures settled payments. It implements economy.PaymentsPool.
type Pool struct{ db DB }

// New is the payments pool over db.
func New(db DB) *Pool { return &Pool{db: db} }

var _ economy.PaymentsPool = (*Pool)(nil)

// costs are the assumed costs of a first payment in one currency, and the royalty share.
type costs struct {
	nameCheck, screening, outsideTransfer int64
	shareBPS                              int64
}

func (c costs) royalty(avoided int64) int64 { return avoided * c.shareBPS / 10000 }

// measure is what one payment was.
type measure struct {
	kind       string
	firstPayer string
	owners     int
	ruledOut   bool
	avoided    int64
	royalty    int64
}

// classify judges a payment by ws to a payee that payers (oldest first) have paid before, by graph's owners.
func classify(ws string, inside bool, payers []string, graph *poolroyalty.IdentityGraph, c costs) measure {
	owner := graph.Component(ws)
	owners := map[string]bool{owner: true}
	paidBefore := false
	for _, w := range payers {
		owners[graph.Component(w)] = true
		paidBefore = paidBefore || w == ws
	}
	m := measure{owners: len(owners)}
	if len(payers) > 0 {
		m.firstPayer = payers[0]
	}
	sameOwner := m.firstPayer != "" && graph.Component(m.firstPayer) == owner
	m.ruledOut = sameOwner && m.firstPayer != ws
	// Another owner's first earns only once enough owners have paid the payee, and never from the same owner.
	earns := m.firstPayer != "" && !sameOwner && m.owners >= OwnersFloor
	switch {
	case inside:
		m.kind, m.avoided = KindInside, c.outsideTransfer
	case m.firstPayer == "":
		m.kind = KindFirst
	case paidBefore || sameOwner:
		m.kind, m.avoided = KindOwnRepeat, c.nameCheck
		earns = false // the owner's own repeat earns nobody
	case m.owners < OwnersFloor:
		m.kind = KindOthersBelowFloor
	default:
		m.kind, m.avoided = KindOthersRepeat, c.nameCheck+c.screening
	}
	if earns {
		m.royalty = c.royalty(m.avoided)
	}
	return m
}

// Measure writes p's row, once: a payment measured again writes nothing.
func (pl *Pool) Measure(ctx context.Context, p economy.SettledPayment) error {
	if p.ID == "" || p.WorkspaceID == "" || p.PayeeHash == "" {
		return errors.New("paypool: a payment to measure has an id, an account and a payee")
	}
	tx, err := pl.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("paypool: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One payee's payments are judged one at a time, so two at once cannot both be its first.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('payments_pool:' || $1, 0))`, p.PayeeHash); err != nil {
		return fmt.Errorf("paypool: %w", err)
	}
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payments_pool_measures WHERE payment_kind = $1 AND payment_id = $2)`,
		p.Kind, p.ID).Scan(&done); err != nil || done {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT workspace_id FROM payments_pool_measures WHERE payee_hash = $1 ORDER BY created_at, id`, p.PayeeHash)
	if err != nil {
		return fmt.Errorf("paypool: %w", err)
	}
	payers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("paypool: %w", err)
	}
	graph, err := poolroyalty.LoadIdentityGraph(ctx, tx)
	if err != nil {
		return fmt.Errorf("paypool: the identity graph: %w", err)
	}
	var c costs
	if err := tx.QueryRow(ctx, `SELECT
		COALESCE((SELECT name_check_minor FROM payments_pool_assumed_costs WHERE currency = $1), 0),
		COALESCE((SELECT screening_minor FROM payments_pool_assumed_costs WHERE currency = $1), 0),
		COALESCE((SELECT outside_transfer_minor FROM payments_pool_assumed_costs WHERE currency = $1), 0),
		COALESCE((SELECT royalty_share_bps FROM payments_pool_settings), 0)`, p.Currency).
		Scan(&c.nameCheck, &c.screening, &c.outsideTransfer, &c.shareBPS); err != nil {
		return fmt.Errorf("paypool: the assumed costs: %w", err)
	}
	m := classify(p.WorkspaceID, p.Inside, payers, graph, c)
	if _, err := tx.Exec(ctx, `INSERT INTO payments_pool_measures (id, payment_kind, payment_id, workspace_id, payee_hash, kind,
		first_payer_workspace_id, owners, ruled_out, currency, cost_avoided_minor, would_royalty_minor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		uuid.NewString(), p.Kind, p.ID, p.WorkspaceID, p.PayeeHash, m.kind, m.firstPayer, m.owners, m.ruledOut, p.Currency,
		m.avoided, m.royalty); err != nil {
		return fmt.Errorf("paypool: %w", err)
	}
	return tx.Commit(ctx)
}

// Week is one week of measures, Monday 00:00 UTC on: counts, and sums by currency. It names no payee, no account
// and no amount of a single payment.
type Week struct {
	Start            time.Time          `json:"week_start"`
	Payments         int64              `json:"payments"`
	Firsts           int64              `json:"firsts"`
	OwnRepeats       int64              `json:"own_repeats"`
	OthersRepeats    int64              `json:"others_repeats"`
	OthersBelowFloor int64              `json:"others_repeats_below_floor"`
	Inside           int64              `json:"inside_talyvor"`
	RuledOut         int64              `json:"ruled_out_by_identity_graph"`
	Currencies       []CurrencyWeekSums `json:"currencies"`
}

// CurrencyWeekSums is a week's sums in one currency, in its minor units. Every one is assumed: no money moved.
type CurrencyWeekSums struct {
	Currency           string `json:"currency"`
	AssumedCostAvoided int64  `json:"assumed_cost_avoided_minor"`
	WouldBeRoyalties   int64  `json:"would_be_royalties_minor"`
	// LargestEarnerBPS is the largest would-be earner's share of the week's would-be royalties, in basis points.
	LargestEarnerBPS int64 `json:"largest_earner_share_bps"`
}

// Weeks is the last n weeks that have measures, newest first.
func (pl *Pool) Weeks(ctx context.Context, n int) ([]Week, error) {
	if n <= 0 || n > 104 {
		n = 12
	}
	rows, err := pl.db.Query(ctx, `SELECT date_trunc('week', created_at AT TIME ZONE 'UTC') AS week, count(*),
		count(*) FILTER (WHERE kind = 'first'), count(*) FILTER (WHERE kind = 'own_repeat'),
		count(*) FILTER (WHERE kind = 'others_repeat'), count(*) FILTER (WHERE kind = 'others_below_floor'),
		count(*) FILTER (WHERE kind = 'inside'), count(*) FILTER (WHERE ruled_out)
		FROM payments_pool_measures GROUP BY week ORDER BY week DESC LIMIT $1`, n)
	if err != nil {
		return nil, fmt.Errorf("paypool: %w", err)
	}
	var weeks []Week
	at := map[time.Time]int{}
	for rows.Next() {
		var w Week
		if err := rows.Scan(&w.Start, &w.Payments, &w.Firsts, &w.OwnRepeats, &w.OthersRepeats, &w.OthersBelowFloor, &w.Inside,
			&w.RuledOut); err != nil {
			rows.Close()
			return nil, fmt.Errorf("paypool: %w", err)
		}
		w.Start = w.Start.UTC()
		w.Currencies = []CurrencyWeekSums{}
		at[w.Start] = len(weeks)
		weeks = append(weeks, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("paypool: %w", err)
	}
	if len(weeks) == 0 {
		return []Week{}, nil
	}
	rows, err = pl.db.Query(ctx, `WITH m AS (
			SELECT date_trunc('week', created_at AT TIME ZONE 'UTC') AS week, * FROM payments_pool_measures
			WHERE created_at >= $1::timestamp AT TIME ZONE 'UTC'),
		earners AS (SELECT week, currency, first_payer_workspace_id, sum(would_royalty_minor) AS earned FROM m
			WHERE would_royalty_minor > 0 GROUP BY week, currency, first_payer_workspace_id)
		SELECT m.week, m.currency, sum(m.cost_avoided_minor)::bigint, sum(m.would_royalty_minor)::bigint,
			COALESCE((SELECT max(earned) FROM earners e WHERE e.week = m.week AND e.currency = m.currency), 0)::bigint
		FROM m GROUP BY m.week, m.currency ORDER BY m.week DESC, m.currency`, weeks[len(weeks)-1].Start)
	if err != nil {
		return nil, fmt.Errorf("paypool: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var week time.Time
		var s CurrencyWeekSums
		var largest int64
		if err := rows.Scan(&week, &s.Currency, &s.AssumedCostAvoided, &s.WouldBeRoyalties, &largest); err != nil {
			return nil, fmt.Errorf("paypool: %w", err)
		}
		if s.WouldBeRoyalties > 0 {
			s.LargestEarnerBPS = largest * 10000 / s.WouldBeRoyalties
		}
		if i, ok := at[week.UTC()]; ok {
			weeks[i].Currencies = append(weeks[i].Currencies, s)
		}
	}
	return weeks, rows.Err()
}
