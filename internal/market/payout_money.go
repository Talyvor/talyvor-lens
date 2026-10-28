package market

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
)

// payout_money.go — B46.22: SELLERS AND LINEAGE ROYALTIES ARE PAID ON THE MONEY LAYER.
//
// A seller who saved payout details on the money layer (economy.SavePayoutDetails: an IBAN, or a USDC address on Base)
// is paid by the weekly run through economy.PayEarnings instead of a Stripe transfer: one market_payouts row of method
// money, of their available balance rounded down to the cent, with no Stripe fees, then paid from the paying
// workspace's account in the asset of their details — verified details first, then dollars, pounds, euros and USDC.
// The part of it their lineage royalties earned, in proportion to what royalties make of their released earnings, is
// paid as a royalty and the rest as a sale, both under the source market:<ISO week>, so a run asked again pays nothing
// twice. The µUSD journal is unchanged: the payout moves from the seller's available balance to money:clearing, and
// is rounded to the asset's minor units only where it is paid — pounds and euros at the ECB's reference rate of the
// day it was made. A seller whose details are not yet verified is held by the money layer, not paid, until they are.

// PayoutMoney is a payout made through the money layer's payouts.
const PayoutMoney = "money"

// EarningsPayer pays earnings out on the money layer. *economy.DualTokenStore satisfies it.
type EarningsPayer interface {
	PayEarnings(ctx context.Context, workspaceID string, e economy.Earnings) ([]economy.EarningsPayout, error)
}

// SetMoneyPayouts pays sellers who saved payout details on the money layer through payer, from workspace from's accounts.
func (s *Store) SetMoneyPayouts(payer EarningsPayer, from string) {
	s.moneyPayer, s.moneyPayerFrom = payer, from
}

// PaysOnMoneyLayer reports whether sellers with payout details on the money layer are paid there.
func (s *Store) PaysOnMoneyLayer() bool { return s.moneyPayer != nil }

// moneyAsset is the asset ws is paid in on the money layer, or "" when it saved no payout details there that could pass.
func moneyAsset(ctx context.Context, q queryRower, ws string) (string, error) {
	var asset string
	err := q.QueryRow(ctx, `SELECT COALESCE((SELECT asset FROM money_payout_details WHERE workspace_id = $1 AND status <> 'failed'
		ORDER BY status = 'verified' DESC, array_position(ARRAY['USD', 'GBP', 'EUR', 'USDC'], asset) LIMIT 1), '')`, ws).Scan(&asset)
	return asset, err
}

// royaltyPart is the part of grossUSDMicros paid to ws that its lineage royalties earned: gross in proportion to what
// royalties make of its released earnings.
func royaltyPart(ctx context.Context, q queryRower, ws string, grossUSDMicros int64, now time.Time) (int64, error) {
	var lineage, all int64
	if err := q.QueryRow(ctx, `SELECT COALESCE(sum(e.share_usd_micros) FILTER (WHERE e.kind = 'lineage'), 0)::bigint, COALESCE(sum(e.share_usd_micros), 0)::bigint
		FROM market_earnings e WHERE e.seller_workspace_id = $1 AND e.payable_at <= $2 AND NOT `+heldSQL+`
		AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)`, ws, now).Scan(&lineage, &all); err != nil {
		return 0, err
	}
	if all <= 0 || lineage <= 0 {
		return 0, nil
	}
	return mulDiv(grossUSDMicros, min(lineage, all), all), nil
}

// moneyDue is one market_payouts row of method money the money layer has not yet taken.
type moneyDue struct {
	id, ws, period, asset string
	net, royalty          int64
	livemode, test        bool
	createdAt             time.Time
}

// payUnpaidMoney hands every money payout the money layer has not yet taken to it, of one kind of seller when test is
// set: what the run does after making them, and on the days between its weekdays. It answers how many it took.
func (s *Store) payUnpaidMoney(ctx context.Context, test *bool) (int, error) {
	if s.moneyPayer == nil {
		return 0, nil
	}
	q, args := `SELECT id, workspace_id, period, asset, net_usd_micros, royalty_usd_micros, livemode, test, created_at FROM market_payouts
		WHERE paid_at IS NULL AND method = 'money'`, []any{}
	if test != nil {
		q, args = q+` AND test = $1`, append(args, *test)
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY created_at, id LIMIT 200`, args...)
	if err != nil {
		return 0, fmt.Errorf("market: money payouts to pay: %w", err)
	}
	todo, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (moneyDue, error) {
		var d moneyDue
		return d, row.Scan(&d.id, &d.ws, &d.period, &d.asset, &d.net, &d.royalty, &d.livemode, &d.test, &d.createdAt)
	})
	if err != nil {
		return 0, fmt.Errorf("market: money payouts to pay: %w", err)
	}
	n := 0
	var firstErr error
	for _, d := range todo {
		if err := s.payMoney(ctx, d); err != nil {
			if _, uerr := s.pool.Exec(ctx, `UPDATE market_payouts SET last_error = $2 WHERE id = $1 AND paid_at IS NULL`, d.id, err.Error()); uerr != nil {
				return n, fmt.Errorf("market: payout %s: %w", d.id, uerr)
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("market: pay out %s on the money layer: %w", d.id, err)
			}
			continue
		}
		if _, err := s.pool.Exec(ctx, `UPDATE market_payouts SET paid_at = now(), last_error = '' WHERE id = $1 AND paid_at IS NULL`, d.id); err != nil {
			return n, fmt.Errorf("market: payout %s: %w", d.id, err)
		}
		n++
	}
	return n, firstErr
}

// payMoney pays d through the money layer: its royalty part as a royalty, the rest as a sale. Asked again, the money
// layer answers the payouts it already made under the week's source.
func (s *Store) payMoney(ctx context.Context, d moneyDue) error {
	total, err := s.payoutMinor(ctx, d.asset, d.net, d.createdAt)
	if err != nil {
		return err
	}
	parts, err := economy.SplitEarnings(total, []int64{d.net - d.royalty, d.royalty})
	if err != nil {
		return err
	}
	for i, kind := range []string{economy.EarningsSale, economy.EarningsRoyalty} {
		if parts[i] == 0 {
			continue
		}
		if _, err := s.moneyPayer.PayEarnings(ctx, s.moneyPayerFrom, economy.Earnings{Kind: kind, Source: "market:" + d.period,
			Agreement: "market_payouts:" + d.id, Currency: d.asset, Funding: funding(d.livemode, d.test),
			Shares: []economy.EarningsShare{{WorkspaceID: d.ws, AmountMinor: parts[i]}}}); err != nil {
			return err
		}
	}
	return nil
}

// payoutMinor is usdMicros in asset's minor units: dollars and USDC as they are, pounds and euros at the ECB's
// reference rate of the day the payout was made, rounded half-up.
func (s *Store) payoutMinor(ctx context.Context, asset string, usdMicros int64, at time.Time) (int64, error) {
	switch asset {
	case economy.CurrencyUSD:
		return usdMicros / usdMicrosPerCent, nil
	case economy.CurrencyUSDC: // millionths of a USDC are µUSD
		return usdMicros, nil
	}
	if s.displayRates == nil {
		return 0, fmt.Errorf("no exchange rate is set to pay %s", asset)
	}
	r, err := s.displayRates.USDRate(ctx, asset, at)
	if err != nil {
		return 0, err
	}
	return r.Convert(usdMicros).AmountMinor, nil
}
