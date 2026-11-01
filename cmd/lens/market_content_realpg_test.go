package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

// B41.185 — a listing labelled 18+ is hidden from a workspace that set the family filter, and no listing publishes
// without a label: driven through the routes talyvor-suite's B41.85 uses.
func TestMarketContent_AnAdultListingIsHiddenFromAFamilyWorkspaceAndNoneIsUnlabelled(t *testing.T) {
	pool := agentRoutesDB(t)
	const seller, family, other = "ws-b41185-seller", "ws-b41185-family", "ws-b41185-other"
	store := market.NewStore(pool)
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	mountMarketDiscoveryRoutes(r, store)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	publish := func(body string) market.Listing {
		t.Helper()
		code, out := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings", body)
		var l market.Listing
		if _ = json.Unmarshal([]byte(out), &l); code != http.StatusCreated {
			t.Fatalf("publish = %d %s", code, out)
		}
		return l
	}
	sees := func(ws, id string) (catalog, search, read bool) {
		t.Helper()
		_, c := call(ws, http.MethodGet, "/v1/marketplace/listings", "")
		_, s := call(ws, http.MethodGet, "/v1/marketplace/search?q=b41185", "")
		code, _ := call(ws, http.MethodGet, "/v1/marketplace/listings/"+id, "")
		return strings.Contains(c, id), strings.Contains(s, id), code == http.StatusOK
	}

	unrated := publish(`{"kind":"prompt","title":"b41185 unrated","artifact":{"template":"Write about {{topic}}"}}`)
	if unrated.AgeRating != "18+" {
		t.Fatalf("a listing published without a rating is rated %q; want 18+", unrated.AgeRating)
	}
	kids := publish(`{"kind":"prompt","title":"b41185 bedtime stories","age_rating":"all","artifact":{"template":"A story about {{animal}}"}}`)
	money := publish(`{"kind":"prompt","title":"b41185 budget coach","age_rating":"all","content_labels":["financial_advice"],"artifact":{"template":"Budget for {{goal}}"}}`)
	if kids.AgeRating != "all" || len(money.ContentLabels) != 1 || money.ContentLabels[0] != "financial_advice" {
		t.Fatalf("published %q %v and %q %v", kids.AgeRating, kids.ContentLabels, money.AgeRating, money.ContentLabels)
	}
	if code, out := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
		`{"kind":"prompt","title":"b41185 adult","age_rating":"13+","content_labels":["adult"],"artifact":{"template":"x {{y}}"}}`); code != http.StatusBadRequest {
		t.Errorf("an adult listing rated 13+ publishes: %d %s; want 400", code, out)
	}

	if code, out := call(family, http.MethodPut, "/v1/workspaces/"+family+"/marketplace/content-filter", `{"max_age_rating":"all","blocked_labels":["financial_advice"]}`); code != http.StatusOK {
		t.Fatalf("set the family filter = %d %s", code, out)
	}
	if c, s, g := sees(family, unrated.ID); c || s || g {
		t.Errorf("the family workspace finds the 18+ listing: catalog %v, search %v, read %v; want none", c, s, g)
	}
	if c, s, g := sees(family, money.ID); c || s || g {
		t.Errorf("the family workspace finds the listing labelled with what it blocks: catalog %v, search %v, read %v; want none", c, s, g)
	}
	if c, s, g := sees(family, kids.ID); !c || !s || !g {
		t.Errorf("the family workspace misses the listing rated all: catalog %v, search %v, read %v; want all three", c, s, g)
	}
	if c, s, g := sees(other, unrated.ID); !c || !s || !g {
		t.Errorf("a workspace without a filter misses the 18+ listing: catalog %v, search %v, read %v", c, s, g)
	}
	if c, _, g := sees(seller, unrated.ID); !c || !g {
		t.Errorf("its seller misses its own listing")
	}

	// A blocked listing leaves the catalog and search, and the filter names it until it is unblocked.
	if code, out := call(family, http.MethodPut, "/v1/workspaces/"+family+"/marketplace/blocked-listings/"+kids.ID, ""); code != http.StatusNoContent {
		t.Fatalf("block = %d %s", code, out)
	}
	if c, s, _ := sees(family, kids.ID); c || s {
		t.Errorf("a blocked listing is still found: catalog %v, search %v", c, s)
	}
	if _, out := call(family, http.MethodGet, "/v1/workspaces/"+family+"/marketplace/content-filter", ""); !strings.Contains(out, `"blocked_listings":["`+kids.ID+`"]`) {
		t.Errorf("the content filter = %s; want it to name the blocked listing", out)
	}
	call(family, http.MethodDelete, "/v1/workspaces/"+family+"/marketplace/blocked-listings/"+kids.ID, "")
	if c, _, _ := sees(family, kids.ID); !c {
		t.Errorf("an unblocked listing is not back in the catalog")
	}

	// Its seller rates the unrated listing for everyone: the family workspace finds it from then on.
	if code, out := call(seller, http.MethodPut, "/v1/workspaces/"+seller+"/marketplace/listings/"+unrated.ID+"/content-labels", `{"age_rating":"all"}`); code != http.StatusOK {
		t.Fatalf("relabel = %d %s", code, out)
	}
	if c, _, g := sees(family, unrated.ID); !c || !g {
		t.Errorf("the relabelled listing is still hidden: catalog %v, read %v", c, g)
	}
}
