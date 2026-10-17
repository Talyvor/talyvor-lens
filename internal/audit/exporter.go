// Package audit exports token_events to compliance-friendly formats:
// JSON, CSV, or NDJSON for SIEM ingestion. The exporter streams rows
// directly to an io.Writer so a 100k-record export never materialises
// in memory; the same path is reused by the synchronous HTTP endpoint
// and the fire-and-forget webhook pusher.
package audit

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/safehttp"
)

// auditWebhookClient is SSRF-guarded: the audit export can carry ALL tenants' token_events (admin +
// empty filter), and webhookURL is caller/config-supplied — so it must not be dispatchable to an
// internal / loopback / cloud-metadata address. (Was http.DefaultClient, which had no guard.)
var auditWebhookClient = safehttp.Client(30 * time.Second)

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type AuditRecord struct {
	Timestamp    time.Time `json:"timestamp"     csv:"timestamp"`
	WorkspaceID  string    `json:"workspace_id"  csv:"workspace_id"`
	Team         string    `json:"team"          csv:"team"`
	Feature      string    `json:"feature"       csv:"feature"`
	Provider     string    `json:"provider"      csv:"provider"`
	Model        string    `json:"model"         csv:"model"`
	InputTokens  int       `json:"input_tokens"  csv:"input_tokens"`
	OutputTokens int       `json:"output_tokens" csv:"output_tokens"`
	CostUSD      float64   `json:"cost_usd"      csv:"cost_usd"`
	Cached       bool      `json:"cached"        csv:"cached"`
	PIIDetected  bool      `json:"pii_detected"  csv:"pii_detected"`
	Branch       string    `json:"branch"        csv:"branch"`
	SessionID    string    `json:"session_id"    csv:"session_id"`
	RequestID    string    `json:"request_id"    csv:"request_id"`
}

type ExportFilter struct {
	WorkspaceID string
	Team        string
	StartTime   time.Time
	EndTime     time.Time
	Provider    string
	MaxRecords  int
}

type ExportFormat string

const (
	FormatJSON   ExportFormat = "json"
	FormatCSV    ExportFormat = "csv"
	FormatNDJSON ExportFormat = "ndjson"

	defaultMaxRecords = 10_000
	hardMaxRecords    = 100_000
)

type Exporter struct {
	pool pgxDB
	// webhookClient dispatches the audit webhook. Defaults to the SSRF-guarded auditWebhookClient; tests
	// inject a loopback-capable client via WithHTTPClient (the guard is never weakened for production).
	webhookClient *http.Client
}

func New(pool *pgxpool.Pool) *Exporter {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newExporter(db)
}

func newExporter(pool pgxDB) *Exporter {
	return &Exporter{pool: pool}
}

// WithHTTPClient overrides the webhook client (test seam: a loopback httptest is blocked by the
// production SSRF guard by design). Nil keeps the guarded default.
func (e *Exporter) WithHTTPClient(c *http.Client) *Exporter {
	e.webhookClient = c
	return e
}

// Export streams rows from token_events to w in the requested format.
// Returns the count of records actually written so callers can stamp
// X-Talyvor-Record-Count on responses or webhooks.
func (e *Exporter) Export(ctx context.Context, filter ExportFilter, format ExportFormat, w io.Writer) (int, error) {
	if e.pool == nil {
		// Empty export — still write a well-formed envelope so the
		// HTTP response body parses cleanly client-side.
		return writeEmpty(w, format)
	}
	sql, args := buildQuery(filter)
	rows, err := e.pool.Query(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("audit: query token_events: %w", err)
	}
	defer rows.Close()

	switch format {
	case FormatCSV:
		return writeCSV(w, rows)
	case FormatNDJSON:
		return writeNDJSON(w, rows)
	case FormatJSON, "":
		return writeJSONArray(w, rows)
	default:
		return 0, fmt.Errorf("audit: unsupported format %q", format)
	}
}

// ExportWebhook materialises the export as NDJSON and POSTs it to
// webhookURL. The body is built into a bytes.Buffer so we know the
// record count for the X-Talyvor-Record-Count header BEFORE sending —
// callers wanting truly-streaming push to a webhook can call Export
// directly with their own http.Request body pipe.
func (e *Exporter) ExportWebhook(ctx context.Context, webhookURL string, filter ExportFilter) error {
	if err := safehttp.CheckURL(webhookURL); err != nil {
		return fmt.Errorf("audit: webhook URL: %w", err)
	}
	var buf bytes.Buffer
	count, err := e.Export(ctx, filter, FormatNDJSON, &buf)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, &buf)
	if err != nil {
		return fmt.Errorf("audit: build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-Talyvor-Export", "true")
	req.Header.Set("X-Talyvor-Record-Count", strconv.Itoa(count))
	client := e.webhookClient
	if client == nil {
		client = auditWebhookClient // SSRF-guarded default
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("audit: webhook POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("audit: webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// buildQuery composes the SELECT with only the WHERE arms the filter
// actually populates. The LIMIT argument is always the last placeholder
// so tests asserting `LIMIT $1` for an otherwise-unfiltered export
// match cleanly.
func buildQuery(filter ExportFilter) (string, []any) {
	limit := filter.MaxRecords
	if limit <= 0 {
		limit = defaultMaxRecords
	}
	if limit > hardMaxRecords {
		limit = hardMaxRecords
	}

	var (
		where  bytes.Buffer
		args   []any
		argIdx int
	)
	add := func(clause string, val any) {
		argIdx++
		if argIdx == 1 {
			where.WriteString(" WHERE ")
		} else {
			where.WriteString(" AND ")
		}
		fmt.Fprintf(&where, clause, argIdx)
		args = append(args, val)
	}
	if filter.WorkspaceID != "" {
		add("workspace_id = $%d", filter.WorkspaceID)
	}
	if filter.Team != "" {
		add("team = $%d", filter.Team)
	}
	if !filter.StartTime.IsZero() {
		add("created_at >= $%d", filter.StartTime)
	}
	if !filter.EndTime.IsZero() {
		add("created_at <= $%d", filter.EndTime)
	}
	if filter.Provider != "" {
		add("provider = $%d", filter.Provider)
	}
	args = append(args, limit)

	// ⚠ `cached` IS DERIVED FROM serve_source AND MUST NOT GO BACK TO THE COLUMN.
	// token_events.cached is BOOLEAN NOT NULL DEFAULT false (0001_init.sql) and has
	// never had a writer — neither production INSERT in alerts.go names it — so
	// selecting it shipped a literal `false` on every row of every export since
	// 0001, including rows that WERE served from cache. Migration 0100 added
	// serve_source for exactly this reason and it IS written on every serve;
	// `LIKE 'cache_hit%'` is that migration's own documented hit predicate, already
	// used verbatim by mcp/server.go#cacheStatsSQL. `<> 'upstream'` would be the
	// wrong rule: 0101 added 'node', and a node serve is a MISS (RecordNodeServe's
	// own doc says so — the node did the compute, no cache produced the bytes).
	// Pinned through the production writers by
	// TestExport_CachedReflectsTheServeThatActuallyHappened.
	//
	// ⚠ pii_detected BELOW IS THE SAME SHAPE AND IS **NOT** FIXED HERE, because
	// nothing writes it and no column of this table carries the answer: the value
	// is computed per request (proxy.go passes piiDetected to the learner) and
	// dropped before the billing insert. Deriving it is impossible; giving it a
	// writer means changing the token_events INSERT path. Measured and recorded
	// rather than guessed — see TestExport_PIIDetectedIsAConstant, which pins it as
	// a KNOWN constant so the day it gains a writer, the pin fails and says so.
	sql := `SELECT created_at, workspace_id, team, feature,
            provider, model, input_tokens, output_tokens,
            cost_usd, (serve_source LIKE 'cache_hit%') AS cached, pii_detected,
            session_id, request_id
        FROM token_events` + where.String() +
		fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argIdx+1)
	return sql, args
}

func scanRow(rows pgx.Rows) (AuditRecord, error) {
	var rec AuditRecord
	err := rows.Scan(
		&rec.Timestamp, &rec.WorkspaceID, &rec.Team, &rec.Feature,
		&rec.Provider, &rec.Model, &rec.InputTokens, &rec.OutputTokens,
		&rec.CostUSD, &rec.Cached, &rec.PIIDetected, &rec.SessionID, &rec.RequestID,
	)
	return rec, err
}

// csvHeader is the canonical column order for all CSV exports. Keeping
// this in one place means a struct addition forces a deliberate update
// here, rather than silently producing rows that don't match the header.
var csvHeader = []string{
	"timestamp", "workspace_id", "team", "feature",
	"provider", "model", "input_tokens", "output_tokens",
	"cost_usd", "cached", "pii_detected", "branch",
	"session_id", "request_id",
}

func writeCSV(w io.Writer, rows pgx.Rows) (int, error) {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write(csvHeader); err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return count, err
		}
		row := []string{
			rec.Timestamp.UTC().Format(time.RFC3339Nano),
			rec.WorkspaceID,
			rec.Team,
			rec.Feature,
			rec.Provider,
			rec.Model,
			strconv.Itoa(rec.InputTokens),
			strconv.Itoa(rec.OutputTokens),
			strconv.FormatFloat(rec.CostUSD, 'f', -1, 64),
			strconv.FormatBool(rec.Cached),
			strconv.FormatBool(rec.PIIDetected),
			rec.Branch,
			rec.SessionID,
			rec.RequestID,
		}
		if err := cw.Write(row); err != nil {
			return count, err
		}
		count++
	}
	cw.Flush()
	return count, rows.Err()
}

func writeNDJSON(w io.Writer, rows pgx.Rows) (int, error) {
	enc := json.NewEncoder(w)
	count := 0
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return count, err
		}
		// Encoder.Encode appends a newline, which is precisely the
		// inter-record separator NDJSON wants.
		if err := enc.Encode(rec); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}

// writeJSONArray streams a JSON array without buffering the slice. Open
// bracket, comma-separated objects, close bracket — done. Empty results
// produce `[]` so the response parses cleanly downstream.
func writeJSONArray(w io.Writer, rows pgx.Rows) (int, error) {
	if _, err := io.WriteString(w, "["); err != nil {
		return 0, err
	}
	enc := json.NewEncoder(w)
	count := 0
	for rows.Next() {
		rec, err := scanRow(rows)
		if err != nil {
			return count, err
		}
		if count > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return count, err
			}
		}
		if err := enc.Encode(rec); err != nil {
			return count, err
		}
		count++
	}
	if _, err := io.WriteString(w, "]"); err != nil {
		return count, err
	}
	return count, rows.Err()
}

func writeEmpty(w io.Writer, format ExportFormat) (int, error) {
	switch format {
	case FormatJSON, "":
		_, err := io.WriteString(w, "[]")
		return 0, err
	case FormatNDJSON:
		return 0, nil
	case FormatCSV:
		cw := csv.NewWriter(w)
		err := cw.Write(csvHeader)
		cw.Flush()
		return 0, err
	}
	return 0, errors.New("audit: unsupported format")
}
