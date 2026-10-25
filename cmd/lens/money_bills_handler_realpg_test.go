package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/screening"
)

// invoicePDF is a one-page PDF with a text layer, one line per entry.
func invoicePDF(lines ...string) []byte {
	var content strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&content, "BT /F1 12 Tf 72 %d Td (%s) Tj ET\n", 740-18*i, l)
	}
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return buf.Bytes()
}

// B41.101 — an uploaded PDF invoice becomes one bill with its supplier, amount, due date and invoice number, linked to
// the saved payee whose bank details it prints; a supplier with no saved payee is flagged new_payee. The same invoice
// uploaded twice, or photographed after it was uploaded, is still one bill.
func TestMoneyBillRoutes_AnUploadedInvoiceBecomesOneBillLinkedToItsPayee(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	const ws = "ws-b41101-bills"
	registry := partners.NewRegistry(nil)
	store := economy.NewDualTokenStore(nil, pool, nil)
	store.SetAccountPartners(registry)
	store.SetPayeeScreener(screening.NewScreener(pool, registry))
	payee, err := store.CreateOutsidePayee(ctx, ws, economy.OutsidePayee{Name: "Northwind Supplies Ltd", Country: "GB",
		SortCode: "200000", AccountNumber: "55779911"})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	mountMoneyBillRoutes(r, store, inProcessConverter{})
	call := func(workspace, method, path, contentType string, body []byte) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: workspace, AuthMethod: auth.MethodJWT,
			UserID: "owner", Scopes: []string{auth.ScopeKeys}}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	northwind := invoicePDF("Northwind Supplies Ltd", "INVOICE", "Invoice number: INV-2026-0042", "Invoice date: 1 October 2026",
		"Due date: 31 October 2026", "Subtotal: GBP 1,000.00", "VAT: GBP 200.00", "Total due: GBP 1,200.00",
		"Sort code: 20-00-00", "Account number: 55779911")
	code, bill := call(ws, http.MethodPost, "/v1/money/bills?filename=inv-0042.pdf", "application/pdf", northwind)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d %v, want 201", code, bill)
	}
	for k, want := range map[string]any{"supplier": "Northwind Supplies Ltd", "invoice_number": "INV-2026-0042", "amount_minor": 120000.0,
		"currency": "GBP", "due_date": "2026-10-31", "payee_id": payee.ID, "payee_match": "matched", "status": "due", "source": "upload"} {
		if bill[k] != want {
			t.Errorf("bill %s = %v, want %v (bill %v)", k, bill[k], want, bill)
		}
	}
	if m, _ := bill["missing"].([]any); len(m) != 0 {
		t.Errorf("bill missing %v, want nothing", m)
	}

	if code, again := call(ws, http.MethodPost, "/v1/money/bills", "application/pdf", northwind); code != http.StatusOK || again["id"] != bill["id"] {
		t.Errorf("the same PDF uploaded again = %d %v, want 200 with bill %v", code, again["id"], bill["id"])
	}
	photo := "Northwind Supplies Ltd\nInvoice No. INV-2026-0042\nPayment due 31/10/2026\nTotal due £1,200.00\n"
	if code, again := call(ws, http.MethodPost, "/v1/money/bills?source=photo", "text/plain", []byte(photo)); code != http.StatusOK || again["id"] != bill["id"] {
		t.Errorf("a photo of the same invoice = %d %v, want 200 with bill %v", code, again["id"], bill["id"])
	}

	fabrikam := invoicePDF("Fabrikam Design GmbH", "Rechnung / Invoice", "Invoice #: FD-981", "Due by 15 November 2026",
		"Total: EUR 480.50", "IBAN: DE89 3704 0044 0532 0130 00", "BIC: COBADEFFXXX")
	code, other := call(ws, http.MethodPost, "/v1/money/bills", "application/pdf", fabrikam)
	if code != http.StatusCreated || other["payee_match"] != "new_payee" || other["payee_id"] != nil || other["amount_minor"] != 48050.0 ||
		other["currency"] != "EUR" || other["iban"] != "DE89370400440532013000" || other["due_date"] != "2026-11-15" {
		t.Errorf("an invoice from a new supplier = %d %v, want 201 flagged new_payee with its IBAN", code, other)
	}

	if code, list := call(ws, http.MethodGet, "/v1/money/bills", "", nil); code != http.StatusOK || len(list["bills"].([]any)) != 2 {
		t.Errorf("bills = %d %v, want the two bills", code, list)
	}
	if code, _ := call("ws-b41101-other", http.MethodGet, "/v1/money/bills/"+bill["id"].(string), "", nil); code != http.StatusNotFound {
		t.Errorf("another workspace reading the bill = %d, want 404", code)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM money_bills WHERE workspace_id = $1`, ws).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("money_bills holds %d rows (%v), want 2", rows, err)
	}
}
