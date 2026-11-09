package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

// B32.25 — remixing a royalty listing returns its artifact and writes one grant with the share locked; remixing a
// listing whose policy is none is refused and writes no grant; a workspace without a grant cannot declare that listing
// as a parent; with one, its remix publishes with the locked share.
func TestMarketRemix_AcceptingTheLicenceOpensTheArtifactAndLocksTheShare(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const seller, remixer, stranger = "ws-remix-seller", "ws-remix-remixer", "ws-remix-stranger"
	r := chi.NewRouter()
	mountMarketRoutes(r, market.NewStore(pool))
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	publish := func(ws, title, extra string) (int, string) {
		t.Helper()
		return call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/marketplace/listings",
			`{"kind":"prompt","title":"`+title+`","visibility":"public","artifact":{"template":"Translate {{text}} into French."}`+extra+`}`)
	}
	listing := func(code int, body string) market.Listing {
		t.Helper()
		var l market.Listing
		if err := json.Unmarshal([]byte(body), &l); code != http.StatusCreated || err != nil {
			t.Fatalf("publish = %d %s", code, body)
		}
		return l
	}
	grants := func(listingID string) (n, share int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(max(share_bps), -1) FROM market_remix_grants WHERE listing_id = $1`,
			listingID).Scan(&n, &share); err != nil {
			t.Fatal(err)
		}
		return n, share
	}

	royalty := listing(publish(seller, "French", `,"remix_policy":"royalty","remix_share_bps":1500`))
	closed := listing(publish(seller, "Closed", ``))

	// Anyone else sees the listing but not its artifact.
	if code, body := call(remixer, http.MethodGet, "/v1/marketplace/listings/"+royalty.ID, ""); code != http.StatusOK ||
		strings.Contains(body, "Translate") {
		t.Fatalf("a buyer reading the listing = %d %s, want it without its artifact", code, body)
	}

	// Without a grant the remixer cannot declare it as a parent.
	code, body := publish(remixer, "Sneaky", `,"parents":[{"listing_id":"`+royalty.ID+`"}]`)
	if code != http.StatusBadRequest || !strings.Contains(body, "Accept that listing's remix licence") {
		t.Fatalf("declare a parent without a grant = %d %s, want 400", code, body)
	}

	// Remix: the artifact comes back and one grant locks the 15% share; pressing it again changes nothing.
	for range 2 {
		code, body = call(remixer, http.MethodPost, "/v1/workspaces/"+remixer+"/marketplace/listings/"+royalty.ID+"/remix", `{"version":1}`)
		var opened market.Remix
		if err := json.Unmarshal([]byte(body), &opened); code != http.StatusOK || err != nil {
			t.Fatalf("remix = %d %s", code, body)
		}
		if !strings.Contains(string(opened.Artifact), "Translate {{text}} into French.") || opened.Grant == nil ||
			opened.Grant.ShareBPS != 1500 || opened.Grant.Version != 1 || opened.Licence != market.RemixLicence {
			t.Fatalf("remix = %s, want the artifact and a grant at 1500 bps on v1", body)
		}
	}
	if n, share := grants(royalty.ID); n != 1 || share != 1500 {
		t.Fatalf("grants = %d at %d bps, want one at 1500", n, share)
	}

	// The seller raises the share; the remix publishes with the share the grant locked.
	if code, body := call(seller, http.MethodPut, "/v1/workspaces/"+seller+"/marketplace/listings/"+royalty.ID+"/remix-terms",
		`{"remix_policy":"royalty","remix_share_bps":2500}`); code != http.StatusOK {
		t.Fatalf("raise the share = %d %s", code, body)
	}
	remix := listing(publish(remixer, "Français", `,"parents":[{"listing_id":"`+royalty.ID+`"}]`))
	var edge int
	if err := pool.QueryRow(ctx, `SELECT share_bps FROM market_lineage WHERE child_listing_id = $1 AND parent_listing_id = $2`,
		remix.ID, royalty.ID).Scan(&edge); err != nil || edge != 1500 {
		t.Fatalf("the remix's edge = %d (%v), want the locked 1500 bps", edge, err)
	}

	// A listing whose policy is none is never opened, and no grant is written.
	code, body = call(stranger, http.MethodPost, "/v1/workspaces/"+stranger+"/marketplace/listings/"+closed.ID+"/remix", `{}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "does not allow remixes") || strings.Contains(body, "Translate") {
		t.Fatalf("remix a closed listing = %d %s, want 400 without the artifact", code, body)
	}
	if n, _ := grants(closed.ID); n != 0 {
		t.Fatalf("grants on the closed listing = %d, want none", n)
	}
}
