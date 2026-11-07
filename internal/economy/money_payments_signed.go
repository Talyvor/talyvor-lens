package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/talyvor/lens/internal/passkey"
)

// money_payments_signed.go — B54.33: A PERSON'S PAYMENT IS SIGNED, OVER THAT PAYMENT.
//
// Once a workspace has a passkey or a device key (the phone app's, behind Face ID), a payment its owner or an admin
// makes is paid only with a signature by one of them over PaymentChallenge's challenge, which names exactly that
// payment: its account, payee, amount, currency, funding, reference, quote and idempotency key. A session alone pays
// nothing. The challenge is spent in the transaction that keeps the payment, so a refused one can be signed again, and
// the same request again answers the payment it made. A revoked device key still counts, as for approvals. An agent's
// own key pays as before, judged by its rules.

// ErrPaymentUnsigned: the workspace signs its payments and this one carried no signature.
var ErrPaymentUnsigned = errors.New("economy: this workspace signs its payments with a passkey or the phone app — sign this payment's challenge (POST /v1/money/payments/challenge)")

// paymentPurpose binds a challenge to one payment exactly as asked, so a signature for it pays no other.
func paymentPurpose(in OutsidePayment) string {
	h := sha256.Sum256([]byte(strings.Join([]string{in.AccountID, in.PayeeID, strconv.FormatInt(in.AmountMinor, 10), in.Currency,
		in.Funding, in.Reference, in.QuoteID, in.IdempotencyKey}, "\x00")))
	return "payment_out:" + hex.EncodeToString(h[:])
}

// PaymentChallenge issues the challenge paying in is signed over, and the workspace's passkeys that may sign it; its
// device keys sign it too.
func (s *DualTokenStore) PaymentChallenge(ctx context.Context, workspaceID string, in OutsidePayment) (challenge string, credentialIDs []string, err error) {
	if err := in.normalise(true); err != nil {
		return "", nil, err
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c, err := s.newChallenge(ctx, workspaceID, paymentPurpose(in))
	return c, credentialIDs, err
}

// PayOutsideSigned is PayOutside for a person paying, with a the signature over PaymentChallenge's challenge for in.
func (s *DualTokenStore) PayOutsideSigned(ctx context.Context, workspaceID string, in OutsidePayment, a *passkey.Assertion) (OutsidePayment, error) {
	in.signed, in.assertion = true, a
	return s.PayOutside(ctx, workspaceID, in)
}

// checkPaymentSigned refuses a person's payment without the signature its workspace needs, leaving the challenge open.
func (s *DualTokenStore) checkPaymentSigned(ctx context.Context, workspaceID string, in OutsidePayment) error {
	if !in.signed {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return s.authorizeSignedTx(ctx, tx, workspaceID, paymentPurpose(in), in.assertion, ErrPaymentUnsigned)
}
