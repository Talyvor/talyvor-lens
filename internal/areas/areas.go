// Package areas is B59.1's list of areas per workspace. Everything Talyvor has that is outside the launch pack is
// an area, and every area is off for every workspace until the operator switches it on for one. A route of an area
// the workspace does not have answers 404, as if it were not there (cmd/lens's areaGate).
//
// Untouched, so never listed here: agent accounts, wallets, cards, payments, payees, approvals, mandates, payout
// batches, statements, keys, sign-in, the connector, and the model routes agents call. Only the old gateway's
// management routes belong to the gateway area; an agent's calls still go through Lens as they do today.
package areas

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/operatoraudit"
)

// Area is one part of Talyvor a workspace has only once the operator switches it on.
type Area string

const (
	Marketplace       Area = "marketplace"
	Rooms             Area = "rooms"
	Chat              Area = "chat"
	Track             Area = "track"
	Docs              Area = "docs"
	Code              Area = "code"
	Edge              Area = "edge"
	Desktop           Area = "desktop"
	Scan              Area = "scan"
	Counter           Area = "counter"
	Trading           Area = "trading"
	Credit            Area = "credit"
	Interest          Area = "interest"
	PredictionMarkets Area = "prediction_markets"
	Gateway           Area = "gateway"
)

// All is every area, in the order the spec lists them. Migration 0372's CHECK lists the same names.
var All = []Area{Marketplace, Rooms, Chat, Track, Docs, Code, Edge, Desktop, Scan, Counter, Trading, Credit,
	Interest, PredictionMarkets, Gateway}

// ErrUnknown is a name that is not an area.
var ErrUnknown = errors.New("areas: unknown area")

// Parse returns the area s names.
func Parse(s string) (Area, error) {
	for _, a := range All {
		if string(a) == s {
			return a, nil
		}
	}
	return "", fmt.Errorf("%w %q", ErrUnknown, s)
}

// routes are the route groups each area owns, as chi patterns with every parameter written {}. A pattern belongs
// to an area when it is one of these or continues one with "/". Areas with no routes in Lens (Track, Docs, Code,
// Edge, desktop, interest, prediction markets) have none here; the list still carries them for the web
// app and the phone.
var routes = []struct {
	prefix string
	area   Area
}{
	{"/v1/marketplace", Marketplace},
	{"/v1/workspaces/{}/marketplace", Marketplace},

	{"/v1/rooms", Rooms},
	{"/v1/room-invites", Rooms},
	{"/v1/workspaces/{}/rooms", Rooms},

	{"/v1/workspaces/{}/chat-shares", Chat},

	{"/v1/negotiations", Counter},

	{"/v1/workspaces/{}/scan-watches", Scan},
	{"/v1/workspaces/{}/agents/{}/scan-watches", Scan},

	{"/v1/markets", Trading},
	{"/v1/workspaces/{}/agents/{}/portfolios", Trading},

	{"/v1/workspaces/{}/loans", Credit},
	{"/v1/workspaces/{}/agents/{}/loans", Credit},
	{"/v1/money/credit-score", Credit},

	// The old gateway's management routes: stored answers, pooling, routing, the catalog, Tare, guardrails.
	{"/v1/workspaces/{}/stored-answers", Gateway},
	{"/v1/workspaces/{}/distill", Gateway},
	{"/v1/workspaces/{}/cache-poolable", Gateway},
	{"/v1/workspaces/{}/distill-poolable", Gateway},
	{"/v1/api/cache", Gateway},
	{"/v1/workspaces/{}/routing", Gateway},
	{"/v1/workspaces/{}/cost-optimize-routing", Gateway},
	{"/v1/routing", Gateway},
	{"/v1/api/routing", Gateway},
	{"/v1/catalog", Gateway},
	{"/v1/api/catalog", Gateway},
	{"/v1/workspaces/{}/tare", Gateway},
	{"/v1/workspaces/{}/tare-model", Gateway},
	{"/v1/workspaces/{}/tare-training", Gateway},
	{"/v1/guardrails", Gateway},
	{"/v1/api/guardrails", Gateway},
	{"/v1/workspaces/{}/guardrails", Gateway},
}

// Of returns the area a chi route pattern belongs to, and false for a route of no area.
func Of(pattern string) (Area, bool) {
	p := blankParams(pattern)
	for _, r := range routes {
		if p == r.prefix || strings.HasPrefix(p, r.prefix+"/") {
			return r.area, true
		}
	}
	return "", false
}

// blankParams writes every {name} or {name:regexp} in a pattern as {}.
func blankParams(pattern string) string {
	var b strings.Builder
	depth := 0
	for _, c := range pattern {
		switch {
		case c == '{':
			if depth == 0 {
				b.WriteByte('{')
			}
			depth++
		case c == '}' && depth > 0:
			depth--
			if depth == 0 {
				b.WriteByte('}')
			}
		case depth == 0:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// AuditAction is the operator_audit action of a switch; the detail says on or off.
const AuditAction = "workspace.area"

// Store reads and writes workspace_areas.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// List returns the areas the workspace has, in All's order; none for a workspace the operator never switched on.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Area, error) {
	rows, err := s.pool.Query(ctx, `SELECT area FROM workspace_areas WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("areas: list: %w", err)
	}
	on, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("areas: list: %w", err)
	}
	rank := make(map[Area]int, len(All))
	for i, a := range All {
		rank[a] = i
	}
	out := make([]Area, 0, len(on))
	for _, a := range on {
		out = append(out, Area(a))
	}
	sort.Slice(out, func(i, j int) bool { return rank[out[i]] < rank[out[j]] })
	return out, nil
}

// Has reports whether the workspace has the area.
func (s *Store) Has(ctx context.Context, workspaceID string, a Area) (bool, error) {
	var has bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_areas WHERE workspace_id = $1 AND area = $2)`,
		workspaceID, string(a)).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("areas: has: %w", err)
	}
	return has, nil
}

// Switch turns the area on or off for one workspace, by the operator named actor, and records it in the operator
// audit trail in the same transaction. Switching an area to the state it is already in still records the switch.
func (s *Store) Switch(ctx context.Context, workspaceID string, a Area, on bool, actor string) error {
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("%w: a workspace is required", operatoraudit.ErrInvalid)
	}
	if _, err := Parse(string(a)); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("areas: switch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	detail := "off"
	if on {
		detail = "on"
		_, err = tx.Exec(ctx, `INSERT INTO workspace_areas (workspace_id, area, switched_on_by) VALUES ($1, $2, $3)
			ON CONFLICT (workspace_id, area) DO NOTHING`, workspaceID, string(a), strings.TrimSpace(actor))
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM workspace_areas WHERE workspace_id = $1 AND area = $2`, workspaceID, string(a))
	}
	if err != nil {
		return fmt.Errorf("areas: switch: %w", err)
	}
	if _, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: actor, Action: AuditAction,
		Target: workspaceID + "/" + string(a), Detail: detail}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("areas: switch: %w", err)
	}
	return nil
}
