package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/controlplane"
)

// node_heartbeat_handlers.go — the three node heartbeat routes. Each one checks
// that the node belongs to the {wsID} workspace (the workspace-scoped UPDATE)
// BEFORE it writes the shared Redis heartbeat, so workspace A cannot keep
// workspace B's node looking alive by naming its id (B42.2). A node that is not
// the caller's answers 404, exactly like a node that does not exist.

func newInferenceHeartbeatHandler(pool *pgxpool.Pool, hb *controlplane.HeartbeatStore) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		nodeID := chi.URLParam(req, "nodeID")
		var in struct {
			ActiveRequests int64    `json:"active_requests"`
			UptimeSeconds  int64    `json:"uptime_seconds"`
			ModelsLoaded   []string `json:"models_loaded"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		if pool != nil {
			tag, err := pool.Exec(req.Context(), `
				UPDATE inference_nodes
				SET last_seen_at = NOW(), uptime_seconds = $2
				WHERE id = $1 AND workspace_id = $3`, nodeID, in.UptimeSeconds, wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tag.RowsAffected() == 0 {
				writeJSONErr(w, http.StatusNotFound, "node not found")
				return
			}
		}
		// Buffer heartbeat in Redis — any instance contributes to the shared
		// liveness view; the Reconciler reads Redis freshness when building
		// NodeSnapshots (xDS HA heartbeat reuse).
		_ = hb.Record(req.Context(), "inference", nodeID, in.UptimeSeconds)
		writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func newCacheHeartbeatHandler(pool *pgxpool.Pool, hb *controlplane.HeartbeatStore) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		nodeID := chi.URLParam(req, "nodeID")
		var in struct {
			Entries int     `json:"entries"`
			SizeMB  float64 `json:"size_mb"`
			HitRate float64 `json:"hit_rate"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		if pool != nil {
			// last_seen_at and metrics must move together atomically;
			// a partial update leaves the node appearing alive with
			// stale metrics or updated metrics with a stale timestamp.
			htx, err := pool.Begin(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			// workspace_id scope: prevents cross-workspace heartbeat hijack.
			tag, err := htx.Exec(req.Context(), `
				UPDATE cache_nodes SET last_seen_at = NOW() WHERE id = $1 AND workspace_id = $2`, nodeID, wsID)
			if err != nil {
				_ = htx.Rollback(req.Context())
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tag.RowsAffected() == 0 {
				_ = htx.Rollback(req.Context())
				writeJSONErr(w, http.StatusNotFound, "node not found")
				return
			}
			if _, err := htx.Exec(req.Context(), `
				INSERT INTO cache_node_metrics (node_id, entries, size_mb, hit_rate, last_updated)
				VALUES ($1, $2, $3, $4, NOW())
				ON CONFLICT (node_id) DO UPDATE
				SET entries = EXCLUDED.entries,
				    size_mb = EXCLUDED.size_mb,
				    hit_rate = EXCLUDED.hit_rate,
				    last_updated = NOW()`,
				nodeID, in.Entries, in.SizeMB, in.HitRate); err != nil {
				_ = htx.Rollback(req.Context())
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if err := htx.Commit(req.Context()); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		// Buffer heartbeat in Redis for xDS HA heartbeat reuse.
		_ = hb.Record(req.Context(), "cache", nodeID, 0)
		writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func newEmbeddingHeartbeatHandler(store *controlplane.NodeStore, hb *controlplane.HeartbeatStore) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		wsID := chi.URLParam(req, "wsID")
		nodeID := chi.URLParam(req, "nodeID")
		var in struct {
			SpeedTPS      int64 `json:"speed_tps"`
			Inflight      int64 `json:"inflight"`
			UptimeSeconds int64 `json:"uptime_seconds"`
		}
		_ = json.NewDecoder(req.Body).Decode(&in)
		// workspace_id scope: prevents cross-workspace heartbeat hijack.
		found, err := store.RecordEmbedHeartbeat(req.Context(), nodeID, wsID, in.UptimeSeconds)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			writeJSONErr(w, http.StatusNotFound, "node not found")
			return
		}
		// Buffer heartbeat in Redis for xDS HA heartbeat reuse.
		_ = hb.Record(req.Context(), "embedding", nodeID, in.UptimeSeconds)
		writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
