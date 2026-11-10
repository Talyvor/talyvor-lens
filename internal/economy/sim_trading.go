package economy

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// sim_trading.go — B22.8: INVESTING AND TRADING, SIMULATED UNTIL A BROKER PARTNER EXISTS.
//
// An agent opens simulated portfolios (OpenPortfolio) and places market and limit orders (PlaceSimOrder) on
// real instruments at real prices, executed here and never sent to any market. The instruments are the
// currencies the European Central Bank publishes its euro foreign exchange reference rates for — already
// fetched into ecb_reference_rates for card pricing (B19.12) — each priced in US dollars from the latest
// published day: USD per unit = (USD per EUR) ÷ (unit per EUR), exactly. The ECB allows its statistics to be
// reused, commercially too, when it is cited as the source: every quote and portfolio names it.
//
// A portfolio's cash is simulated US dollars fixed when it opens: never credits, never money, so nothing an
// order does touches a wallet. Cash and positions are derived from the filled orders. A market order fills at
// once at the quote; a limit order fills when the quote crosses its limit (a buy at or under it, a sale at or
// over it) — at once, or on the tick after a new day's rates (RunSimulatedOrders). Live trading is class RED
// (invest_and_trade): only ever through an authorised broker partner, never by Talyvor, so a live order is
// refused naming the class.

// CapabilityInvestAndTrade is the wallet capability live trading would be.
const CapabilityInvestAndTrade = "invest_and_trade"

// MarketDataSource names where every simulated price comes from, as the ECB's reuse terms ask.
const MarketDataSource = "European Central Bank euro foreign exchange reference rates (source: ECB, free at www.ecb.europa.eu)"

// SimulatedNotice is on every portfolio and order.
const SimulatedNotice = "Simulated: executed by Talyvor's simulator at the ECB reference rate. No order is ever sent to a market."

// ErrLiveTrading: an order that asks to go to a real market.
var ErrLiveTrading = errors.New("economy: Investing and trading real assets is class RED: a live order goes only through an authorised broker partner, never Talyvor itself. Portfolios here are simulated")

// ErrNoQuote: no reference rate for the instrument yet.
var ErrNoQuote = errors.New("economy: no quote for this instrument")

// ErrPortfolioNotFound: no such portfolio of this agent.
var ErrPortfolioNotFound = errors.New("economy: no such portfolio")

// ErrSimOrderNotFound: no such open order in this portfolio.
var ErrSimOrderNotFound = errors.New("economy: no such open order")

// ErrSimOrder: an order or a portfolio that is not one.
var ErrSimOrder = errors.New("economy: order")

// ErrSimHoldings: the portfolio does not hold the simulated cash or units the order needs.
var ErrSimHoldings = errors.New("economy: the simulated portfolio does not hold enough for this order")

// maxSimCashUUSD is the most simulated cash a portfolio opens with: 1 000 000 000 USD.
const maxSimCashUUSD = int64(1_000_000_000) * 1_000_000

// Quote is an instrument's simulated price: US dollars per unit, from the ECB's day.
type Quote struct {
	Instrument string `json:"instrument"`
	PriceUSD   string `json:"price_usd"`
	RateDate   string `json:"rate_date"`
	price      *big.Rat
}

// Quotes is every instrument the simulator trades, at the latest ECB day.
type Quotes struct {
	Simulated  bool    `json:"simulated"`
	MarketData string  `json:"market_data"`
	RateDate   string  `json:"rate_date,omitempty"`
	Quotes     []Quote `json:"quotes"`

	byInstrument map[string]Quote
}

// SimOrder is one order in a simulated portfolio.
type SimOrder struct {
	ID             string     `json:"id"`
	PortfolioID    string     `json:"portfolio_id"`
	Instrument     string     `json:"instrument"`
	Side           string     `json:"side"` // buy | sell
	Type           string     `json:"type"` // market | limit
	QuantityMicros int64      `json:"quantity_micros"`
	LimitPriceUSD  string     `json:"limit_price_usd,omitempty"`
	Status         string     `json:"status"` // open | filled | cancelled | rejected
	FillPriceUSD   string     `json:"fill_price_usd,omitempty"`
	FillRateDate   string     `json:"fill_rate_date,omitempty"`
	CashUUSD       int64      `json:"cash_uusd"` // − bought, + sold
	Reason         string     `json:"reason,omitempty"`
	Simulated      bool       `json:"simulated"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
}

// Position is what a portfolio holds of one instrument, valued at its quote.
type Position struct {
	Instrument     string `json:"instrument"`
	QuantityMicros int64  `json:"quantity_micros"`
	PriceUSD       string `json:"price_usd"`
	ValueUUSD      int64  `json:"value_uusd"`
}

// Portfolio is an agent's simulated portfolio, valued at the latest quotes.
type Portfolio struct {
	ID               string     `json:"id"`
	AgentID          string     `json:"agent_id"`
	Name             string     `json:"name"`
	Simulated        bool       `json:"simulated"`
	Notice           string     `json:"notice"`
	MarketData       string     `json:"market_data"`
	RateDate         string     `json:"rate_date,omitempty"`
	StartingCashUUSD int64      `json:"starting_cash_uusd"`
	CashUUSD         int64      `json:"cash_uusd"`
	Positions        []Position `json:"positions"`
	ValueUUSD        int64      `json:"value_uusd"` // cash + every position at its quote
	Orders           []SimOrder `json:"orders"`
	CreatedAt        time.Time  `json:"created_at"`
}

// SimOrderInput is what an agent asks for.
type SimOrderInput struct {
	Instrument     string `json:"instrument"`
	Side           string `json:"side"`
	Type           string `json:"type"`
	QuantityMicros int64  `json:"quantity_micros"`
	LimitPriceUSD  string `json:"limit_price_usd,omitempty"`
	Mode           string `json:"mode,omitempty"` // "" or "simulated"; anything else is live, and refused
}

// SimQuotes reads the simulator's quotes: every ECB currency and the euro, in USD, at the latest day that has a
// USD rate.
func (s *DualTokenStore) SimQuotes(ctx context.Context) (Quotes, error) {
	return simQuotes(ctx, s.pool)
}

func simQuotes(ctx context.Context, q pgxDB) (Quotes, error) {
	out := Quotes{Simulated: true, MarketData: MarketDataSource, Quotes: []Quote{}, byInstrument: map[string]Quote{}}
	rows, err := q.Query(ctx, `SELECT r.currency, r.per_eur::text, u.per_eur::text, r.rate_date::text FROM ecb_reference_rates u
		JOIN ecb_reference_rates r ON r.rate_date = u.rate_date AND r.currency <> 'USD'
		WHERE u.currency = 'USD' AND u.rate_date = (SELECT max(rate_date) FROM ecb_reference_rates WHERE currency = 'USD')
		ORDER BY r.currency`)
	if err != nil {
		return out, fmt.Errorf("economy: quotes: %w", err)
	}
	defer rows.Close()
	add := func(instrument, perEUR, usdPerEUR, date string) {
		p, ok1 := new(big.Rat).SetString(usdPerEUR)
		d, ok2 := new(big.Rat).SetString(perEUR)
		if !ok1 || !ok2 || d.Sign() <= 0 {
			return
		}
		p.Quo(p, d)
		qt := Quote{Instrument: instrument, PriceUSD: p.FloatString(8), RateDate: date, price: p}
		out.Quotes = append(out.Quotes, qt)
		out.byInstrument[instrument] = qt
		out.RateDate = date
	}
	for rows.Next() {
		var ccy, perEUR, usd, date string
		if err := rows.Scan(&ccy, &perEUR, &usd, &date); err != nil {
			return out, err
		}
		if len(out.Quotes) == 0 {
			add("EUR", "1", usd, date)
		}
		add(ccy, perEUR, usd, date)
	}
	sort.Slice(out.Quotes, func(i, j int) bool { return out.Quotes[i].Instrument < out.Quotes[j].Instrument })
	return out, rows.Err()
}

// cashFor is what quantity µunits at price move in µUSD: a purchase rounds its cost up, a sale its proceeds
// down.
func cashFor(side string, quantityMicros int64, price *big.Rat) int64 {
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(quantityMicros), price)
	q, r := new(big.Int).QuoRem(v.Num(), v.Denom(), new(big.Int))
	if side == "buy" {
		if r.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
		return -q.Int64()
	}
	return q.Int64()
}

// crosses says whether a limit order fills at price.
func crosses(side string, limit, price *big.Rat) bool {
	if side == "buy" {
		return price.Cmp(limit) <= 0
	}
	return price.Cmp(limit) >= 0
}

// OpenPortfolio opens a simulated portfolio for workspaceID's agent with cashUUSD of simulated US dollars.
func (s *DualTokenStore) OpenPortfolio(ctx context.Context, workspaceID, agentID, name string, cashUUSD int64) (Portfolio, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || len(name) > 64:
		return Portfolio{}, fmt.Errorf("%w: a portfolio needs a name of up to 64 characters", ErrSimOrder)
	case cashUUSD <= 0 || cashUUSD > maxSimCashUUSD:
		return Portfolio{}, fmt.Errorf("%w: simulated cash must be more than 0 and at most 1 000 000 000 USD", ErrSimOrder)
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO agent_portfolios (id, workspace_id, agent_id, name, starting_cash_uusd)
		SELECT $1, a.workspace_id, a.id, $4, $5 FROM agent_accounts a WHERE a.id = $3 AND a.workspace_id = $2 RETURNING id`,
		"pf_"+uuid.NewString(), workspaceID, agentID, name, cashUUSD).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Portfolio{}, ErrAgentNotFound
	}
	if err != nil && strings.Contains(err.Error(), "agent_portfolios_agent_id_name_key") {
		return Portfolio{}, fmt.Errorf("%w: the agent already has a portfolio called %q", ErrSimOrder, name)
	}
	if err != nil {
		return Portfolio{}, fmt.Errorf("economy: open portfolio: %w", err)
	}
	return s.GetPortfolio(ctx, workspaceID, agentID, id)
}

// holdings is a portfolio's simulated cash and positions, from its filled orders.
func holdings(ctx context.Context, q pgxDB, portfolioID string) (cash int64, units map[string]int64, err error) {
	if err = q.QueryRow(ctx, `SELECT p.starting_cash_uusd + COALESCE((SELECT sum(cash_uusd) FROM agent_sim_orders
		WHERE portfolio_id = p.id AND status = 'filled'), 0)::bigint FROM agent_portfolios p WHERE p.id = $1`, portfolioID).Scan(&cash); err != nil {
		return 0, nil, err
	}
	rows, err := q.Query(ctx, `SELECT instrument, sum(CASE side WHEN 'buy' THEN quantity_micros ELSE -quantity_micros END)::bigint
		FROM agent_sim_orders WHERE portfolio_id = $1 AND status = 'filled' GROUP BY instrument`, portfolioID)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	units = map[string]int64{}
	for rows.Next() {
		var ins string
		var n int64
		if err := rows.Scan(&ins, &n); err != nil {
			return 0, nil, err
		}
		if n != 0 {
			units[ins] = n
		}
	}
	return cash, units, rows.Err()
}

// covered says why the portfolio cannot fill side quantity of instrument moving cash µUSD, or "" when it can.
func covered(side, instrument string, quantity, cash int64, have int64, units map[string]int64) string {
	if side == "buy" && have+cash < 0 {
		return fmt.Sprintf("it costs %d µUSD and the portfolio holds %d µUSD of simulated cash", -cash, have)
	}
	if side == "sell" && units[instrument] < quantity {
		return fmt.Sprintf("it sells %d µ%s and the portfolio holds %d", quantity, instrument, units[instrument])
	}
	return ""
}

// PlaceSimOrder places an order in the agent's simulated portfolio. A market order, or a limit order its quote
// already crosses, fills at once at the quote; any other limit order stays open until it does.
func (s *DualTokenStore) PlaceSimOrder(ctx context.Context, workspaceID, agentID, portfolioID string, in SimOrderInput) (SimOrder, error) {
	if m := strings.ToLower(strings.TrimSpace(in.Mode)); m != "" && m != "simulated" {
		return SimOrder{}, ErrLiveTrading
	}
	in.Instrument = strings.ToUpper(strings.TrimSpace(in.Instrument))
	var limit *big.Rat
	switch {
	case in.Side != "buy" && in.Side != "sell":
		return SimOrder{}, fmt.Errorf("%w: side must be buy or sell", ErrSimOrder)
	case in.Type != "market" && in.Type != "limit":
		return SimOrder{}, fmt.Errorf("%w: type must be market or limit", ErrSimOrder)
	case in.QuantityMicros <= 0:
		return SimOrder{}, fmt.Errorf("%w: quantity_micros must be positive", ErrSimOrder)
	case in.Type == "limit":
		var ok bool
		if limit, ok = new(big.Rat).SetString(strings.TrimSpace(in.LimitPriceUSD)); !ok || limit.Sign() <= 0 {
			return SimOrder{}, fmt.Errorf("%w: a limit order needs limit_price_usd, a positive decimal", ErrSimOrder)
		}
	case in.LimitPriceUSD != "":
		return SimOrder{}, fmt.Errorf("%w: a market order has no limit_price_usd", ErrSimOrder)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SimOrder{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM agent_portfolios WHERE id = $1 AND workspace_id = $2 AND agent_id = $3 FOR UPDATE`,
		portfolioID, workspaceID, agentID).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
		return SimOrder{}, ErrPortfolioNotFound
	} else if err != nil {
		return SimOrder{}, err
	}
	quotes, err := simQuotes(ctx, tx)
	if err != nil {
		return SimOrder{}, err
	}
	quote, ok := quotes.byInstrument[in.Instrument]
	if !ok {
		return SimOrder{}, fmt.Errorf("%w: %q (the simulator trades %s)", ErrNoQuote, in.Instrument, instrumentList(quotes))
	}
	have, units, err := holdings(ctx, tx, portfolioID)
	if err != nil {
		return SimOrder{}, err
	}
	o := SimOrder{ID: "ord_" + uuid.NewString(), PortfolioID: portfolioID, Instrument: in.Instrument, Side: in.Side, Type: in.Type,
		QuantityMicros: in.QuantityMicros, Status: "open"}
	var limitText any
	if limit != nil {
		limitText = limit.FloatString(8)
	}
	fill := in.Type == "market" || crosses(in.Side, limit, quote.price)
	// Open, a limit order must be coverable at its limit; filling, at the quote.
	at := quote.price
	if !fill {
		at = limit
	}
	if why := covered(in.Side, in.Instrument, in.QuantityMicros, cashFor(in.Side, in.QuantityMicros, at), have, units); why != "" {
		return SimOrder{}, fmt.Errorf("%w: %s", ErrSimHoldings, why)
	}
	var price, date any
	if fill {
		o.Status, o.CashUUSD, price, date = "filled", cashFor(in.Side, in.QuantityMicros, quote.price), quote.price.FloatString(10), quote.RateDate
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_sim_orders (id, portfolio_id, instrument, side, type, quantity_micros, limit_price,
		status, fill_price, fill_rate_date, cash_uusd, decided_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9::numeric, $10::date, $11, CASE WHEN $8 = 'filled' THEN now() END)`,
		o.ID, o.PortfolioID, o.Instrument, o.Side, o.Type, o.QuantityMicros, limitText, o.Status, price, date, o.CashUUSD); err != nil {
		return SimOrder{}, fmt.Errorf("economy: place order: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SimOrder{}, err
	}
	return s.simOrder(ctx, o.ID)
}

func instrumentList(q Quotes) string {
	if len(q.Quotes) == 0 {
		return "nothing yet: no ECB reference rates have been fetched"
	}
	names := make([]string, 0, len(q.Quotes))
	for _, x := range q.Quotes {
		names = append(names, x.Instrument)
	}
	return strings.Join(names, ", ")
}

// CancelSimOrder cancels an open order in the agent's portfolio.
func (s *DualTokenStore) CancelSimOrder(ctx context.Context, workspaceID, agentID, portfolioID, orderID string) (SimOrder, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE agent_sim_orders o SET status = 'cancelled', decided_at = now() FROM agent_portfolios p
		WHERE o.id = $4 AND o.portfolio_id = p.id AND p.id = $3 AND p.agent_id = $2 AND p.workspace_id = $1 AND o.status = 'open'`,
		workspaceID, agentID, portfolioID, orderID)
	if err != nil {
		return SimOrder{}, err
	}
	if tag.RowsAffected() == 0 {
		return SimOrder{}, ErrSimOrderNotFound
	}
	return s.simOrder(ctx, orderID)
}

// SimRunResult is what one run of the open orders did.
type SimRunResult struct {
	Filled, Rejected int
}

// RunSimulatedOrders fills every open limit order its instrument's quote now crosses, each portfolio in its own
// transaction. One the portfolio can no longer cover is rejected, saying why.
func (s *DualTokenStore) RunSimulatedOrders(ctx context.Context) (SimRunResult, error) {
	var res SimRunResult
	quotes, err := s.SimQuotes(ctx)
	if err != nil || len(quotes.Quotes) == 0 {
		return res, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, instrument, side, limit_price::text FROM agent_sim_orders WHERE status = 'open'
		ORDER BY created_at, id LIMIT $1`, maxTicksPerRun)
	if err != nil {
		return res, fmt.Errorf("economy: open orders: %w", err)
	}
	var due []string
	for rows.Next() {
		var id, ins, side, lim string
		if err := rows.Scan(&id, &ins, &side, &lim); err != nil {
			rows.Close()
			return res, err
		}
		limit, _ := new(big.Rat).SetString(lim)
		if q, ok := quotes.byInstrument[ins]; ok && limit != nil && crosses(side, limit, q.price) {
			due = append(due, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, id := range due {
		filled, err := s.fillSimOrder(ctx, id)
		if err != nil {
			return res, err
		}
		if filled {
			res.Filled++
		} else {
			res.Rejected++
		}
	}
	return res, nil
}

// fillSimOrder fills the open order id at its quote, holding its portfolio, or rejects it when the portfolio
// cannot cover it. An order that is no longer open, or no longer crosses, is left.
func (s *DualTokenStore) fillSimOrder(ctx context.Context, id string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pf, ins, side, lim string
	var qty int64
	err = tx.QueryRow(ctx, `SELECT p.id, o.instrument, o.side, o.limit_price::text, o.quantity_micros FROM agent_sim_orders o
		JOIN agent_portfolios p ON p.id = o.portfolio_id WHERE o.id = $1 AND o.status = 'open' FOR UPDATE OF p, o`, id).Scan(&pf, &ins, &side, &lim, &qty)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	quotes, err := simQuotes(ctx, tx)
	if err != nil {
		return false, err
	}
	q, ok := quotes.byInstrument[ins]
	limit, _ := new(big.Rat).SetString(lim)
	if !ok || limit == nil || !crosses(side, limit, q.price) {
		return false, nil
	}
	have, units, err := holdings(ctx, tx, pf)
	if err != nil {
		return false, err
	}
	cash := cashFor(side, qty, q.price)
	if why := covered(side, ins, qty, cash, have, units); why != "" {
		if _, err := tx.Exec(ctx, `UPDATE agent_sim_orders SET status = 'rejected', reason = $2, decided_at = now() WHERE id = $1`, id, why); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_sim_orders SET status = 'filled', fill_price = $2::numeric, fill_rate_date = $3::date,
		cash_uusd = $4, decided_at = now() WHERE id = $1`, id, q.price.FloatString(10), q.RateDate, cash); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

const simOrderColumns = `id, portfolio_id, instrument, side, type, quantity_micros, COALESCE(trim_scale(limit_price)::text, ''), status,
	COALESCE(trim_scale(fill_price)::text, ''), COALESCE(fill_rate_date::text, ''), cash_uusd, reason, created_at, decided_at`

func scanSimOrder(row pgx.Row) (SimOrder, error) {
	o := SimOrder{Simulated: true}
	err := row.Scan(&o.ID, &o.PortfolioID, &o.Instrument, &o.Side, &o.Type, &o.QuantityMicros, &o.LimitPriceUSD, &o.Status,
		&o.FillPriceUSD, &o.FillRateDate, &o.CashUUSD, &o.Reason, &o.CreatedAt, &o.DecidedAt)
	return o, err
}

func (s *DualTokenStore) simOrder(ctx context.Context, id string) (SimOrder, error) {
	return scanSimOrder(s.pool.QueryRow(ctx, `SELECT `+simOrderColumns+` FROM agent_sim_orders WHERE id = $1`, id))
}

// GetPortfolio reads one of the agent's portfolios, valued at the latest quotes, with its orders.
func (s *DualTokenStore) GetPortfolio(ctx context.Context, workspaceID, agentID, portfolioID string) (Portfolio, error) {
	list, err := s.portfolios(ctx, workspaceID, agentID, portfolioID)
	if err != nil {
		return Portfolio{}, err
	}
	if len(list) == 0 {
		return Portfolio{}, ErrPortfolioNotFound
	}
	return list[0], nil
}

// ListPortfolios reads the agent's portfolios, valued at the latest quotes, with their orders.
func (s *DualTokenStore) ListPortfolios(ctx context.Context, workspaceID, agentID string) ([]Portfolio, error) {
	return s.portfolios(ctx, workspaceID, agentID, "")
}

func (s *DualTokenStore) portfolios(ctx context.Context, workspaceID, agentID, portfolioID string) ([]Portfolio, error) {
	quotes, err := s.SimQuotes(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, agent_id, name, starting_cash_uusd, created_at FROM agent_portfolios
		WHERE workspace_id = $1 AND agent_id = $2 AND ($3 = '' OR id = $3) ORDER BY created_at, id`, workspaceID, agentID, portfolioID)
	if err != nil {
		return nil, fmt.Errorf("economy: portfolios: %w", err)
	}
	out := []Portfolio{}
	for rows.Next() {
		p := Portfolio{Simulated: true, Notice: SimulatedNotice, MarketData: MarketDataSource, RateDate: quotes.RateDate,
			Positions: []Position{}, Orders: []SimOrder{}}
		if err := rows.Scan(&p.ID, &p.AgentID, &p.Name, &p.StartingCashUUSD, &p.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		p := &out[i]
		cash, units, err := holdings(ctx, s.pool, p.ID)
		if err != nil {
			return nil, err
		}
		p.CashUUSD, p.ValueUUSD = cash, cash
		for ins, n := range units {
			pos := Position{Instrument: ins, QuantityMicros: n}
			if q, ok := quotes.byInstrument[ins]; ok {
				pos.PriceUSD, pos.ValueUUSD = q.PriceUSD, cashFor("sell", n, q.price)
			}
			p.ValueUUSD += pos.ValueUUSD
			p.Positions = append(p.Positions, pos)
		}
		sort.Slice(p.Positions, func(a, b int) bool { return p.Positions[a].Instrument < p.Positions[b].Instrument })
		orows, err := s.pool.Query(ctx, `SELECT `+simOrderColumns+` FROM agent_sim_orders WHERE portfolio_id = $1 ORDER BY created_at, id`, p.ID)
		if err != nil {
			return nil, fmt.Errorf("economy: orders: %w", err)
		}
		if p.Orders, err = pgx.CollectRows(orows, func(row pgx.CollectableRow) (SimOrder, error) { return scanSimOrder(row) }); err != nil {
			return nil, err
		}
	}
	return out, nil
}
