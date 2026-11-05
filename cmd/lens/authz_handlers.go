package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/guardrails"
)

// authz_handlers.go extracts the Phase-1 cross-tenant-fix routes (#146) into
// named handler constructors so the wiring — each route actually funnelling its
// caller-supplied workspace input through effectiveWorkspaceID — is provable
// over HTTP against the real handler (see authz_routes_test.go). run() registers
// these exact constructors; behavior is unchanged from the prior inline closures.

// apiKeysRetired answers POST /v1/api/keys (B50.33). It minted keys with no scopes, which pass no scope gate any more;
// the owner mints a key with explicit scopes on the workspace route instead.
func apiKeysRetired(w http.ResponseWriter, _ *http.Request) {
	writeJSONErr(w, http.StatusGone, "POST /v1/api/keys is retired: the workspace's owner mints a key with explicit scopes "+
		"with POST /v1/workspaces/{wsID}/api-keys")
}

// auditReportExporter is the slice of *audit.Exporter the export handler needs.
type auditReportExporter interface {
	Export(ctx context.Context, filter audit.ExportFilter, format audit.ExportFormat, w io.Writer) (int, error)
}

// newAuditExportHandler streams an audit export. Authz (#146): a non-admin is
// scoped to its OWN workspace ALWAYS — an empty workspace_id must never mean
// "all tenants" for a tenant; only the global admin may export across workspaces.
func newAuditExportHandler(exporter auditReportExporter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		format := audit.ExportFormat(q.Get("format"))
		if format == "" {
			format = audit.FormatJSON
		}
		effWS, _, ok := effectiveWorkspaceID(req, q.Get("workspace_id"))
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		filter := audit.ExportFilter{
			WorkspaceID: effWS,
			Team:        q.Get("team"),
			Provider:    q.Get("provider"),
		}
		if v := q.Get("start"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				filter.StartTime = t
			}
		}
		if v := q.Get("end"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				filter.EndTime = t
			}
		}
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				filter.MaxRecords = n
			}
		}
		var ct, ext string
		switch format {
		case audit.FormatCSV:
			ct, ext = "text/csv", "csv"
		case audit.FormatNDJSON:
			ct, ext = "application/x-ndjson", "ndjson"
		default:
			ct, ext = "application/json", "json"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="audit-%s.%s"`, time.Now().UTC().Format("2006-01-02"), ext))
		w.WriteHeader(http.StatusOK)
		if _, err := exporter.Export(req.Context(), filter, format, w); err != nil {
			slog.Warn("audit: export failed mid-stream", slog.String("err", err.Error()))
		}
	}
}

// newGuardrailsPolicyPutHandler replaces a workspace's guardrail policy. Authz
// (#146): the policy is written to the CALLER's workspace for non-admins; admin
// honors the body (empty → "default").
func newGuardrailsPolicyPutHandler(engine *guardrails.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in guardrails.GuardrailPolicy
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		in.WorkspaceID = eff
		if in.WorkspaceID == "" {
			in.WorkspaceID = "default"
		}
		if err := engine.SetPolicy(req.Context(), in.WorkspaceID, in); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// marketplaceTradesReader is the slice of *economy.MarketplaceStore trades needs.
type marketplaceTradesReader interface {
	GetTrades(ctx context.Context, workspaceID string, limit int) ([]economy.MarketplaceTrade, error)
}

// newMarketplaceTradesHandler lists a workspace's trades. Authz (#146, closes
// #144): a non-admin reads only its OWN trades; admin may read any workspace.
func newMarketplaceTradesHandler(market marketplaceTradesReader) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID, _, ok := effectiveWorkspaceID(req, req.URL.Query().Get("workspace_id"))
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		if wsID == "" {
			writeJSONErr(w, http.StatusBadRequest, "workspace_id query param required")
			return
		}
		limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
		trades, err := market.GetTrades(req.Context(), wsID, limit)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, trades)
	}
}
