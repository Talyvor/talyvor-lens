package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/ratelimit"
)

// b5441Bank confirms every deal it is asked to and counts them.
type b5441Bank struct {
	negotiationBank
	confirmed []string
}

func (b *b5441Bank) ConfirmNegotiationDeal(_ context.Context, _, domain, _, _, _ string) error {
	b.confirmed = append(b.confirmed, domain)
	return nil
}

// B54.41 — the public inbound email route makes DNS lookups for domains the sender chooses, so it counts every
// request against the public limiter, per client IP and per signing domain, before any lookup: over either it answers
// 429 with Retry-After and looks nothing up; under both a DKIM-signed email still confirms its deal.
func TestB5441_InboundDealEmailIsRateLimitedBeforeAnyDNSLookup(t *testing.T) {
	mr := miniredis.RunT(t)
	limiter := ratelimit.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}), []ratelimit.RateRule{{RequestsPerMinute: 2}})

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	lookups := 0
	lookup := func(_ context.Context, name string) ([]string, error) {
		lookups++
		return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
	}
	bank := &b5441Bank{}
	r := chi.NewRouter()
	mountNegotiationEmailRoutes(r, limiter, lookup, bank)

	raw := "From: Acme Sales <sales@acme.example>\r\nTo: counter@talyvor.example\r\n" +
		"Subject: Confirm RFQ-ACME-9 for negotiation 0b6c1a52-3f1e-4c55-9d1e-6a1f0e2b7c90\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nYes, that's our RFQ.\r\n"
	var signed bytes.Buffer
	if err := dkim.Sign(&signed, strings.NewReader(raw), &dkim.SignOptions{Domain: "mail.acme.example", Selector: "s1",
		Signer: priv, HeaderKeys: []string{"From", "To", "Subject", "Date"}}); err != nil {
		t.Fatal(err)
	}
	deliver := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/inbound/negotiations/deal", strings.NewReader(signed.String()))
		req.RemoteAddr = ip + ":40000"
		req.Header.Set("Content-Type", "message/rfc822")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Under the limit, Acme's signed email confirms its deal.
	for i := 0; i < 2; i++ {
		if w := deliver("198.51.100.7"); w.Code != http.StatusOK {
			t.Fatalf("signed email %d under the limit = %d %s, want 200", i+1, w.Code, w.Body.String())
		}
	}
	if len(bank.confirmed) != 2 || bank.confirmed[0] != "mail.acme.example" {
		t.Fatalf("confirmed = %v, want mail.acme.example twice", bank.confirmed)
	}

	// Over it from the same address: 429 with Retry-After, and no lookup.
	before := lookups
	w := deliver("198.51.100.7")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("third email from one address = %d (Retry-After %q), want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	// And from a new address signed by the same domain: that domain's DNS has been asked enough this minute.
	w = deliver("203.0.113.9")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("third email signed by mail.acme.example, from another address = %d (Retry-After %q), want 429", w.Code, w.Header().Get("Retry-After"))
	}
	if lookups != before {
		t.Errorf("rate-limited emails made %d DNS lookups, want 0", lookups-before)
	}
	if len(bank.confirmed) != 2 {
		t.Errorf("confirmed = %v after the limit, want no more", bank.confirmed)
	}
}
