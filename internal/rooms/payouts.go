package rooms

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
)

// payouts.go — B46.11: A ROOM'S EARNINGS, SPLIT AS ITS TERMS AGREE.
//
// The room's owner pays out an amount from the room's wallet to its members, split by the room's terms at their
// current version: equal shares, shares by the votes on each member's accepted contributions, or the shares the owner
// decides (in basis points summing to 10,000); a member's votes on their own work, and votes of anyone no longer a
// voting member, do not count. Only a live member who is not a viewer and has accepted the current
// terms is paid. The parts sum to the amount exactly (economy.SplitEarnings), and economy.PayEarnings pays each member
// once, on the money layer, under the agreement "room <id> terms v<n> <rule>".

// SplitPayout is the room owner's request to pay out AmountMinor of Currency; Shares, by member workspace in basis
// points, only when the terms leave the split to the owner.
type SplitPayout struct {
	AmountMinor int64          `json:"amount_minor"`
	Currency    string         `json:"currency"`
	Shares      map[string]int `json:"shares,omitempty"`
}

// Split is roomID's payout of in as its terms agree, for its owner callerWS to pay out under key: the earnings to pay
// from the room's wallet, each member's share with its amount.
func (s *Store) Split(ctx context.Context, roomID, callerWS, key string, in SplitPayout) (economy.Earnings, error) {
	var e economy.Earnings
	if key == "" || len(key) > 128 {
		return e, invalid("a payout carries an Idempotency-Key of up to 128 characters")
	}
	var owner, status, rule string
	var version int
	var wallet *string
	err := s.pool.QueryRow(ctx, `SELECT r.owner_workspace_id, r.status, r.wallet_agent_id, t.version, t.split_rule FROM rooms r
		JOIN room_terms t ON t.room_id = r.id AND t.version = r.terms_version WHERE r.id = $1`, roomID).Scan(&owner, &status, &wallet, &version, &rule)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return e, ErrNotFound
	case err != nil:
		return e, fmt.Errorf("rooms: payout: %w", err)
	case owner != callerWS:
		return e, ErrForbidden
	case status == Locked || status == Closed:
		// shortcut: checked before PayEarnings, not under a row lock; a lock landing mid-payout lets that one through.
		return e, fmt.Errorf("%w: a %s room pays nothing out of its budget", ErrConflict, status)
	case wallet == nil:
		return e, invalid("the room has no wallet to pay out from")
	}
	rows, err := s.pool.Query(ctx, `SELECT m.workspace_id, COALESCE((SELECT sum(GREATEST(v.tally, 0)) FROM (SELECT COALESCE(sum(rv.value), 0) AS tally
			FROM room_contributions c LEFT JOIN room_votes rv ON rv.contribution_id = c.id AND rv.workspace_id <> c.author_workspace_id
				AND EXISTS (SELECT 1 FROM room_members vm WHERE vm.room_id = c.room_id AND vm.workspace_id = rv.workspace_id
					AND vm.removed_at IS NULL AND vm.role <> 'viewer')
			WHERE c.room_id = m.room_id AND c.author_workspace_id = m.workspace_id AND c.status = 'accepted' GROUP BY c.id) v), 0)::bigint
		FROM room_members m WHERE m.room_id = $1 AND m.removed_at IS NULL AND m.role <> 'viewer' AND m.terms_version = $2
		ORDER BY m.joined_at, m.workspace_id`, roomID, version)
	if err != nil {
		return e, fmt.Errorf("rooms: payout: %w", err)
	}
	type member struct {
		ws    string
		votes int64
	}
	members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (member, error) {
		var m member
		return m, r.Scan(&m.ws, &m.votes)
	})
	if err != nil {
		return e, fmt.Errorf("rooms: payout: %w", err)
	}
	weights := make([]int64, len(members))
	given, named := 0, 0
	for i, m := range members {
		switch rule {
		case SplitEqual:
			weights[i] = 1
		case SplitByVotes:
			weights[i] = m.votes
		default:
			bps, ok := in.Shares[m.ws]
			if ok && (bps < 0 || bps > 10_000) {
				return e, invalid("a share is 0 to 10000 basis points")
			}
			if ok {
				named++
			}
			weights[i], given = int64(bps), given+bps
		}
	}
	if rule == SplitOwnerDecides && (given != 10_000 || named != len(in.Shares)) {
		return e, invalid("the owner decides this room's split: shares name only members who accepted the current terms, in basis points summing to 10000")
	}
	parts, err := economy.SplitEarnings(in.AmountMinor, weights)
	if err != nil {
		return e, invalid("%v", err)
	}
	var sum int64
	for _, w := range weights {
		sum += w
	}
	e = economy.Earnings{Kind: economy.EarningsRoomSplit, Source: "room:" + roomID + ":" + key, AgentID: *wallet, Currency: in.Currency,
		Agreement: fmt.Sprintf("room %s terms v%d %s", roomID, version, rule)}
	for i, m := range members {
		e.Shares = append(e.Shares, economy.EarningsShare{WorkspaceID: m.ws, ShareBPS: int(weights[i] * 10_000 / sum), AmountMinor: parts[i]})
	}
	return e, nil
}
