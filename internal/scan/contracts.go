package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ── USAspending contract awards (B52.23) ────────────────────────────────────────────────────────────────────────────

// Listed is a listed company and the contract recipients that are certainly it.
type Listed struct {
	Search, Ticker, CIK string
	// UEIs are recipients checked by hand to be this company: the parent-level USAspending recipient whose legal name
	// is the SEC registrant's. A subsidiary, or anything that only looks like the name, is not here and stays unmatched.
	UEIs []string
}

// ListedRecipients are the companies whose contract awards are collected. Add one only with its UEIs checked.
var ListedRecipients = []Listed{
	// USAspending: XFJMYSYFJEK4 is LOCKHEED MARTIN CORPORATION at parent level (P); SEC: CIK 936468, LOCKHEED MARTIN CORP.
	{Search: "LOCKHEED MARTIN", Ticker: "LMT", CIK: "936468", UEIs: []string{"XFJMYSYFJEK4"}},
}

// contractsLookback is the span of action dates each run asks for: the Defense Department publishes its awards 90
// days late, so a shorter window would never see them.
const contractsLookback = 100 * 24 * time.Hour

// USAspending collects contract transactions (award types A to D) of ListedRecipients by name search, one point per
// award and action date: series usaspending.contracts.listed.<CIK>.<award> when the recipient's UEI is one of the
// company's checked UEIs, otherwise usaspending.contracts.unmatched.<UEI>.<award> — the name search also returns
// subsidiaries and namesakes, and none of them is matched on its name.
type USAspending struct {
	Base   string
	Listed []Listed // ListedRecipients when nil
}

func (*USAspending) Source() string  { return "usaspending" }
func (*USAspending) Setting() string { return "" }

type usaTransaction struct {
	ID        json.Number `json:"internal_id"`
	Award     string      `json:"generated_internal_id"`
	Recipient string      `json:"Recipient Name"`
	UEI       string      `json:"Recipient UEI"`
	Date      string      `json:"Action Date"`
	Amount    json.Number `json:"Transaction Amount"`
}

func (u *USAspending) Collect(ctx context.Context, f *Fetcher, _, now time.Time, put func(Observation) error) error {
	listed := u.Listed
	if listed == nil {
		listed = ListedRecipients
	}
	type key struct{ series, day string }
	type point struct {
		Observation
		n int
	}
	seen := map[string]bool{}
	for _, l := range listed {
		certain := map[string]bool{}
		for _, uei := range l.UEIs {
			certain[uei] = true
		}
		for page := 1; ; page++ {
			body, _ := json.Marshal(map[string]any{
				"filters": map[string]any{"award_type_codes": []string{"A", "B", "C", "D"}, "recipient_search_text": []string{l.Search},
					"time_period": []map[string]string{{"start_date": day(now.Add(-contractsLookback)), "end_date": day(now)}}},
				"fields": []string{"Award ID", "Recipient Name", "Recipient UEI", "Action Date", "Transaction Amount", "Awarding Agency"},
				"sort":   "Action Date", "order": "desc", "limit": 100, "page": page,
			})
			req, _ := http.NewRequest(http.MethodPost, or(u.Base, "https://api.usaspending.gov")+"/api/v2/search/spending_by_transaction/", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			b, hash, at, err := f.Get(ctx, req)
			if err != nil {
				return err
			}
			var resp struct {
				Results []usaTransaction `json:"results"`
				Page    struct {
					HasNext bool `json:"hasNext"`
				} `json:"page_metadata"`
			}
			if err := decodeJSON(b, &resp); err != nil {
				return fmt.Errorf("usaspending: %w", err)
			}
			// Summed within a page, so each point's content hash is the page it was read from. shortcut: an award's
			// transactions of one day split across two pages keep only the first page's sum; key points by transaction if it matters.
			points := map[key]*point{}
			var order []key
			for _, t := range resp.Results {
				amount, err := t.Amount.Float64()
				observed, derr := time.Parse("2006-01-02", t.Date)
				if seen[t.ID.String()] || err != nil || derr != nil || t.UEI == "" || t.Award == "" {
					continue // a transaction with no recipient UEI has nothing it could be matched on
				}
				seen[t.ID.String()] = true
				series, match := "usaspending.contracts.unmatched."+t.UEI+"."+t.Award,
					fmt.Sprintf("recipient %s (UEI %s) is not a checked UEI of a listed company: left unmatched", t.Recipient, t.UEI)
				if certain[t.UEI] {
					series, match = "usaspending.contracts.listed."+l.CIK+"."+t.Award,
						fmt.Sprintf("recipient UEI %s is %s (SEC CIK %s) by the checked register", t.UEI, l.Ticker, l.CIK)
				}
				k := key{series, t.Date}
				p := points[k]
				if p == nil {
					p = &point{Observation: Observation{SourceID: "usaspending", Series: series, ObservedAt: observed, Unit: "USD",
						RetrievedAt: at, ContentHash: hash}}
					p.Transforms = []string{"", match}
					points[k] = p
					order = append(order, k)
				}
				p.Value += amount
				p.n++
				p.Transforms[0] = fmt.Sprintf("sum of Transaction Amount of %d contract transaction(s) of award %s with action date %s", p.n, t.Award, t.Date)
			}
			for _, k := range order {
				if err := put(points[k].Observation); err != nil {
					return err
				}
			}
			if !resp.Page.HasNext {
				break
			}
		}
	}
	return nil
}
