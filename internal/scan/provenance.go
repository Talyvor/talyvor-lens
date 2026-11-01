// Package scan is Talyvor Scan's data layer (B52.20): the source register and a store in which every figure knows
// where it came from. An observed point carries its source, the licence it was retrieved under, the retrieval time,
// the SHA-256 of the content it was read from and every transformation applied; a derived figure lists every input
// it came from; and a figure is shown to a user only with its sources' attribution. The database refuses a point
// without its provenance too (migrations/0287_scan_provenance.sql), so this package is not the only guard.
package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNoProvenance   = errors.New("scan: a point needs its source, retrieval time, content hash and transformations")
	ErrUnknownSource  = errors.New("scan: no such source in the register")
	ErrSourceOff      = errors.New("scan: the source is switched off")
	ErrRuledOut       = errors.New("scan: the source is ruled out and cannot be switched on")
	ErrNoInputs       = errors.New("scan: a derived figure must list the inputs it came from")
	ErrNotDisplayable = errors.New("scan: a source's licence does not allow showing this figure to users")
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Source is one entry of the register.
type Source struct {
	ID                  string
	Name                string
	Licence             string
	CommercialUse       bool
	PublicDisplay       bool
	RedistributeDerived bool
	Attribution         string
	Cost                string
	RequestsPerMinute   *int
	Enabled             bool
	RefusedReason       string
}

// Observation is a figure read from a source.
type Observation struct {
	SourceID    string
	Series      string
	ObservedAt  time.Time
	Value       float64
	Unit        string
	RetrievedAt time.Time
	ContentHash string   // hex SHA-256 of the content the value was read from
	Transforms  []string // every step from that content to Value, in order; empty when Value is the content as read
}

// Derivation is a figure computed from other stored figures.
type Derivation struct {
	Series     string
	ObservedAt time.Time
	Value      float64
	Unit       string
	Transforms []string // the method, at least one step
	Inputs     []int64
}

// Displayable is a figure ready to show a user, with the attribution every source behind it requires.
type Displayable struct {
	Value       float64
	Unit        string
	Attribution []string
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Source(ctx context.Context, id string) (Source, error) {
	var src Source
	var refused *string
	err := s.pool.QueryRow(ctx, `SELECT id, name, licence, commercial_use, public_display, redistribute_derived,
		attribution, cost, requests_per_minute, enabled, refused_reason FROM scan_sources WHERE id = $1`, id).
		Scan(&src.ID, &src.Name, &src.Licence, &src.CommercialUse, &src.PublicDisplay, &src.RedistributeDerived,
			&src.Attribution, &src.Cost, &src.RequestsPerMinute, &src.Enabled, &refused)
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrUnknownSource
	}
	if refused != nil {
		src.RefusedReason = *refused
	}
	return src, err
}

// SetEnabled switches a source on or off. A ruled-out source is refused by name.
func (s *Store) SetEnabled(ctx context.Context, id string, on bool) error {
	src, err := s.Source(ctx, id)
	if err != nil {
		return err
	}
	if on && src.RefusedReason != "" {
		return fmt.Errorf("%w: %s (%s)", ErrRuledOut, src.Name, src.RefusedReason)
	}
	_, err = s.pool.Exec(ctx, `UPDATE scan_sources SET enabled = $2 WHERE id = $1`, id, on)
	return err
}

// Record stores an observed point from a switched-on source, under the licence the register holds for it now.
func (s *Store) Record(ctx context.Context, o Observation) (int64, error) {
	if o.SourceID == "" || o.Series == "" || o.Unit == "" || o.ObservedAt.IsZero() || o.RetrievedAt.IsZero() ||
		!sha256Hex.MatchString(o.ContentHash) || o.Transforms == nil {
		return 0, ErrNoProvenance
	}
	src, err := s.Source(ctx, o.SourceID)
	if err != nil {
		return 0, err
	}
	if !src.Enabled {
		return 0, fmt.Errorf("%w: %s", ErrSourceOff, src.Name)
	}
	transforms, _ := json.Marshal(o.Transforms)
	var id int64
	err = s.pool.QueryRow(ctx, `INSERT INTO scan_points
		(kind, series, observed_at, value, unit, source_id, licence, retrieved_at, content_hash, transforms)
		VALUES ('observed', $1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		o.Series, o.ObservedAt, o.Value, o.Unit, src.ID, src.Licence, o.RetrievedAt, o.ContentHash, transforms).Scan(&id)
	return id, err
}

// Derive stores a figure computed from stored points, with every input it came from.
func (s *Store) Derive(ctx context.Context, d Derivation) (int64, error) {
	if len(d.Inputs) == 0 {
		return 0, ErrNoInputs
	}
	if d.Series == "" || d.Unit == "" || d.ObservedAt.IsZero() || len(d.Transforms) == 0 {
		return 0, ErrNoProvenance
	}
	transforms, _ := json.Marshal(d.Transforms)
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO scan_points (kind, series, observed_at, value, unit, transforms)
			VALUES ('derived', $1, $2, $3, $4, $5) RETURNING id`,
			d.Series, d.ObservedAt, d.Value, d.Unit, transforms).Scan(&id); err != nil {
			return err
		}
		for _, in := range d.Inputs {
			if _, err := tx.Exec(ctx, `INSERT INTO scan_point_inputs (point_id, input_id) VALUES ($1, $2)
				ON CONFLICT DO NOTHING`, id, in); err != nil {
				return fmt.Errorf("input %d: %w", in, err)
			}
		}
		return nil
	})
	return id, err
}

// Inputs lists the points a derived figure came from directly.
func (s *Store) Inputs(ctx context.Context, id int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT input_id FROM scan_point_inputs WHERE point_id = $1 ORDER BY input_id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// ForDisplay returns a figure with the attribution of every source behind it, walking derived figures back to their
// observations. It refuses when a source's licence does not allow public display, or, for a derived figure, does not
// allow redistributing figures derived from it.
func (s *Store) ForDisplay(ctx context.Context, id int64) (Displayable, error) {
	var out Displayable
	var kind string
	if err := s.pool.QueryRow(ctx, `SELECT kind, value, unit FROM scan_points WHERE id = $1`, id).
		Scan(&kind, &out.Value, &out.Unit); err != nil {
		return Displayable{}, err
	}
	rows, err := s.pool.Query(ctx, `WITH RECURSIVE tree(id) AS (
			SELECT $1::bigint
			UNION SELECT i.input_id FROM scan_point_inputs i JOIN tree t ON i.point_id = t.id)
		SELECT DISTINCT s.name, s.attribution, s.public_display, s.redistribute_derived
		FROM tree t JOIN scan_points p ON p.id = t.id JOIN scan_sources s ON s.id = p.source_id
		ORDER BY s.name`, id)
	if err != nil {
		return Displayable{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, attribution string
		var display, derived bool
		if err := rows.Scan(&name, &attribution, &display, &derived); err != nil {
			return Displayable{}, err
		}
		if !display || (kind == "derived" && !derived) {
			return Displayable{}, fmt.Errorf("%w: %s", ErrNotDisplayable, name)
		}
		out.Attribution = append(out.Attribution, attribution)
	}
	return out, rows.Err()
}
