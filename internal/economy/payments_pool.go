package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// payments_pool.go — B63.1: THE PAYMENTS POOL, MEASURED ONLY.
//
// Once a Sandbox payment out has settled (completed, in applyPartnerResult) or a Sandbox payment to a person has
// committed (PayPerson), the payments pool is told which payment it was, which account paid it and who it paid — the
// payee by a hash of its own details, never its name. It is told after the payment's transaction commits, on its own
// goroutine with its own deadline, so measuring never fails, delays or changes the payment: a measure that cannot be
// written is logged and the payment is as it was.

// The kinds of payment the payments pool measures.
const (
	PoolPaymentOut    = "payment_out"
	PoolPersonPayment = "person_payment"
)

// SettledPayment is a settled payment as the payments pool measures it.
type SettledPayment struct {
	Kind        string // PoolPaymentOut or PoolPersonPayment
	ID          string // the payment out's id, or the payment to a person's money entry
	WorkspaceID string // the account that paid it
	PayeeHash   string // the payee's own details, hashed (PayeeDetailsHash, PersonPayeeHash)
	Inside      bool   // the payee holds a Talyvor account, so no outside bank or card carried it
	Currency    string
}

// PaymentsPool measures settled payments (internal/paypool). It moves no money.
type PaymentsPool interface {
	Measure(ctx context.Context, p SettledPayment) error
}

// SetPaymentsPool has pool told of each settled Sandbox payment out and payment to a person; nil, none is measured.
func (s *DualTokenStore) SetPaymentsPool(pool PaymentsPool) {
	s.paymentsPool, s.poolMeasures = pool, &sync.WaitGroup{}
}

// WaitPaymentsPool waits until every measure already begun is written or given up.
func (s *DualTokenStore) WaitPaymentsPool() {
	if s.poolMeasures != nil {
		s.poolMeasures.Wait()
	}
}

// PayeeDetailsHash is an outside payee by its own details — sort code and account number, IBAN, routing and account
// number, or network and address — so two accounts' payees with the same details are one payee whatever they named it.
func PayeeDetailsHash(p OutsidePayee) string {
	h := sha256.Sum256([]byte("talyvor-payments-pool\x00" + strings.Join([]string{p.SortCode, p.AccountNumber, p.IBAN, p.RoutingNumber,
		p.Network, p.Address}, "|")))
	return hex.EncodeToString(h[:])
}

// PersonPayeeHash is a person on Talyvor as a payee.
func PersonPayeeHash(personID string) string {
	h := sha256.Sum256([]byte("talyvor-payments-pool\x00person\x00" + personID))
	return hex.EncodeToString(h[:])
}

// measurePayment tells the payments pool of p, a payment of funding money that has settled, on its own goroutine.
// payee, when p has no PayeeHash yet, reads it.
func (s *DualTokenStore) measurePayment(ctx context.Context, funding string, p SettledPayment, payee func(context.Context) (string, error)) {
	if s.paymentsPool == nil || s.poolMeasures == nil || funding != FundingTest {
		return
	}
	s.poolMeasures.Add(1)
	go func() {
		defer s.poolMeasures.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("economy: the payments pool did not measure a payment", "payment_id", p.ID, "panic", r)
			}
		}()
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if payee != nil {
			h, err := payee(mctx)
			if err != nil {
				slog.Warn("economy: the payments pool did not read a payment's payee", "payment_id", p.ID, "err", err)
				return
			}
			p.PayeeHash = h
		}
		if err := s.paymentsPool.Measure(mctx, p); err != nil {
			slog.Warn("economy: the payments pool did not measure a payment", "payment_id", p.ID, "err", err)
		}
	}()
}

// measurePaymentOut measures p, a payment out that has just completed.
func (s *DualTokenStore) measurePaymentOut(ctx context.Context, p OutsidePayment) {
	s.measurePayment(ctx, p.Funding, SettledPayment{Kind: PoolPaymentOut, ID: p.ID, WorkspaceID: p.WorkspaceID, Currency: p.Currency},
		func(ctx context.Context) (string, error) {
			payee, err := s.outsidePayee(ctx, s.pool, p.WorkspaceID, p.PayeeID, "")
			if err != nil {
				return "", err
			}
			return PayeeDetailsHash(payee), nil
		})
}
