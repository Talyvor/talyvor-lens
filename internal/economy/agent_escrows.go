package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/workspace"
)

// agent_escrows.go — B22.6: ESCROW, MONEY HELD UNTIL THE DEAL IS DONE.
//
// An agent pays into escrow for another agent (PayIntoEscrow): the credits leave the payer and are held until
// they are released to the payee. The payer's side confirms delivery (ConfirmEscrow), or the agreed deadline
// passes without a dispute (RunEscrowDeadlines, on the schedules' tick), and they are released. The payer's
// side may dispute before the deadline (DisputeEscrow): the credits stay held until the operator decides
// (DecideEscrow, `lens escrows`) — released to the payee or returned to the payer.
//
// Held credits are in no workspace's balance. Paying in is ONE agent_postings entry, agent:<payer> −amount and
// escrow:<id> +amount in the payer's workspace, and the LXC leaves the payer's lxc_balances (an lxc_ledger row
// of type LXCTypeAgentEscrow), so neither the payer's agents nor the workspace itself can spend it while it is
// held. Releasing is escrow:<id> −amount and agent:<payee> +amount, the LXC arriving in the payee's workspace;
// returning is the same back to the payer. Test-funded credits come out test-funded wherever they go.
//
// The payer's rules (B19.2) judge paying in as spending and both owners must be verified (B19.11), as for a
// transfer (B22.3). Between one owner's agents escrow is GREEN; between different owners it is class AMBER —
// test-funded credits only until the operator clears escrow. Both statements carry every escrow and its states.

// LXCTypeAgentEscrow marks credits leaving a workspace into escrow, or reaching one out of it.
const LXCTypeAgentEscrow = "agent_escrow"

// CapabilityEscrow is the wallet capability escrow between different owners moves money as.
const CapabilityEscrow = "escrow"

// ErrEscrowNotFound: no such escrow this workspace can act on in its state.
var ErrEscrowNotFound = errors.New("economy: no such escrow")

// ErrEscrowTerms: an escrow, a dispute or a decision that cannot be made as asked.
var ErrEscrowTerms = errors.New("economy: escrow")

// EscrowEvent is one state an escrow passed into.
type EscrowEvent struct {
	Kind     string    `json:"kind"`  // held | disputed | released | returned
	Actor    string    `json:"actor"` // payer | deadline | operator
	Operator string    `json:"operator,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	EntryID  string    `json:"entry_id,omitempty"` // the agent_postings entry that moved the credits
	At       time.Time `json:"at"`
}

// Escrow is credits one agent paid into escrow for another, as both sides see it.
type Escrow struct {
	ID               string        `json:"id"`
	PayerWorkspaceID string        `json:"payer_workspace_id"`
	PayerAgentID     string        `json:"payer_agent_id"`
	PayeeWorkspaceID string        `json:"payee_workspace_id"`
	PayeeAgentID     string        `json:"payee_agent_id"`
	AmountULXC       int64         `json:"amount_ulxc"`
	Memo             string        `json:"memo,omitempty"`
	Class            string        `json:"class"`
	TestFundedULXC   int64         `json:"test_funded_ulxc"`
	ReleaseAt        time.Time     `json:"release_at"`
	Status           string        `json:"status"` // held | disputed | released | returned
	CreatedAt        time.Time     `json:"created_at"`
	DecidedAt        *time.Time    `json:"decided_at,omitempty"`
	Events           []EscrowEvent `json:"events"`
}

func escrowAccount(id string) string { return "escrow:" + id }

const escrowColumns = `id, payer_workspace_id, payer_agent_id, payee_workspace_id, payee_agent_id, amount_ulxc, memo, class,
	test_funded_ulxc, release_at, status, created_at, decided_at`

func scanEscrow(row pgx.Row) (Escrow, error) {
	var e Escrow
	err := row.Scan(&e.ID, &e.PayerWorkspaceID, &e.PayerAgentID, &e.PayeeWorkspaceID, &e.PayeeAgentID, &e.AmountULXC, &e.Memo, &e.Class,
		&e.TestFundedULXC, &e.ReleaseAt, &e.Status, &e.CreatedAt, &e.DecidedAt)
	e.Events = []EscrowEvent{}
	return e, err
}

// PayIntoEscrow pays amount µLXC from workspaceID's agent payerAgentID into escrow for the agent at
// payeeAddress, released to it on confirmation or at releaseAt.
func (s *DualTokenStore) PayIntoEscrow(ctx context.Context, workspaceID, payerAgentID, payeeAddress string, amount int64, memo string,
	releaseAt time.Time) (Escrow, error) {
	switch {
	case amount <= 0:
		return Escrow{}, fmt.Errorf("%w: the amount must be positive", ErrEscrowTerms)
	case !releaseAt.After(time.Now()):
		return Escrow{}, fmt.Errorf("%w: release_at must be in the future", ErrEscrowTerms)
	}
	to, err := s.ResolveWallet(ctx, payeeAddress)
	if err != nil {
		return Escrow{}, err
	}
	if to.WalletID == payerAgentID {
		return Escrow{}, ErrSameAgent
	}
	e := Escrow{ID: "escrow_" + uuid.NewString(), PayerWorkspaceID: workspaceID, PayerAgentID: payerAgentID,
		PayeeWorkspaceID: to.WorkspaceID, PayeeAgentID: to.WalletID, AmountULXC: amount, Memo: memo, ReleaseAt: releaseAt}
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: paymentFingerprint(payerAgentID, to.WalletID, amount, "escrow\x00"+memo),
		Payee: Payee{Kind: "agent", ID: to.WalletID, Name: to.Name}, Memo: memo})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Escrow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.payIntoEscrowTx(ctx, tx, &e); err != nil {
		return Escrow{}, s.refusedMovement(ctx, tx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Escrow{}, err
	}
	return s.GetEscrow(ctx, workspaceID, e.ID)
}

func (s *DualTokenStore) payIntoEscrowTx(ctx context.Context, tx pgx.Tx, e *Escrow) error {
	if err := workspace.CheckMoneyWall(ctx, tx, e.PayerWorkspaceID, e.PayeeWorkspaceID); err != nil {
		return err
	}
	// Both agents' rows, in id order (the lock order of every transfer), then the payer's balance row.
	type side struct{ ws, agent string }
	sides := []side{{e.PayerWorkspaceID, e.PayerAgentID}, {e.PayeeWorkspaceID, e.PayeeAgentID}}
	if sides[1].agent < sides[0].agent {
		sides[0], sides[1] = sides[1], sides[0]
	}
	owners := map[string]string{}
	for _, x := range sides {
		if err := lockAgent(ctx, tx, x.ws, x.agent); err != nil {
			return err
		}
		var owner string
		if err := tx.QueryRow(ctx, `SELECT owner_user_id FROM agent_accounts WHERE id = $1`, x.agent).Scan(&owner); err != nil {
			return fmt.Errorf("economy: agent owner: %w", err)
		}
		verified := false
		if owner != "" && s.ownerVerifier != nil {
			var err error
			if verified, err = s.ownerVerifier.MayEarn(ctx, tx, x.ws); err != nil {
				return fmt.Errorf("economy: owner verification: %w", err)
			}
		}
		if !verified {
			return ErrOwnerUnverified
		}
		owners[x.agent] = owner
	}
	capability := "move_between_own_agents"
	if owners[e.PayerAgentID] != owners[e.PayeeAgentID] {
		capability = CapabilityEscrow
	}
	c, _ := CapabilityByKey(capability)
	e.Class = string(c.Class)
	bal, err := accountBalance(ctx, tx, e.PayerWorkspaceID, agentAccount(e.PayerAgentID))
	if err != nil {
		return err
	}
	if bal < e.AmountULXC {
		return agentHolds(ctx, tx, ErrAgentFunds, e.PayerAgentID, bal, 0)
	}
	if err := enforceAgentRules(ctx, tx, e.PayerWorkspaceID, e.PayerAgentID, e.AmountULXC, e.ID); err != nil {
		return err
	}
	if _, _, _, err := readLXCBalance(ctx, tx, e.PayerWorkspaceID); err != nil {
		return err
	}
	testBefore, err := testFundedULXC(ctx, tx, e.PayerWorkspaceID)
	if err != nil {
		return err
	}
	// B22.1: uncleared, AMBER escrow takes test-funded credits only, or is refused naming the class.
	if _, err := spendForCapability(ctx, tx, e.PayerWorkspaceID, capability, e.AmountULXC); err != nil {
		return err
	}
	if err := moveEscrowLXC(ctx, tx, e.PayerWorkspaceID, -e.AmountULXC, *e, "credits paid into escrow"); err != nil {
		return err
	}
	testAfter, err := testFundedULXC(ctx, tx, e.PayerWorkspaceID)
	if err != nil {
		return err
	}
	e.TestFundedULXC = max(testBefore-testAfter, 0)
	entry, err := postEscrowEntry(ctx, tx, e.ID,
		escrowLeg{e.PayerWorkspaceID, agentAccount(e.PayerAgentID), -e.AmountULXC}, escrowLeg{e.PayerWorkspaceID, escrowAccount(e.ID), e.AmountULXC})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_escrows (id, payer_workspace_id, payer_agent_id, payee_workspace_id, payee_agent_id,
		amount_ulxc, memo, class, test_funded_ulxc, release_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID, e.PayerWorkspaceID, e.PayerAgentID, e.PayeeWorkspaceID, e.PayeeAgentID, e.AmountULXC, e.Memo, e.Class, e.TestFundedULXC,
		e.ReleaseAt); err != nil {
		return fmt.Errorf("economy: record escrow: %w", err)
	}
	return escrowEvent(ctx, tx, e.ID, EscrowEvent{Kind: "held", Actor: "payer", EntryID: entry})
}

type escrowLeg struct {
	ws, account string
	amount      int64
}

// postEscrowEntry writes one balanced entry of kind 'escrow' whose legs may be in two workspaces, and returns
// its id.
func postEscrowEntry(ctx context.Context, tx pgx.Tx, escrowID string, legs ...escrowLeg) (string, error) {
	entry := uuid.New()
	for _, l := range legs {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref) VALUES ($1, $2, $3, $4, 'escrow', $5)`,
			entry, l.ws, l.account, l.amount, escrowID); err != nil {
			return "", fmt.Errorf("economy: post escrow: %w", err)
		}
	}
	return entry.String(), nil
}

// moveEscrowLXC moves delta µLXC out of workspaceID's balance into escrow e, or into it out of escrow, with its
// ledger row.
func moveEscrowLXC(ctx context.Context, tx pgx.Tx, workspaceID string, delta int64, e Escrow, desc string) error {
	bal, minted, spent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if err := insertLXCLedger(ctx, tx, workspaceID, delta, bal+delta, LXCTypeAgentEscrow, desc, map[string]interface{}{
		"escrow_id": e.ID, "payer_agent_id": e.PayerAgentID, "payee_agent_id": e.PayeeAgentID, "class": e.Class}); err != nil {
		return err
	}
	return writeLXCBalance(ctx, tx, workspaceID, bal+delta, minted, spent)
}

// settleEscrowTx moves a held or disputed escrow's credits to its payee (released) or back to its payer
// (returned), inside tx, which holds the escrow's row.
func settleEscrowTx(ctx context.Context, tx pgx.Tx, e Escrow, ev EscrowEvent) error {
	ws, agent, desc := e.PayeeWorkspaceID, e.PayeeAgentID, "credits released from escrow"
	if ev.Kind == "returned" {
		ws, agent, desc = e.PayerWorkspaceID, e.PayerAgentID, "credits returned from escrow"
	} else if err := workspace.CheckMoneyWall(ctx, tx, e.PayerWorkspaceID, e.PayeeWorkspaceID); err != nil {
		return err
	}
	if err := lockAgent(ctx, tx, ws, agent); err != nil {
		return err
	}
	if err := moveEscrowLXC(ctx, tx, ws, e.AmountULXC, e, desc); err != nil {
		return err
	}
	if err := addTestFunded(ctx, tx, ws, e.TestFundedULXC); err != nil { // test money comes out test money
		return err
	}
	entry, err := postEscrowEntry(ctx, tx, e.ID, escrowLeg{e.PayerWorkspaceID, escrowAccount(e.ID), -e.AmountULXC}, escrowLeg{ws, agentAccount(agent), e.AmountULXC})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_escrows SET status = $2, decided_at = now() WHERE id = $1`, e.ID, ev.Kind); err != nil {
		return fmt.Errorf("economy: settle escrow: %w", err)
	}
	ev.EntryID = entry
	return escrowEvent(ctx, tx, e.ID, ev)
}

func escrowEvent(ctx context.Context, tx pgx.Tx, escrowID string, ev EscrowEvent) error {
	if _, err := tx.Exec(ctx, `INSERT INTO agent_escrow_events (escrow_id, kind, actor, operator, detail, entry_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		escrowID, ev.Kind, ev.Actor, ev.Operator, ev.Detail, ev.EntryID); err != nil {
		return fmt.Errorf("economy: escrow event: %w", err)
	}
	return nil
}

// payerEscrowTx reads, holding its row, an escrow workspaceID paid in and that is still held.
func payerEscrowTx(ctx context.Context, tx pgx.Tx, workspaceID, escrowID string) (Escrow, error) {
	e, err := scanEscrow(tx.QueryRow(ctx, `SELECT `+escrowColumns+` FROM agent_escrows
		WHERE id = $1 AND payer_workspace_id = $2 AND status = 'held' FOR UPDATE`, escrowID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return e, ErrEscrowNotFound
	}
	return e, err
}

// ConfirmEscrow is the payer's side saying the deal is done: the escrow is released to the payee.
func (s *DualTokenStore) ConfirmEscrow(ctx context.Context, workspaceID, escrowID string) (Escrow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Escrow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := payerEscrowTx(ctx, tx, workspaceID, escrowID)
	if err != nil {
		return Escrow{}, err
	}
	if err := settleEscrowTx(ctx, tx, e, EscrowEvent{Kind: "released", Actor: "payer", Detail: "delivery confirmed"}); err != nil {
		return Escrow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Escrow{}, err
	}
	return s.GetEscrow(ctx, workspaceID, escrowID)
}

// DisputeEscrow is the payer's side, before the deadline, holding the escrow for the operator to decide.
func (s *DualTokenStore) DisputeEscrow(ctx context.Context, workspaceID, escrowID, reason string) (Escrow, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Escrow{}, fmt.Errorf("%w: a dispute needs a reason the operator can decide on", ErrEscrowTerms)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Escrow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := payerEscrowTx(ctx, tx, workspaceID, escrowID)
	if err != nil {
		return Escrow{}, err
	}
	if !time.Now().Before(e.ReleaseAt) {
		return Escrow{}, fmt.Errorf("%w: its deadline has passed, so it is released to the payee", ErrEscrowTerms)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_escrows SET status = 'disputed' WHERE id = $1`, e.ID); err != nil {
		return Escrow{}, fmt.Errorf("economy: dispute escrow: %w", err)
	}
	if err := escrowEvent(ctx, tx, e.ID, EscrowEvent{Kind: "disputed", Actor: "payer", Detail: reason}); err != nil {
		return Escrow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Escrow{}, err
	}
	return s.GetEscrow(ctx, workspaceID, escrowID)
}

// DecideEscrow is the operator deciding a disputed escrow: released to the payee, or returned to the payer.
func (s *DualTokenStore) DecideEscrow(ctx context.Context, escrowID string, release bool, operator, note string) (Escrow, error) {
	if strings.TrimSpace(operator) == "" || strings.TrimSpace(note) == "" {
		return Escrow{}, fmt.Errorf("%w: a decision needs who made it and a note", ErrEscrowTerms)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Escrow{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := scanEscrow(tx.QueryRow(ctx, `SELECT `+escrowColumns+` FROM agent_escrows WHERE id = $1 AND status = 'disputed' FOR UPDATE`, escrowID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Escrow{}, ErrEscrowNotFound
	}
	if err != nil {
		return Escrow{}, err
	}
	kind := "returned"
	if release {
		kind = "released"
	}
	if err := settleEscrowTx(ctx, tx, e, EscrowEvent{Kind: kind, Actor: "operator", Operator: operator, Detail: note}); err != nil {
		return Escrow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Escrow{}, err
	}
	return s.GetEscrow(ctx, e.PayerWorkspaceID, escrowID)
}

// RunEscrowDeadlines releases every held escrow whose deadline is by now, each in its own transaction. One that
// cannot be released (its payee is gone) is disputed instead, for the operator.
func (s *DualTokenStore) RunEscrowDeadlines(ctx context.Context, now time.Time) (released int, err error) {
	for range maxTicksPerRun {
		done, err := s.runEscrowDeadline(ctx, now)
		if err != nil || !done {
			return released, err
		}
		released++
	}
	return released, nil
}

func (s *DualTokenStore) runEscrowDeadline(ctx context.Context, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := scanEscrow(tx.QueryRow(ctx, `SELECT `+escrowColumns+` FROM agent_escrows
		WHERE status = 'held' AND release_at <= $1 ORDER BY release_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("economy: due escrow: %w", err)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	if serr := settleEscrowTx(ctx, sp, e, EscrowEvent{Kind: "released", Actor: "deadline", Detail: "the deadline passed without a dispute"}); serr != nil {
		if err := sp.Rollback(ctx); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_escrows SET status = 'disputed' WHERE id = $1`, e.ID); err != nil {
			return false, err
		}
		if err := escrowEvent(ctx, tx, e.ID, EscrowEvent{Kind: "disputed", Actor: "deadline",
			Detail: "could not be released at its deadline: " + serr.Error()}); err != nil {
			return false, err
		}
	} else if err := sp.Commit(ctx); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// GetEscrow reads an escrow workspaceID paid in or is owed, with its events.
func (s *DualTokenStore) GetEscrow(ctx context.Context, workspaceID, escrowID string) (Escrow, error) {
	list, err := s.escrows(ctx, `WHERE id = $2 AND (payer_workspace_id = $1 OR payee_workspace_id = $1)`, workspaceID, escrowID)
	if err != nil {
		return Escrow{}, err
	}
	if len(list) == 0 {
		return Escrow{}, ErrEscrowNotFound
	}
	return list[0], nil
}

// ListEscrows reads the escrows workspaceID's agents paid in or are owed, newest first, with their events.
func (s *DualTokenStore) ListEscrows(ctx context.Context, workspaceID string) ([]Escrow, error) {
	return s.escrows(ctx, `WHERE payer_workspace_id = $1 OR payee_workspace_id = $1`, workspaceID)
}

// ListDisputedEscrows reads every escrow waiting on the operator, oldest first.
func (s *DualTokenStore) ListDisputedEscrows(ctx context.Context) ([]Escrow, error) {
	list, err := s.escrows(ctx, `WHERE status = 'disputed'`)
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list, err
}

func (s *DualTokenStore) escrows(ctx context.Context, where string, args ...any) ([]Escrow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+escrowColumns+` FROM agent_escrows `+where+` ORDER BY created_at DESC, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("economy: escrows: %w", err)
	}
	out := []Escrow{}
	byID := map[string]int{}
	for rows.Next() {
		e, err := scanEscrow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		byID[e.ID] = len(out)
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	ids := make([]string, 0, len(out))
	for id := range byID {
		ids = append(ids, id)
	}
	rows, err = s.pool.Query(ctx, `SELECT escrow_id, kind, actor, operator, detail, entry_id, at
		FROM agent_escrow_events WHERE escrow_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, fmt.Errorf("economy: escrow events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ev EscrowEvent
		if err := rows.Scan(&id, &ev.Kind, &ev.Actor, &ev.Operator, &ev.Detail, &ev.EntryID, &ev.At); err != nil {
			return nil, err
		}
		out[byID[id]].Events = append(out[byID[id]].Events, ev)
	}
	return out, rows.Err()
}
