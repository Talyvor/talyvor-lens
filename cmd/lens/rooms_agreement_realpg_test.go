package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
	"github.com/talyvor/lens/internal/rooms"
)

// B41.150 (B41.50's DONE line, from Lens alone) — a contributor who has not signed the contributor agreement earns no
// points; after signing, their next accepted contribution earns points and the signature is stored with its text version.
func TestRooms_ContributorAgreementSignedBeforePointsCount(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const owner, author = "ws-b41150-owner", "ws-b41150-author"
	for _, ws := range []string{owner, author} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic) VALUES ($1, $1, $1, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	marketStore := market.NewStore(pool)
	store := rooms.NewStore(pool, 3000)
	store.SetMarket(marketStore)
	r := chi.NewRouter()
	mountMarketRoutes(r, marketStore)
	mountRoomRoutes(r, store)
	mountRoomOwnershipRoutes(r, store)
	do := func(want int, v any, ws, method, path, body string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(auth.WithAuthContext(req.Context(),
			&auth.AuthContext{WorkspaceID: ws, AuthMethod: auth.MethodJWT, UserID: "user-" + ws, Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want || (v != nil && json.Unmarshal(w.Body.Bytes(), v) != nil) {
			t.Fatalf("%s %s as %s = %d %s, want %d", method, path, ws, w.Code, w.Body.String(), want)
		}
	}
	var room rooms.Detail
	do(http.StatusCreated, &room, owner, http.MethodPost, "/v1/workspaces/"+owner+"/rooms", `{"title":"Signed build"}`)
	base := "/v1/rooms/" + room.ID
	do(http.StatusCreated, nil, author, http.MethodPost, base+"/join", `{"terms_version":1}`)
	do(http.StatusOK, nil, owner, http.MethodPut, base+"/rate-card", `{"pool_points":10000,"sizes":[{"size":"M","hours":8,"points_per_hour":50}]}`)
	contributeAccepted := func(title string) rooms.Contribution {
		t.Helper()
		var c rooms.Contribution
		do(http.StatusCreated, &c, author, http.MethodPost, base+"/contributions", `{"kind":"prompt","title":"`+title+`","artifact":{"template":"Hi."}}`)
		do(http.StatusOK, nil, author, http.MethodPut, base+"/contributions/"+c.ID+"/size", `{"size":"M"}`)
		do(http.StatusOK, nil, owner, http.MethodPatch, base+"/contributions/"+c.ID, `{"status":"accepted"}`)
		return c
	}
	earned := func(c rooms.Contribution) (rows int, points int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(points), 0)::bigint FROM room_points WHERE contribution_id = $1 AND workspace_id = $2`,
			c.ID, author).Scan(&rows, &points); err != nil {
			t.Fatal(err)
		}
		return rows, points
	}

	// The draft reads as the draft it is, and the author has not signed it.
	var a rooms.Agreement
	do(http.StatusOK, &a, author, http.MethodGet, base+"/agreement", "")
	sum := sha256.Sum256([]byte(a.Text))
	if a.Version != rooms.AgreementVersion || !a.Draft || a.Marked != "Test rooms only" || a.TextSHA256 != hex.EncodeToString(sum[:]) ||
		!strings.Contains(a.Text, "assign") || !strings.Contains(a.Text, "licence") || a.Signature != nil {
		t.Fatalf("the agreement reads %+v, want the unsigned draft marked Test rooms only, with an assignment and a licence back", a)
	}

	// Unsigned, an accepted contribution earns no points.
	first := contributeAccepted("Before signing")
	if rows, _ := earned(first); rows != 0 {
		t.Fatalf("the unsigned author has %d points rows for an accepted contribution, want none", rows)
	}

	// A signature for any text but the current one is refused, and so is one without a name.
	do(http.StatusConflict, nil, author, http.MethodPost, base+"/agreement", `{"version":"draft-0","signed_name":"Ada Author"}`)
	do(http.StatusBadRequest, nil, author, http.MethodPost, base+"/agreement", `{"version":"`+a.Version+`","signed_name":"  "}`)
	do(http.StatusCreated, &a, author, http.MethodPost, base+"/agreement", `{"version":"`+a.Version+`","signed_name":"Ada Author"}`)
	if s := a.Signature; s == nil || s.SignedName != "Ada Author" || s.UserID != "user-"+author || s.Version != rooms.AgreementVersion ||
		s.TextSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("the signature is %+v, want Ada Author's, by user-%s, on %s with the text's hash", a.Signature, author, rooms.AgreementVersion)
	}
	do(http.StatusOK, nil, author, http.MethodPost, base+"/agreement", `{"version":"`+a.Version+`","signed_name":"Ada Author"}`)

	// The signature is kept as evidence with its text version and cannot be changed.
	var version, hash string
	if err := pool.QueryRow(ctx, `SELECT agreement_version, text_sha256 FROM room_agreement_signatures WHERE room_id = $1 AND workspace_id = $2`,
		room.ID, author).Scan(&version, &hash); err != nil || version != rooms.AgreementVersion || hash != a.TextSHA256 {
		t.Fatalf("the stored signature is %q %q (%v), want %s with the text's hash", version, hash, err, rooms.AgreementVersion)
	}
	if _, err := pool.Exec(ctx, `UPDATE room_agreement_signatures SET signed_name = 'Someone else' WHERE room_id = $1`, room.ID); err == nil {
		t.Fatal("a signature was changed: signatures must be append-only")
	}

	// Signed, the author's next accepted contribution earns its M size, 400 points.
	if rows, points := earned(contributeAccepted("After signing")); rows != 1 || points != 400 {
		t.Fatalf("the signed author has %d rows worth %d for an accepted M contribution, want one row of 400", rows, points)
	}

	// Anyone who is not a member neither reads nor signs it.
	do(http.StatusNotFound, nil, "ws-b41150-stranger", http.MethodGet, base+"/agreement", "")
	do(http.StatusNotFound, nil, "ws-b41150-stranger", http.MethodPost, base+"/agreement", `{"version":"`+a.Version+`","signed_name":"X"}`)
}
