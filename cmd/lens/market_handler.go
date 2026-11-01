package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/storedanswers"
)

// B20.1 — PUBLISH: a listing is a versioned agent, prompt, skill, evaluation or pipeline (internal/market).
//
//	POST /v1/workspaces/{wsID}/marketplace/listings                {kind, title, description, price_per_use_ulxc, visibility, artifact, changelog,
//	                                                               remix_policy, remix_share_bps, parents: [{listing_id, version}]}
//	POST /v1/workspaces/{wsID}/marketplace/listings/{id}/versions  {artifact, changelog, parents}   a new version; the old ones stay usable
//	PUT  /v1/workspaces/{wsID}/marketplace/listings/{id}/offers    {offers: [...]}   B32.18: replace how it is sold
//	PUT  /v1/workspaces/{wsID}/marketplace/listings/{id}/remix-terms {remix_policy, remix_share_bps}   B32.24: may others build on it
//	POST /v1/workspaces/{wsID}/marketplace/listings/{id}/remix      {version}   B32.25: accept its remix licence and open its artifact
//	PUT  /v1/workspaces/{wsID}/marketplace/listings/{id}/content-labels {age_rating, content_labels}   B41.185: relabel it
//	GET  /v1/workspaces/{wsID}/marketplace/content-filter          B41.185: what it is shown, the listings it blocked, and the vocabulary
//	PUT  /v1/workspaces/{wsID}/marketplace/content-filter          {max_age_rating, blocked_labels}   B41.185: a family filter is "all"
//	PUT  /v1/workspaces/{wsID}/marketplace/blocked-listings/{id}   B41.185: never show it in its catalog, search or collections again
//	DELETE /v1/workspaces/{wsID}/marketplace/blocked-listings/{id} B41.185: show it again
//	GET  /v1/workspaces/{wsID}/marketplace/listings?currency=      the workspace's own listings
//	GET  /v1/marketplace/listings?kind=&currency=                  the public catalog
//	GET  /v1/marketplace/listings/{id}?currency=                   a listing and its versions (artifacts for its owner only)
//	GET  /v1/marketplace/listings/{id}/lineage?version=            B32.24: its ancestors with each edge's share, and its remixes
//	GET  /v1/marketplace/listings/{id}/trust                       B32.49: verified publisher, reviews, eval score, claims and lineage
//	PUT  /v1/workspaces/{wsID}/marketplace/listings/{id}/review    {rating, text}   B32.49: a paying buyer's review
//	PUT  /v1/workspaces/{wsID}/marketplace/listings/{id}/reviews/{reviewID}/reply {reply}   B32.49: the seller's reply
//	POST /v1/marketplace/listings/{id}/reports                     {reason, details}   B20.4: report a listing
//	POST /v1/workspaces/{wsID}/marketplace/ip-claims               {listing_id, original_listing_id | original_reference, evidence, good_faith}
//	GET  /v1/workspaces/{wsID}/marketplace/ip-claims               B32.47: claims against its listings, and claims it filed
//	POST /v1/workspaces/{wsID}/marketplace/ip-claims/{id}/counter  {statement}   B32.47: the seller's counter-notice
//
// A listing is sold through its offers (internal/market/offers.go): per_use, buy, rent or subscribe, each under a
// personal, commercial or enterprise licence, at a price in µUSD. A publish may carry them ("offers"); without them a
// price_per_use_ulxc is one per_use commercial offer. Replacing the offers changes the next charge and no past one.
//
// A publish sent again with the Idempotency-Key it was first sent with answers 200 with the listing that key
// made, and publishes nothing (B17.34). A publish the scan refuses is 422 with what it found (secrets, personal data, prompt injection); one the
// review holds (B20.4, internal/market/review.go) is 201 with review_status "held" and the reason, and only
// its owner sees it until an admin approves it. Publishing takes the workspace's owner or an admin; reading
// and reporting take any of its credentials.
//
// B32.24: a version may declare the listings it builds on (parents). Each must be one the publisher may see whose
// remix_policy is free or royalty — or the publisher's own — and none may descend from the version's own listing
// (400). Each parent's share is locked when it is declared: changing a listing's remix terms never changes a remix
// already made, and a new version keeps the parents of the one before it.
//
// B32.25: a listing's artifact is its owner's to read, but pressing Remix on someone else's free or royalty listing
// accepts its remix licence (docs/terms/remix.md): one grant per workspace and version, with the share locked, and the
// version's artifact to edit. Declaring someone else's listing as a parent needs that grant, and its edge carries the
// grant's share. A listing whose remix_policy is none is never opened (400).
//
// B32.31: a room's contribution (visibility room, published through POST /v1/rooms/{roomID}/contributions) is seen, with
// its artifacts, by its owner and its room's live members only; anyone else gets 404 and the catalog never lists it. A
// member that declares it as a parent records a room_fork edge at the room's remix share. A publish here cannot set
// visibility room (400).
//
// B32.46: every publish and every new version is compared with the approved public listings and the contributions of
// the rooms its publisher belongs to (internal/market/similarity.go). One at or above LENS_MARKET_SIMILARITY_HOLD to a
// listing that is neither a declared parent nor the publisher's own is 201 held: scan.similar names the nearest listing,
// its score and whether it is remixable, and review_reason says how to go on — remix it and declare it as a parent, or
// wait for a review.
//
// B32.47: a workspace that believes a listing copies its work files an IP claim (internal/market/ipclaims.go). The
// listing stays up; every billed use of it from then on is held, its earnings kept in holdback until the claim is
// decided. The seller finds it under "against" in their claims and may counter it until its counter_by
// (LENS_IP_COUNTER_DAYS after filing); the operator then decides it (POST /v1/admin/marketplace/ip-claims/{id}/decide).
// Filing and countering take the workspace's owner or an admin.
//
// B41.185: a publish may carry age_rating (all, 13+, 16+ or 18+; without one it is 18+) and content_labels (adult, violence,
// medical_advice, financial_advice, legal_advice; adult is 18+ only). A workspace whose content filter does not show a
// listing — rated above its max_age_rating, or carrying a label it blocks — never finds it in the catalog, search
// (here and MCP's market_search) or a collection, and reads it as 404; a listing it blocked stays out of the first three.
// It always sees its own. Changing the filter, labels or blocks takes the workspace's owner or an admin.
//
// B32.51: every listing read — these, search, a collection's listings and MCP's market_listing and market_search —
// shows each offer's price beside its US-dollar one in the reader's currency (display, internal/market/display.go):
// ?currency= (a three-letter ISO code; 400 otherwise), or by default that of the country the reader's tax profile
// resolves to, at the ECB reference rate. A consumer Talyvor charges VAT sees it included ("incl. VAT"), a business the
// price before it ("+ VAT"). The charge stays in US dollars on the monthly marketplace bill, and price_note says so.
//
// B32.49: the trust panel (internal/market/trust.go) is one read of what a buyer weighs: whether the publisher is
// verified (payouts enabled, no IP claim upheld against its listings in 12 months), the reviews of buyers who paid for a
// use or a licence and are not linked to the seller, the eval score of the latest version, the claims against the
// listing, and the originals and remixes the lineage read gives. Only such a buyer may review a listing (403 otherwise),
// once, rewriting it as often as it likes; the seller may reply to each review. Both take the workspace's owner or an
// admin.

// showPrices shows the listings' offers in the reader's currency (B32.51): ?currency=, or the buyer's own.
func showPrices(store *market.Store, req *http.Request, buyer string, listings ...*market.Listing) error {
	return store.ShowPrices(req.Context(), buyer, req.URL.Query().Get("currency"), listings...)
}

// listingRefs points at each listing of list, to be filled in place.
func listingRefs(list []market.Listing) []*market.Listing {
	refs := make([]*market.Listing, len(list))
	for i := range list {
		refs[i] = &list[i]
	}
	return refs
}

func mountMarketRoutes(r chi.Router, store *market.Store) {
	writeErr := func(w http.ResponseWriter, err error) {
		var refused *market.RefusedError
		switch {
		case errors.As(err, &refused):
			writeJSONOK(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "scan": refused.Scan})
		case errors.Is(err, market.ErrInvalid):
			writeJSONErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, market.ErrNotFound):
			writeJSONErr(w, http.StatusNotFound, err.Error())
		case errors.Is(err, market.ErrTakenDown):
			writeJSONErr(w, http.StatusGone, err.Error())
		case errors.Is(err, market.ErrNotReviewer):
			writeJSONErr(w, http.StatusForbidden, err.Error())
		default:
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
		}
	}
	r.Post("/v1/workspaces/{wsID}/marketplace/listings", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var d market.Draft
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, market.MaxArtifactBytes+64<<10)).Decode(&d); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be the listing: "+err.Error())
			return
		}
		// B17.34: a publish sent again with the same Idempotency-Key answers the listing it made (200).
		key := req.Header.Get("Idempotency-Key")
		if len(key) > 128 {
			writeJSONErr(w, http.StatusBadRequest, "the Idempotency-Key must be at most 128 characters")
			return
		}
		l, again, err := store.PublishOnce(req.Context(), chi.URLParam(req, "wsID"), key, d)
		if err != nil {
			writeErr(w, err)
			return
		}
		if again {
			writeJSONOK(w, http.StatusOK, l)
			return
		}
		writeJSONOK(w, http.StatusCreated, l)
	}))
	r.Post("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/versions", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Artifact  json.RawMessage    `json:"artifact"`
			Changelog string             `json:"changelog"`
			Parents   []market.ParentRef `json:"parents"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, market.MaxArtifactBytes+64<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {artifact, changelog, parents}: "+err.Error())
			return
		}
		v, err := store.PublishVersion(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), in.Artifact, in.Changelog, in.Parents)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, v)
	}))
	r.Put("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/offers", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Offers []market.Offer `json:"offers"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 64<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {offers: [{kind, licence, price_usd_micros, ...}]}: "+err.Error())
			return
		}
		offers, err := store.ReplaceOffers(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), in.Offers)
		if err != nil {
			writeErr(w, err)
			return
		}
		if offers == nil {
			offers = []market.Offer{}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"offers": offers})
	}))
	r.Put("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/remix-terms", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in market.RemixTerms
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {remix_policy, remix_share_bps}: "+err.Error())
			return
		}
		terms, err := store.SetRemixTerms(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, terms)
	}))
	r.Put("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/content-labels", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			AgeRating     string   `json:"age_rating"`
			ContentLabels []string `json:"content_labels"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {age_rating, content_labels}: "+err.Error())
			return
		}
		l, err := store.SetContentLabels(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), in.AgeRating, in.ContentLabels)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	}))
	r.Get("/v1/workspaces/{wsID}/marketplace/content-filter", func(w http.ResponseWriter, req *http.Request) {
		f, err := store.ContentFilter(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, f)
	})
	r.Put("/v1/workspaces/{wsID}/marketplace/content-filter", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			MaxAgeRating  string   `json:"max_age_rating"`
			BlockedLabels []string `json:"blocked_labels"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {max_age_rating, blocked_labels}: "+err.Error())
			return
		}
		f, err := store.SetContentFilter(req.Context(), chi.URLParam(req, "wsID"), in.MaxAgeRating, in.BlockedLabels)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, f)
	}))
	r.Put("/v1/workspaces/{wsID}/marketplace/blocked-listings/{listingID}", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		if err := store.BlockListing(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r.Delete("/v1/workspaces/{wsID}/marketplace/blocked-listings/{listingID}", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		if err := store.UnblockListing(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r.Post("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/remix", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Version int `json:"version"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
			writeJSONErr(w, http.StatusBadRequest, "body must be {version}: "+err.Error())
			return
		}
		if in.Version < 0 {
			writeJSONErr(w, http.StatusBadRequest, "version must be a positive whole number")
			return
		}
		remix, err := store.Remix(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), in.Version)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, remix)
	}))
	r.Get("/v1/workspaces/{wsID}/marketplace/listings", func(w http.ResponseWriter, req *http.Request) {
		list, err := store.OwnListings(req.Context(), chi.URLParam(req, "wsID"))
		if err == nil {
			err = showPrices(store, req, chi.URLParam(req, "wsID"), listingRefs(list)...)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"listings": list})
	})
	r.Get("/v1/marketplace/listings", func(w http.ResponseWriter, req *http.Request) {
		viewer, _ := auth.WorkspaceIdentity(req.Context())
		list, err := store.Catalog(req.Context(), viewer, req.URL.Query().Get("kind"))
		if err == nil {
			err = showPrices(store, req, viewer, listingRefs(list)...)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"listings": list})
	})
	r.Get("/v1/marketplace/listings/{listingID}", func(w http.ResponseWriter, req *http.Request) {
		viewer, _ := auth.WorkspaceIdentity(req.Context())
		l, err := store.Get(req.Context(), viewer, chi.URLParam(req, "listingID"))
		if err == nil {
			err = showPrices(store, req, viewer, &l)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	})
	r.Get("/v1/marketplace/listings/{listingID}/lineage", func(w http.ResponseWriter, req *http.Request) {
		viewer, _ := auth.WorkspaceIdentity(req.Context())
		version := 0
		if v := req.URL.Query().Get("version"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				writeJSONErr(w, http.StatusBadRequest, "version must be a positive whole number")
				return
			}
			version = n
		}
		lineage, err := store.Lineage(req.Context(), viewer, chi.URLParam(req, "listingID"), version)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, lineage)
	})
	r.Get("/v1/marketplace/listings/{listingID}/trust", func(w http.ResponseWriter, req *http.Request) {
		viewer, _ := auth.WorkspaceIdentity(req.Context())
		trust, err := store.Trust(req.Context(), viewer, chi.URLParam(req, "listingID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, trust)
	})
	r.Put("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/review", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in market.ReviewInput
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {rating, text}: "+err.Error())
			return
		}
		review, err := store.Review(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, review)
	}))
	r.Put("/v1/workspaces/{wsID}/marketplace/listings/{listingID}/reviews/{reviewID}/reply", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Reply string `json:"reply"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {reply}: "+err.Error())
			return
		}
		review, err := store.ReplyToReview(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "listingID"), chi.URLParam(req, "reviewID"), in.Reply)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, review)
	}))
	r.Post("/v1/workspaces/{wsID}/marketplace/ip-claims", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in market.IPClaimFiling
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {listing_id, original_listing_id or original_reference, evidence, good_faith}: "+err.Error())
			return
		}
		c, err := store.FileIPClaim(req.Context(), chi.URLParam(req, "wsID"), in, time.Now())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusCreated, c)
	}))
	r.Get("/v1/workspaces/{wsID}/marketplace/ip-claims", func(w http.ResponseWriter, req *http.Request) {
		against, filed, err := store.IPClaims(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"against": against, "filed": filed})
	})
	r.Post("/v1/workspaces/{wsID}/marketplace/ip-claims/{claimID}/counter", marketOwnerOnly(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Statement string `json:"statement"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {statement}: "+err.Error())
			return
		}
		c, err := store.CounterIPClaim(req.Context(), chi.URLParam(req, "wsID"), chi.URLParam(req, "claimID"), in.Statement, time.Now())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, c)
	}))
	r.Post("/v1/marketplace/listings/{listingID}/reports", func(w http.ResponseWriter, req *http.Request) {
		reporter, _ := auth.WorkspaceIdentity(req.Context())
		if reporter == "" {
			writeJSONErr(w, http.StatusForbidden, "a report needs a workspace's credential")
			return
		}
		var in struct {
			Reason  string `json:"reason"`
			Details string `json:"details"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {reason, details}: "+err.Error())
			return
		}
		rep, err := store.Report(req.Context(), reporter, chi.URLParam(req, "listingID"), in.Reason, in.Details)
		if err != nil {
			writeErr(w, err)
			return
		}
		status := http.StatusCreated
		if rep.AlreadyMade {
			status = http.StatusOK
		}
		writeJSONOK(w, status, rep)
	})
}

// B20.4 — the admin's half of marketplace safety, registered in main.go behind requireAdmin:
//
//	GET  /v1/admin/marketplace/review                        held listings and reported ones, most reported first
//	POST /v1/admin/marketplace/listings/{id}/approve         keep it up: release a hold, resolve its reports as kept
//	POST /v1/admin/marketplace/listings/{id}/takedown        {reason}   take it down and refund its uses inside the holdback
//
//	GET  /v1/admin/marketplace/ip-claims?status=             B32.47: undecided IP claims, oldest first (or those in status)
//	POST /v1/admin/marketplace/ip-claims/{id}/decide         {outcome: upheld|attributed|rejected, share_bps, reason, actor}
//
// A takedown answers with the refunds it wrote (market_refunds rows); a buyer's credit Stripe did not
// accept is named in credit_error and retried by refundTakenDownMarketUses. An IP claim is decided once the seller
// has countered it or their counter window is over; the operator is the one X-Talyvor-Operator names, or the body's
// actor, and the decision is recorded under that name in the operator audit trail.

func writeMarketAdminErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, market.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, market.ErrNotFound), errors.Is(err, market.ErrNotParked):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, market.ErrTakenDown):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

func newMarketReviewQueueHandler(store *market.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q, err := store.ReviewQueue(req.Context())
		if err != nil {
			writeMarketAdminErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"listings": q})
	})
}

// parkedUseLister is the slice of *market.Store the parked-uses read needs.
type parkedUseLister interface {
	ParkedUses(ctx context.Context) ([]market.ParkedUse, error)
}

// newMarketParkedUsesHandler answers GET /v1/admin/marketplace/parked-uses (B26.3): every billed use Stripe
// refused market.MaxMeterRefusals times, with Stripe's reason — off its buyer's bill until an operator acts.
func newMarketParkedUsesHandler(store parkedUseLister) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		parked, err := store.ParkedUses(req.Context())
		if err != nil {
			writeMarketAdminErr(w, err)
			return
		}
		if parked == nil {
			parked = []market.ParkedUse{}
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"parked_uses": parked})
	})
}

// parkedUseRetrier is the slice of *market.Store the retry needs.
type parkedUseRetrier interface {
	RetryParkedUse(ctx context.Context, useID string) error
}

// newMarketParkedUseRetryHandler answers POST /v1/admin/marketplace/parked-uses/{useID}/retry (B27.19): the
// operator has seen to it, so the next metering pass tries it again.
func newMarketParkedUseRetryHandler(store parkedUseRetrier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := chi.URLParam(req, "useID")
		if err := store.RetryParkedUse(req.Context(), id); err != nil {
			writeMarketAdminErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"id": id, "retrying": true})
	})
}

func newMarketApproveHandler(store *market.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		l, err := store.Approve(req.Context(), chi.URLParam(req, "listingID"))
		if err != nil {
			writeMarketAdminErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, l)
	})
}

func newMarketTakedownHandler(store *market.Store, refunder market.Refunder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {reason}: "+err.Error())
			return
		}
		t, err := store.TakeDown(req.Context(), refunder, chi.URLParam(req, "listingID"), in.Reason)
		if err != nil {
			writeMarketAdminErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, t)
	})
}

func newMarketIPClaimQueueHandler(store *market.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claims, err := store.IPClaimQueue(req.Context(), req.URL.Query().Get("status"))
		if err != nil {
			writeMarketAdminErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"claims": claims})
	})
}

func newMarketIPClaimDecideHandler(store *market.Store, refunder market.Refunder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			market.IPClaimDecision
			Actor string `json:"actor"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 16<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {outcome, share_bps, reason, actor}: "+err.Error())
			return
		}
		actor, ok := contractActor(req, in.Actor)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		out, err := store.DecideIPClaim(req.Context(), refunder, chi.URLParam(req, "claimID"), actor, in.IPClaimDecision, time.Now())
		if err != nil {
			writeMarketAdminErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, out)
	})
}

// marketOwnerOnly admits the workspace's owner or an admin — the rule the agent routes use.
func marketOwnerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := storedanswers.OwnerOrAdmin(req.Context()); !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner or an admin may publish its listings")
			return
		}
		next(w, req)
	}
}
