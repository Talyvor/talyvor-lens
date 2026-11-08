package smartaccount

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/money"
)

// spendpermission.go — B48.24: THE OWNER'S LIMITS HOLD ON CHAIN AND AT THE PROVIDER, NOT ONLY AT TALYVOR'S GATE. One
// passkey signature (GrantCall) takes the agent's key out of the account's owners, puts Coinbase's
// SpendPermissionManager (github.com/coinbase/spend-permissions v1.0.0, audited by Cantina, made for Coinbase Smart
// Wallet) in its place and approves, for each token the owner's Permission allows, a spend permission whose spender is
// the agent's own smart account (SpenderAccount), which its key alone owns. The manager then holds on chain the
// allowance per period, the token and the expiry: a spend in a token with no permission, over the period's allowance
// or after the end is reverted whatever signed it. The cap on one payment and the counterparties allowed and blocked
// are the provider's (ProviderPolicy): the agent's key spends only through the provider's user-operation API, whose
// policy engine reads the calls (SpendCalls) — and Talyvor's gate checks all of it first (SendSpendAsAgent).

// SpendPermissionManager is Coinbase's SpendPermissionManager where Coinbase deployed it, at the same address on every
// chain it is on (the repository's README at v1.0.0).
const SpendPermissionManager = "0xf85210B21cC50302F477BA56686d2019dC9b67Ad"

// SpendPermission is one token's spend permission as the manager holds it: Spender may take up to Allowance of Token
// from Account in each Period seconds from Start, until End.
type SpendPermission struct {
	Account   string
	Spender   string
	Token     string
	Allowance *big.Int
	Period    uint64 // seconds
	Start     uint64 // unix seconds, inclusive
	End       uint64 // unix seconds, exclusive
	Salt      *big.Int
}

// periodSeconds is each allowance period as the manager counts it, from Start: a fixed number of seconds.
//
// shortcut: a month is 31 days on chain, so its windows drift from calendar months and a window can straddle two:
// Talyvor's gate still holds the calendar month, and the chain can refuse a payment the gate allowed late in a month.
// Upgrade by having the owner's device sign a fresh month's permission at each month's start.
var periodSeconds = map[string]uint64{PeriodDay: 86_400, PeriodWeek: 7 * 86_400, PeriodMonth: 31 * 86_400}

// maxUint160 is the most the manager takes as an allowance or a spend.
var maxUint160 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))

func (sp SpendPermission) value() (tuple, error) {
	var ws [3]word
	for i, a := range []string{sp.Account, sp.Spender, sp.Token} {
		w, err := addressW(a)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		ws[i] = w
	}
	if sp.Allowance == nil || sp.Allowance.Sign() <= 0 || sp.Allowance.Cmp(maxUint160) > 0 || sp.Period == 0 || sp.Start >= sp.End ||
		sp.End >= 1<<48 || sp.Salt == nil || sp.Salt.Sign() < 0 {
		return nil, fmt.Errorf("%w: a spend permission's allowance, period, start and end", ErrInvalid)
	}
	// The manager's struct: account, spender, token, allowance, period, start, end, salt and extraData, always empty.
	return tuple{ws[0], ws[1], ws[2], bigW(sp.Allowance), uintW(sp.Period), uintW(sp.Start), uintW(sp.End), bigW(sp.Salt), bytesV(nil)}, nil
}

const spendPermissionType = "(address,address,address,uint160,uint48,uint48,uint48,uint256,bytes)"

// manager is the chain's SpendPermissionManager: SpendManager, or Coinbase's deployment.
func (c Chain) manager() string {
	if c.SpendManager != "" {
		return Checksum(c.SpendManager)
	}
	return SpendPermissionManager
}

// OnChain is p as the manager holds it for account and spender: one spend permission per token, from the start of
// the period containing now to p's expiry, salted with the permission's number so that every version is a new one.
func (p Permission) OnChain(account, spender string, number int64, now time.Time) ([]SpendPermission, error) {
	if number < 0 {
		return nil, fmt.Errorf("%w: a permission's number", ErrInvalid)
	}
	out := make([]SpendPermission, 0, len(p.Tokens))
	for _, l := range p.Tokens {
		period, ok := periodSeconds[l.Period]
		if !ok || l.Allowance <= 0 {
			return nil, fmt.Errorf("%w: %s's allowance and period", ErrInvalid, l.Token)
		}
		sp := SpendPermission{Account: Checksum(account), Spender: Checksum(spender), Token: Checksum(l.Token), Allowance: big.NewInt(l.Allowance),
			Period: period, Start: uint64(PeriodStart(l.Period, now).Unix()), End: uint64(p.Expires.Unix()), Salt: big.NewInt(number)}
		if _, err := sp.value(); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, nil
}

// GrantCall is the account's call, which the person's passkey signs, that revokes the spend permissions in revoke and
// approves those in approve; with handOver, it first adds the manager to the account's owners and then takes the
// agent's key out of them, so from then on the agent's key spends only through the manager.
func (c Chain) GrantCall(a Account, handOver bool, revoke, approve []SpendPermission) ([]byte, error) {
	var items []batchItem
	if handOver {
		w, _ := addressW(c.manager())
		items = append(items, batchItem{a.Address, nil, call("addOwnerAddress(address)", w)})
	}
	for i, sp := range append(append([]SpendPermission{}, revoke...), approve...) {
		v, err := sp.value()
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(sp.Account, a.Address) {
			return nil, fmt.Errorf("%w: a spend permission of another account", ErrInvalid)
		}
		fn := "approve"
		if i < len(revoke) {
			fn = "revoke"
		}
		items = append(items, batchItem{c.manager(), nil, call(fn+"("+spendPermissionType+")", v)})
	}
	if handOver {
		old, index, err := a.agent()
		if err != nil {
			return nil, err
		}
		items = append(items, batchItem{a.Address, nil, removeOwner(index, old)})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: nothing to grant or revoke", ErrInvalid)
	}
	return executeBatch(items...)
}

// SpenderAccount is the agent's own smart account, which its key alone owns: the provider's smart account for the key,
// at the address factory f gives it, and the spender of the owner's spend permissions.
func SpenderAccount(f Factory, agentKey string) (Account, error) {
	o, err := AddressOwner(agentKey)
	if err != nil {
		return Account{}, err
	}
	return newAccount(f, [][]byte{o})
}

// SpendCalls are the agent's own account's calls that pay amount of sp's token to to: the manager's spend, which
// moves it from the person's account into the agent's, and its payment out, in one user operation.
func (c Chain) SpendCalls(sp SpendPermission, to string, amount *big.Int) ([]money.Call, error) {
	v, err := sp.value()
	if err != nil {
		return nil, err
	}
	tw, err := addressW(to)
	if err != nil || strings.EqualFold(to, sp.Spender) || strings.EqualFold(to, sp.Account) {
		return nil, fmt.Errorf("%w: a payment goes to a counterparty", ErrInvalid)
	}
	if amount == nil || amount.Sign() <= 0 || amount.Cmp(maxUint160) > 0 {
		return nil, fmt.Errorf("%w: a payment pays something", ErrInvalid)
	}
	spend := money.Call{To: c.manager(), Value: "0", Data: hexBytes(call("spend("+spendPermissionType+",uint160)", v, bigW(amount)))}
	if sp.Token == Native {
		return []money.Call{spend, {To: Checksum(to), Value: amount.String(), Data: "0x"}}, nil
	}
	return []money.Call{spend, {To: sp.Token, Value: "0", Data: hexBytes(call("transfer(address,uint256)", tw, bigW(amount)))}}, nil
}

// SpendPaymentOf reads what the agent's own account spender's calls pay, and under which spend permission: they must
// be exactly SpendCalls of a permission whose spender is spender. Any other calls are not a payment.
func (c Chain) SpendPaymentOf(spender string, calls []money.Call) (SpendPermission, Payment, error) {
	notPayment := fmt.Errorf("%w: the agent's key spends only a payment under the owner's spend permission", ErrPermission)
	// spend's calldata: the selector, the struct's offset and the value, then the struct's nine words and its empty
	// extraData's length.
	if len(calls) != 2 || !strings.EqualFold(calls[0].To, c.manager()) {
		return SpendPermission{}, Payment{}, notPayment
	}
	data, err := hex.DecodeString(strings.TrimPrefix(calls[0].Data, "0x"))
	if err != nil || len(data) != 4+2*32+10*32 {
		return SpendPermission{}, Payment{}, notPayment
	}
	w := func(i int) []byte { return data[4+32*i : 4+32*(i+1)] }
	n := func(i int) *big.Int { return new(big.Int).SetBytes(w(i)) }
	u := func(i int) uint64 {
		if !n(i).IsUint64() {
			return 0
		}
		return n(i).Uint64()
	}
	sp := SpendPermission{Account: checksum(w(2)[12:]), Spender: checksum(w(3)[12:]), Token: checksum(w(4)[12:]), Allowance: n(5),
		Period: u(6), Start: u(7), End: u(8), Salt: n(9)}
	amount := n(1)
	pay := Payment{Token: sp.Token, Amount: amount}
	if sp.Token == Native {
		pay.To = Checksum(calls[1].To)
	} else {
		out, err := hex.DecodeString(strings.TrimPrefix(calls[1].Data, "0x"))
		if err != nil || len(out) != 4+64 {
			return SpendPermission{}, Payment{}, notPayment
		}
		pay.To = checksum(out[16:36])
	}
	want, err := c.SpendCalls(sp, pay.To, amount)
	if err != nil || !strings.EqualFold(sp.Spender, spender) || len(want) != 2 {
		return SpendPermission{}, Payment{}, notPayment
	}
	for i := range want { // only the calls' one exact form is read
		if !strings.EqualFold(want[i].To, calls[i].To) || want[i].Value != calls[i].Value || !strings.EqualFold(want[i].Data, calls[i].Data) {
			return SpendPermission{}, Payment{}, notPayment
		}
	}
	return sp, pay, nil
}

// SendSpendAsAgent is how Talyvor has the agent's key spend: calls must be one payment from the agent's own account
// spender under a spend permission, and authorise — the owner's permission, through Talyvor's gate — must allow it,
// before the provider is asked to send them; the provider's policy and then the manager on chain judge them again.
func (c Chain) SendSpendAsAgent(ctx context.Context, s money.Spenders, key, spender string, calls []money.Call, authorise func(Payment) error) (money.Result, error) {
	_, pay, err := c.SpendPaymentOf(spender, calls)
	if err != nil {
		return money.Result{}, err
	}
	if err := authorise(pay); err != nil {
		return money.Result{}, err
	}
	return s.SendCalls(ctx, key, spender, calls)
}

// ProviderPolicy is the provider's policy for the agent's key that holds what the chain cannot: p's cap on one payment
// and its counterparties allowed and blocked. Its key's user operations may call the chain's manager, pay the chain's
// coin up to the cap and transfer an allowed token up to its cap; a call to spender itself (whose executeBatch would
// make calls the engine never reads), a blocked counterparty, or one not allowed when p names those allowed, is
// refused, and so is every other call.
//
// shortcut: the cap is on each call that pays, so one user operation with several payments can pay more than the cap
// in all, up to the period's allowance the chain holds. Upgrade with a criterion on an operation's total, if the
// provider offers one.
func (c Chain) ProviderPolicy(p Permission, spender string) money.Policy {
	const op = "sendUserOperation"
	pol := money.Policy{Scope: "account", Description: "Talyvor: the owner's spend permission", Rules: []money.PolicyRule{{Action: "reject",
		Operation: op, Criteria: []money.PolicyCriterion{{Type: "evmAddress", Operator: "in", Addresses: []string{Checksum(spender)}}}}}}
	if len(p.Blocked) > 0 {
		pol.Rules = append(pol.Rules,
			money.PolicyRule{Action: "reject", Operation: op, Criteria: []money.PolicyCriterion{{Type: "evmAddress", Operator: "in", Addresses: p.Blocked}}},
			money.PolicyRule{Action: "reject", Operation: op, Criteria: []money.PolicyCriterion{{Type: "evmData", ABI: "erc20",
				Conditions: []money.PolicyCondition{{Function: "transfer", Params: []money.PolicyParam{{Name: "to", Operator: "in", Values: p.Blocked}}}}}}})
	}
	pol.Rules = append(pol.Rules, money.PolicyRule{Action: "accept", Operation: op,
		Criteria: []money.PolicyCriterion{{Type: "evmAddress", Operator: "in", Addresses: []string{c.manager()}}, {Type: "ethValue", Operator: "<=", EthValue: "0"}}})
	for _, l := range p.Tokens {
		limit := fmt.Sprint(l.PerPayment)
		if Checksum(l.Token) == Native {
			r := money.PolicyRule{Action: "accept", Operation: op, Criteria: []money.PolicyCriterion{
				{Type: "ethValue", Operator: ">", EthValue: "0"}, {Type: "ethValue", Operator: "<=", EthValue: limit}}}
			if len(p.Allowed) > 0 {
				r.Criteria = append(r.Criteria, money.PolicyCriterion{Type: "evmAddress", Operator: "in", Addresses: p.Allowed})
			}
			pol.Rules = append(pol.Rules, r)
			continue
		}
		params := []money.PolicyParam{{Name: "value", Operator: "<=", Value: limit}}
		if len(p.Allowed) > 0 {
			params = append(params, money.PolicyParam{Name: "to", Operator: "in", Values: p.Allowed})
		}
		pol.Rules = append(pol.Rules, money.PolicyRule{Action: "accept", Operation: op, Criteria: []money.PolicyCriterion{
			{Type: "evmAddress", Operator: "in", Addresses: []string{Checksum(l.Token)}}, {Type: "ethValue", Operator: "<=", EthValue: "0"},
			{Type: "evmData", ABI: "erc20", Conditions: []money.PolicyCondition{{Function: "transfer", Params: params}}}}})
	}
	return pol
}
