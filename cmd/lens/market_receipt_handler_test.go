package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/brand"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/market"
)

type fakeReceipts struct{ r market.Receipt }

func (f fakeReceipts) Receipt(_ context.Context, ws, id string) (market.Receipt, error) {
	if ws != f.r.BuyerWorkspaceID || id != f.r.ID {
		return market.Receipt{}, market.ErrNoReceipt
	}
	return f.r, nil
}

func (f fakeReceipts) ReceiptsOf(context.Context, string) ([]market.ReceiptSummary, error) {
	return []market.ReceiptSummary{{ID: f.r.ID, Number: f.r.Number}}, nil
}

// B32.40 — the workspace's owner reads a receipt as JSON, as its page (?format=html) and as its PDF (Accept:
// application/pdf); another workspace's receipt is 404, and an agent's key — not the owner — is 403.
func TestMarketReceiptRoutes_AnswerJSONHTMLAndPDF(t *testing.T) {
	r := chi.NewRouter()
	rc := market.Receipt{ID: "rcpt_1", Number: "2026-000001", BuyerWorkspaceID: "ws-1", InvoiceID: "in_1", IssuedAt: time.Now(), PaidAt: time.Now(),
		Supplier: market.Supplier{LegalName: "TALYVOR LTD"}, Lines: []market.ReceiptLine{{Description: "Adder — rental", NetUSDMicros: 10_000_000,
			RateBps: 2000, TaxUSDMicros: 2_000_000}}, NetUSDMicros: 10_000_000, TaxUSDMicros: 2_000_000, GrossUSDMicros: 12_000_000}
	mountMarketReceiptRoutes(r, fakeReceipts{rc})
	owner := &auth.AuthContext{WorkspaceID: "ws-1", AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}
	caller := owner
	get := func(path, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), caller))
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	const one = "/v1/workspaces/ws-1/marketplace/receipts/rcpt_1"
	if w := get(one, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"number":"2026-000001"`) {
		t.Fatalf("JSON = %d %s", w.Code, w.Body.String())
	}
	if w := get(one+"?format=html", ""); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(w.Body.String(), "$12.00") {
		t.Fatalf("HTML = %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get(one, "application/pdf"); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/pdf" ||
		!bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("PDF = %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get("/v1/workspaces/ws-2/marketplace/receipts/rcpt_1", ""); w.Code != http.StatusNotFound {
		t.Fatalf("another workspace's receipt = %d; want 404", w.Code)
	}
	if w := get("/v1/workspaces/ws-1/marketplace/receipts", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "rcpt_1") {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	caller = &auth.AuthContext{WorkspaceID: "ws-1", AuthMethod: auth.MethodWorkspaceKey, APIKeyID: "agent"}
	if w := get(one, ""); w.Code != http.StatusForbidden {
		t.Fatalf("an agent's key reading a receipt = %d; want 403", w.Code)
	}
}

// B57.14 — a receipt's page and its PDF are in Pocket's colours, with none of today's, and the page keeps the policy
// it had before.
func TestMarketReceipt_WearsPocket(t *testing.T) {
	r := chi.NewRouter()
	rc := market.Receipt{ID: "rcpt_1", Number: "2026-000001", BuyerWorkspaceID: "ws-1", IssuedAt: time.Now(), PaidAt: time.Now(),
		Supplier: market.Supplier{LegalName: "TALYVOR LTD"}, Preview: true, PreviewReason: "Sandbox",
		Lines: []market.ReceiptLine{{Description: "Adder — rental", NetUSDMicros: 10_000_000}}, NetUSDMicros: 10_000_000, GrossUSDMicros: 10_000_000}
	mountMarketReceiptRoutes(r, fakeReceipts{rc})
	get := func(q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/marketplace/receipts/rcpt_1?format="+q, nil)
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "ws-1", AuthMethod: auth.MethodJWT, UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	page := get("html")
	if got, want := page.Header().Get("Content-Security-Policy"), "default-src 'none'; style-src 'unsafe-inline'; img-src data:"; got != want {
		t.Errorf("receipt page policy = %q, want %q", got, want)
	}
	// The mark's own file keeps its colours until B57.81 replaces it; everything else on the receipt is Pocket's.
	body := strings.ToUpper(strings.Replace(page.Body.String(), brand.Mark, "", 1))
	for _, want := range []string{"#111216", "#FF7A1A", "#0F1013", `"FIGTREE"`} {
		if !strings.Contains(body, want) {
			t.Errorf("receipt page is missing Pocket's %s", want)
		}
	}
	for _, old := range []string{"#060A12", "#0F7A6C"} {
		if strings.Contains(body, old) {
			t.Errorf("the receipt page still carries today's %s", old)
		}
	}
	// The PDF's text and rules; today's ink stays only in the mark's own paths, which B57.81 replaces.
	pdf := get("pdf").Body.String()
	for old, colour := range map[string]string{"#060A12": "Tf 0.024 0.039 0.071 rg", "#0F7A6C": "0.059 0.478 0.424", "#8A6A12": "0.541 0.416 0.071"} {
		if strings.Contains(pdf, colour) {
			t.Errorf("the receipt PDF still draws today's %s", old)
		}
	}
	if !strings.Contains(pdf, "0.067 0.071 0.086 rg") { // Pocket ink, #111216
		t.Error("the receipt PDF is not drawn in Pocket's ink")
	}
}
