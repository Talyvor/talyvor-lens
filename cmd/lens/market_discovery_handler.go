package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

// B32.50 — discovery: trending by distinct buyers, search by capability and price, public collections.
//
//	GET    /v1/marketplace/search?q=&capability=&kind=&licence=&max_price_per_use=&min_eval=&verified_only=&sort=&page=&currency=
//	GET    /v1/marketplace/ask?q=&volume=&currency=                       B41.171: up to three listings for a job, each with its estimate
//	GET    /v1/marketplace/capabilities                                   the controlled list a listing declares from
//	PUT    /v1/workspaces/{wsID}/marketplace/listings/{id}/capabilities   {capabilities: [...]}   what the listing can do
//	GET    /v1/marketplace/collections                                    the public collections, the featured first
//	GET    /v1/marketplace/collections/{id}?currency=                     a collection and its listings, in its order
//	GET    /v1/workspaces/{wsID}/marketplace/collections                  the workspace's own collections
//	POST   /v1/workspaces/{wsID}/marketplace/collections                  {title, description, public, listing_ids}
//	PUT    /v1/workspaces/{wsID}/marketplace/collections/{id}             the same: replaces it
//	DELETE /v1/workspaces/{wsID}/marketplace/collections/{id}
//
// Search reads the public listings the review approved (internal/market/discovery.go): q is full-text search on the
// title and description — a sentence is matched by any of its words and re-ranked by meaning —, max_price_per_use is
// in µUSD (50000 is $0.05) and keeps a listing that is free or whose per-use price is at or under it, min_eval is a
// percentage of eval cases passed, verified_only is true or false, and sort is relevance (the default with q),
// trending (the default without; distinct buyers of the last seven days, the seller's own and linked workspaces never
// counted), new or price. Each page has 50 listings; total and has_more say how many there are. Writing a listing's
// capabilities or a collection takes the workspace's owner or an admin; reading takes any credential.
//
//	POST /v1/admin/marketplace/collections/{id}/feature                  {featured, actor}   the operator, in main.go
func mountMarketDiscoveryRoutes(r chi.Router, store *market.Store) {
	writeErr := func(w http.ResponseWriter, err error) {
		switch {
		case errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, market.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, market.ErrTakenDown):
			writeJSONErr(w, http.StatusGone, err.Error())
		default:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		}
	}
	r.Get("/v1/marketplace/search", func(w http.ResponseWriter, req *http.Request) {
		v := req.URL.Query()
		q := market.DiscoverQuery{Q: v.Get("q"), Capability: v.Get("capability"), Kind: v.Get("kind"), Licence: v.Get("licence"), Sort: v.Get("sort")}
		whole := func(name string, into *int) bool {
			if s := v.Get(name); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil {
					writeJSONErr(w, http.StatusBadRequest, name+" must be a whole number")
					return false
				}
				*into = n
			}
			return true
		}
		if !whole("page", &q.Page) || !whole("min_eval", &q.MinEval) {
			return
		}
		if s := v.Get("max_price_per_use"); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "max_price_per_use must be a whole number of µUSD (50000 is $0.05)")
				return
			}
			q.MaxPricePerUse = &n
		}
		if s := v.Get("verified_only"); s != "" {
			b, err := strconv.ParseBool(s)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "verified_only must be true or false")
				return
			}
			q.VerifiedOnly = b
		}
		page, err := store.Discover(req.Context(), q)
		if err == nil { // B32.51: each offer's price in the reader's currency too
			viewer, _ := auth.WorkspaceIdentity(req.Context())
			hits := make([]*market.Listing, len(page.Listings))
			for i := range page.Listings {
				hits[i] = &page.Listings[i].Listing
			}
			err = showPrices(store, req, viewer, hits...)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, page)
	})
	// B41.171: q is the job in words; volume is how many uses to price (absent: the first number in q, or one).
	r.Get("/v1/marketplace/ask", func(w http.ResponseWriter, req *http.Request) {
		v := req.URL.Query()
		volume := 0
		if s := v.Get("volume"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				writeJSONErr(w, http.StatusBadRequest, "volume must be a whole number of uses, at least 1")
				return
			}
			volume = n
		}
		viewer, _ := auth.WorkspaceIdentity(req.Context())
		answer, err := store.Ask(req.Context(), viewer, v.Get("currency"), v.Get("q"), volume)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, answer)
	})
	r.Get("/v1/marketplace/capabilities", func(w http.ResponseWriter, req *http.Request) {
		caps, err := store.Capabilities(req.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"capabilities": caps})
	})
	r.Put("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/capabilities", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Capabilities []string `json:"capabilities"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {capabilities: [...]}: "+err.Error())
			return
		}
		caps, err := store.SetCapabilities(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), in.Capabilities)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"capabilities": caps})
	}))
	r.Get("/v1/marketplace/collections", func(w http.ResponseWriter, req *http.Request) {
		list, err := store.PublicCollections(req.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"collections": list})
	})
	r.Get("/v1/marketplace/collections/{collectionID}", func(w http.ResponseWriter, req *http.Request) {
		viewer, _ := auth.WorkspaceIdentity(req.Context())
		c, err := store.GetCollection(req.Context(), viewer, chi.URLParam(req, "collectionID"))
		if err == nil {
			err = showPrices(store, req, viewer, listingRefs(c.Listings)...)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	})
	r.Get("/v1/workspaces/{wsID}/marketplace/collections", func(w http.ResponseWriter, req *http.Request) {
		list, err := store.OwnCollections(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"collections": list})
	})
	save := func(created bool) http.HandlerFunc {
		return marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
			var in market.CollectionInput
			if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "body must be {title, description, public, listing_ids}: "+err.Error())
				return
			}
			c, err := store.SaveCollection(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "collectionID"), in)
			if err != nil {
				writeErr(w, err)
				return
			}
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			writeJSONOK(w, status, c)
		})
	}
	r.Post("/v1/workspaces/{wsID}/marketplace/collections", save(true))
	r.Put("/v1/workspaces/{wsID}/marketplace/collections/{collectionID}", save(false))
	r.Delete("/v1/workspaces/{wsID}/marketplace/collections/{collectionID}", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		id := chi.URLParam(req, "collectionID")
		if err := store.DeleteCollection(req.Context(), chi.URLParam(req, "wsID"), id); err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
	}))
}

// newMarketFeatureCollectionHandler answers POST /v1/admin/marketplace/collections/{id}/feature: the operator marks a
// public collection featured ({featured: true}) or no longer ({featured: false}), recorded in the operator audit trail.
func newMarketFeatureCollectionHandler(store *market.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		in := struct {
			Featured *bool  `json:"featured"`
			Actor    string `json:"actor"`
		}{}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeJSONErr(w, http.StatusBadRequest, "body must be {featured, actor}: "+err.Error())
			return
		}
		featured := in.Featured == nil || *in.Featured
		actor, ok := contractActor(req, in.Actor)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		c, err := store.FeatureCollection(req.Context(), chi.URLParam(req, "collectionID"), actor, featured)
		if err != nil {
			writeMarketAdminErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	})
}

// refreshMarketDiscovery is the nightly job: once each UTC day — the first hour it runs on a day — it rewrites the
// marketplace's daily listing stats and trending scores (market.Store.RefreshDiscoveryStats).
func refreshMarketDiscovery(ctx context.Context, store *market.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	done := ""
	for {
		if day := time.Now().UTC().Format(time.DateOnly); day != done {
			if n, err := store.RefreshDiscoveryStats(ctx, time.Now()); err != nil {
				slog.Warn("market: refreshing the discovery stats and trending scores", "err", err)
			} else {
				done = day
				slog.Info("market: refreshed the discovery stats and trending scores", "stats_rows", n.StatsRows, "trending", n.Trending)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
