package market

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// prizes.go — B32.35: A ROOM'S PRIZE IS A PURCHASE OF THE WINNING CONTRIBUTION.
//
// Awarding a room's prize, the room's owner buys the winning contribution's listing at the prize's amount: one billed
// market_uses row of use_kind prize on the owner's monthly marketplace bill, judged by the rules of the room's wallet as
// the agent that buys it, and a perpetual commercial licence to the version that won (kind prize, no offer). It is a
// marketplace purchase on the buy_marketplace_listings path like any other: once the owner's invoice is paid it clears,
// and the contribution's author earns their share through the normal clearing and payout. Nothing is escrowed and
// nothing moves between owners.

// SourcePrize is a licence won as a room's prize.
const SourcePrize = "prize"

// UseKindPrize is the market_uses row a prize's award writes.
const UseKindPrize = "prize"

// PrizeAward is a prize paid to one listing version: its amount, the room it was awarded in and who awarded it, and
// what names it for an approval.
type PrizeAward struct {
	ListingID        string
	Version          int
	AmountUSDMicros  int64
	RoomID           string
	ActorWorkspaceID string
	What             string
}

// AwardPrize records buyerWorkspaceID's purchase of a.ListingID's version a.Version as a prize, bought by agentID: the
// billed prize use at a.AmountUSDMicros and the perpetual commercial licence beside it, after the money wall and the
// single-party check, judged by agentID's rules. then runs in the same transaction once both rows are written — the
// caller's own record of the award, whose error writes nothing. The use is metered onto the buyer's bill after it
// commits.
func (s *Store) AwardPrize(ctx context.Context, deps LicenceDeps, buyerWorkspaceID, agentID string, a PrizeAward,
	then func(pgx.Tx, Licence) error) (Licence, error) {
	if a.AmountUSDMicros <= 0 {
		return Licence{}, invalid("a prize pays more than nothing")
	}
	l, _, version, err := s.resolve(ctx, buyerWorkspaceID, a.ListingID, a.Version)
	if err != nil {
		return Licence{}, err
	}
	if l.WorkspaceID == buyerWorkspaceID {
		return Licence{}, invalid("a prize is paid to another workspace's work: this listing is the buyer's own")
	}
	lic := Licence{ID: "lic_" + uuid.NewString(), ListingID: l.ID, Title: l.Title, AgentID: agentID, Licence: LicenceCommercial,
		Terms: LicenceTerms[LicenceCommercial], Kind: UseKindPrize, PinnedVersion: &version, Status: LicenceActive, Source: SourcePrize,
		UseID: "use_" + uuid.NewString(), Charge: ChargeBilled, PriceULXC: a.AmountUSDMicros * ulxcPerUSDMicro}
	if err := workspace.CheckMoneyWall(ctx, s.pool, buyerWorkspaceID, l.WorkspaceID); err != nil {
		return Licence{}, err
	}
	linked, err := s.linked(ctx, l.WorkspaceID, buyerWorkspaceID)
	if err != nil {
		return Licence{}, err
	}
	if linked {
		lic.Charge, lic.PriceULXC = ChargeLinked, 0
	}
	if lic.Charge == ChargeBilled {
		if deps.Meter == nil {
			return Licence{}, ErrNoBill
		}
		if deps.Capabilities == nil {
			return Licence{}, errors.New("market: a prize's charge cannot be recorded here: no wallet capability answers for it")
		}
		if err := deps.Capabilities.RequireBilledCapability(ctx, buyerWorkspaceID, economy.CapabilityBuyListings); err != nil {
			return Licence{}, err
		}
		if err := s.sellable(ctx, buyerWorkspaceID); err != nil { // B32.39
			return Licence{}, err
		}
	}

	record := func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO market_licences (id, listing_id, buyer_workspace_id, agent_id, licence, kind, pinned_version,
				source, starts_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING starts_at, ends_at, created_at`,
			lic.ID, l.ID, buyerWorkspaceID, agentID, lic.Licence, lic.Kind, version, lic.Source, s.now()).
			Scan(&lic.StartsAt, &lic.EndsAt, &lic.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id,
				price_ulxc, charge, use_kind, licence_id, used_at, ran_at, room_id, actor_workspace_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11, $12, $13)`,
			lic.UseID, l.ID, version, l.WorkspaceID, buyerWorkspaceID, agentID, lic.PriceULXC, lic.Charge, UseKindPrize, lic.ID, lic.StartsAt,
			a.RoomID, a.ActorWorkspaceID); err != nil {
			return err
		}
		if then != nil {
			return then(tx, lic)
		}
		return nil
	}
	if agentID != "" && deps.Agents != nil {
		what := a.What
		if what == "" {
			what = fmt.Sprintf("prize:%s:%s:%d:%d", agentID, l.ID, version, lic.PriceULXC)
		}
		c := economy.Commitment{Kind: UseKindPrize, Licence: LicenceCommercial, ULXC: lic.PriceULXC}
		memo := fmt.Sprintf("A prize for %s, %s on your monthly marketplace bill", l.Title, economy.LXCMoney(lic.PriceULXC))
		err = deps.Agents.JudgeAgentPurchase(ctx, buyerWorkspaceID, agentID, l.ID, lic.PriceULXC, c, what, memo, record)
	} else {
		err = pgx.BeginFunc(ctx, s.pool, record)
	}
	if err != nil {
		return Licence{}, err
	}
	if lic.Charge == ChargeBilled {
		if err := s.meter(ctx, deps.Meter, lic.UseID, buyerWorkspaceID, lic.PriceULXC, lic.StartsAt); err != nil {
			lic.MeterError = err.Error() // the prize is awarded; MeterPending bills it on its next pass
		}
	}
	return lic, nil
}
