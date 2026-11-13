package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/ecbrate"
)

// B59.49 — on the migrated schema, the ECB reference rates the status page reads: none held and a failed fetch's
// error while the ECB answers 503; then, once the ECB's file is stored, its day as the latest held, the fetch's time,
// and no error.
func TestB5949_ECBRatesHealth(t *testing.T) {
	pool := agentRoutesDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	var up atomic.Bool
	ecb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
<Cube><Cube time="%s"><Cube currency="USD" rate="1.0800"/><Cube currency="GBP" rate="0.8600"/></Cube></Cube></gesmes:Envelope>`, today)
	}))
	defer ecb.Close()
	book := ecbrate.New(pool, ecb.URL)

	if err := book.Refresh(ctx); err == nil {
		t.Fatal("a 503 refreshed the rates")
	}
	h, err := book.Health(ctx)
	if err != nil || h.LatestDay != nil || h.FetchedAt != nil || h.LastError != "the ECB answered 503 Service Unavailable" {
		t.Fatalf("after a failed fetch with none held: %+v, %v", h, err)
	}

	up.Store(true)
	before := time.Now().UTC()
	if err := book.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	h, err = book.Health(ctx)
	if err != nil || h.LatestDay == nil || h.LatestDay.Format("2006-01-02") != today || h.FetchedAt == nil ||
		h.FetchedAt.Before(before) || h.LastError != "" {
		t.Fatalf("after a stored fetch: %+v, %v", h, err)
	}
}
