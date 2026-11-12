package money

import (
	"context"
	"errors"
	"fmt"
)

// gate is the one way into an adapter: it checks the request, asks the owner's limits before money moves out, makes
// the call, and records it in the audit trail.
type gate struct {
	r       *Registry
	partner string
	rt      Route
	c       Capability
}

func (r *Registry) gate(a *Adapter, rt Route, c Capability) gate {
	return gate{r: r, partner: a.Name, rt: rt, c: c}
}

// call is one call through g. A write carries a key; money out carries the amount and the payee.
type call struct {
	method string
	key    string
	ref    string
	out    *Amount // set when the call moves money out
	payee  string
	to     Payee
	write  bool
}

// through runs fn as c: checked, judged by the owner's limits when it moves money out, and audited however it ends.
// A call whose audit link cannot be written answers that error — it is idempotent on its key, so asking again is safe.
func through[T any](ctx context.Context, g gate, c call, fn func() (T, error)) (T, error) {
	var zero T
	if c.write {
		if err := checkKey(c.key); err != nil {
			return zero, err
		}
	}
	op := Op{Route: g.rt, Partner: g.partner, Capability: g.c, Method: c.method, Key: c.key, Ref: c.ref, Payee: c.payee, To: c.to}
	if c.out != nil {
		if err := c.out.check(); err != nil {
			return zero, err
		}
		op.Amount = *c.out
		if err := g.r.cfg.Limits.Allow(ctx, op); err != nil {
			if !errors.Is(err, ErrLimit) && !errors.Is(err, ErrApprovalNeeded) {
				err = fmt.Errorf("%w: the owner's limits could not be read: %v", ErrLimit, err)
			}
			return zero, g.audit(ctx, op, nil, err)
		}
	}
	res, err := fn()
	return res, g.audit(ctx, op, res, err)
}

// audit appends op and how it ended to the trail, and answers err — or the audit's own error when only that failed.
func (g gate) audit(ctx context.Context, op Op, res any, err error) error {
	body := map[string]any{"partner": op.Partner, "capability": string(op.Capability), "method": op.Method,
		"key": op.Key, "country": op.Country, "wallet_capability": op.Route.Capability, "outcome": Outcome(err)}
	if op.Agent != "" {
		body["agent_id"] = op.Agent
	}
	if op.Ref != "" {
		body["ref"] = op.Ref
	}
	if op.Amount.Asset != "" {
		body["amount_minor"], body["asset"] = op.Amount.Minor, op.Amount.Asset
	}
	if ref, status := refOf(res); ref != "" {
		body["partner_ref"], body["status"] = ref, status
	}
	subject := op.Key
	if subject == "" {
		subject = op.Ref
	}
	aerr := g.r.cfg.Audit.AppendAudit(ctx, op.Workspace, "money:"+subject, "money."+string(op.Capability)+"."+op.Method, body)
	if err != nil {
		return err
	}
	if aerr != nil {
		return fmt.Errorf("money: the call was made but its audit link was not written: %w", aerr)
	}
	return nil
}

// Outcome is how a call that answered err ended, in one word: ok, or the kind of its error.
func Outcome(err error) string {
	for _, k := range []struct {
		err  error
		name string
	}{{ErrInsufficientFunds, "insufficient_funds"}, {ErrLimit, "limit"}, {ErrApprovalNeeded, "approval_needed"},
		{ErrPartnerDown, "partner_down"}, {ErrRefused, "refused"}, {ErrHeld, "held"}, {ErrInvalid, "invalid"}, {ErrNotFound, "not_found"},
		{ErrKeyReused, "key_reused"}} {
		if errors.Is(err, k.err) {
			return k.name
		}
	}
	if err != nil {
		return "error"
	}
	return "ok"
}

// refOf is the partner's reference and status in a call's answer, when it has them.
func refOf(res any) (string, string) {
	switch v := res.(type) {
	case Result:
		return v.Ref, string(v.Status)
	case Account:
		return v.Ref, string(v.Status)
	case Verification:
		return v.Ref, string(v.Status)
	case Wallet:
		return v.Ref, ""
	case Card:
		return v.Ref, v.State
	case ConvertQuote:
		return v.Ref, ""
	}
	return "", ""
}

type gatedAccounts struct {
	g gate
	a Accounts
}

func (w gatedAccounts) Open(ctx context.Context, req OpenAccount) (Account, error) {
	return through(ctx, w.g, call{method: "open", key: req.Key, write: true}, func() (Account, error) { return w.a.Open(ctx, req) })
}

func (w gatedAccounts) Close(ctx context.Context, key, ref string) (Result, error) {
	return through(ctx, w.g, call{method: "close", key: key, ref: ref, write: true}, func() (Result, error) { return w.a.Close(ctx, key, ref) })
}

func (w gatedAccounts) Details(ctx context.Context, ref string) (Details, error) {
	return through(ctx, w.g, call{method: "details", ref: ref}, func() (Details, error) { return w.a.Details(ctx, ref) })
}

type gatedWallets struct {
	g gate
	a Wallets
}

func (w gatedWallets) Create(ctx context.Context, req CreateWallet) (Wallet, error) {
	return through(ctx, w.g, call{method: "create", key: req.Key, write: true}, func() (Wallet, error) { return w.a.Create(ctx, req) })
}

func (w gatedWallets) Balance(ctx context.Context, ref string) (Amount, error) {
	return through(ctx, w.g, call{method: "balance", ref: ref}, func() (Amount, error) { return w.a.Balance(ctx, ref) })
}

func (w gatedWallets) Receive(ctx context.Context, ref string) (Details, error) {
	return through(ctx, w.g, call{method: "receive", ref: ref}, func() (Details, error) { return w.a.Receive(ctx, ref) })
}

type gatedTransfers struct {
	g gate
	a Transfers
}

func (w gatedTransfers) Internal(ctx context.Context, req Internal) (Result, error) {
	return through(ctx, w.g, call{method: "internal", key: req.Key, ref: req.From, out: &req.Amount, payee: req.To, write: true},
		func() (Result, error) { return w.a.Internal(ctx, req) })
}

func (w gatedTransfers) Outbound(ctx context.Context, req Outbound) (Result, error) {
	return through(ctx, w.g, call{method: "outbound", key: req.Key, ref: req.From, out: &req.Amount, payee: req.Payee.Name, to: req.Payee, write: true},
		func() (Result, error) { return w.a.Outbound(ctx, req) })
}

func (w gatedTransfers) Credit(ctx context.Context, req Credit) (Result, error) {
	if err := req.Amount.check(); err != nil {
		return Result{}, err
	}
	return through(ctx, w.g, call{method: "credit", key: req.Key, ref: req.To, write: true}, func() (Result, error) { return w.a.Credit(ctx, req) })
}

type gatedCards struct {
	g gate
	a Cards
}

// A card's cap is money that may leave, so issuing and locking a card are judged by the owner's limits. A physical
// card ordered with no cap (B53.19) lets nothing leave: Lens declines its every charge, so there is nothing to judge.
func (w gatedCards) Issue(ctx context.Context, req IssueCard) (Card, error) {
	c := call{method: "issue", key: req.Key, ref: req.Wallet, out: &req.Cap, payee: req.Merchant, write: true}
	if req.FormFactor == CardPhysical && req.Cap == (Amount{}) {
		c.out = nil
	}
	return through(ctx, w.g, c, func() (Card, error) { return w.a.Issue(ctx, req) })
}

func (w gatedCards) Lock(ctx context.Context, req LockCard) (Card, error) {
	return through(ctx, w.g, call{method: "lock", key: req.Key, ref: req.Ref, out: &req.Cap, payee: req.Merchant, write: true},
		func() (Card, error) { return w.a.Lock(ctx, req) })
}

func (w gatedCards) Freeze(ctx context.Context, key, ref string) (Card, error) {
	return through(ctx, w.g, call{method: "freeze", key: key, ref: ref, write: true}, func() (Card, error) { return w.a.Freeze(ctx, key, ref) })
}

func (w gatedCards) Close(ctx context.Context, key, ref string) (Card, error) {
	return through(ctx, w.g, call{method: "close", key: key, ref: ref, write: true}, func() (Card, error) { return w.a.Close(ctx, key, ref) })
}

// gatedIssuer is a CardIssuer through the gate: every write audited, and a cap judged by the owner's limits.
type gatedIssuer struct {
	gatedCards
	a CardIssuer
}

func (w gatedIssuer) Unfreeze(ctx context.Context, key, ref string) (Card, error) {
	return through(ctx, w.g, call{method: "unfreeze", key: key, ref: ref, write: true}, func() (Card, error) { return w.a.Unfreeze(ctx, key, ref) })
}

func (w gatedIssuer) Activate(ctx context.Context, key, ref string) (Card, error) {
	return through(ctx, w.g, call{method: "activate", key: key, ref: ref, write: true}, func() (Card, error) { return w.a.Activate(ctx, key, ref) })
}

func (w gatedIssuer) Tokens(ctx context.Context, ref string) ([]WalletToken, error) {
	return through(ctx, w.g, call{method: "tokens", ref: ref}, func() ([]WalletToken, error) { return w.a.Tokens(ctx, ref) })
}

func (w gatedIssuer) SuspendToken(ctx context.Context, key, token string) (WalletToken, error) {
	return through(ctx, w.g, call{method: "suspend_token", key: key, ref: token, write: true},
		func() (WalletToken, error) { return w.a.SuspendToken(ctx, key, token) })
}

func (w gatedIssuer) ResumeToken(ctx context.Context, key, token string) (WalletToken, error) {
	return through(ctx, w.g, call{method: "resume_token", key: key, ref: token, write: true},
		func() (WalletToken, error) { return w.a.ResumeToken(ctx, key, token) })
}

func (w gatedIssuer) DeleteToken(ctx context.Context, key, token string) (WalletToken, error) {
	return through(ctx, w.g, call{method: "delete_token", key: key, ref: token, write: true},
		func() (WalletToken, error) { return w.a.DeleteToken(ctx, key, token) })
}

type gatedPayouts struct {
	g gate
	a Payouts
}

func (w gatedPayouts) Pay(ctx context.Context, req Payout) (Result, error) {
	return through(ctx, w.g, call{method: "pay", key: req.Key, ref: req.From, out: &req.Amount, payee: req.To.Name, to: req.To, write: true},
		func() (Result, error) { return w.a.Pay(ctx, req) })
}

type gatedKYC struct {
	g gate
	a KYC
}

func (w gatedKYC) Start(ctx context.Context, req StartKYC) (Verification, error) {
	return through(ctx, w.g, call{method: "start", key: req.Key, write: true}, func() (Verification, error) { return w.a.Start(ctx, req) })
}

func (w gatedKYC) Status(ctx context.Context, ref string) (Verification, error) {
	return through(ctx, w.g, call{method: "status", ref: ref}, func() (Verification, error) { return w.a.Status(ctx, ref) })
}
