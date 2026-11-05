package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/money"
)

// money_physical_cards.go — B53.19: THE OWNER'S PHYSICAL CARD — ORDERED, POSTED, ACTIVATED, REPLACED.
//
// The owner orders a physical card on their own pounds account, in the issuer's standard design, to an address
// checked here. The issuer posts it; it stays inactive until it has shipped and the owner activates it with its last
// four digits. A lost, stolen or damaged card is frozen at the issuer before the new one is asked for, and the new one
// goes to the same address. Every call goes through the money layer's CardIssuer: the fake issuer until a live one
// serves cards. The card is ordered with no cap, so Lens declines its every charge: it pays nothing yet.

var (
	// ErrPhysicalCard: an address, last four digits, reason or delivery status that fails its check.
	ErrPhysicalCard = errors.New("economy: invalid physical card request")
	// ErrPhysicalCardNotFound: no such physical card in this workspace.
	ErrPhysicalCardNotFound = errors.New("economy: no such physical card")
	// ErrPhysicalCardState: the card is not in a state the request can move it from.
	ErrPhysicalCardState = errors.New("economy: the physical card cannot do that now")
)

// The delivery statuses of a physical card, in order.
const (
	DeliveryOrdered   = "ordered"
	DeliveryShipped   = "shipped"
	DeliveryDelivered = "delivered"

	physicalCardOrdering = "ordering" // the issuer not yet answered; never shown
)

var deliveryOrder = map[string]int{DeliveryOrdered: 0, DeliveryShipped: 1, DeliveryDelivered: 2}

// physicalCardPostcodes is every country the issuer posts cards to, with the shape of its postcodes: GB to start.
var physicalCardPostcodes = map[string]*regexp.Regexp{
	"GB": regexp.MustCompile(`^(GIR 0AA|[A-Z]{1,2}[0-9][A-Z0-9]? [0-9][A-Z]{2})$`),
}

// CardAddress is where a physical card is posted.
type CardAddress struct {
	Line1    string `json:"line1"`
	Line2    string `json:"line2"`
	City     string `json:"city"`
	Postcode string `json:"postcode"`
	Country  string `json:"country"`
}

// check trims a and checks it is somewhere the issuer posts to, with a postcode of that country's shape.
func (a *CardAddress) check() error {
	for _, f := range []*string{&a.Line1, &a.Line2, &a.City} {
		*f = strings.Join(strings.Fields(*f), " ")
	}
	a.Country = strings.ToUpper(strings.TrimSpace(a.Country))
	pc := strings.ToUpper(strings.Join(strings.Fields(a.Postcode), ""))
	if len(pc) > 3 {
		pc = pc[:len(pc)-3] + " " + pc[len(pc)-3:]
	}
	a.Postcode = pc
	shape, ships := physicalCardPostcodes[a.Country]
	switch {
	case a.Line1 == "" || len(a.Line1) > 100 || len(a.Line2) > 100:
		return fmt.Errorf("%w: the address needs its first line, up to 100 characters", ErrPhysicalCard)
	case a.City == "" || len(a.City) > 60:
		return fmt.Errorf("%w: the address needs its town or city", ErrPhysicalCard)
	case !ships:
		return fmt.Errorf("%w: cards are posted only to the United Kingdom (GB) for now, not %q", ErrPhysicalCard, a.Country)
	case !shape.MatchString(a.Postcode):
		return fmt.Errorf("%w: %q is not a %s postcode", ErrPhysicalCard, a.Postcode, a.Country)
	}
	return nil
}

// CardDelivery is where a physical card is in the post.
type CardDelivery struct {
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PhysicalCard is the owner's physical card. It never carries the card number: the issuer's own screens show that,
// and set the PIN.
type PhysicalCard struct {
	ID         string       `json:"id"`
	Last4      string       `json:"last4"`
	State      string       `json:"state"` // inactive, active, frozen or closed
	Design     string       `json:"design"`
	NameOnCard string       `json:"name_on_card"`
	Address    CardAddress  `json:"address"`
	Delivery   CardDelivery `json:"delivery"`
	PinURL     string       `json:"pin_url"` // the issuer's own PIN screen; "" while it gives none (the fake gives none)
	Replaces   string       `json:"replaces"`
	ReplacedBy string       `json:"replaced_by"`
	CreatedAt  time.Time    `json:"created_at"`

	partner, partnerRef string
}

// physicalCardRoute is the owner's own: no agent.
func physicalCardRoute(workspaceID string) money.Route { return cardRoute(workspaceID, "") }

// OrderPhysicalCard orders a physical card on workspaceID's own pounds account, posted to address.
func (s *DualTokenStore) OrderPhysicalCard(ctx context.Context, workspaceID, nameOnCard string, address CardAddress) (PhysicalCard, error) {
	if s.moneyLayer == nil {
		return PhysicalCard{}, ErrAgentWalletsOff
	}
	nameOnCard = strings.Join(strings.Fields(nameOnCard), " ")
	if nameOnCard == "" || len(nameOnCard) > 26 {
		return PhysicalCard{}, fmt.Errorf("%w: the name on the card is up to 26 characters", ErrPhysicalCard)
	}
	if err := address.check(); err != nil {
		return PhysicalCard{}, err
	}
	return s.orderPhysicalCard(ctx, workspaceID, nameOnCard, address, "", "")
}

// orderPhysicalCard files the card, asks the issuer for it, and records what it answered. A card the issuer refused is
// not kept. replaces, when set, is claimed for this card first: a card is replaced once.
func (s *DualTokenStore) orderPhysicalCard(ctx context.Context, workspaceID, nameOnCard string, address CardAddress, replaces, reason string) (PhysicalCard, error) {
	from, err := s.payingAccount(ctx, workspaceID, OutsidePayment{Currency: CurrencyGBP})
	if err != nil {
		return PhysicalCard{}, err
	}
	issuer, partner, err := s.moneyLayer.CardIssuer(ctx, physicalCardRoute(workspaceID))
	if err != nil {
		return PhysicalCard{}, err
	}
	id := "pcard_" + uuid.NewString()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PhysicalCard{}, fmt.Errorf("economy: physical card: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if replaces != "" {
		tag, err := tx.Exec(ctx, `UPDATE money_physical_cards SET replaced_by = $3, replace_reason = $4, updated_at = now()
			WHERE id = $1 AND workspace_id = $2 AND replaced_by = ''`, replaces, workspaceID, id, reason)
		if err != nil {
			return PhysicalCard{}, fmt.Errorf("economy: physical card: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return PhysicalCard{}, fmt.Errorf("%w: the card has already been replaced", ErrPhysicalCardState)
		}
	}
	addr, err := json.Marshal(address)
	if err != nil {
		return PhysicalCard{}, fmt.Errorf("economy: physical card: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO money_physical_cards (id, workspace_id, account_id, wallet_ref, state, name_on_card, address, replaces)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8)`, id, workspaceID, from.id, from.partnerRef, physicalCardOrdering, nameOnCard, string(addr), replaces); err != nil {
		return PhysicalCard{}, fmt.Errorf("economy: physical card: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PhysicalCard{}, fmt.Errorf("economy: physical card: %w", err)
	}
	ctx = context.WithoutCancel(ctx) // a caller that goes away does not leave the card half ordered
	// undo takes the order back: the card it replaces may be replaced again.
	undo := func() {
		_, _ = s.pool.Exec(ctx, `UPDATE money_physical_cards SET replaced_by = '', replace_reason = '' WHERE replaced_by = $1`, id)
		_, _ = s.pool.Exec(ctx, `DELETE FROM money_physical_cards WHERE id = $1`, id)
	}
	card, err := issuer.Issue(ctx, money.IssueCard{Key: id, Wallet: from.partnerRef, FormFactor: money.CardPhysical})
	if err != nil {
		undo()
		return PhysicalCard{}, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE money_physical_cards SET state = $2, partner = $3, partner_ref = $4, last4 = $5, updated_at = now()
		WHERE id = $1`, id, card.State, partner, card.Ref, card.Last4); err != nil {
		// A card Lens cannot record (its ref already names another) is closed at the issuer, and the order undone.
		_, _ = issuer.Close(ctx, "undo:"+id, card.Ref)
		undo()
		return PhysicalCard{}, fmt.Errorf("economy: physical card: %w", err)
	}
	return s.physicalCard(ctx, workspaceID, id)
}

// PhysicalCards is workspaceID's physical cards, newest first, at most 50.
func (s *DualTokenStore) PhysicalCards(ctx context.Context, workspaceID string) ([]PhysicalCard, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+physicalCardCols+` FROM money_physical_cards WHERE workspace_id = $1 AND state <> $2
		ORDER BY created_at DESC, id LIMIT 50`, workspaceID, physicalCardOrdering)
	if err != nil {
		return nil, fmt.Errorf("economy: physical cards: %w", err)
	}
	defer rows.Close()
	out := []PhysicalCard{}
	for rows.Next() {
		var c PhysicalCard
		if err := rows.Scan(c.fields()...); err != nil {
			return nil, err
		}
		out = append(out, c.shown())
	}
	return out, rows.Err()
}

// ActivatePhysicalCard activates workspaceID's card id, once it has shipped, when last4 are its last four digits.
func (s *DualTokenStore) ActivatePhysicalCard(ctx context.Context, workspaceID, id, last4 string) (PhysicalCard, error) {
	if s.moneyLayer == nil {
		return PhysicalCard{}, ErrAgentWalletsOff
	}
	// The card's row is locked until the issuer has answered and its answer is written, as a replace locks it, so a
	// replace never lands between the two and leaves the row active while the issuer holds the card frozen.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PhysicalCard{}, fmt.Errorf("economy: physical card: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := readPhysicalCard(ctx, tx, workspaceID, id, " FOR UPDATE")
	switch {
	case err != nil:
		return c, err
	case c.State == money.CardActive:
		return c, nil
	case c.State != money.CardInactive:
		return c, fmt.Errorf("%w: the card is %s", ErrPhysicalCardState, c.State)
	case c.Delivery.Status == DeliveryOrdered:
		return c, fmt.Errorf("%w: the card has not shipped yet", ErrPhysicalCardState)
	case strings.TrimSpace(last4) != c.Last4:
		return c, fmt.Errorf("%w: those are not the last four digits of this card", ErrPhysicalCard)
	}
	issuer, _, err := s.moneyLayer.CardIssuer(ctx, physicalCardRoute(workspaceID))
	if err != nil {
		return c, err
	}
	ctx = context.WithoutCancel(ctx)
	card, err := issuer.Activate(ctx, id, c.partnerRef)
	if err != nil {
		return c, err
	}
	if err := setPhysicalCardState(ctx, tx, workspaceID, id, card.State); err != nil {
		return c, err
	}
	if err := tx.Commit(ctx); err != nil {
		return c, fmt.Errorf("economy: physical card: %w", err)
	}
	return s.physicalCard(ctx, workspaceID, id)
}

// ReplacePhysicalCard freezes workspaceID's card id at the issuer, at once, and orders a new one to the same address:
// the old card as it now stands, and the new one.
func (s *DualTokenStore) ReplacePhysicalCard(ctx context.Context, workspaceID, id, reason string) (old, card PhysicalCard, err error) {
	if s.moneyLayer == nil {
		return old, card, ErrAgentWalletsOff
	}
	if reason != "lost" && reason != "stolen" && reason != "damaged" {
		return old, card, fmt.Errorf("%w: a card is replaced as lost, stolen or damaged, not %q", ErrPhysicalCard, reason)
	}
	// The card's row is locked while it is frozen at the issuer and the freeze written, as an activation locks it.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return old, card, fmt.Errorf("economy: physical card: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if old, err = readPhysicalCard(ctx, tx, workspaceID, id, " FOR UPDATE"); err != nil {
		return old, card, err
	}
	switch {
	case old.ReplacedBy != "":
		return old, card, fmt.Errorf("%w: the card has already been replaced", ErrPhysicalCardState)
	case old.State == money.CardClosed:
		return old, card, fmt.Errorf("%w: the card is closed", ErrPhysicalCardState)
	}
	issuer, _, err := s.moneyLayer.CardIssuer(ctx, physicalCardRoute(workspaceID))
	if err != nil {
		return old, card, err
	}
	ctx = context.WithoutCancel(ctx)
	frozen, err := issuer.Freeze(ctx, "replace:"+id, old.partnerRef)
	if err != nil {
		return old, card, err
	}
	if err = setPhysicalCardState(ctx, tx, workspaceID, id, frozen.State); err != nil {
		return old, card, err
	}
	if err = tx.Commit(ctx); err != nil {
		return old, card, fmt.Errorf("economy: physical card: %w", err)
	}
	if card, err = s.orderPhysicalCard(ctx, workspaceID, old.NameOnCard, old.Address, id, reason); err != nil {
		return old, card, err
	}
	old, err = s.physicalCard(ctx, workspaceID, id)
	return old, card, err
}

// DeliverPhysicalCard is the issuer telling Lens where workspaceID's card id is in the post: it only moves forward.
func (s *DualTokenStore) DeliverPhysicalCard(ctx context.Context, workspaceID, id, status string) (PhysicalCard, error) {
	if status != DeliveryShipped && status != DeliveryDelivered {
		return PhysicalCard{}, fmt.Errorf("%w: a delivery event is shipped or delivered, not %q", ErrPhysicalCard, status)
	}
	c, err := s.physicalCard(ctx, workspaceID, id)
	switch {
	case err != nil:
		return c, err
	case c.Delivery.Status == status:
		return c, nil
	case deliveryOrder[status] < deliveryOrder[c.Delivery.Status]:
		return c, fmt.Errorf("%w: the card is already %s", ErrPhysicalCardState, c.Delivery.Status)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE money_physical_cards SET delivery_status = $3, delivery_updated_at = now(), updated_at = now()
		WHERE id = $1 AND workspace_id = $2`, id, workspaceID, status); err != nil {
		return c, fmt.Errorf("economy: physical card: %w", err)
	}
	return s.physicalCard(ctx, workspaceID, id)
}

func setPhysicalCardState(ctx context.Context, tx pgx.Tx, workspaceID, id, state string) error {
	if _, err := tx.Exec(ctx, `UPDATE money_physical_cards SET state = $3, updated_at = now() WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID, state); err != nil {
		return fmt.Errorf("economy: physical card: %w", err)
	}
	return nil
}

const physicalCardCols = `id, last4, state, name_on_card, address, delivery_status, delivery_updated_at, replaces, replaced_by, created_at,
	partner, partner_ref`

func (c *PhysicalCard) fields() []any {
	return []any{&c.ID, &c.Last4, &c.State, &c.NameOnCard, &c.Address, &c.Delivery.Status, &c.Delivery.UpdatedAt, &c.Replaces, &c.ReplacedBy,
		&c.CreatedAt, &c.partner, &c.partnerRef}
}

func (c PhysicalCard) shown() PhysicalCard {
	c.Design = "standard"
	return c
}

// physicalCard reads workspaceID's card id, unless it is still being ordered.
func (s *DualTokenStore) physicalCard(ctx context.Context, workspaceID, id string) (PhysicalCard, error) {
	return readPhysicalCard(ctx, s.pool, workspaceID, id, "")
}

// readPhysicalCard is physicalCard read through db, with lock (" FOR UPDATE", or "") appended to the query.
func readPhysicalCard(ctx context.Context, db pgxDB, workspaceID, id, lock string) (PhysicalCard, error) {
	var c PhysicalCard
	err := db.QueryRow(ctx, `SELECT `+physicalCardCols+` FROM money_physical_cards WHERE id = $1 AND workspace_id = $2 AND state <> $3`+lock,
		id, workspaceID, physicalCardOrdering).Scan(c.fields()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrPhysicalCardNotFound
	}
	if err != nil {
		return c, fmt.Errorf("economy: physical card: %w", err)
	}
	return c.shown(), nil
}
