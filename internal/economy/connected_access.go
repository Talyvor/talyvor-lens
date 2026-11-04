package economy

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// connected_access.go — B41.115: EVERYTHING A WORKSPACE HAS LET ACT FOR IT, ON ONE LIST.
//
// The permission mandates its owner signed for assistants and agents (B40.2), the phone device keys (B40.4) and the
// passkeys (B19.16) that sign for it, each as one row: what it may do, when it was last used, when it expires, its
// status and the route that revokes it. Connected assistants (the OAuth grants) live in the suite, which lists them
// beside these; workspace and agent keys are listed by the keys routes. Nothing here changes anything.

// AccessItem is one thing allowed to act for the workspace.
type AccessItem struct {
	Kind       string     `json:"kind"` // mandate, device or passkey
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	May        string     `json:"may"` // what it may do, in words
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Status     string     `json:"status"` // active, not_yet_valid, expired or revoked
	Revoke     string     `json:"revoke"` // the POST that revokes it; "" when none is offered yet
}

// ConnectedAccess lists workspaceID's mandates, device keys and passkeys, newest first.
func (s *DualTokenStore) ConnectedAccess(ctx context.Context, workspaceID string) ([]AccessItem, error) {
	mandates, err := s.PermissionMandates(ctx, workspaceID, "")
	if err != nil {
		return nil, err
	}
	// A mandate was last used when an item of a payment request was last judged under it.
	used := map[string]time.Time{}
	rows, err := s.pool.Query(ctx, `SELECT i.mandate_id, max(i.created_at) FROM payment_request_items i
		JOIN payment_requests r ON r.id = i.request_id
		WHERE r.workspace_id = $1 AND i.mandate_id <> '' GROUP BY i.mandate_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: connected access: %w", err)
	}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			rows.Close()
			return nil, err
		}
		used[id] = at
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	devices, err := s.ListDeviceKeys(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	passkeys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	out := []AccessItem{}
	for _, m := range mandates {
		it := AccessItem{Kind: "mandate", ID: m.ID, Name: strings.Join(m.Clients, ", "), May: mandateMay(m.PermissionMandateTerms),
			CreatedAt: m.CreatedAt, Status: m.Status, Revoke: "/v1/money/permission-mandates/" + m.ID + "/revoke"}
		if at, ok := used[m.ID]; ok {
			it.LastUsedAt = &at
		}
		if !m.ExpiresAt.IsZero() {
			exp := m.ExpiresAt
			it.ExpiresAt = &exp
		}
		out = append(out, it)
	}
	for _, d := range devices {
		it := AccessItem{Kind: "device", ID: d.ID, Name: d.Name, May: "sign approvals and mandates", CreatedAt: d.CreatedAt,
			LastUsedAt: d.LastUsedAt, Status: "active", Revoke: "/v1/money/devices/" + d.ID + "/revoke"}
		if d.RevokedAt != nil {
			it.Status = "revoked"
		}
		out = append(out, it)
	}
	for _, p := range passkeys {
		// shortcut: no passkey revoke route yet (queued separately), so Revoke stays "" until it lands.
		out = append(out, AccessItem{Kind: "passkey", ID: p.CredentialID, Name: p.Name, May: "sign approvals and mandates",
			CreatedAt: p.CreatedAt, LastUsedAt: p.LastUsedAt, Status: "active"})
	}
	slices.SortStableFunc(out, func(a, b AccessItem) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// mandateMay says what a mandate lets its clients do: "prepare and pay 2 payees in GBP, up to GBP 100.00 a payment".
func mandateMay(t PermissionMandateTerms) string {
	var what []string
	for _, n := range []struct {
		n    int
		one  string
		many string
	}{{len(t.Scope.Payees), "payee", "payees"}, {len(t.Scope.PayeeCategories), "payee category", "payee categories"},
		{len(t.Scope.Sellers), "marketplace seller", "marketplace sellers"}, {len(t.Scope.Rooms), "room", "rooms"}} {
		switch {
		case n.n == 1:
			what = append(what, "1 "+n.one)
		case n.n > 1:
			what = append(what, fmt.Sprintf("%d %s", n.n, n.many))
		}
	}
	s := "prepare and pay " + strings.Join(what, ", ")
	if len(what) == 0 {
		s = "prepare payments"
	}
	if len(t.Scope.Currencies) > 0 {
		s += " in " + strings.Join(t.Scope.Currencies, ", ")
	}
	for _, c := range t.Scope.Currencies {
		if v, ok := t.Limits.PerPayment[c]; ok {
			s += ", up to " + formatMinor(c, v) + " a payment"
		}
	}
	return s
}
