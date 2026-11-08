package smartaccount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// permission.go — B48.3: THE OWNER'S LIMITS ON WHAT THE AGENT'S KEY MAY PAY. The person signs a Permission with their
// passkey: per token an allowance per day, week or month and a cap on one payment, the counterparties it may pay and
// those it may never pay, and when it ends. Talyvor's gate checks every operation the agent's key is asked to sign
// against it (SignPaymentAsAgent), and Freeze takes the agent's key out of the account's owners on chain, by the
// person's passkey, after which the key can sign nothing the account accepts.

// Native is a chain's own coin where a token's address goes (ERC-7528).
const Native = "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE"

// The periods an allowance runs over: calendar days, ISO weeks (from Monday) and months, in UTC.
const (
	PeriodDay   = "day"
	PeriodWeek  = "week"
	PeriodMonth = "month"
)

// TokenLimit is what the agent may pay in one token, in the token's base units.
type TokenLimit struct {
	Token      string `json:"token"`       // the token's contract, or Native
	Allowance  int64  `json:"allowance"`   // in all, per period
	Period     string `json:"period"`      // day, week or month
	PerPayment int64  `json:"per_payment"` // in one payment
}

// Permission is what the person allows their agent's key to pay. No tokens allows nothing: that is how it is revoked.
type Permission struct {
	Tokens  []TokenLimit `json:"tokens"`
	Allowed []string     `json:"allowed"` // the only counterparties it may pay; none: any that is not blocked
	Blocked []string     `json:"blocked"` // counterparties it may never pay
	Expires time.Time    `json:"expires"`
}

// Payment is what one operation would pay: Amount of Token to To, and — B48.25, when an ERC-20 paymaster takes the
// operation's network fee in the same token — at most Fee more to Paymaster, which the operation approves it.
type Payment struct {
	Token     string
	To        string
	Amount    *big.Int
	Fee       *big.Int // nil: the network fee is not paid in the token
	Paymaster string
}

// Total is the most the operation can take from the account in Token: its amount, and its most possible fee.
func (p Payment) Total() *big.Int {
	if p.Fee == nil {
		return p.Amount
	}
	return new(big.Int).Add(p.Amount, p.Fee)
}

var (
	// ErrPermission wraps every payment the owner's permission refuses.
	ErrPermission = errors.New("smartaccount: outside the owner's permission")
	// ErrFrozen: the owner froze the agent; its key signs nothing.
	ErrFrozen = errors.New("smartaccount: the owner froze this agent")
)

// Normalise puts p in the one form it is signed in: addresses EIP-55, lists sorted, the expiry in whole UTC seconds.
func (p *Permission) Normalise() {
	for i := range p.Tokens {
		if c := Checksum(p.Tokens[i].Token); c != "" {
			p.Tokens[i].Token = c
		}
	}
	slices.SortFunc(p.Tokens, func(a, b TokenLimit) int { return strings.Compare(a.Token, b.Token) })
	set := func(in []string) []string {
		out := []string{}
		for _, a := range in {
			if c := Checksum(strings.TrimSpace(a)); c != "" {
				a = c
			}
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
		slices.Sort(out)
		return out
	}
	p.Allowed, p.Blocked = set(p.Allowed), set(p.Blocked)
	p.Expires = p.Expires.UTC().Truncate(time.Second)
}

// Validate refuses a permission that cannot be signed: an address that is not one, a token twice, a period that is
// not one, a limit that is not positive or a cap above its allowance, a counterparty both allowed and blocked, or an
// expiry not after now.
func (p Permission) Validate(now time.Time) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	seen := map[string]bool{}
	for _, l := range p.Tokens {
		if Checksum(l.Token) == "" {
			return bad("%q is not a token's address", l.Token)
		}
		if seen[Checksum(l.Token)] {
			return bad("%s is limited twice", l.Token)
		}
		seen[Checksum(l.Token)] = true
		if l.Period != PeriodDay && l.Period != PeriodWeek && l.Period != PeriodMonth {
			return bad("an allowance runs per day, week or month, not %q", l.Period)
		}
		if l.Allowance <= 0 || l.PerPayment <= 0 || l.PerPayment > l.Allowance {
			return bad("%s: the allowance and the cap on one payment are positive, the cap no more than the allowance", l.Token)
		}
	}
	for _, a := range append(slices.Clone(p.Allowed), p.Blocked...) {
		if Checksum(a) == "" {
			return bad("%q is not an address", a)
		}
	}
	for _, a := range p.Allowed {
		if slices.ContainsFunc(p.Blocked, func(b string) bool { return strings.EqualFold(a, b) }) {
			return bad("%s is both allowed and blocked", a)
		}
	}
	if !p.Expires.After(now) {
		return bad("a permission ends after it is signed")
	}
	return nil
}

// PeriodStart is when the period containing now began.
func PeriodStart(period string, now time.Time) time.Time {
	d := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case PeriodWeek:
		return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
	case PeriodMonth:
		return d.AddDate(0, 0, 1-d.Day())
	}
	return d
}

// Limit is p's limit on token, and whether it allows token at all.
func (p Permission) Limit(token string) (TokenLimit, bool) {
	for _, l := range p.Tokens {
		if strings.EqualFold(l.Token, token) {
			return l, true
		}
	}
	return TokenLimit{}, false
}

// Check is the gate: whether p allows pay at now, when spent of the same token has already been paid this period.
func (p Permission) Check(pay Payment, spent int64, now time.Time) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrPermission, fmt.Sprintf(format, args...))
	}
	same := func(a string) bool { return strings.EqualFold(a, pay.To) }
	switch {
	case !now.Before(p.Expires):
		return refuse("it ended at %s", p.Expires.Format(time.RFC3339))
	case slices.ContainsFunc(p.Blocked, same):
		return refuse("%s is blocked", pay.To)
	case pay.Fee != nil && slices.ContainsFunc(p.Blocked, func(a string) bool { return strings.EqualFold(a, pay.Paymaster) }):
		return refuse("the paymaster %s is blocked", pay.Paymaster)
	case len(p.Allowed) > 0 && !slices.ContainsFunc(p.Allowed, same):
		return refuse("%s is not one of the counterparties allowed", pay.To)
	case pay.Fee != nil && len(p.Allowed) > 0 && !slices.ContainsFunc(p.Allowed, func(a string) bool { return strings.EqualFold(a, pay.Paymaster) }):
		return refuse("the paymaster %s is not one of the counterparties allowed", pay.Paymaster)
	}
	l, ok := p.Limit(pay.Token)
	switch {
	case !ok:
		return refuse("it does not allow paying in %s", pay.Token)
	case pay.Amount == nil || pay.Amount.Sign() <= 0:
		return refuse("a payment pays something")
	case pay.Fee != nil && pay.Fee.Sign() < 0:
		return refuse("a fee is not negative")
	case pay.Total().Cmp(big.NewInt(l.PerPayment)) > 0:
		return refuse("%s is over the cap of %d on one payment", pay.Total(), l.PerPayment)
	case new(big.Int).Add(pay.Total(), big.NewInt(spent)).Cmp(big.NewInt(l.Allowance)) > 0:
		return refuse("%s with %d already paid this %s is over the allowance of %d", pay.Total(), spent, l.Period, l.Allowance)
	}
	return nil
}

var (
	transferSelector     = selector("transfer(address,uint256)")
	approveSelector      = selector("approve(address,uint256)")
	executeBatchSelector = selector("executeBatch((address,uint256,bytes)[])")
)

// PaymentOf reads what an account's call pays: execute(to, value, "") pays value of the chain's coin to to,
// execute(token, 0, transfer(to, amount)) pays amount of token to to, and — B48.25 — executeBatch of that transfer then
// token's approve(paymaster, fee) pays it with at most fee more for its network fee (SponsorInToken). Any other call is
// not a payment.
func PaymentOf(callData []byte) (Payment, error) {
	notPayment := fmt.Errorf("%w: the agent's key signs only a payment: a coin, or a token's transfer", ErrPermission)
	if bytes.HasPrefix(callData, executeBatchSelector) {
		return tokenFeePaymentOf(callData, notPayment)
	}
	if len(callData) < 4+3*32+32 || !bytes.Equal(callData[:4], executeSelector) {
		return Payment{}, notPayment
	}
	target := checksum(callData[4+12 : 4+32])
	value := new(big.Int).SetBytes(callData[36:68])
	n := new(big.Int).SetBytes(callData[4+96 : 4+128])
	if !n.IsInt64() || n.Int64() > int64(len(callData)) {
		return Payment{}, notPayment
	}
	data := callData[4+128:]
	data = data[:min(len(data), int(n.Int64()))]
	// Only the call's one exact encoding is read: anything a node would decode otherwise is refused.
	if canonical, err := Execute(target, value, data); err != nil || !bytes.Equal(canonical, callData) {
		return Payment{}, notPayment
	}
	switch {
	case len(data) == 0:
		return Payment{Token: Native, To: target, Amount: value}, nil
	case value.Sign() == 0 && len(data) == 4+64 && bytes.Equal(data[:4], transferSelector) && bytes.Equal(data[4:16], make([]byte, 12)):
		return Payment{Token: target, To: checksum(data[16:36]), Amount: new(big.Int).SetBytes(data[36:68])}, nil
	}
	return Payment{}, notPayment
}

// tokenFeePaymentOf reads executeBatch([token.transfer(to, amount), token.approve(paymaster, fee)]), in its one exact
// encoding: two calls of 4+64 bytes each, at their canonical places.
func tokenFeePaymentOf(callData []byte, notPayment error) (Payment, error) {
	const item = 32*4 + 96 // target, value, the data's offset and length, the data padded to 96 bytes
	if len(callData) != 4+32*4+2*item {
		return Payment{}, notPayment
	}
	first, second := callData[4+32*4:], callData[4+32*4+item:]
	token, approved := checksum(first[12:32]), checksum(second[12:32])
	transfer, approve := first[128:128+68], second[128:128+68]
	canonical, err := executeBatch(batchItem{token, nil, transfer}, batchItem{approved, nil, approve})
	switch {
	case err != nil || !bytes.Equal(canonical, callData) || token != approved:
		return Payment{}, notPayment
	case !bytes.Equal(transfer[:4], transferSelector) || !bytes.Equal(transfer[4:16], make([]byte, 12)):
		return Payment{}, notPayment
	case !bytes.Equal(approve[:4], approveSelector) || !bytes.Equal(approve[4:16], make([]byte, 12)):
		return Payment{}, notPayment
	}
	return Payment{Token: token, To: checksum(transfer[16:36]), Amount: new(big.Int).SetBytes(transfer[36:68]),
		Fee: new(big.Int).SetBytes(approve[36:68]), Paymaster: checksum(approve[16:36])}, nil
}

// SignPaymentAsAgent is the only way Talyvor has the agent's key sign: op must be one payment out of a, and
// authorise — the owner's permission, through Talyvor's gate — must allow it, before the provider is asked to sign.
func (c Chain) SignPaymentAsAgent(ctx context.Context, s Signer, a Account, agentKey string, op *UserOp, authorise func(Payment) error) error {
	pay, err := PaymentOf(op.CallData)
	if err != nil {
		return err
	}
	if err := authorise(pay); err != nil {
		return err
	}
	return c.SignAsAgent(ctx, s, a, agentKey, op)
}

// FreezeCall is the account's call that takes the agent's key out of its owners: once the person's passkey, or the key
// itself (SignRetirementAsAgent), has signed it in a user operation on a chain, the key signs nothing the account
// accepts there.
func FreezeCall(a Account) ([]byte, error) { return RetireCall(a, "") }
