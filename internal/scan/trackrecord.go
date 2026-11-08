package scan

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// trackrecord.go — B52.26: SCAN'S TRACK RECORD, COMPLETE AND PUBLIC.
//
// When a person approves a report (Decide), the same statement records what it observed and the range it gave for a
// later date in scan_track_record. Score fills in each record's outcome once the series' reading for that date
// arrives — the reading within half a step of the date, as the backtest reads it — and whether it fell inside the
// range. TrackRecord is every record, hits, misses and those still waiting, at TrackRecordPath. The database refuses
// any other change to a record, so none is edited after it is published.

// TrackEntry is one published report's record.
type TrackEntry struct {
	ReportID     int64      `json:"report_id"`
	Series       string     `json:"series"`
	Title        string     `json:"title"`
	Sources      []string   `json:"sources"`
	PublishedAt  time.Time  `json:"published_at"`
	ObservedAt   time.Time  `json:"observed_at"`
	Value        float64    `json:"value"`
	Unit         string     `json:"unit"`
	NormalLow    float64    `json:"normal_low"`
	NormalHigh   float64    `json:"normal_high"`
	OutlookAt    time.Time  `json:"outlook_at"`
	OutlookLow   float64    `json:"outlook_low"`
	OutlookHigh  float64    `json:"outlook_high"`
	Outcome      *float64   `json:"outcome,omitempty"`
	OutcomeAt    *time.Time `json:"outcome_at,omitempty"`
	OutcomePoint *int64     `json:"outcome_point,omitempty"`
	Hit          *bool      `json:"hit,omitempty"` // nil until scored
	ScoredAt     *time.Time `json:"scored_at,omitempty"`
}

// TrackRecord is every published report's record, newest first, and the tally.
type TrackRecord struct {
	Records []TrackEntry `json:"records"`
	Hits    int          `json:"hits"`
	Misses  int          `json:"misses"`
	Waiting int          `json:"waiting"` // published, outcome not yet known
}

// recordPublished is the INSERT that runs inside Decide's statement, from the approved row d and the finding it was
// written from.
const recordPublished = `INSERT INTO scan_track_record (report_id, series, title, sources, published_at, observed_at,
	value, unit, normal_low, normal_high, outlook_at, outlook_low, outlook_high)
	SELECT d.id, d.series, coalesce(d.draft ->> 'title', ''), coalesce(d.draft -> 'sources', '[]'), d.reviewed_at,
		(b.finding ->> 'at')::timestamptz, (b.finding ->> 'value')::float8, coalesce(b.finding ->> 'unit', ''),
		(b.finding ->> 'normal_low')::float8, (b.finding ->> 'normal_high')::float8,
		(b.finding ->> 'outlook_at')::timestamptz, (b.finding ->> 'outlook_low')::float8,
		(b.finding ->> 'outlook_high')::float8
	FROM d JOIN scan_backtests b ON b.id = d.backtest_id WHERE d.status = 'approved'`

// Score scores every waiting record whose outcome has arrived, and returns how many it scored.
func (w *Writer) Score(ctx context.Context) (int, error) {
	rows, err := w.store.pool.Query(ctx, `SELECT id, series, unit, outlook_at FROM scan_track_record
		WHERE scored_at IS NULL ORDER BY outlook_at, id`)
	if err != nil {
		return 0, err
	}
	type due struct {
		id           int64
		series, unit string
		at           time.Time
	}
	var todo []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.series, &d.unit, &d.at); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	scored := 0
	for _, d := range todo {
		f, ok := FamilyOf(d.series)
		if !ok {
			continue
		}
		half := f.Step / 2
		// The outcome is the reading nearest the outlook's date within half a step; the latest stored if revised.
		tag, err := w.store.pool.Exec(ctx, `UPDATE scan_track_record t SET outcome_point = p.id,
			outcome_at = p.observed_at, outcome_value = p.value, hit = p.value BETWEEN t.outlook_low AND t.outlook_high,
			scored_at = NOW()
			FROM (SELECT id, observed_at, value FROM scan_points WHERE kind = 'observed' AND series = $2 AND unit = $3
				AND observed_at >= $4 AND observed_at < $5
				ORDER BY abs(extract(epoch FROM observed_at - $6::timestamptz)), id DESC LIMIT 1) p
			WHERE t.id = $1 AND t.scored_at IS NULL`, d.id, d.series, d.unit, d.at.Add(-half), d.at.Add(half), d.at)
		if err != nil {
			return scored, err
		}
		scored += int(tag.RowsAffected())
	}
	return scored, nil
}

// TrackRecord is every published report's record: none is left out, hit or miss.
func (w *Writer) TrackRecord(ctx context.Context) (TrackRecord, error) {
	// shortcut: every record in one reply, a few a week; page it when it reaches thousands.
	return readTrackRecord(w.store.pool.Query(ctx, `SELECT report_id, series, title, sources, published_at, observed_at, value,
		unit, normal_low, normal_high, outlook_at, outlook_low, outlook_high, outcome_value, outcome_at, outcome_point,
		hit, scored_at FROM scan_track_record ORDER BY published_at DESC, id DESC`))
}

// readTrackRecord is the records rows hold, in TrackEntry's order, and their tally.
func readTrackRecord(rows pgx.Rows, err error) (TrackRecord, error) {
	if err != nil {
		return TrackRecord{}, err
	}
	defer rows.Close()
	out := TrackRecord{Records: []TrackEntry{}}
	for rows.Next() {
		var e TrackEntry
		var sources []byte
		if err := rows.Scan(&e.ReportID, &e.Series, &e.Title, &sources, &e.PublishedAt, &e.ObservedAt, &e.Value,
			&e.Unit, &e.NormalLow, &e.NormalHigh, &e.OutlookAt, &e.OutlookLow, &e.OutlookHigh, &e.Outcome,
			&e.OutcomeAt, &e.OutcomePoint, &e.Hit, &e.ScoredAt); err != nil {
			return out, err
		}
		if err := json.Unmarshal(sources, &e.Sources); err != nil {
			return out, err
		}
		switch {
		case e.Hit == nil:
			out.Waiting++
		case *e.Hit:
			out.Hits++
		default:
			out.Misses++
		}
		out.Records = append(out.Records, e)
	}
	return out, rows.Err()
}

func (w *Writer) score(ctx context.Context) {
	if _, err := w.Score(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("scan: scoring the track record failed", slog.String("err", err.Error()))
	}
}
