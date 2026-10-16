package povi

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ChallengeStore persists challenge audit records (the real challengeStore
// impl). Reuses the package pgxDB seam.
type ChallengeStore struct {
	pool pgxDB
}

// NewChallengeStore guards the typed-nil interface trap; a nil pool no-ops.
func NewChallengeStore(pool *pgxpool.Pool) *ChallengeStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &ChallengeStore{pool: db}
}

func newChallengeStore(pool pgxDB) *ChallengeStore { return &ChallengeStore{pool: pool} }

const insertChallengeSQL = `INSERT INTO povi_challenges
    (id, request_id, node_id, workspace_id, positions, result, slashed_amount, reason, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (request_id) DO NOTHING`

const updateChallengeSQL = `UPDATE povi_challenges
SET result=$2, slashed_amount=$3, reason=$4
WHERE id=$1 AND workspace_id=$5`

const existsChallengeSQL = `SELECT EXISTS (SELECT 1 FROM povi_challenges WHERE request_id = $1)`

const getChallengeSQL = `SELECT id, request_id, node_id, workspace_id, positions, result, slashed_amount, reason, created_at
FROM povi_challenges WHERE id = $1`

const listChallengesSQL = `SELECT id, request_id, node_id, workspace_id, positions, result, slashed_amount, reason, created_at
FROM povi_challenges WHERE ($1 = '' OR node_id = $1) AND ($2 = '' OR workspace_id = $2) ORDER BY created_at DESC LIMIT 100`

func joinPositions(ps []int) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

func parsePositions(s string) []int {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Record atomically claims the receipt by inserting a pending challenge row.
// Returns ErrAlreadyChallenged when the UNIQUE constraint on request_id fires
// (RowsAffected == 0 via ON CONFLICT DO NOTHING), meaning another instance
// already claimed this receipt — the caller must not proceed to Slash.
func (s *ChallengeStore) Record(ctx context.Context, c Challenge) error {
	if s.pool == nil {
		return nil
	}
	tag, err := s.pool.Exec(ctx, insertChallengeSQL,
		c.ID, c.RequestID, c.NodeID, c.WorkspaceID, joinPositions(c.Positions),
		string(c.Result), c.SlashedAmount, c.Reason, c.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("povi: insert challenge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyChallenged
	}
	return nil
}

// UpdateResult writes the final outcome (pass/fail/timeout) and slash amount to the
// previously-claimed pending challenge row, CONFINED to the challenge's owning workspace.
// Keying the write on (id, workspace_id) — not id alone — makes it a money-path identity guard:
// a wrong or foreign challenge id can never settle or slash another workspace's challenge (SEC-11:
// a scoping key must hold every identity it protects). slashedAmount (µLENS, SEC-2) is written
// UNCHANGED — this hardens only which row the write may touch, never the amount.
func (s *ChallengeStore) UpdateResult(ctx context.Context, workspaceID, id string, result ChallengeResult, slashedAmount int64, reason string) error {
	if s.pool == nil {
		return nil
	}
	if _, err := s.pool.Exec(ctx, updateChallengeSQL, id, string(result), slashedAmount, reason, workspaceID); err != nil {
		return fmt.Errorf("povi: update challenge result: %w", err)
	}
	return nil
}

// AlreadyChallenged reports whether a receipt has already been challenged (the
// double-slash guard).
func (s *ChallengeStore) AlreadyChallenged(ctx context.Context, requestID string) (bool, error) {
	if s.pool == nil {
		return false, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, existsChallengeSQL, requestID).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func scanChallenge(row interface{ Scan(...any) error }) (Challenge, error) {
	var c Challenge
	var positions, result string
	err := row.Scan(&c.ID, &c.RequestID, &c.NodeID, &c.WorkspaceID,
		&positions, &result, &c.SlashedAmount, &c.Reason, &c.CreatedAt)
	c.Positions = parsePositions(positions)
	c.Result = ChallengeResult(result)
	return c, err
}

// Get fetches one challenge by id.
func (s *ChallengeStore) Get(ctx context.Context, id string) (*Challenge, error) {
	if s.pool == nil {
		return nil, nil
	}
	c, err := scanChallenge(s.pool.QueryRow(ctx, getChallengeSQL, id))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// List returns recent challenges, optionally filtered by node and by workspace (empty = all).
func (s *ChallengeStore) List(ctx context.Context, nodeID, workspaceID string) ([]Challenge, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, listChallengesSQL, nodeID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Challenge
	for rows.Next() {
		c, err := scanChallenge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
