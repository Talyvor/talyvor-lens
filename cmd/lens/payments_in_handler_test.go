package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/partners"
)

type fakePaymentsIn struct{ got []economy.InboundPayment }

func (f *fakePaymentsIn) ReceivePayment(_ context.Context, in economy.InboundPayment) (economy.MoneyEntry, error) {
	f.got = append(f.got, in)
	return economy.MoneyEntry{ID: "mle_1"}, nil
}
func (f *fakePaymentsIn) SuspenseItems(context.Context, int) ([]economy.SuspenseItem, error) {
	return nil, nil
}
func (f *fakePaymentsIn) AssignSuspense(context.Context, string, string, string) (economy.MoneyEntry, error) {
	return economy.MoneyEntry{}, nil
}

// B30.15 — the payments-in webhook posts only what the partner signed, and money the Test partner reports is test
// money whatever the body says.
func TestPaymentsInWebhook_OnlyASignedNoticePostsAndTheTestPartnersIsTestMoney(t *testing.T) {
	store := &fakePaymentsIn{}
	h := newPaymentsInWebhook("whsec_b3015", store, partners.NewRegistry(nil))
	body := `{"partner_account_ref":"acct_1","partner_ref":"in_1","payer":"Acme Ltd","reference":"INV 7","amount_minor":12000,"currency":"GBP","funding":"live"}`
	mac := hmac.New(sha256.New, []byte("whsec_b3015"))
	mac.Write([]byte(body))
	for _, c := range []struct {
		sig  string
		want int
	}{{"", http.StatusUnauthorized}, {hex.EncodeToString(mac.Sum([]byte("x"))), http.StatusUnauthorized}, {hex.EncodeToString(mac.Sum(nil)), http.StatusOK}} {
		req := httptest.NewRequest(http.MethodPost, "/v1/money/payments-in/webhook", strings.NewReader(body))
		req.Header.Set("X-Partner-Signature", c.sig)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("signature %q = %d %s; want %d", c.sig, w.Code, w.Body.String(), c.want)
		}
	}
	if len(store.got) != 1 || store.got[0].Funding != economy.FundingTest || store.got[0].AmountMinor != 12000 {
		t.Fatalf("posted %+v; want the one signed £120.00, as test money", store.got)
	}
}
