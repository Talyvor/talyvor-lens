package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// audit_chain.go — B40.5: AN APPEND-ONLY, HASH-CHAINED RECORD OF EVERY STEP, PER SUBJECT.
//
// Each subject (a payment request's id) has its own chain: entry n's hash is the SHA-256 of entry n-1's hash, n, the
// entry's kind and its JSON body, the first entry's previous hash being "". The table refuses UPDATE, DELETE and
// TRUNCATE (migration 0259), so an entry is never rewritten, and a chain exported per subject is checked on its own by
// VerifyAuditChain. B28.69's wallet audit log is not built yet; this is the chain it extends.

// ErrAuditChainBroken: an exported chain whose links do not hold.
var ErrAuditChainBroken = errors.New("economy: the record's hash chain is broken")

// AuditEntry is one link of a subject's chain.
type AuditEntry struct {
	Seq      int64  `json:"seq"`
	Kind     string `json:"kind"`
	Body     string `json:"body"` // the JSON exactly as hashed
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

func auditHash(prev string, seq int64, kind, body string) string {
	return sha256Hex(prev + "\n" + strconv.FormatInt(seq, 10) + "\n" + kind + "\n" + body)
}

// appendAudit adds one entry of kind to subject's chain inside tx, with body and the time it was written.
func appendAudit(ctx context.Context, tx pgx.Tx, workspaceID, subject, kind string, body map[string]any) error {
	body["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("economy: audit entry: %w", err)
	}
	// One writer per chain at a time, so two entries never take the same place.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('audit_chain:' || $1))`, subject); err != nil {
		return fmt.Errorf("economy: audit lock: %w", err)
	}
	var seq int64
	var prev string
	err = tx.QueryRow(ctx, `SELECT seq, hash FROM audit_chain WHERE workspace_id = $1 AND subject = $2 ORDER BY seq DESC LIMIT 1`,
		workspaceID, subject).Scan(&seq, &prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("economy: audit chain: %w", err)
	}
	seq++
	if _, err := tx.Exec(ctx, `INSERT INTO audit_chain (workspace_id, subject, seq, kind, body, prev_hash, hash) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		workspaceID, subject, seq, kind, string(b), prev, auditHash(prev, seq, kind, string(b))); err != nil {
		return fmt.Errorf("economy: audit entry: %w", err)
	}
	return nil
}

// AuditRecord is subject's chain in workspaceID, first entry first.
func (s *DualTokenStore) AuditRecord(ctx context.Context, workspaceID, subject string) ([]AuditEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT seq, kind, body, prev_hash, hash FROM audit_chain WHERE workspace_id = $1 AND subject = $2 ORDER BY seq`,
		workspaceID, subject)
	if err != nil {
		return nil, fmt.Errorf("economy: audit chain: %w", err)
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.Seq, &e.Kind, &e.Body, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifyAuditChain checks an exported chain: numbered 1, 2, 3 …, each entry linked to the one before and hashing to
// its own hash.
func VerifyAuditChain(entries []AuditEntry) error {
	prev := ""
	for i, e := range entries {
		switch {
		case e.Seq != int64(i+1):
			return fmt.Errorf("%w: entry %d is numbered %d", ErrAuditChainBroken, i+1, e.Seq)
		case e.PrevHash != prev:
			return fmt.Errorf("%w: entry %d does not follow entry %d", ErrAuditChainBroken, e.Seq, e.Seq-1)
		case auditHash(prev, e.Seq, e.Kind, e.Body) != e.Hash:
			return fmt.Errorf("%w: entry %d is not what was hashed", ErrAuditChainBroken, e.Seq)
		}
		prev = e.Hash
	}
	return nil
}
