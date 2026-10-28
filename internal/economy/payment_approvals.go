package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/passkey"
)

// payment_approvals.go — B40.5: ONE APPROVAL FOR A WHOLE BATCH, THEN EACH ITEM PAID EXACTLY ONCE.
//
// The person approves a prepared request (B40.3) once, for the whole batch: PrepareRequestApproval writes the
// statement they sign — the request's frozen payload hash, each item approved (payee, amount, reference), the totals
// and an expiry — and they may leave items out first. ApprovePaymentRequest takes the statement back with a passkey or
// device key signature over its SHA-256 (as a mandate is signed, B40.2), and keeps it only if it is still exactly the
// request as frozen: any amount or payee changed since makes it invalid (SCA-RTS art 5(1), 5(3)(b)). A request is
// approved at most once. PayPaymentRequest then pays each approved item, and each item B40.3 judged auto under a
// signed mandate (the mandate is its authority, checked again), as a payment out (B30.17, the B30.21 engine) under
// its own idempotency key, so no item is ever paid twice. Every step goes on the request's hash chain (audit_chain.go).
// Payments out are RED: test money only.

// ErrPaymentRequestApproved: the request was approved already; approving it again pays nothing.
var ErrPaymentRequestApproved = errors.New("economy: this payment request was already approved: nothing is paid again")

// ApprovalStatementVersion names the statement form signed.
const ApprovalStatementVersion = "talyvor.payment_approval.v1"

// approvalStatementTTL is how long a statement may be signed and sent back.
const approvalStatementTTL = 10 * time.Minute

// ApprovalStatementItem is one item as the person approves it.
type ApprovalStatementItem struct {
	Position    int    `json:"position"`
	PayeeID     string `json:"payee_id"`
	PayeeName   string `json:"payee_name"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	Reference   string `json:"reference"`
	// B41.103: the flags the person approves the item despite — a duplicate or a changed bank detail.
	Overrides []string `json:"overrides,omitempty"`
}

// ApprovalStatement is what the person signs.
type ApprovalStatement struct {
	Version     string                  `json:"version"`
	RequestID   string                  `json:"request_id"`
	WorkspaceID string                  `json:"workspace_id"`
	PayloadHash string                  `json:"payload_hash"`
	Items       []ApprovalStatementItem `json:"items"`
	Totals      map[string]int64        `json:"totals"`
	// B41.103: why the person approves the flagged items it overrides; required when any item has overrides.
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// frozenIntact says whether r's items still hash to the payload frozen when it was prepared.
func frozenIntact(workspaceID string, r PaymentRequest) bool {
	c := canonicalRequest{Version: PaymentRequestVersion, ID: r.ID, WorkspaceID: workspaceID, Kind: r.Kind, Items: []canonicalItem{}}
	for _, it := range r.Items {
		c.Items = append(c.Items, canonicalItem{it.PayeeID, it.ListingID, it.Currency, it.AmountMinor, it.Reference, it.BillID})
	}
	b, _ := json.Marshal(c)
	return string(b) == r.Canonical && sha256Hex(r.Canonical) == r.PayloadHash
}

// statementFor is the statement approving positions of r: those that need approval, all of them when none is named;
// reason is why flagged items are approved (B41.103).
func statementFor(workspaceID string, r PaymentRequest, positions []int, reason string, expires time.Time) (ApprovalStatement, error) {
	st := ApprovalStatement{Version: ApprovalStatementVersion, RequestID: r.ID, WorkspaceID: workspaceID, PayloadHash: r.PayloadHash,
		Items: []ApprovalStatementItem{}, Totals: map[string]int64{}, Reason: reason, ExpiresAt: expires}
	for _, it := range r.Items {
		named := slices.Contains(positions, it.Position)
		if it.Decision != ItemNeedsApproval {
			if named {
				return st, fmt.Errorf("%w: item %d is %s: only an item that needs approval is approved", ErrPaymentRequest, it.Position, it.Decision)
			}
			continue
		}
		if len(positions) > 0 && !named {
			continue // the person left it out
		}
		st.Items = append(st.Items, ApprovalStatementItem{it.Position, it.PayeeID, it.PayeeName, it.Currency, it.AmountMinor, it.Reference,
			reasonFlags(it.Flags)})
		st.Totals[it.Currency] += it.AmountMinor
	}
	for _, p := range positions {
		if p < 1 || p > len(r.Items) {
			return st, fmt.Errorf("%w: the request has no item %d", ErrPaymentRequest, p)
		}
	}
	if len(st.Items) == 0 {
		return st, fmt.Errorf("%w: no item of this request needs approval", ErrPaymentRequest)
	}
	return st, approvalReason(st)
}

// approvable reads workspaceID's request id and refuses one that cannot be approved now.
func (s *DualTokenStore) approvable(ctx context.Context, workspaceID, id string) (PaymentRequest, error) {
	r, err := s.PaymentRequest(ctx, workspaceID, "", id)
	switch {
	case err != nil:
		return r, err
	case r.Status == "approved":
		return r, ErrPaymentRequestApproved
	case r.Status != "pending_approval":
		return r, fmt.Errorf("%w: the request is %s: there is nothing to approve", ErrPaymentRequest, r.Status)
	case time.Since(r.CreatedAt) >= s.approvalLife():
		return r, fmt.Errorf("%w: the request waited %s unanswered and expired: ask again", ErrPaymentRequest, s.approvalLife())
	case !frozenIntact(workspaceID, r):
		return r, fmt.Errorf("%w: the request was changed after it was frozen: it cannot be approved", ErrPaymentRequest)
	}
	return r, nil
}

// PrepareRequestApproval writes the statement approving positions of workspaceID's request id (every item that needs
// approval when none is named), the challenge it is signed over, and the passkeys that may sign it. reason is why
// items flagged a duplicate or a changed bank detail are approved: without one they cannot be (B41.103).
func (s *DualTokenStore) PrepareRequestApproval(ctx context.Context, workspaceID, id string, positions []int, reason string) (statement, challenge string, credentialIDs []string, err error) {
	r, err := s.approvable(ctx, workspaceID, id)
	if err != nil {
		return "", "", nil, err
	}
	st, err := statementFor(workspaceID, r, positions, strings.TrimSpace(reason), time.Now().Add(approvalStatementTTL).UTC().Truncate(time.Second))
	if err != nil {
		return "", "", nil, err
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", "", nil, err
	}
	var devices int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1 AND revoked_at IS NULL`,
		workspaceID).Scan(&devices); err != nil {
		return "", "", nil, fmt.Errorf("economy: device keys: %w", err)
	}
	if len(keys) == 0 && devices == 0 {
		return "", "", nil, fmt.Errorf("%w: a payment is approved with a passkey or the phone app's device key, and this workspace has neither: register one first", ErrPaymentRequest)
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	b, _ := json.Marshal(st)
	return string(b), MandateChallenge(string(b)), credentialIDs, nil
}

// signOver checks a, by one of workspaceID's passkeys or live device keys, is a signature over challenge, inside tx.
func (s *DualTokenStore) signOver(ctx context.Context, tx pgx.Tx, workspaceID, challenge string, a passkey.Assertion) error {
	if a.DeviceID != "" {
		return signDevice(ctx, tx, workspaceID, challenge, a)
	}
	if s.approvalRP == nil {
		return fmt.Errorf("%w: passkeys are not configured on this server", passkey.ErrInvalid)
	}
	var spki []byte
	var count int64
	err := tx.QueryRow(ctx, `SELECT public_key, sign_count FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 FOR UPDATE`,
		a.CredentialID, workspaceID).Scan(&spki, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: not a passkey of this workspace", passkey.ErrInvalid)
	}
	if err != nil {
		return fmt.Errorf("economy: passkey: %w", err)
	}
	next, err := s.approvalRP.VerifyAssertion(a, challenge, spki, uint32(count))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_passkeys SET sign_count = $2, last_used_at = now() WHERE credential_id = $1`,
		a.CredentialID, int64(next)); err != nil {
		return fmt.Errorf("economy: passkey: %w", err)
	}
	return nil
}

// ApprovePaymentRequest keeps by's approval of workspaceID's request id — statement as PrepareRequestApproval wrote
// it, signed by a — and then pays it (PayPaymentRequest). A statement altered, expired, another request's, or no longer
// the request as it stands is refused; a second approval is ErrPaymentRequestApproved and pays nothing.
func (s *DualTokenStore) ApprovePaymentRequest(ctx context.Context, workspaceID, id, by, statement string, a *passkey.Assertion) (PaymentRequest, error) {
	var st ApprovalStatement
	dec := json.NewDecoder(strings.NewReader(statement))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return PaymentRequest{}, fmt.Errorf("%w: the statement is not an approval statement: %v", ErrPaymentRequest, err)
	}
	if b, _ := json.Marshal(st); string(b) != statement || st.Version != ApprovalStatementVersion || st.WorkspaceID != workspaceID || st.RequestID != id {
		return PaymentRequest{}, fmt.Errorf("%w: the statement is not this request's, as prepared for signing", ErrPaymentRequest)
	}
	if !time.Now().Before(st.ExpiresAt) {
		return PaymentRequest{}, fmt.Errorf("%w: the statement expired at %s: prepare the approval again", ErrPaymentRequest, st.ExpiresAt.Format(time.RFC3339))
	}
	if a == nil {
		return PaymentRequest{}, fmt.Errorf("%w: the approval is not signed: sign its challenge with a passkey or the phone app", ErrPaymentRequest)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaymentRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The request's row is held until the approval is kept: two approvals at once are one, then a replay.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM payment_requests WHERE id = $1 AND workspace_id = $2 FOR UPDATE`, id, workspaceID); err != nil {
		return PaymentRequest{}, fmt.Errorf("economy: payment request: %w", err)
	}
	r, err := s.approvable(ctx, workspaceID, id)
	if err != nil {
		return r, err
	}
	positions := []int{}
	for _, it := range st.Items {
		positions = append(positions, it.Position)
	}
	want, err := statementFor(workspaceID, r, positions, st.Reason, st.ExpiresAt)
	if err != nil {
		return r, err
	}
	if b, _ := json.Marshal(want); string(b) != statement {
		return r, fmt.Errorf("%w: the request is no longer what the statement shows: the approval is invalid", ErrPaymentRequest)
	}
	if err := s.signOver(ctx, tx, workspaceID, MandateChallenge(statement), *a); err != nil {
		return r, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_request_approvals (request_id, workspace_id, statement, credential_id, device_id, client_data_json,
		authenticator_data, signature, approved_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, workspaceID, statement, a.CredentialID, a.DeviceID, a.ClientDataJSON, a.AuthenticatorData, a.Signature, by)
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
		return r, ErrPaymentRequestApproved
	}
	if err != nil {
		return r, fmt.Errorf("economy: approval: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_requests SET status = 'approved' WHERE id = $1`, id); err != nil {
		return r, fmt.Errorf("economy: approval: %w", err)
	}
	if err := appendAudit(ctx, tx, workspaceID, id, "shown", map[string]any{"statement": statement, "challenge": MandateChallenge(statement)}); err != nil {
		return r, err
	}
	if err := appendAudit(ctx, tx, workspaceID, id, "signature", map[string]any{"by": by, "credential_id": a.CredentialID, "device_id": a.DeviceID,
		"client_data_json": a.ClientDataJSON, "authenticator_data": a.AuthenticatorData, "signature": a.Signature}); err != nil {
		return r, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, err
	}
	return s.PayPaymentRequest(ctx, workspaceID, "", id)
}

// paymentRequestItemKey is the idempotency key item position of request id is paid under, its only one.
func paymentRequestItemKey(id string, position int) string {
	return "preq:" + id + ":" + strconv.Itoa(position)
}

// Where an item stands once paying it was tried, besides a payment's own states.
const (
	PayNotApproved = "not_approved" // the person left it out of the approval
	PayRefused     = "refused"      // its mandate no longer lets it be paid
	PayNotPaidHere = "not_paid"     // a marketplace purchase: not paid from a request yet
	PayFailed      = "failed"       // paying it failed before any money moved; trying again uses the same key
)

// PayPaymentRequest pays what workspaceID's request id may pay now — agentID, when not "", only that agent's
// request: each item the person approved, and each auto item whose mandate still allows it. An item paid, left out
// or refused is not tried again; one that failed is, under the same key.
func (s *DualTokenStore) PayPaymentRequest(ctx context.Context, workspaceID, agentID, id string) (PaymentRequest, error) {
	ctx = context.WithoutCancel(ctx) // a caller that goes away mid-batch does not leave items unrecorded
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PaymentRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One payer of a request at a time: a second waits, then finds each item already paid.
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM payment_requests WHERE id = $1 AND workspace_id = $2 AND ($3 = '' OR agent_id = $3) FOR UPDATE`,
		id, workspaceID, agentID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentRequest{}, ErrPaymentRequestNotFound
	}
	if err != nil {
		return PaymentRequest{}, fmt.Errorf("economy: payment request: %w", err)
	}
	r, err := s.PaymentRequest(ctx, workspaceID, agentID, id)
	if err != nil {
		return r, err
	}
	if status == "cancelled" {
		return r, fmt.Errorf("%w: the request was cancelled: nothing is paid", ErrPaymentRequest)
	}
	if !frozenIntact(workspaceID, r) {
		return r, fmt.Errorf("%w: the request was changed after it was frozen: nothing is paid", ErrPaymentRequest)
	}
	approved := map[int]ApprovalStatementItem{}
	var lapsed bool // the approval is past its life (B50.18): an approved item not paid yet is not paid on it
	if status == "approved" {
		var statement string
		var approvedAt time.Time
		if err := tx.QueryRow(ctx, `SELECT statement, approved_at FROM payment_request_approvals WHERE request_id = $1`, id).Scan(&statement, &approvedAt); err != nil {
			return r, fmt.Errorf("economy: approval: %w", err)
		}
		var st ApprovalStatement
		if err := json.Unmarshal([]byte(statement), &st); err != nil || st.PayloadHash != r.PayloadHash {
			return r, fmt.Errorf("%w: the approval kept is not this request's: nothing is paid", ErrPaymentRequest)
		}
		for _, it := range st.Items {
			approved[it.Position] = it
		}
		lapsed = time.Since(approvedAt) >= s.approvalLife()
	}

	for _, it := range r.Items {
		if it.PaymentID != "" || slices.Contains([]string{PayNotApproved, PayRefused, PayNotPaidHere}, it.PayStatus) {
			continue
		}
		var authority, payStatus, detail, paymentID string
		var call map[string]any
		signed, isApproved := approved[it.Position]
		switch {
		case it.Decision == ItemAuto:
			authority = "mandate:" + it.MandateID
			d, err := s.EvaluateMandatePayment(ctx, workspaceID, it.MandateID, PreparedPayment{Client: r.Client, AgentID: r.AgentID,
				PayeeID: it.PayeeID, SellerID: it.PayeeID, Currency: it.Currency, AmountMinor: it.AmountMinor, BatchTotalMinor: it.AmountMinor})
			if err != nil && !errors.Is(err, ErrPermissionMandateNotFound) {
				return r, err
			}
			if err != nil || d.Decision == MandateBlock {
				payStatus, detail = PayRefused, "the mandate no longer allows it: "+d.Reason
			}
		case it.Decision == ItemNeedsApproval && isApproved && lapsed:
			authority, payStatus, detail = "approval", PayRefused, "the approval expired, ask again"
		case it.Decision == ItemNeedsApproval && isApproved:
			authority = "approval"
			if signed.AmountMinor != it.AmountMinor || signed.PayeeID != it.PayeeID || signed.Currency != it.Currency || signed.Reference != it.Reference {
				return r, fmt.Errorf("%w: item %d is not what was approved: nothing is paid", ErrPaymentRequest, it.Position)
			}
		case it.Decision == ItemNeedsApproval && status == "approved":
			payStatus, detail = PayNotApproved, "left out of the approval"
		default:
			continue // blocked, or waiting for the person's approval
		}
		if payStatus == "" && it.ListingID != "" {
			payStatus, detail = PayNotPaidHere, "a marketplace purchase is not paid from a request yet"
		}
		if payStatus == "" {
			key := paymentRequestItemKey(id, it.Position)
			p, err := s.PayOutside(ctx, workspaceID, OutsidePayment{AgentID: r.AgentID, PayeeID: it.PayeeID, AmountMinor: it.AmountMinor,
				Currency: it.Currency, Funding: FundingTest, Reference: it.Reference, IdempotencyKey: key})
			call = map[string]any{"position": it.Position, "authority": authority, "idempotency_key": key}
			if err != nil {
				payStatus, detail, call["error"] = PayFailed, err.Error(), err.Error()
			} else {
				paymentID, payStatus, detail = p.ID, p.Status, p.Detail
				// shortcut: a payment returned later leaves its bill paid; reopen the bill once returns are reconciled.
				if it.BillID != "" && p.Status != PaymentFailed {
					if _, err := tx.Exec(ctx, `UPDATE money_bills SET status = 'paid' WHERE id = $1 AND workspace_id = $2`, it.BillID, workspaceID); err != nil {
						return r, fmt.Errorf("economy: bill paid: %w", err)
					}
				}
				call["payment_id"], call["partner"], call["partner_ref"], call["status"], call["detail"] = p.ID, p.Partner, p.PartnerRef, p.Status, p.Detail
			}
		}
		if call == nil {
			call = map[string]any{"position": it.Position, "authority": authority, "status": payStatus, "detail": detail}
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_request_items SET payment_id = $3, pay_status = $4, pay_detail = $5, authority = $6
			WHERE request_id = $1 AND position = $2`, id, it.Position, paymentID, payStatus, detail, authority); err != nil {
			return r, fmt.Errorf("economy: payment request item: %w", err)
		}
		kind := "provider_call"
		if paymentID == "" && payStatus != PayFailed {
			kind = "not_paid"
		}
		if err := appendAudit(ctx, tx, workspaceID, id, kind, call); err != nil {
			return r, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return r, err
	}
	return s.PaymentRequest(ctx, workspaceID, agentID, id)
}
