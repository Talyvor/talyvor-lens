package agentsig

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/operatoraudit"
)

// Store keeps the nonces already used and the businesses Talyvor has checked (migration 0265).
type Store struct{ Pool *pgxpool.Pool }

func (s Store) Use(ctx context.Context, keyID, nonce string, until time.Time) (bool, error) {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM agent_signature_nonces WHERE expires_at < now()`); err != nil {
		return false, err
	}
	tag, err := s.Pool.Exec(ctx, `INSERT INTO agent_signature_nonces (key_id, nonce, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (key_id, nonce) DO NOTHING`, keyID, nonce, until)
	return tag.RowsAffected() == 1, err
}

func (s Store) Business(ctx context.Context, domain string) (string, bool, error) {
	var name string
	err := s.Pool.QueryRow(ctx, `SELECT business FROM checked_agent_operators WHERE domain = $1`, domain).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}

// CheckedOperator is one signing domain Talyvor has checked, and the business it belongs to.
type CheckedOperator struct {
	Domain    string    `json:"domain"`
	Business  string    `json:"business"`
	CheckedBy string    `json:"checked_by"`
	CheckedAt time.Time `json:"checked_at"`
}

func (s Store) CheckedOperators(ctx context.Context) ([]CheckedOperator, error) {
	rows, err := s.Pool.Query(ctx, `SELECT domain, business, checked_by, checked_at FROM checked_agent_operators ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckedOperator{}
	for rows.Next() {
		var o CheckedOperator
		if err := rows.Scan(&o.Domain, &o.Business, &o.CheckedBy, &o.CheckedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetChecked records that domain belongs to business, as checked by operator, with an operator audit row in the
// same transaction.
func (s Store) SetChecked(ctx context.Context, domain, business, by string) (CheckedOperator, error) {
	o := CheckedOperator{Domain: domain, Business: business, CheckedBy: by}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO checked_agent_operators (domain, business, checked_by) VALUES ($1, $2, $3)
			ON CONFLICT (domain) DO UPDATE SET business = EXCLUDED.business, checked_by = EXCLUDED.checked_by, checked_at = now()
			RETURNING checked_at`, domain, business, by).Scan(&o.CheckedAt); err != nil {
			return err
		}
		_, err := operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: by, Action: "agent_operator.check", Target: domain, Detail: business})
		return err
	})
	return o, err
}

// RemoveChecked forgets a domain, with an operator audit row; its signatures fall back to "Signed by <domain>".
// false means it was not there.
func (s Store) RemoveChecked(ctx context.Context, domain, by string) (bool, error) {
	gone := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var business string
		err := tx.QueryRow(ctx, `DELETE FROM checked_agent_operators WHERE domain = $1 RETURNING business`, domain).Scan(&business)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		gone = true
		_, err = operatoraudit.RecordIn(ctx, tx, operatoraudit.Entry{Actor: by, Action: "agent_operator.uncheck", Target: domain, Detail: business})
		return err
	})
	return gone, err
}

var hostname = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// NormaliseDomain lowercases a signing domain and accepts only a bare host name: no scheme, port or path.
func NormaliseDomain(s string) (string, bool) {
	d := strings.ToLower(strings.TrimSpace(s))
	return d, len(d) <= 253 && hostname.MatchString(d)
}
