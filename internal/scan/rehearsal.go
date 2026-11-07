package scan

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// rehearsal.go — B52.40: A SCAN REPORT REHEARSED ON A TEST WORKSPACE, FOR THE NIGHTLY.
//
// Rehearse runs the real pipeline on Lens's recorded data — every week EIA publishes of US distillate stocks — in a
// transaction it rolls back: the weeks recorded as observed points, the model backtested and assessed, the report
// drafted by the template with its figures and box. The draft is then worded, planted and checked exactly as the
// hourly writer checks one (checked), and kept in scan_rehearsals for that workspace alone. Nothing of it stays in
// scan_points, scan_backtests, scan_reports or scan_track_record, so it is never in the review queue, the public
// library or the public track record; its figures' point and backtest ids name the rolled-back rows. The test
// reviewer's approval puts it on the workspace's own track record, and a planted outcome scores it as Score does.

const (
	// RehearsalSeries is the one series Lens holds recorded data for.
	RehearsalSeries = "eia.stocks.us-distillate"
	// RehearsalReviewer is who approves a rehearsed report: the nightly, holding the synthetic key.
	RehearsalReviewer = "test reviewer (synthetic key)"
)

// recordedWeeksJSON is every week EIA publishes of US distillate stocks, August 1982 on. Not Cushing crude: the tank
// pilot's Cushing weeks are too few for the model's backtest (minTested), and on Cushing's whole history since 2004
// its 80% range held 72% of outcomes, so the model fails there and writes no report. Distillate's whole history
// passes it as recorded, with no window chosen.
//
//go:embed recorded/eia-us-distillate-weekly.json
var recordedWeeksJSON []byte

// rehearsalRetrieved is when the recorded weeks were read from EIA, as the file's source line says.
var rehearsalRetrieved = time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

var (
	ErrNoRecordedData = errors.New("scan: Lens holds recorded data only for " + RehearsalSeries)
	ErrNoConflicts    = errors.New("scan: no report is written until the conflicts statement is set (LENS_SCAN_CONFLICTS)")
)

// rehearsalColumns are queuedColumns as scan_rehearsals holds them, for scanQueued.
const rehearsalColumns = `id, series, (finding ->> 'at')::timestamptz, draft, writer, model_check, status,
	coalesce(refusal, ''), created_at, coalesce(reviewed_by, ''), reviewed_at, review_note`

// Rehearse drafts and checks a report on series' recorded data for the test workspace ws, with plant (when set)
// appended to "why it may matter" before the code rules, and keeps it as refused or pending.
func (w *Writer) Rehearse(ctx context.Context, ws, series, plant string) (QueuedReport, error) {
	if series != RehearsalSeries {
		return QueuedReport{}, ErrNoRecordedData
	}
	if w.conflicts == "" {
		return QueuedReport{}, ErrNoConflicts
	}
	b, d, terr, err := w.rehearsedDraft(ctx, series)
	if err != nil {
		return QueuedReport{}, err
	}
	q, err := w.checked(ctx, b, d, terr, plant)
	if err != nil {
		return q, err
	}
	finding, err := json.Marshal(b.finding)
	if err != nil {
		return q, err
	}
	draft, err := json.Marshal(q.Draft)
	if err != nil {
		return q, err
	}
	var refusal *string
	if q.Refusal != "" {
		refusal = &q.Refusal
	}
	return scanQueued(w.store.pool.QueryRow(ctx, `INSERT INTO scan_rehearsals (workspace_id, series, finding, draft, writer,
		model_check, status, refusal) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+rehearsalColumns,
		ws, q.Series, string(finding), string(draft), q.Writer, q.ModelCheck, q.Status, refusal))
}

// rehearsedDraft is the template's draft of series' recorded weeks, and the backtest it was written from, read in a
// transaction that is rolled back before any model is asked. terr is the template's own error, for checked.
func (w *Writer) rehearsedDraft(ctx context.Context, series string) (b backtestRow, d Draft, terr, err error) {
	var weeks eiaWeeks
	if err := json.Unmarshal(recordedWeeksJSON, &weeks); err != nil {
		return b, d, nil, err
	}
	sum := sha256.Sum256(recordedWeeksJSON)
	tx, err := w.store.pool.Begin(ctx)
	if err != nil {
		return b, d, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ts := &Store{pool: tx}
	if err := ts.SetEnabled(ctx, "eia", true); err != nil {
		return b, d, nil, err
	}
	for _, wk := range weeks.Stocks {
		at, err := time.Parse(time.DateOnly, wk.WeekEnding)
		if err != nil {
			return b, d, nil, fmt.Errorf("recorded week %q: %w", wk.WeekEnding, err)
		}
		if _, err := ts.Record(ctx, Observation{SourceID: "eia", Series: series, ObservedAt: at, Value: wk.Stocks,
			Unit: "thousand barrels", RetrievedAt: rehearsalRetrieved, ContentHash: hex.EncodeToString(sum[:]),
			Transforms: []string{fmt.Sprintf("thousand_barrels of the week ending %s in Lens's recorded EIA "+
				"WDISTUS1 weeks (internal/scan/recorded/eia-us-distillate-weekly.json)", wk.WeekEnding)}}); err != nil {
			return b, d, nil, err
		}
	}
	r, err := NewModels(ts).RunSeries(ctx, series)
	if err != nil {
		return b, d, nil, err
	}
	if !r.Passed || r.Finding == nil {
		return b, d, nil, fmt.Errorf("%w: %s", ErrModelRefused, r.Verdict)
	}
	b = backtestRow{series: series, method: r.Method, verdict: r.Verdict, ranAt: r.RanAt, from: r.DataFrom, to: r.DataTo,
		tested: r.Tested, hitRate: r.HitRate, error: r.Error, normalError: r.NormalError, coverage: r.Coverage,
		finding: *r.Finding}
	if err := tx.QueryRow(ctx, `SELECT id FROM scan_backtests WHERE series = $1 AND model = $2
		ORDER BY ran_at DESC, id DESC LIMIT 1`, series, ModelID).Scan(&b.id); err != nil {
		return b, d, nil, err
	}
	rw := *w
	rw.store = ts
	d, terr = rw.template(ctx, b)
	if terr != nil && !errors.Is(terr, ErrNotDisplayable) {
		return b, d, nil, terr
	}
	return b, d, terr, tx.Rollback(ctx)
}

// ApproveRehearsal is the test reviewer's approval of ws's pending rehearsed report id: it is then on ws's track
// record. ErrReportDecided unless it is pending.
func (w *Writer) ApproveRehearsal(ctx context.Context, ws string, id int64, note string) (QueuedReport, error) {
	q, err := scanQueued(w.store.pool.QueryRow(ctx, `UPDATE scan_rehearsals SET status = 'approved', reviewed_by = $3,
		reviewed_at = NOW(), review_note = $4 WHERE id = $1 AND workspace_id = $2 AND status = 'pending'
		RETURNING `+rehearsalColumns, id, ws, RehearsalReviewer, strings.TrimSpace(note)))
	if errors.Is(err, pgx.ErrNoRows) {
		return q, w.rehearsalMissing(ctx, ws, id)
	}
	return q, err
}

// PlantRehearsalOutcome records value as the outcome at ws's approved rehearsed report's outlook date and scores it
// as Score does — a hit when it lies inside the outlook's range, ends included — once. ErrReportDecided unless it is
// approved and not yet scored.
func (w *Writer) PlantRehearsalOutcome(ctx context.Context, ws string, id int64, value float64) (int, error) {
	tag, err := w.store.pool.Exec(ctx, `UPDATE scan_rehearsals SET outcome_value = $3,
		outcome_at = (finding ->> 'outlook_at')::timestamptz,
		hit = $3 BETWEEN (finding ->> 'outlook_low')::float8 AND (finding ->> 'outlook_high')::float8, scored_at = NOW()
		WHERE id = $1 AND workspace_id = $2 AND status = 'approved' AND scored_at IS NULL`, id, ws, value)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, w.rehearsalMissing(ctx, ws, id)
	}
	return int(tag.RowsAffected()), nil
}

func (w *Writer) rehearsalMissing(ctx context.Context, ws string, id int64) error {
	var exists bool
	if err := w.store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scan_rehearsals WHERE id = $1 AND workspace_id = $2)`,
		id, ws).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrReportDecided
	}
	return ErrReportNotFound
}

// RehearsalTrackRecord is ws's approved rehearsed reports' record, newest first, and the tally.
func (w *Writer) RehearsalTrackRecord(ctx context.Context, ws string) (TrackRecord, error) {
	return readTrackRecord(w.store.pool.Query(ctx, `SELECT id, series, coalesce(draft ->> 'title', ''), coalesce(draft -> 'sources', '[]'),
		reviewed_at, (finding ->> 'at')::timestamptz, (finding ->> 'value')::float8, coalesce(finding ->> 'unit', ''),
		(finding ->> 'normal_low')::float8, (finding ->> 'normal_high')::float8, (finding ->> 'outlook_at')::timestamptz,
		(finding ->> 'outlook_low')::float8, (finding ->> 'outlook_high')::float8, outcome_value, outcome_at,
		NULL::bigint, hit, scored_at FROM scan_rehearsals WHERE workspace_id = $1 AND status = 'approved'
		ORDER BY reviewed_at DESC, id DESC`, ws))
}
