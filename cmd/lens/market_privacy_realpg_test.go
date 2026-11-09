package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

// B41.193 — a listing's privacy label, as the listing page reads it: a pipeline that calls two models and keeps
// nothing shows both models, the tool its step uses and "keeps nothing", drawn from its own record; a listing that
// keeps something says what; and a "keeps" that is not a list of names is refused at publish.
func TestMarketListing_ItsPrivacyLabelShowsEveryModelEveryToolAndWhatItKeeps(t *testing.T) {
	pool := agentRoutesDB(t)
	const seller, translatorCo, buyer = "ws-privacy-seller", "ws-privacy-translator", "ws-privacy-buyer"
	store := market.NewStore(pool)
	r := chi.NewRouter()
	mountMarketRoutes(r, store)
	call := func(ws, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "owner-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	publish := func(ws, body string) market.Listing {
		t.Helper()
		code, out := call(ws, http.MethodPost, "/v1/workspaces/"+ws+"/marketplace/listings", body)
		var l market.Listing
		if _ = json.Unmarshal([]byte(out), &l); code != http.StatusCreated {
			t.Fatalf("publish = %d %s", code, out)
		}
		return l
	}
	label := func(id string) market.Privacy {
		t.Helper()
		code, out := call(buyer, http.MethodGet, "/v1/marketplace/listings/"+id, "")
		var l market.Listing
		if _ = json.Unmarshal([]byte(out), &l); code != http.StatusOK || len(l.Versions) == 0 {
			t.Fatalf("GET listing = %d %s", code, out)
		}
		if l.Versions[len(l.Versions)-1].Artifact != nil {
			t.Fatalf("a buyer reading the label was given the artifact: %s", out)
		}
		return l.Versions[len(l.Versions)-1].Privacy
	}

	summarise := publish(seller, `{"kind":"prompt","title":"One-line summary","artifact":{"template":"Summarise in one line: {{text}}","model":"sum"}}`)
	french := publish(translatorCo, `{"kind":"skill","title":"French","artifact":{"instructions":"Translate into French.","model":"fr","tools":["glossary"]}}`)
	pipeline := publish(seller, `{"kind":"pipeline","title":"Summary in French","artifact":{"steps":[{"use":"prompt:`+summarise.ID+`"},{"use":"listing:`+french.ID+`"}]}}`)

	if p := label(pipeline.ID); !slices.Equal(p.Models, []string{"sum", "fr"}) || !slices.Equal(p.Tools, []string{"glossary"}) ||
		len(p.Keeps) != 0 || !p.KeepsNothing {
		t.Fatalf("the pipeline's label = %+v; want models [sum fr], tools [glossary] and keeps nothing", p)
	}

	notes := publish(seller, `{"kind":"agent","title":"Note taker","artifact":{"system_prompt":"Take notes.","model":"sum","keeps":["your notes, for 30 days"]}}`)
	if p := label(notes.ID); !slices.Equal(p.Models, []string{"sum"}) || !slices.Equal(p.Keeps, []string{"your notes, for 30 days"}) || p.KeepsNothing {
		t.Fatalf("the note taker's label = %+v; want model [sum] and keeps [your notes, for 30 days]", p)
	}

	if code, out := call(seller, http.MethodPost, "/v1/workspaces/"+seller+"/marketplace/listings",
		`{"kind":"agent","title":"Vague","artifact":{"system_prompt":"Hi.","model":"sum","keeps":"everything"}}`); code != http.StatusBadRequest {
		t.Fatalf(`a "keeps" that is not a list of names = %d %s; want 400`, code, out)
	}
}
