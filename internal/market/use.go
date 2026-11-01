package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	stripe "github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/fees"
	"github.com/talyvor/lens/internal/workspace"
)

// use.go — B20.2: USE A LISTING, PAY PER USE, AND THE SELLER EARNS.
//
// A use runs one version of a listing for a buyer workspace, through Lens as the buyer (so the models it
// calls are billed to the buyer as usual). A paid listing's price is then METERED onto the buyer's monthly
// marketplace bill (a Stripe usage-based subscription, internal/billing/market_bill.go) — never taken from
// prepaid credits. When the invoice carrying the use is paid, the use clears and the seller earns their
// share in USD: the price less Talyvor's take (internal/fees, B32.8) from the first dollar; each earning is
// payable after a 14-day holdback (B20.5 pays it out). A seller using their own listing, or a buyer linked to the seller by a
// card or an owner (the single-party detector), is charged nothing and earns the seller nothing.

// Holdback is how long an earning waits after its use clears before it can be paid out.
const Holdback = 14 * 24 * time.Hour

// ulxcPerUSDMicro is how many µLXC make one µUSD at the LXC peg (10 at $0.10).
var ulxcPerUSDMicro = int64(math.Round(1 / economy.LXCUSDValue))

// Charges: what a use costs the buyer.
const (
	ChargeBilled = "billed" // the listing's price, metered onto the buyer's bill
	ChargeFree   = "free"   // the listing is free
	ChargeOwn    = "own"    // the seller used their own listing
	ChargeLinked = "linked" // buyer and seller are one party: a wash trade, charged and earned nothing
)

var (
	// ErrNotRunnable: a kind this version of the marketplace cannot run.
	ErrNotRunnable = errors.New("market: this listing cannot be run")
	// ErrNoModel: nothing names the model to run the listing on.
	ErrNoModel = errors.New(`market: name the model to run this listing on ("model")`)
	// ErrNoBill: a paid use, but no marketplace bill is configured to put it on.
	ErrNoBill = errors.New("market: paid listings cannot be used here: no marketplace bill is configured")
	// ErrOverMaxPrice: the use would cost more than the buyer's max_price_usd_micros (B32.23).
	ErrOverMaxPrice = errors.New("market: this use costs more than its max_price_usd_micros")
)

// Message is one chat message a use sends to a model.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Runner runs one model call through Lens as the caller.
type Runner interface {
	Run(ctx context.Context, model string, messages []Message) (output string, err error)
}

// Meter puts a billed use on the buyer's monthly marketplace bill. The use's id is the meter event's
// identifier, so a retry never bills it twice.
type Meter interface {
	MeterMarketUse(ctx context.Context, buyerWorkspaceID, useID string, ulxc int64, at time.Time) error
}

// AgentJudge judges an agent's use of a listing against its spending rules (B19.2, B19.14), recording the
// use in the same transaction so its daily and monthly limits count it — and a licence it takes against its licence
// rules (B32.22).
type AgentJudge interface {
	JudgeAgentPurchase(ctx context.Context, workspaceID, agentID, listingID string, amount int64, c economy.Commitment, what string,
		record func(pgx.Tx) error) error
}

// UseRequest is what a buyer asks of a listing.
type UseRequest struct {
	Version   int               `json:"version"`   // 0: the latest
	Model     string            `json:"model"`     // overrides the artifact's model
	Input     string            `json:"input"`     // the user's message (an agent or a skill)
	Variables map[string]string `json:"variables"` // a prompt's {{variables}}
	Person    string            `json:"-"`         // who is using it: a personal or enterprise licence counts its people (B32.19)
	// B32.23: the most this use may cost, in µUSD — a pipeline's billed steps included. Above it nothing runs and no
	// row is written. nil: no cap; 0: only a use that costs nothing.
	MaxPriceUSDMicros *int64 `json:"max_price_usd_micros,omitempty"`
	// B32.32: a charge on a room records the room and the member who spent; the buyer is the room's owner and the
	// agent its wallet. Set by the room's run (internal/rooms Spender), never by the buyer's request body.
	RoomID           string `json:"-"`
	ActorWorkspaceID string `json:"-"`
}

// CaseResult is one evaluation case's outcome.
type CaseResult struct {
	Input    string `json:"input"`
	Expected string `json:"expected"`
	Output   string `json:"output"`
	Passed   bool   `json:"passed"`
}

// Use is one use of a listing, as its buyer sees it.
type Use struct {
	ID         string       `json:"id"`
	ListingID  string       `json:"listing_id"`
	Version    int          `json:"version"`
	Kind       string       `json:"kind"`
	Model      string       `json:"model"`
	Charge     string       `json:"charge"`
	PriceULXC  int64        `json:"price_ulxc"`
	Output     string       `json:"output,omitempty"`
	Cases      []CaseResult `json:"cases,omitempty"`
	UsedAt     time.Time    `json:"used_at"`
	AgentID    string       `json:"agent_id,omitempty"`
	LicenceID  string       `json:"licence_id,omitempty"` // the licence it ran under (B32.19)
	Steps      []StepResult `json:"steps,omitempty"`      // a pipeline's steps, in the order they ran (B20.7)
	MeterError string       `json:"-"`

	// B32.21: a trial use, or one with a step that was — free, and what it would have cost had it been billed.
	Trial                  bool  `json:"trial,omitempty"`
	TrialUsesLeft          *int  `json:"trial_uses_left,omitempty"` // of the listing's trial uses, for the buyer's owner
	WouldHaveCostUSDMicros int64 `json:"would_have_cost_usd_micros,omitempty"`
	trialULXC              int64 // a trial's would-be price
}

// StepResult is one step of a pipeline use: the listing it ran, what it answered, and — when the step is
// another seller's listing — its own use on the buyer's bill.
type StepResult struct {
	Step      int    `json:"step"`
	ListingID string `json:"listing_id"`
	Version   int    `json:"version"`
	Kind      string `json:"kind"`
	Model     string `json:"model"`
	Output    string `json:"output"`
	UseID     string `json:"use_id,omitempty"` // "": the pipeline seller's own listing, covered by the pipeline's price
	Charge    string `json:"charge,omitempty"`
	PriceULXC int64  `json:"price_ulxc,omitempty"`
}

// UseDeps are what a use needs besides the catalog.
type UseDeps struct {
	Runner Runner
	Meter  Meter      // nil: paid listings cannot be used by anyone but their seller
	Agents AgentJudge // nil: no agent keys
	// Charged, when set, is told the use as charged once it is recorded, before it runs (B28.186: Chat names the
	// charge on the answer it is about to stream).
	Charged func(Use)
}

// Use runs listingID for buyerWorkspaceID (agentID when an agent's key asked; "" otherwise) and records
// the use: its charge is metered onto the buyer's bill once the run has answered.
func (s *Store) Use(ctx context.Context, deps UseDeps, buyerWorkspaceID, agentID, listingID string, req UseRequest) (Use, error) {
	if req.MaxPriceUSDMicros != nil && *req.MaxPriceUSDMicros < 0 {
		return Use{}, invalid("max_price_usd_micros cannot be negative")
	}
	// B32.19: a licence the buyer holds runs its pinned version unless the use names one, and covers the charge.
	lic, err := s.coverUse(ctx, buyerWorkspaceID, agentID, req.Person, listingID, req.Version)
	if err != nil {
		return Use{}, err
	}
	version := req.Version
	if lic != nil && version == 0 && lic.pinned != nil {
		version = *lic.pinned
	}
	l, artifact, version, err := s.resolve(ctx, buyerWorkspaceID, listingID, version)
	if err != nil {
		return Use{}, err
	}
	var calls []call
	var model string
	var steps []pipelineStep
	if l.Kind == "pipeline" {
		steps, err = s.pipelineSteps(ctx, l, artifact, buyerWorkspaceID, req)
	} else {
		calls, model, err = plan(l.Kind, artifact, req)
	}
	if err != nil {
		return Use{}, err
	}
	u := Use{ID: "use_" + uuid.NewString(), ListingID: l.ID, Version: version, Kind: l.Kind, Model: model, AgentID: agentID}
	var ids []string
	var billed map[string]int64

	// The use is recorded before it runs, so an agent's limits count it the moment it is judged; a run that
	// fails removes it again, and only a use that ran is ever metered.
	record := func(tx pgx.Tx) error {
		ids = []string{u.ID}
		if u.Charge == ChargeLicensed {
			if err := claimLicence(ctx, tx, s.now(), u.LicenceID, buyerWorkspaceID, agentID, req.Person, l.ID, version); err != nil {
				return err
			}
		}
		left, err := claimTrials(ctx, tx, buyerWorkspaceID, &u, steps)
		if err != nil {
			return err
		}
		if u.Charge == ChargeTrial {
			u.TrialUsesLeft = &left
		}
		// Stamped by the clock a licence's periods are (B32.20): a rent's or subscription's included uses count per period.
		if err := tx.QueryRow(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, price_ulxc, charge,
				licence_id, person_id, used_at, use_kind, room_id, actor_workspace_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11, $12, $13, $14) RETURNING used_at`,
			u.ID, l.ID, version, l.WorkspaceID, buyerWorkspaceID, agentID, u.PriceULXC, u.Charge, u.LicenceID, req.Person, s.now(),
			useKind(u.Charge), req.RoomID, req.ActorWorkspaceID).Scan(&u.UsedAt); err != nil {
			return err
		}
		for _, st := range steps {
			if st.UseID == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, price_ulxc, charge, used_at,
					use_kind, room_id, actor_workspace_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
				st.UseID, st.ListingID, st.Version, st.seller, buyerWorkspaceID, agentID, st.PriceULXC, st.Charge, u.UsedAt, useKind(st.Charge),
				req.RoomID, req.ActorWorkspaceID); err != nil {
				return err
			}
			ids = append(ids, st.UseID)
		}
		return nil
	}
	noTrial := false
	for attempt := 0; ; attempt++ {
		if lic != nil && lic.charged && l.WorkspaceID != buyerWorkspaceID {
			u.Charge, u.PriceULXC, u.LicenceID = ChargeLicensed, 0, lic.id
		} else if u.Charge, u.PriceULXC, err = s.chargeFor(ctx, l, buyerWorkspaceID); err != nil {
			return Use{}, err
		} else {
			u.LicenceID = ""
		}
		// B32.21: a billed use, or step, of a listing its buyer's owner still has a trial use of is a trial.
		u.Trial, u.trialULXC, u.TrialUsesLeft = false, 0, nil
		for i := range steps {
			steps[i].Charge, steps[i].PriceULXC, steps[i].trialULXC = steps[i].charge, steps[i].priceULXC, 0
		}
		if !noTrial {
			if err := s.offerTrials(ctx, buyerWorkspaceID, &u, steps); err != nil {
				return Use{}, err
			}
		}
		// Every billed use this makes: the listing's own, and each pipeline step that is another seller's.
		billed = map[string]int64{}
		if u.Charge == ChargeBilled {
			billed[u.ID] = u.PriceULXC
		}
		total := u.PriceULXC
		for _, st := range steps {
			if st.UseID != "" && st.Charge == ChargeBilled {
				billed[st.UseID] = st.PriceULXC
				total += st.PriceULXC
			}
		}
		if most := req.MaxPriceUSDMicros; most != nil && total/ulxcPerUSDMicro > *most {
			return Use{}, fmt.Errorf("%w: it costs %d µUSD now, above your %d — nothing ran and nothing was charged",
				ErrOverMaxPrice, total/ulxcPerUSDMicro, *most)
		}
		if len(billed) > 0 && deps.Meter == nil {
			return Use{}, ErrNoBill
		}
		// An agent's every use is judged — a free one too, since its rules may name the listings it may use. A
		// pipeline is judged once, for all it bills.
		if agentID != "" && deps.Agents != nil {
			what := fmt.Sprintf("market:%s:%s:%d:%d", agentID, l.ID, version, total)
			err = deps.Agents.JudgeAgentPurchase(ctx, buyerWorkspaceID, agentID, l.ID, total, economy.Commitment{}, what, record)
		} else if err = pgx.BeginFunc(ctx, s.pool, record); err != nil {
			err = fmt.Errorf("market: record use: %w", err)
		}
		if errors.Is(err, errLicenceUsedUp) && attempt == 0 {
			lic.charged = false // its last included use went to another use first: this one is billed
			continue
		}
		if errors.Is(err, errTrialUsedUp) && !noTrial {
			noTrial = true // another use took the last trial first: this one is billed
			continue
		}
		if err != nil {
			return Use{}, err
		}
		break
	}

	if deps.Charged != nil {
		deps.Charged(u)
	}
	started := time.Now()
	if l.Kind == "pipeline" {
		err = runPipeline(ctx, deps.Runner, steps, req, &u)
	} else {
		err = run(ctx, deps.Runner, calls, model, &u)
	}
	// B41.173: every run's outcome is kept for the listing's 30-day performance; a buyer who hung up is not the listing failing.
	// shortcut: a pipeline's outcome is the pipeline listing's alone, not each step listing's; record steps when step timings exist.
	outcome := func(q interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, ok bool) error {
		_, err := q.Exec(context.WithoutCancel(ctx), `INSERT INTO market_use_outcomes (use_id, listing_id, ok, latency_ms, charge, price_ulxc)
			VALUES ($1, $2, $3, $4, $5, $6)`, u.ID, l.ID, ok, time.Since(started).Milliseconds(), u.Charge, u.PriceULXC)
		return err
	}
	if err != nil {
		if _, derr := s.pool.Exec(ctx, `DELETE FROM market_uses WHERE id = ANY($1) AND metered_at IS NULL`, ids); derr != nil {
			return Use{}, errors.Join(err, derr)
		}
		if !errors.Is(err, context.Canceled) {
			if oerr := outcome(s.pool, false); oerr != nil {
				return Use{}, errors.Join(err, oerr)
			}
		}
		return Use{}, err
	}
	// A trial is journalled once it has answered, with the use it explains.
	if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE market_uses SET ran_at = now() WHERE id = ANY($1)`, ids); err != nil {
			return err
		}
		if err := outcome(tx, true); err != nil {
			return err
		}
		if u.Charge == ChargeTrial {
			if err := postTrialTx(ctx, tx, u.ID, l.WorkspaceID, u.trialULXC/ulxcPerUSDMicro, u.UsedAt); err != nil {
				return err
			}
			u.WouldHaveCostUSDMicros += u.trialULXC / ulxcPerUSDMicro
		}
		for _, st := range steps {
			if st.Charge == ChargeTrial {
				if err := postTrialTx(ctx, tx, st.UseID, st.seller, st.trialULXC/ulxcPerUSDMicro, u.UsedAt); err != nil {
					return err
				}
				u.WouldHaveCostUSDMicros += st.trialULXC / ulxcPerUSDMicro
			}
		}
		return nil
	}); err != nil {
		return u, fmt.Errorf("market: record use: %w", err)
	}
	for _, id := range ids {
		if price, ok := billed[id]; ok {
			if err := s.meter(ctx, deps.Meter, id, buyerWorkspaceID, price, u.UsedAt); err != nil {
				u.MeterError = err.Error() // the buyer has the answer; MeterPending bills it on its next pass
			}
		}
	}
	return u, nil
}

// chargeFor says what one use of l costs buyer: nothing for their own or a free listing, nothing for a
// seller they share a card or an owner with (a wash trade), else the price of its per_use commercial offer (B32.18).
func (s *Store) chargeFor(ctx context.Context, l Listing, buyer string) (string, int64, error) {
	if l.WorkspaceID == buyer {
		return ChargeOwn, 0, nil
	}
	price, err := perUsePrice(ctx, s.pool, l.ID)
	if err != nil {
		return "", 0, err
	}
	if price == 0 {
		return ChargeFree, 0, nil
	}
	if err := workspace.CheckMoneyWall(ctx, s.pool, buyer, l.WorkspaceID); err != nil {
		return "", 0, err
	}
	linked, err := s.linked(ctx, l.WorkspaceID, buyer)
	if err != nil {
		return "", 0, err
	}
	if linked {
		return ChargeLinked, 0, nil
	}
	if err := s.sellable(ctx, buyer); err != nil {
		return "", 0, err
	}
	return ChargeBilled, price, nil
}

// A use Stripe refuses waits MeterRefusalBackoff before it is tried again, three times longer after each
// refusal (10m, 30m, 1h30, 4h30); its MaxMeterRefusals-th refusal parks it for an operator.
const (
	MaxMeterRefusals    = 5
	MeterRefusalBackoff = 10 * time.Minute
)

// meterRefusal is Stripe's reason when it answered a meter event with a refusal of that one use — a 4xx
// other than 401/403 (Lens's own key), 409 (a request in flight) and 429 (slow down), which would refuse
// every use alike. "" when it did not: Stripe unreachable, or our side.
func meterRefusal(err error) string {
	if errors.Is(err, billing.ErrNoMarketTax) { // B32.39: its bill cannot carry its tax — only this use's, never every use's
		return err.Error()
	}
	var se *stripe.Error
	if !errors.As(err, &se) || se.HTTPStatusCode < 400 || se.HTTPStatusCode >= 500 {
		return ""
	}
	switch se.HTTPStatusCode {
	case 401, 403, 409, 429:
		return ""
	}
	reason := se.Msg
	if se.Code != "" {
		reason = string(se.Code) + ": " + reason
	}
	if reason == "" {
		reason = fmt.Sprintf("Stripe answered %d", se.HTTPStatusCode)
	}
	return reason
}

// meter puts one use on its buyer's bill, and then its tax beside it (B32.39) — each once: the price is metered_at
// when Stripe has it, the tax tax_metered_at (or when it is found to be nothing), so a retry sends only what is
// missing, and a tax that cannot be worked out or billed yet never holds the price back. A use Stripe refuses records
// the reason, counts the refusal and waits out its backoff before MeterPending tries it again; its
// MaxMeterRefusals-th refusal parks it.
func (s *Store) meter(ctx context.Context, m Meter, useID, buyer string, ulxc int64, at time.Time) error {
	var priceBilled, taxBilled bool
	if err := s.pool.QueryRow(ctx, `SELECT metered_at IS NOT NULL, tax_metered_at IS NOT NULL FROM market_uses WHERE id = $1`, useID).
		Scan(&priceBilled, &taxBilled); err != nil {
		return fmt.Errorf("market: meter %s: %w", useID, err)
	}
	var err error
	if !priceBilled {
		if err = m.MeterMarketUse(ctx, buyer, useID, ulxc, at); err == nil {
			_, err = s.pool.Exec(ctx, `UPDATE market_uses SET metered_at = now() WHERE id = $1 AND metered_at IS NULL`, useID)
		}
	}
	if err == nil && !taxBilled {
		var tax *TaxLine
		if tax, err = s.taxUse(ctx, useID, buyer, ulxc, at); err == nil {
			err = meterTax(ctx, m, tax, buyer, useID, at)
		}
		if err == nil {
			_, err = s.pool.Exec(ctx, `UPDATE market_uses SET tax_metered_at = now() WHERE id = $1 AND tax_metered_at IS NULL`, useID)
		}
	}
	if reason := meterRefusal(err); reason != "" {
		if _, uerr := s.pool.Exec(ctx, `UPDATE market_uses SET meter_refusals = meter_refusals + 1, meter_refused_reason = $2,
			meter_retry_at = now() + make_interval(secs => $3 * power(3, meter_refusals)),
			meter_parked_at = CASE WHEN meter_refusals + 1 >= $4 THEN now() END
			WHERE id = $1 AND (metered_at IS NULL OR tax_metered_at IS NULL)`, useID, reason, MeterRefusalBackoff.Seconds(), MaxMeterRefusals); uerr != nil {
			return errors.Join(err, uerr)
		}
	}
	return err
}

// MeterPending bills the uses that ran but whose meter event — or tax meter event (B32.39) — Stripe has not yet
// accepted: a Stripe outage when they were used. Each keeps its id as the meter event's identifier, so none is
// billed twice. A use Stripe refuses is skipped and every other use still billed; one refused too often stays parked
// (ParkedUses). Stripe unreachable stops the pass: the next one tries again.
func (s *Store) MeterPending(ctx context.Context, m Meter, olderThan time.Duration) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, buyer_workspace_id, price_ulxc, used_at FROM market_uses
		WHERE charge = 'billed' AND (metered_at IS NULL OR tax_metered_at IS NULL) AND ran_at IS NOT NULL AND ran_at < now() - make_interval(secs => $1)
		  AND meter_parked_at IS NULL AND (meter_retry_at IS NULL OR meter_retry_at <= now())
		  AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = market_uses.id) -- refunded before it was billed: never billed
		ORDER BY used_at LIMIT 200`, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("market: pending uses: %w", err)
	}
	type pending struct {
		id, buyer string
		ulxc      int64
		at        time.Time
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.buyer, &p.ulxc, &p.at); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("market: pending uses: %w", err)
	}
	n := 0
	for _, p := range todo {
		if err := s.meter(ctx, m, p.id, p.buyer, p.ulxc, p.at); err != nil {
			if meterRefusal(err) != "" {
				continue
			}
			return n, err
		}
		n++
	}
	return n, nil
}

// ParkedUse is a billed use Stripe refused MaxMeterRefusals times: never on its buyer's bill until an
// operator sees to it.
type ParkedUse struct {
	ID               string    `json:"id"`
	ListingID        string    `json:"listing_id"`
	BuyerWorkspaceID string    `json:"buyer_workspace_id"`
	PriceULXC        int64     `json:"price_ulxc"`
	UsedAt           time.Time `json:"used_at"`
	Refusals         int       `json:"refusals"`
	Reason           string    `json:"reason"` // Stripe's, from its last refusal
	ParkedAt         time.Time `json:"parked_at"`
}

// ParkedUses lists every parked use still off its buyer's bill, most recently parked first.
func (s *Store) ParkedUses(ctx context.Context) ([]ParkedUse, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, listing_id, buyer_workspace_id, price_ulxc, used_at, meter_refusals, meter_refused_reason, meter_parked_at
		FROM market_uses WHERE meter_parked_at IS NOT NULL AND (metered_at IS NULL OR tax_metered_at IS NULL) ORDER BY meter_parked_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("market: parked uses: %w", err)
	}
	parked, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ParkedUse, error) {
		var p ParkedUse
		err := r.Scan(&p.ID, &p.ListingID, &p.BuyerWorkspaceID, &p.PriceULXC, &p.UsedAt, &p.Refusals, &p.Reason, &p.ParkedAt)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("market: parked uses: %w", err)
	}
	return parked, nil
}

// ErrNotParked: no parked use by that id still off its buyer's bill.
var ErrNotParked = errors.New("market: no such parked use")

// RetryParkedUse un-parks a use an operator has seen to, so the next MeterPending pass tries it at once.
// Its refusals are kept: Stripe refusing it once more parks it again.
func (s *Store) RetryParkedUse(ctx context.Context, useID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE market_uses SET meter_parked_at = NULL, meter_retry_at = NULL
		WHERE id = $1 AND meter_parked_at IS NOT NULL AND (metered_at IS NULL OR tax_metered_at IS NULL)`, useID)
	if err != nil {
		return fmt.Errorf("market: retry parked use: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotParked
	}
	return nil
}

// resolve reads the listing and the version the buyer may use, with its artifact. A held listing is its
// owner's alone to use; a taken-down one is nobody's.
func (s *Store) resolve(ctx context.Context, buyer, listingID string, version int) (Listing, map[string]any, int, error) {
	l, err := visibleListing(ctx, s.pool, buyer, listingID, "")
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, nil, 0, ErrNotFound
	}
	if err != nil {
		return Listing{}, nil, 0, fmt.Errorf("market: listing: %w", err)
	}
	if l.ReviewStatus == ReviewTakenDown {
		return Listing{}, nil, 0, ErrTakenDown
	}
	if version == 0 {
		version = l.LatestVersion
	} else if version > l.LatestVersion {
		return Listing{}, nil, 0, fmt.Errorf("%w: it has no version %d", ErrNotFound, version) // B17.206: held, or not there
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, `SELECT artifact FROM market_listing_versions WHERE listing_id = $1 AND version = $2`, listingID, version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Listing{}, nil, 0, fmt.Errorf("%w: it has no version %d", ErrNotFound, version)
	}
	if err != nil {
		return Listing{}, nil, 0, fmt.Errorf("market: version: %w", err)
	}
	var artifact map[string]any
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return Listing{}, nil, 0, fmt.Errorf("market: version %d of %s is unreadable: %w", version, listingID, err)
	}
	return l, artifact, version, nil
}

// linked is the single-party detector's question (U6 PR2, poolroyalty.sharedFingerprintSQL): do the two
// workspaces share a captured card fingerprint or an owner key?
func (s *Store) linked(ctx context.Context, a, b string) (bool, error) {
	var linked bool
	err := s.pool.QueryRow(ctx, `SELECT `+linkedSQL("$1", "$2"), a, b).Scan(&linked)
	if err != nil {
		return false, fmt.Errorf("market: single-party check: %w", err)
	}
	return linked, nil
}

// linkedSQL is that question as SQL, for the workspaces the expressions a and b name.
func linkedSQL(a, b string) string {
	return fmt.Sprintf(`(EXISTS (SELECT 1 FROM workspace_card_fingerprints x JOIN workspace_card_fingerprints y ON x.fingerprint_hash = y.fingerprint_hash
		        WHERE x.workspace_id = %[1]s AND y.workspace_id = %[2]s)
		OR EXISTS (SELECT 1 FROM workspace_owner_links x JOIN workspace_owner_links y ON x.owner_key = y.owner_key
		        WHERE x.workspace_id = %[1]s AND y.workspace_id = %[2]s))`, a, b)
}

var variable = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// maxCases bounds the model calls one evaluation use makes.
const maxCases = 50

type call struct {
	messages []Message
	expected string
	input    string
}

// plan turns a version's artifact and the buyer's request into the model calls a use makes.
func plan(kind string, artifact map[string]any, req UseRequest) ([]call, string, error) {
	model := req.Model
	if model == "" {
		model, _ = artifact["model"].(string)
	}
	if kind == "pipeline" {
		return nil, "", ErrNotRunnable // a pipeline's steps are planned one at a time (pipeline.go)
	}
	if model == "" {
		return nil, "", ErrNoModel
	}
	str := func(k string) string { v, _ := artifact[k].(string); return v }
	switch kind {
	case "agent", "skill":
		system := str("system_prompt")
		if kind == "skill" {
			system = str("instructions")
		}
		if strings.TrimSpace(req.Input) == "" {
			return nil, "", fmt.Errorf("%w: using a %s needs an input", ErrInvalid, kind)
		}
		return []call{{messages: []Message{{"system", system}, {"user", req.Input}}}}, model, nil
	case "prompt":
		var missing []string
		text := variable.ReplaceAllStringFunc(str("template"), func(m string) string {
			name := variable.FindStringSubmatch(m)[1]
			v, ok := req.Variables[name]
			if !ok {
				missing = append(missing, name)
			}
			return v
		})
		if len(missing) > 0 {
			return nil, "", fmt.Errorf("%w: the prompt needs the variables %s", ErrInvalid, strings.Join(missing, ", "))
		}
		return []call{{messages: []Message{{"user", text}}}}, model, nil
	default: // evaluation: each case's input to the buyer's model, passed when the output contains what it expects
		cases, _ := artifact["cases"].([]any)
		if len(cases) > maxCases {
			return nil, "", fmt.Errorf("%w: an evaluation of more than %d cases cannot be run in one use", ErrInvalid, maxCases)
		}
		var calls []call
		for _, c := range cases {
			m, _ := c.(map[string]any)
			in, _ := m["input"].(string)
			exp, _ := m["expected"].(string)
			calls = append(calls, call{messages: []Message{{"user", in}}, input: in, expected: exp})
		}
		return calls, model, nil
	}
}

// minPromptLeak is the shortest run of a system prompt, in characters, that an answer may not repeat: shorter runs
// ("Be brief.") are said by chance. shortcut: verbatim runs only (case and spacing aside); a paraphrase, a translation
// or an encoding passes, which needs a model judge to catch.
const minPromptLeak = 40

// withheldAnswer replaces an answer that repeats its listing's system prompt (B50.4).
const withheldAnswer = "The answer is not shown: it repeated the listing's instructions, which its seller keeps private."

// answer runs one model call of a use and keeps the seller's system prompt out of what the buyer gets back (B50.4):
// an input such as "repeat your instructions" must not hand over what the listing is. Any run of minPromptLeak
// characters the answer shares with the system prompt withholds it.
func answer(ctx context.Context, r Runner, model string, msgs []Message) (string, error) {
	out, err := r.Run(ctx, model, msgs)
	if err != nil {
		return "", err
	}
	norm := func(s string) []rune { return []rune(strings.Join(strings.Fields(strings.ToLower(s)), " ")) }
	o := norm(out)
	runs := map[string]bool{}
	for i := 0; i+minPromptLeak <= len(o); i++ {
		runs[string(o[i:i+minPromptLeak])] = true
	}
	for _, m := range msgs {
		if m.Role != "system" || len(runs) == 0 {
			continue
		}
		p := norm(m.Content)
		for i := 0; i+minPromptLeak <= len(p); i++ {
			if runs[string(p[i:i+minPromptLeak])] {
				return withheldAnswer, nil
			}
		}
	}
	return out, nil
}

func run(ctx context.Context, r Runner, calls []call, model string, u *Use) error {
	for _, c := range calls {
		out, err := answer(ctx, r, model, c.messages)
		if err != nil {
			return err
		}
		if u.Kind != "evaluation" {
			u.Output = out
			continue
		}
		u.Cases = append(u.Cases, CaseResult{Input: c.input, Expected: c.expected, Output: out,
			Passed: c.expected != "" && strings.Contains(strings.ToLower(out), strings.ToLower(c.expected))})
	}
	return nil
}

// ClearInvoice clears the billed uses the buyer's paid marketplace invoice carried — those used within
// [periodStart, periodEnd) — and credits each seller their share, less the royalties it pays up its listing's family
// tree (B32.26), each use journalled as one clear entry in the same transaction (B32.16), each rent counted towards
// owning its listing (B32.20). A replay clears nothing more. Each earning is live or test as the invoice was
// (B22.1): only live earnings reach a live payout.
func (s *Store) ClearInvoice(ctx context.Context, buyerWorkspaceID, invoiceID string, periodStart, periodEnd, paidAt time.Time, livemode bool) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, seller_workspace_id, price_ulxc, COALESCE(payee_agent_id, '') <> '', COALESCE(licence_id, ''), use_kind,
			       listing_id, version FROM market_uses
			WHERE buyer_workspace_id = $1 AND charge = 'billed' AND metered_at IS NOT NULL AND cleared_at IS NULL
			  AND used_at >= $2 AND used_at < $3
			ORDER BY used_at, id FOR UPDATE`, buyerWorkspaceID, periodStart, periodEnd)
		if err != nil {
			return err
		}
		type cleared struct {
			id, seller string
			ulxc       int64
			payment    bool // a payment to another company's agent, not a use of a listing
			licence    string
			kind       string // its use_kind
			sold       versionRef
		}
		var uses []cleared
		for rows.Next() {
			var c cleared
			if err := rows.Scan(&c.id, &c.seller, &c.ulxc, &c.payment, &c.licence, &c.kind, &c.sold.listing, &c.sold.version); err != nil {
				rows.Close()
				return err
			}
			uses = append(uses, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range uses {
			// A use refunded before its invoice was paid (its listing was taken down, B20.4) earns nothing: the
			// buyer is credited it on their next bill. Asked under the use's lock, so a refund racing this sees
			// the earning, or this sees the refund.
			var refunded bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM market_refunds WHERE use_id = $1)`, c.id).Scan(&refunded); err != nil {
				return err
			}
			if refunded {
				continue
			}
			gross := c.ulxc / ulxcPerUSDMicro
			share := SellerShare(gross, takeBPS(c.payment))
			// B32.26: the pool flows up the sold version's family tree; the seller keeps what does not.
			keep, royalties := share, []royalty(nil)
			if !c.payment {
				var test bool // a test workspace's earning is test money whatever paid it (B25.1)
				if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`, c.seller).Scan(&test); err != nil {
					return err
				}
				if keep, royalties, err = s.lineageRoyaltiesTx(ctx, tx, buyerWorkspaceID, c.seller, c.sold, share, funding(livemode, test) == "live"); err != nil {
					return err
				}
			}
			payees := make([]Posting, 0, 1+len(royalties))
			earn := func(payee, kind, ref string, depth int, gross, fee, amount int64) error {
				var test bool // set by the database from the payee's workspace (0173)
				if err := tx.QueryRow(ctx, `INSERT INTO market_earnings (use_id, seller_workspace_id, kind, source_ref, depth, gross_usd_micros, share_usd_micros,
					fee_usd_micros, invoice_id, cleared_at, payable_at, livemode) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING test`,
					c.id, payee, kind, ref, depth, gross, amount, fee, invoiceID, paidAt, paidAt.Add(Holdback), livemode).Scan(&test); err != nil {
					return err
				}
				payees = append(payees, Posting{Account: SellerHoldback(payee), AmountUSDMicros: -amount, Funding: funding(livemode, test)})
				return nil
			}
			if err := earn(c.seller, EarningSale, "", 0, gross, gross-share, keep); err != nil {
				return err
			}
			for _, r := range royalties {
				if err := earn(r.payee, EarningLineage, r.edgeID, r.depth, 0, 0, r.amount); err != nil {
					return err
				}
			}
			// B32.39: the invoice collected the use's tax beside its price, once it was metered; it is owed to the tax
			// authority.
			var jurisdiction string
			var tax int64
			if err := tx.QueryRow(ctx, `SELECT t.jurisdiction, t.tax_usd_micros FROM market_tax_lines t JOIN market_uses u ON u.id = t.use_id
				WHERE t.use_id = $1 AND u.tax_metered_at IS NOT NULL`, c.id).
				Scan(&jurisdiction, &tax); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if tax > 0 {
				payees = append(payees, Posting{Account: AccountTax(jurisdiction), AmountUSDMicros: -tax, Funding: payees[0].Funding})
			}
			if err := postClearTx(ctx, tx, c.id, gross+tax, gross-share, payees[0].Funding, paidAt, payees...); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE market_uses SET cleared_invoice_id = $2, cleared_at = $3 WHERE id = $1`, c.id, invoiceID, paidAt); err != nil {
				return err
			}
			if c.licence != "" && (c.kind == OfferRent || c.kind == "renewal") { // B32.20: rents add up to ownership
				if err := rentCleared(ctx, tx, c.licence, gross, paidAt); err != nil {
					return err
				}
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("market: clear invoice %s: %w", invoiceID, err)
	}
	return n, nil
}

// SellerShare is what a seller keeps of gross (µUSD, the price before tax) when Talyvor takes takeBPS of it,
// from the first dollar: gross × (10,000 − takeBPS) ÷ 10,000, rounded down. Talyvor's fee is gross − share, so
// rounding never pays out more than the sale.
func SellerShare(gross, takeBPS int64) int64 {
	keep := fees.BPSDenominator - takeBPS
	return gross/fees.BPSDenominator*keep + gross%fees.BPSDenominator*keep/fees.BPSDenominator
}

// takeBPS is Talyvor's take on a use of a listing, or on a payment to another company's agent (a service).
func takeBPS(payment bool) int64 {
	if payment {
		return fees.Current().ServicesTakeBPS
	}
	return fees.Current().MarketTakeBPS
}

// Earnings is a seller's marketplace earnings, in µUSD.
type Earnings struct {
	// Pending: uses of the seller's listings billed to buyers who have not yet paid that bill — how many,
	// and what the seller's share of them would be today. They become earnings when the invoice is paid.
	PendingUses         int64     `json:"pending_uses"`
	PendingUSDMicros    int64     `json:"pending_usd_micros"`
	PayableUSDMicros    int64     `json:"payable_usd_micros"`     // every share earned and not yet paid out (B20.5)
	InHoldbackUSDMicros int64     `json:"in_holdback_usd_micros"` // of which still inside the 14-day holdback
	AvailableUSDMicros  int64     `json:"available_usd_micros"`   // of which past it
	PaidOutUSDMicros    int64     `json:"paid_out_usd_micros"`    // every payout, in money or credits (B20.5)
	OwedUSDMicros       int64     `json:"owed_usd_micros"`        // reversed after it was paid out: recovered from future earnings
	LifetimeGrossMicros int64     `json:"lifetime_gross_usd_micros"`
	RefundedUSDMicros   int64     `json:"refunded_usd_micros"` // shares reversed by refunds and chargebacks, already out of the totals above
	Earnings            []Earning `json:"earnings"`
}

// Earning is one cleared use's share: the seller's, or an original's royalty from a remix's sale (B32.26).
type Earning struct {
	UseID          string     `json:"use_id"`
	ListingID      string     `json:"listing_id"`
	Kind           string     `json:"kind"`                          // sale, or lineage: a royalty from a remix's sale
	Depth          int        `json:"depth,omitempty"`               // a royalty's generation: 1 a parent of the listing sold
	OriginalID     string     `json:"original_listing_id,omitempty"` // a royalty's listing: the original the listing sold builds on
	GrossUSDMicros int64      `json:"gross_usd_micros"`              // what the buyer paid: on the sale, 0 on a royalty
	ShareUSDMicros int64      `json:"share_usd_micros"`              // what this payee keeps
	FeeUSDMicros   int64      `json:"fee_usd_micros"`                // Talyvor's take, on the sale (0 on one cleared before B32.8)
	InvoiceID      string     `json:"invoice_id"`
	ClearedAt      time.Time  `json:"cleared_at"`
	PayableAt      time.Time  `json:"payable_at"`
	RefundedAt     *time.Time `json:"refunded_at,omitempty"`    // its share was reversed: the listing was taken down
	PayeeAgentID   string     `json:"payee_agent_id,omitempty"` // a payment to this company's agent, not a use of a listing (B19.15)
	HeldFor        string     `json:"held_for,omitempty"`       // dispute or ip_claim: kept in the holdback until the hold is released (B32.17)
}

// SellerEarnings reads a seller's earnings: the totals and the latest 100. An earning a refund or chargeback
// reversed counts in none of the totals but RefundedUSDMicros, and every payout comes out of what is
// available; a payout the reversals have since overtaken is owed, out of what the holdback still holds.
func (s *Store) SellerEarnings(ctx context.Context, sellerWorkspaceID string, now time.Time) (Earnings, error) {
	e := Earnings{Earnings: []Earning{}}
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(e.gross_usd_micros), 0)::bigint,
		       COALESCE(sum(e.share_usd_micros) FILTER (WHERE r.use_id IS NOT NULL), 0)::bigint
		FROM market_earnings e LEFT JOIN market_refunds r ON r.use_id = e.use_id
		WHERE e.seller_workspace_id = $1`, sellerWorkspaceID).
		Scan(&e.LifetimeGrossMicros, &e.RefundedUSDMicros); err != nil {
		return e, fmt.Errorf("market: earnings: %w", err)
	}
	released, inHoldback, paid, err := sellerBalance(ctx, s.pool, sellerWorkspaceID, now)
	if err != nil {
		return e, fmt.Errorf("market: earnings: %w", err)
	}
	e.InHoldbackUSDMicros, e.PaidOutUSDMicros = inHoldback, paid
	e.AvailableUSDMicros, e.OwedUSDMicros = max(released-paid, 0), max(paid-released, 0)
	e.PayableUSDMicros = max(inHoldback+released-paid, 0)
	var usesULXC, paymentsULXC int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*),
		       COALESCE(sum(price_ulxc) FILTER (WHERE COALESCE(payee_agent_id, '') = ''), 0)::bigint,
		       COALESCE(sum(price_ulxc) FILTER (WHERE COALESCE(payee_agent_id, '') <> ''), 0)::bigint
		FROM market_uses u
		WHERE seller_workspace_id = $1 AND charge = 'billed' AND ran_at IS NOT NULL AND cleared_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = u.id)`,
		sellerWorkspaceID).Scan(&e.PendingUses, &usesULXC, &paymentsULXC); err != nil {
		return e, fmt.Errorf("market: pending earnings: %w", err)
	}
	e.PendingUSDMicros = SellerShare(usesULXC/ulxcPerUSDMicro, takeBPS(false)) + SellerShare(paymentsULXC/ulxcPerUSDMicro, takeBPS(true))
	rows, err := s.pool.Query(ctx, `SELECT e.use_id, COALESCE(u.listing_id, ''), e.kind, e.depth, COALESCE(lin.parent_listing_id, ''), e.gross_usd_micros, e.share_usd_micros, e.fee_usd_micros, e.invoice_id, e.cleared_at, e.payable_at, r.refunded_at,
		       COALESCE(u.payee_agent_id, ''),
		       COALESCE((SELECT h.reason FROM market_holds h WHERE h.use_id = e.use_id AND h.released_at IS NULL ORDER BY h.opened_at LIMIT 1), '')
		FROM market_earnings e LEFT JOIN market_uses u ON u.id = e.use_id LEFT JOIN market_refunds r ON r.use_id = e.use_id
		LEFT JOIN market_lineage lin ON lin.id = e.source_ref AND e.kind = 'lineage'
		WHERE e.seller_workspace_id = $1 ORDER BY e.cleared_at DESC, e.use_id, e.id LIMIT 100`, sellerWorkspaceID)
	if err != nil {
		return e, fmt.Errorf("market: earnings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x Earning
		if err := rows.Scan(&x.UseID, &x.ListingID, &x.Kind, &x.Depth, &x.OriginalID, &x.GrossUSDMicros, &x.ShareUSDMicros, &x.FeeUSDMicros, &x.InvoiceID, &x.ClearedAt, &x.PayableAt, &x.RefundedAt,
			&x.PayeeAgentID, &x.HeldFor); err != nil {
			return e, err
		}
		e.Earnings = append(e.Earnings, x)
	}
	return e, rows.Err()
}

// BillLine is one use on a buyer's marketplace bill.
type BillLine struct {
	UseID     string     `json:"use_id"`
	ListingID string     `json:"listing_id"`
	Title     string     `json:"title"`
	AgentID   string     `json:"agent_id,omitempty"`
	PriceULXC int64      `json:"price_ulxc"`
	UsedAt    time.Time  `json:"used_at"`
	Cleared   *time.Time `json:"cleared_at,omitempty"`
	Refunded  *time.Time `json:"refunded_at,omitempty"` // credited back: the listing was taken down (B20.4)
	// A payment to another company's agent (B19.15): the agent paid, and the memo.
	PayeeAgentID string `json:"payee_agent_id,omitempty"`
	Memo         string `json:"memo,omitempty"`
	// B32.39: the tax on the line, on top of its price — none until the use is metered.
	TaxUSDMicros    int64  `json:"tax_usd_micros"`
	TaxRateBps      int    `json:"tax_rate_bps"`
	TaxJurisdiction string `json:"tax_jurisdiction,omitempty"`
	TaxTreatment    string `json:"tax_treatment,omitempty"`
	TaxNote         string `json:"tax_note,omitempty"`
}

// Bill is a buyer's marketplace uses billed in one month (UTC). The totals are what the buyer owes for
// them: a refunded use is listed, and counts in RefundedULXC instead. TotalULXC and TotalUSDMicros are the prices
// before tax; the net, tax and gross totals are what the buyer pays, its tax included (B32.39).
type Bill struct {
	Month          string     `json:"month"`
	TotalULXC      int64      `json:"total_ulxc"`
	TotalUSDMicros int64      `json:"total_usd_micros"`
	RefundedULXC   int64      `json:"refunded_ulxc"`
	NetUSDMicros   int64      `json:"net_usd_micros"`
	TaxUSDMicros   int64      `json:"tax_usd_micros"`
	GrossUSDMicros int64      `json:"gross_usd_micros"`
	Lines          []BillLine `json:"lines"`
	Invoice        *Invoice   `json:"invoice,omitempty"` // the invoice it was read for (B28.140, invoices.go)
}

// MonthBill reads the billed uses buyerWorkspaceID made in month ("2006-01").
func (s *Store) MonthBill(ctx context.Context, buyerWorkspaceID string, month time.Time) (Bill, error) {
	from := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	return s.bill(ctx, Bill{Month: from.Format("2006-01")}, `u.ran_at IS NOT NULL AND u.used_at >= $2 AND u.used_at < $3`,
		buyerWorkspaceID, from, from.AddDate(0, 1, 0))
}

// bill reads into b the buyer's ($1) billed uses that match where, and totals them.
func (s *Store) bill(ctx context.Context, b Bill, where string, args ...any) (Bill, error) {
	b.Lines = []BillLine{}
	rows, err := s.pool.Query(ctx, `SELECT u.id, u.listing_id, COALESCE(l.title, 'Payment to ' || a.name, ''), u.agent_id, u.price_ulxc, u.used_at, u.cleared_at,
		       r.refunded_at, u.payee_agent_id, u.memo, COALESCE(t.tax_usd_micros, 0), COALESCE(t.rate_bps, 0), COALESCE(t.jurisdiction, ''),
		       COALESCE(t.treatment, ''), COALESCE(t.note, '')
		FROM market_uses u LEFT JOIN market_listings l ON l.id = u.listing_id LEFT JOIN market_refunds r ON r.use_id = u.id
		LEFT JOIN agent_accounts a ON a.id = u.payee_agent_id AND u.payee_agent_id <> ''
		LEFT JOIN market_tax_lines t ON t.use_id = u.id
		WHERE u.buyer_workspace_id = $1 AND u.charge = 'billed' AND `+where+`
		ORDER BY u.used_at, u.id`, args...)
	if err != nil {
		return b, fmt.Errorf("market: bill: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x BillLine
		if err := rows.Scan(&x.UseID, &x.ListingID, &x.Title, &x.AgentID, &x.PriceULXC, &x.UsedAt, &x.Cleared, &x.Refunded,
			&x.PayeeAgentID, &x.Memo, &x.TaxUSDMicros, &x.TaxRateBps, &x.TaxJurisdiction, &x.TaxTreatment, &x.TaxNote); err != nil {
			return b, err
		}
		if x.Refunded != nil {
			b.RefundedULXC += x.PriceULXC
		} else {
			b.TotalULXC += x.PriceULXC
			b.NetUSDMicros += x.PriceULXC / ulxcPerUSDMicro
			b.TaxUSDMicros += x.TaxUSDMicros
		}
		b.Lines = append(b.Lines, x)
	}
	b.TotalUSDMicros = b.TotalULXC / ulxcPerUSDMicro
	b.GrossUSDMicros = b.NetUSDMicros + b.TaxUSDMicros
	return b, rows.Err()
}
