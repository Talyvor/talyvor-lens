package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/workspace"
)

// market_tools.go — B32.23: AGENTS SHOP THE MARKETPLACE THEMSELVES.
//
// The market_* tools let an agent's OWN key search the marketplace, read a listing's needs and offers, take a
// licence (buy, rent or subscribe), use a listing with the most it will pay, and see and cancel its licences. As with
// the wallet tools, the agent is the key's, never an argument. Each tool calls the store exactly as the marketplace's
// routes do, so the agent's rules judge it as they would over HTTP — its per-request cap, its max_commitment and the
// licences it may take (B32.22); a refusal is a tool result marked isError that names the rule. Every call is logged
// in agent_tool_calls, refused ones too.

// MarketDeps gives the market tools what a use and a licence need beside the store: the workspace's marketplace bill,
// the agent's judge and, for a use, a runner that calls the models as the agent's own key.
type MarketDeps interface {
	UseDeps(ctx context.Context, workspaceID string) market.UseDeps
	LicenceDeps(ctx context.Context, workspaceID string) market.LicenceDeps
}

// SetMarket enables the market tools; they need the agent tools too (SetAgentBank).
func (s *Server) SetMarket(store *market.Store, deps MarketDeps) {
	s.market, s.marketDeps = store, deps
}

type callerRequestKey struct{}

// CallerRequest is the HTTP request a tool call came in on: a market use runs its model calls with its credential.
func CallerRequest(ctx context.Context) *http.Request {
	r, _ := ctx.Value(callerRequestKey{}).(*http.Request)
	return r
}

// marketRefusals are the marketplace's answers that say no to what the agent asked, rather than that Lens failed.
var marketRefusals = []error{
	market.ErrNotFound, market.ErrTakenDown, market.ErrInvalid, market.ErrNotSoldPerUse, market.ErrOverMaxPrice,
	market.ErrNoModel, market.ErrNotRunnable, market.ErrNoBill, market.ErrKeyReused,
	economy.ErrAgentRule, economy.ErrApprovalRequired, economy.ErrAgentFunds, economy.ErrCapabilityNotCleared,
	economy.ErrOwnerUnverified, economy.ErrAgentOwnerless, workspace.ErrMoneyWall, market.ErrNotSoldHere,
}

func isMarketTool(name string) bool { return strings.HasPrefix(name, "market_") }

func marketToolDefinitions() []map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	tool := func(name, desc string, props map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"name": name, "description": desc,
			"inputSchema": map[string]any{"type": "object", "properties": props, "required": required}}
	}
	listing := str("the listing's id")
	version := num("the version to run or pin; 0 or absent: the latest (or the one your licence pins)")
	return []map[string]any{
		tool("market_search", "Search the Talyvor marketplace: prompts, skills, agents and pipelines other owners sell, with their offers.",
			map[string]any{"text": str("words in its title or description"), "kind": str("prompt, skill, agent, evaluation or pipeline"),
				"capability": str("what it should do: one of GET /v1/marketplace/capabilities, e.g. summarize or extract"), "licence": str("sold under this licence: personal, commercial or enterprise"),
				"max_price_usd_micros": num("one use costs at most this, in µUSD (1 USD = 1,000,000)")}),
		tool("market_listing", "One listing: what each version needs (input, variables, model), every offer with its licence terms, and its trust "+
			"summary: whether the publisher is verified, reviews from paying buyers, eval score, IP claims, the originals it credits, its remixes, "+
			"and its success rate, median latency and cost per use over the last 30 days.",
			map[string]any{"listing_id": listing}, "listing_id"),
		tool("market_license", "Buy, rent or subscribe to a listing through one of its offers, within your rules. Its uses are then covered. "+
			"Send the same idempotency_key again to retry without buying twice.",
			map[string]any{"listing_id": listing, "offer_id": str("a buy, rent or subscribe offer from market_listing"), "version": version,
				"idempotency_key": str("1 to 128 characters, unique to this purchase"),
				"auto_renew":      map[string]any{"type": "boolean", "description": "a rent renews only when true; a subscription renews unless false"}},
			"listing_id", "offer_id", "idempotency_key"),
		tool("market_use", "Use a listing: it runs on your key, and a licence covers it or its per-use price goes on your owner's marketplace bill. "+
			"Set max_price_usd_micros and a use that costs more runs nothing and is charged nothing.",
			map[string]any{"listing_id": listing, "version": version, "input": str("your message, for an agent or a skill"),
				"variables":            map[string]any{"type": "object", "description": "a prompt's {{variables}}", "additionalProperties": map[string]any{"type": "string"}},
				"model":                str("the model to run it on, when the listing names none"),
				"max_price_usd_micros": num("the most this use may cost, in µUSD; 0 allows only a use that costs nothing")},
			"listing_id"),
		tool("market_licences", "The licences that cover your uses: yours, and your workspace's commercial and enterprise ones.", map[string]any{}),
		tool("market_cancel", "Stop a licence you took from renewing: it runs to its end and nothing more is charged.",
			map[string]any{"licence_id": str("a licence from market_licences that you took")}, "licence_id"),
	}
}

type marketArgs struct {
	Text              string            `json:"text"`
	Kind              string            `json:"kind"`
	Capability        string            `json:"capability"`
	Licence           string            `json:"licence"`
	MaxPriceUSDMicros *int64            `json:"max_price_usd_micros"`
	ListingID         string            `json:"listing_id"`
	OfferID           string            `json:"offer_id"`
	Version           int               `json:"version"`
	IdempotencyKey    string            `json:"idempotency_key"`
	AutoRenew         *bool             `json:"auto_renew"`
	Input             string            `json:"input"`
	Variables         map[string]string `json:"variables"`
	Model             string            `json:"model"`
	LicenceID         string            `json:"licence_id"`
}

func (s *Server) runMarketTool(ctx context.Context, name, ws, agent string, raw json.RawMessage) (any, error) {
	if s.market == nil || s.marketDeps == nil {
		return nil, errors.New("the market tools are not configured")
	}
	var a marketArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &toolRefusal{"invalid arguments: " + err.Error()}
		}
	}
	res, err := s.marketTool(ctx, name, ws, agent, a)
	var need *economy.ApprovalNeededError
	if errors.As(err, &need) {
		return nil, &toolRefusal{err.Error()}
	}
	for _, e := range marketRefusals {
		if errors.Is(err, e) {
			return nil, &toolRefusal{err.Error()}
		}
	}
	return res, err
}

// listingWithTrust is market_listing's answer: the listing, and its trust summary beside it.
type listingWithTrust struct {
	market.Listing
	Trust market.Trust `json:"trust"`
}

func (s *Server) marketTool(ctx context.Context, name, ws, agent string, a marketArgs) (any, error) {
	switch name {
	case "market_search":
		found, err := s.market.Search(ctx, market.SearchQuery{Text: a.Text, Kind: a.Kind, Capability: a.Capability, Licence: a.Licence,
			MaxPriceUSDMicros: a.MaxPriceUSDMicros})
		if err == nil { // B32.51: each offer's price in the workspace's currency too
			refs := make([]*market.Listing, len(found))
			for i := range found {
				refs[i] = &found[i]
			}
			err = s.market.ShowPrices(ctx, ws, "", refs...)
		}
		return map[string]any{"listings": found}, err
	case "market_listing":
		l, err := s.market.Get(ctx, ws, a.ListingID)
		if err == nil {
			err = s.market.ShowPrices(ctx, ws, "", &l) // B32.51: each offer's price in the workspace's currency too
		}
		if err != nil {
			return nil, err
		}
		trust, err := s.market.Trust(ctx, ws, a.ListingID) // B32.49: the trust panel's read, as GET …/listings/{id}/trust gives it
		return listingWithTrust{l, trust}, err
	case "market_license":
		lic, _, err := s.market.License(ctx, s.marketDeps.LicenceDeps(ctx, ws), ws, agent, a.ListingID, a.IdempotencyKey,
			market.LicenceRequest{OfferID: a.OfferID, Version: a.Version, AutoRenew: a.AutoRenew})
		return lic, err
	case "market_use":
		return s.market.Use(ctx, s.marketDeps.UseDeps(ctx, ws), ws, agent, a.ListingID, market.UseRequest{Version: a.Version, Model: a.Model,
			Input: a.Input, Variables: a.Variables, MaxPriceUSDMicros: a.MaxPriceUSDMicros})
	case "market_licences":
		all, err := s.market.Licences(ctx, ws)
		mine := []market.Licence{}
		for _, l := range all {
			if l.Licence != market.LicencePersonal { // a personal licence never covers an agent key
				mine = append(mine, l)
			}
		}
		return map[string]any{"licences": mine}, err
	case "market_cancel":
		all, err := s.market.Licences(ctx, ws)
		if err != nil {
			return nil, err
		}
		for _, l := range all {
			if l.ID == a.LicenceID && l.AgentID == agent {
				return s.market.CancelLicence(ctx, ws, l.ID)
			}
		}
		return nil, market.ErrNotFound
	}
	return nil, &toolRefusal{"unknown market tool: " + name}
}
