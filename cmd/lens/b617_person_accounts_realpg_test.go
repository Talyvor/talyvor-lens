package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// B61.7: one person (the identity the BFF presents) holds several accounts. Provision's list, open and session modes,
// against a real Postgres. provisionManager migrates a fresh private schema once a run, so no identity here meets an
// account from an earlier run.

type personAccountsBody struct {
	Accounts []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		Kind     string `json:"kind"`
		OpenedAt string `json:"opened_at"`
	} `json:"accounts"`
	Error string `json:"error"`
}

func b617Router(t *testing.T) (chi.Router, *ecdsaKeyFixture, *pgxpool.Pool) {
	t.Helper()
	mgr, pool := provisionManager(t)
	keys := newKeyFixture(t)
	r := chi.NewRouter()
	mountProvisionRoute(r, provisionSecret, mgr, nil, keys.mint, nil)
	return r, keys, pool
}

func b617List(t *testing.T, r chi.Router, identity string) personAccountsBody {
	t.Helper()
	rec, _ := doProvision(t, r, provisionSecret, `{"identity":"`+identity+`","mode":"list"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var out personAccountsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return out
}

// An identity with one account opens a second of another type and kind; both are listed, and the call without a mode
// still answers the first.
func TestB617_SecondAccountOfAnotherTypeIsOpenedAndListed(t *testing.T) {
	r, keys, _ := b617Router(t)
	const who = "b617-person-one"

	rec, first := doProvision(t, r, provisionSecret, `{"identity":"`+who+`","display_name":"Acme Ltd","account_type":"business","money_kind":"hybrid","country":"GB","registration_number":"01234567"}`)
	if rec.Code != http.StatusOK || !first.Created {
		t.Fatalf("first sign-up = %d created=%v: %s", rec.Code, first.Created, rec.Body.String())
	}
	rec, second := doProvision(t, r, provisionSecret, `{"identity":"`+who+`","mode":"open","display_name":"Me","account_type":"personal","money_kind":"fiat","country":"GB"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("open another = %d: %s", rec.Code, rec.Body.String())
	}
	if second.WorkspaceID == first.WorkspaceID || !second.Created {
		t.Fatalf("open another answered workspace %q created=%v; the first is %q — a person who opens another account must get a new one",
			second.WorkspaceID, second.Created, first.WorkspaceID)
	}
	// The second account's token is for the second account and no other.
	if code := isolationProbe(t, keys, second.Token, second.WorkspaceID); code != http.StatusOK {
		t.Errorf("second account's token on itself = %d, want 200", code)
	}
	if code := isolationProbe(t, keys, second.Token, first.WorkspaceID); code != http.StatusForbidden {
		t.Errorf("second account's token on the first account = %d, want 403", code)
	}
	if code := isolationProbe(t, keys, first.Token, second.WorkspaceID); code != http.StatusForbidden {
		t.Errorf("first account's token on the second account = %d, want 403", code)
	}

	list := b617List(t, r, who)
	if len(list.Accounts) != 2 {
		t.Fatalf("listed %d accounts, want 2: %+v", len(list.Accounts), list.Accounts)
	}
	got := map[string]string{}
	for _, a := range list.Accounts {
		got[a.ID] = a.Name + "/" + a.Type + "/" + a.Kind
		if a.OpenedAt == "" {
			t.Errorf("account %s has no opened_at", a.ID)
		}
	}
	if got[first.WorkspaceID] != "Acme Ltd/business/hybrid" || got[second.WorkspaceID] != "Me/personal/fiat" {
		t.Errorf("listed %v", got)
	}
	if list.Accounts[0].ID != first.WorkspaceID {
		t.Errorf("first listed is %s, want the first opened %s", list.Accounts[0].ID, first.WorkspaceID)
	}

	// Today's call (no mode) keeps answering the identity's first account.
	rec, again := doProvision(t, r, provisionSecret, `{"identity":"`+who+`","account_type":"personal"}`)
	if rec.Code != http.StatusOK || again.WorkspaceID != first.WorkspaceID || again.Created {
		t.Errorf("call without a mode = %d %q created=%v, want the first account %q", rec.Code, again.WorkspaceID, again.Created, first.WorkspaceID)
	}

	// A session for the second account names it.
	rec, sess := doProvision(t, r, provisionSecret, `{"identity":"`+who+`","mode":"session","account_id":"`+second.WorkspaceID+`"}`)
	if rec.Code != http.StatusOK || sess.WorkspaceID != second.WorkspaceID || sess.Created {
		t.Fatalf("session for the second account = %d %q created=%v: %s", rec.Code, sess.WorkspaceID, sess.Created, rec.Body.String())
	}
	if code := isolationProbe(t, keys, sess.Token, second.WorkspaceID); code != http.StatusOK {
		t.Errorf("session token on its account = %d, want 200", code)
	}
	if code := isolationProbe(t, keys, sess.Token, first.WorkspaceID); code != http.StatusForbidden {
		t.Errorf("session token for the second account on the first = %d, want 403", code)
	}
}

// A session for an account the identity does not own is 404, whether it belongs to another person or does not exist.
func TestB617_SessionForAnotherIdentitysAccountIs404(t *testing.T) {
	r, _, _ := b617Router(t)
	const alice, bob = "b617-alice", "b617-bob"

	_, a := doProvision(t, r, provisionSecret, `{"identity":"`+alice+`"}`)
	_, aOther := doProvision(t, r, provisionSecret, `{"identity":"`+alice+`","mode":"open","account_type":"personal","country":"GB"}`)
	_, b := doProvision(t, r, provisionSecret, `{"identity":"`+bob+`"}`)
	if a.WorkspaceID == "" || aOther.WorkspaceID == "" || b.WorkspaceID == "" {
		t.Fatalf("setup: %q %q %q", a.WorkspaceID, aOther.WorkspaceID, b.WorkspaceID)
	}
	for _, target := range []string{a.WorkspaceID, aOther.WorkspaceID, "u-does-not-exist"} {
		rec, out := doProvision(t, r, provisionSecret, `{"identity":"`+bob+`","mode":"session","account_id":"`+target+`"}`)
		if rec.Code != http.StatusNotFound || out.Token != "" {
			t.Errorf("bob's session for %s = %d token=%v, want 404 and no token", target, rec.Code, out.Token != "")
		}
	}
	if list := b617List(t, r, bob); len(list.Accounts) != 1 || list.Accounts[0].ID != b.WorkspaceID {
		t.Errorf("bob lists %+v, want only his own %s", list.Accounts, b.WorkspaceID)
	}
}

// A person holds at most ten accounts; the eleventh is refused in words and nothing is made.
func TestB617_EleventhAccountIsRefused(t *testing.T) {
	r, _, pool := b617Router(t)
	const who = "b617-ten-accounts"

	if rec, _ := doProvision(t, r, provisionSecret, `{"identity":"`+who+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	for i := 2; i <= 10; i++ {
		rec, _ := doProvision(t, r, provisionSecret, `{"identity":"`+who+`","mode":"open","display_name":"Account `+strconv.Itoa(i)+`","account_type":"personal","country":"GB"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("account %d = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	rec, out := doProvision(t, r, provisionSecret, `{"identity":"`+who+`","mode":"open","display_name":"Eleven","account_type":"personal","country":"GB"}`)
	if rec.Code != http.StatusConflict || out.Token != "" || !strings.Contains(rec.Body.String(), "10 accounts") {
		t.Fatalf("eleventh = %d: %s, want 409 naming the limit of 10 accounts", rec.Code, rec.Body.String())
	}
	if n := len(b617List(t, r, who).Accounts); n != 10 {
		t.Errorf("after the refusal the person holds %d accounts, want 10", n)
	}
	var named int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM workspaces WHERE name = 'Eleven'`).Scan(&named); err != nil || named != 0 {
		t.Errorf("a refused eleventh account left %d workspaces (err %v)", named, err)
	}
}
