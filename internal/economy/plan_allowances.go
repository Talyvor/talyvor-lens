package economy

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/plans"
)

// plan_allowances.go — B61.58: FEES FOLLOW THE PLAN: WHAT IS FREE EACH MONTH, THEN THE FEE.
//
// Each plan has, per account and calendar month (UTC), what it converts free (fees.FXFreeMinor, in minor units of the
// currency sold and counted per currency) and how many payments out it sends free in its own currency
// (fees.LocalFreePerMonth) and abroad (fees.IntlFreePerMonth). Above them the fee is taken: on a conversion, or an
// Instant transfer's, the plan's margin on the part above the line only; a payment out its fee, an international one
// less the plan's discount. A payment to another Talyvor account is never charged.
//
// What used each allowance is kept in plan_allowance_uses, one row per conversion, transfer or payment that went free
// (or, for a conversion, the part of it that did). It is written in the transaction that moves the money, under a
// lock on the account's allowance, so two moves at the line never both go free; a quote priced before another move
// took the free part is refused, and asked again. A payment that fails or comes back, and an Instant transfer that
// is reversed, gives its use back. A plan change takes the new plan's lines at once: the month's uses count against
// whichever plan the account is on when it next moves money.

// The allowances.
const (
	AllowanceFX            = "fx"
	AllowanceLocal         = "local"
	AllowanceInternational = "international"
)

// allowanceMonth is the calendar month, in UTC, that at falls in, as its first day.
func allowanceMonth(at time.Time) time.Time {
	at = at.UTC()
	return time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// lockAllowance holds workspaceID's allowance until tx ends: every move that may use it waits its turn.
func lockAllowance(ctx context.Context, tx pgx.Tx, workspaceID, allowance string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('plan_allowance:' || $1 || ':' || $2, 0))`,
		workspaceID, allowance); err != nil {
		return fmt.Errorf("economy: the plan's allowance: %w", err)
	}
	return nil
}

// allowanceUsed is what workspaceID has used of allowance in the month of at: for AllowanceFX the minor units of
// currency converted free, otherwise the payments sent free.
func allowanceUsed(ctx context.Context, q FeeQuerier, workspaceID, allowance, currency string, at time.Time) (int64, error) {
	var used int64
	if err := q.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM plan_allowance_uses WHERE workspace_id = $1 AND allowance = $2
		AND ($3 = '' OR currency = $3) AND month = $4 AND released_at IS NULL`, workspaceID, allowance, currency, allowanceMonth(at)).Scan(&used); err != nil {
		return 0, fmt.Errorf("economy: the plan's allowance: %w", err)
	}
	return used, nil
}

// allowanceLeft is what is left of limit after used, fees.Unlimited for no limit; never below zero.
func allowanceLeft(limit, used int64) int64 {
	if limit == fees.Unlimited {
		return fees.Unlimited
	}
	return max(limit-used, 0)
}

// useAllowance keeps that ref, a move in currency, used amount of workspaceID's allowance in the month of at.
func useAllowance(ctx context.Context, tx pgx.Tx, workspaceID, allowance, ref, currency string, amount int64, at time.Time) error {
	if amount <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO plan_allowance_uses (workspace_id, allowance, ref, currency, amount, month)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (allowance, ref) DO NOTHING`, workspaceID, allowance, ref, currency, amount, allowanceMonth(at)); err != nil {
		return fmt.Errorf("economy: the plan's allowance: %w", err)
	}
	return nil
}

// releaseAllowance gives back what ref used of allowance: it failed, came back or was reversed.
func releaseAllowance(ctx context.Context, tx pgx.Tx, allowance, ref string) error {
	if _, err := tx.Exec(ctx, `UPDATE plan_allowance_uses SET released_at = now() WHERE allowance = $1 AND ref = $2 AND released_at IS NULL`,
		allowance, ref); err != nil {
		return fmt.Errorf("economy: the plan's allowance: %w", err)
	}
	return nil
}

// fxBucket is the currency selling currency counts against, and how many of currency's minor units make one of its:
// a stablecoin account's is the USD figure.
func fxBucket(currency string) (string, int64) {
	if currency == CurrencyUSDC {
		return CurrencyUSD, 10_000 // millionths of a USDC to cents
	}
	return currency, 1
}

// fxFree is how much of selling sellMinor of currency converts free for workspaceID on plan in the month of at, and
// so the margin at bps on the rest, rounded up.
func fxFree(ctx context.Context, q FeeQuerier, workspaceID, plan, currency string, sellMinor, bps int64, at time.Time) (free, margin int64, err error) {
	bucket, scale := fxBucket(currency)
	used, err := allowanceUsed(ctx, q, workspaceID, AllowanceFX, bucket, at)
	if err != nil {
		return 0, 0, err
	}
	free = sellMinor
	if left := allowanceLeft(fees.Current().FXFreeMinor[plan], used); left != fees.Unlimited {
		free = min(sellMinor, left*scale)
	}
	return free, PlatformFee(sellMinor-free, bps), nil
}

// takeFXAllowance keeps, in tx and under the lock on workspaceID's conversions, the free part of selling sellMinor of
// currency that quote quoteID priced for ref, the move made at it, quotedFree. A quote is made at its own figures:
// when the month's allowance no longer leaves that much free — another move took it, or the plan changed — it is
// refused with ErrQuote, to be asked again. The plan is the one the quote's margin was read with (s.fxMargin).
func (s *DualTokenStore) takeFXAllowance(ctx context.Context, tx pgx.Tx, workspaceID, quoteID, ref, currency string, sellMinor, bps, quotedFree int64, at time.Time) error {
	if err := lockAllowance(ctx, tx, workspaceID, AllowanceFX); err != nil {
		return err
	}
	if s.fxMargin == nil {
		return fmt.Errorf("%w: no FX margin is set, so nothing is converted", ErrConversion)
	}
	_, plan, err := s.fxMargin(ctx, tx, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: the FX margin of %s: %w", workspaceID, err)
	}
	free, _, err := fxFree(ctx, tx, workspaceID, plan, currency, sellMinor, bps, at)
	if err != nil {
		return err
	}
	if free < quotedFree {
		return fmt.Errorf("%w: quote %s was priced with %s converting free this month, and the plan now leaves %s. Ask for a new one",
			ErrQuote, quoteID, Money(quotedFree, currency), Money(free, currency))
	}
	bucket, scale := fxBucket(currency)
	return useAllowance(ctx, tx, workspaceID, AllowanceFX, ref, bucket, (quotedFree+scale-1)/scale, at)
}

// paymentFee is what workspaceID pays to send a payment out in currency, its own currency's (local) or abroad
// (international, before the partner's cost), and whether it goes free under the plan's allowance this month.
func paymentFee(ctx context.Context, q FeeQuerier, workspaceID, allowance, currency string, at time.Time) (fee int64, free bool, err error) {
	plan, err := plans.PlanOf(ctx, q, workspaceID)
	if err != nil {
		return 0, false, err
	}
	f := fees.Current()
	limits, perPayment, kind, setting := f.LocalFreePerMonth, f.LocalPaymentFeeMinor, "local", "LENS_LOCAL_PAYMENT_FEE_MINOR"
	if allowance == AllowanceInternational {
		limits, perPayment, kind, setting = f.IntlFreePerMonth, f.IntlPaymentFeeMinor, "international", "LENS_INTL_PAYMENT_FEE_MINOR"
	}
	used, err := allowanceUsed(ctx, q, workspaceID, allowance, "", at)
	if err != nil {
		return 0, false, err
	}
	if allowanceLeft(limits[plan], used) != 0 {
		return 0, true, nil
	}
	fee, ok := perPayment[currency]
	if !ok {
		return 0, false, fmt.Errorf("%w: no %s payment fee is set for %s (%s), and one is never sent free", ErrOutsidePayment, kind, currency, setting)
	}
	if allowance == AllowanceInternational { // B61.58: Fleet pays 20% less; rounded up, so a fee never rounds away
		fee = intlDiscounted(fee, f.IntlDiscountBPS[plan])
	}
	return fee, false, nil
}

// intlDiscounted is fee less discountBPS of it, the discount rounded down so the fee rounds up.
func intlDiscounted(fee, discountBPS int64) int64 {
	return fee - fee*discountBPS/fees.BPSDenominator
}

// PlanAllowance is one of an account's allowances this month, for its Plan page.
type PlanAllowance struct {
	Allowance string `json:"allowance"`          // fx, local or international
	Currency  string `json:"currency,omitempty"` // fx: the currency sold, each counted on its own
	Free      int64  `json:"free"`               // a month: fx in minor units, payments in payments; -1 is no limit
	Used      int64  `json:"used"`
	Left      int64  `json:"left"` // -1 is no limit
	// Above it: fx the margin in basis points; a payment its fee in minor units of each currency.
	FeeBPS   *int64           `json:"fee_bps,omitempty"`
	FeeMinor map[string]int64 `json:"fee_minor,omitempty"`
}

// PlanAllowances is what workspaceID, on plan with an FX margin of fxBPS, has used and has left of each allowance in
// the month of at.
func PlanAllowances(ctx context.Context, q FeeQuerier, workspaceID, plan string, fxBPS int64, at time.Time) ([]PlanAllowance, error) {
	f := fees.Current()
	out := []PlanAllowance{}
	for _, c := range []string{CurrencyGBP, CurrencyEUR, CurrencyUSD} {
		used, err := allowanceUsed(ctx, q, workspaceID, AllowanceFX, c, at)
		if err != nil {
			return nil, err
		}
		free := f.FXFreeMinor[plan]
		out = append(out, PlanAllowance{Allowance: AllowanceFX, Currency: c, Free: free, Used: used, Left: allowanceLeft(free, used), FeeBPS: &fxBPS})
	}
	for _, a := range []struct {
		name   string
		free   int64
		perPay map[string]int64
	}{
		{AllowanceLocal, f.LocalFreePerMonth[plan], f.LocalPaymentFeeMinor},
		{AllowanceInternational, f.IntlFreePerMonth[plan], f.IntlPaymentFeeMinor},
	} {
		used, err := allowanceUsed(ctx, q, workspaceID, a.name, "", at)
		if err != nil {
			return nil, err
		}
		fee := map[string]int64{}
		for c, n := range a.perPay {
			if a.name == AllowanceInternational {
				n = intlDiscounted(n, f.IntlDiscountBPS[plan])
			}
			fee[c] = n
		}
		out = append(out, PlanAllowance{Allowance: a.name, Free: a.free, Used: used, Left: allowanceLeft(a.free, used), FeeMinor: fee})
	}
	return out, nil
}
