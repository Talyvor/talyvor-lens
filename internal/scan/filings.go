package scan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── SEC EDGAR: Form 4 and 13F (B52.23) ──────────────────────────────────────────────────────────────────────────────

// edgarUserAgent is the declared User-Agent the SEC asks every automated client for, naming who to contact. Requests
// are paced by the Fetcher at sec-edgar's register limit of 600 a minute: the SEC's ten a second.
const edgarUserAgent = "Talyvor Scan hello@talyvor.com"

// A cluster is ClusterInsiders or more distinct insiders of one company each buying its shares on the open market
// (Form 4 transaction code P) within ClusterWindow of the first buy. A joint filing counts as one insider.
const (
	ClusterInsiders = 3
	ClusterWindow   = 14 * 24 * time.Hour
)

// filingsLookback is how many days of filings each run walks: the cluster window plus the two business days a Form 4
// may take, weekends and late filers. A filing already read by this replica is not fetched again.
const filingsLookback = ClusterWindow + 7*24*time.Hour

// EDGAR collects insider transactions (Form 4) and fund holdings (13F-HR) from the SEC's daily form index, and records
// every insider-buying cluster it finds as a derived figure listing the buys it came from.
type EDGAR struct {
	Base  string
	Store *Store
	seen  map[string]bool // accession → read by this replica
}

func (*EDGAR) Source() string  { return "sec-edgar" }
func (*EDGAR) Setting() string { return "" }

// Collect walks the daily form index for filingsLookback before now (the runner's since is the newest observation,
// which says nothing about filings published since), then finds clusters among the buys stored for that span.
func (e *EDGAR) Collect(ctx context.Context, f *Fetcher, _, now time.Time, put func(Observation) error) error {
	if e.seen == nil {
		e.seen = map[string]bool{}
	}
	base := or(e.Base, "https://www.sec.gov/Archives")
	from := now.Add(-filingsLookback)
	for d := from; !d.After(now); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		idx := fmt.Sprintf("%s/edgar/daily-index/%d/QTR%d/form.%s.idx", base, d.Year(), (int(d.Month())+2)/3, d.Format("20060102"))
		body, _, _, err := e.get(ctx, f, idx)
		if errors.Is(err, ErrNotPublished) { // a holiday, or today's index not out yet
			continue
		}
		if err != nil {
			return err
		}
		for _, l := range dailyIndex(body) {
			if (l.form != "4" && l.form != "13F-HR") || e.seen[l.accession()] {
				continue
			}
			filing, hash, at, err := e.get(ctx, f, base+"/"+l.file)
			if err != nil {
				return err
			}
			var points []Observation
			if l.form == "4" {
				points, err = Form4(filing)
			} else {
				points, err = Form13F(filing, l.cik)
			}
			if err != nil { // one filing that does not read must not stop every later run at the same place
				slog.Warn("scan: sec-edgar filing skipped", slog.String("accession", l.accession()), slog.String("err", err.Error()))
			}
			for _, p := range points {
				p.RetrievedAt, p.ContentHash = at, hash
				if err := put(p); err != nil {
					return err
				}
			}
			e.seen[l.accession()] = true
		}
	}
	return e.Store.RecordClusters(ctx, from.Add(-ClusterWindow))
}

func (e *EDGAR) get(ctx context.Context, f *Fetcher, url string) ([]byte, string, time.Time, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", edgarUserAgent)
	return f.Get(ctx, req)
}

type indexLine struct{ form, cik, file string }

func (l indexLine) accession() string { return strings.TrimSuffix(path.Base(l.file), ".txt") }

// dailyIndex reads EDGAR's form.YYYYMMDD.idx: after the dashed rule, each line is form type, company name, CIK, date
// filed and file name. A filing appears once per party (issuer and each reporting owner), under one accession.
func dailyIndex(b []byte) []indexLine {
	var out []indexLine
	rule := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if !rule {
			rule = len(f) == 1 && strings.HasPrefix(f[0], "-----")
			continue
		}
		if len(f) >= 5 {
			out = append(out, indexLine{form: f[0], cik: f[len(f)-3], file: f[len(f)-1]})
		}
	}
	return out
}

var xmlBlock = regexp.MustCompile(`(?s)<XML>\s*(.*?)\s*</XML>`)

// documents returns the XML documents of an EDGAR full submission (.txt).
func documents(filing []byte) [][]byte {
	var out [][]byte
	for _, m := range xmlBlock.FindAllSubmatch(filing, -1) {
		out = append(out, m[1])
	}
	return out
}

func cik(s string) string { return strings.TrimLeft(strings.TrimSpace(s), "0") }

type ownershipDocument struct {
	XMLName      xml.Name `xml:"ownershipDocument"`
	IssuerCIK    string   `xml:"issuer>issuerCik"`
	OwnerCIKs    []string `xml:"reportingOwner>reportingOwnerId>rptOwnerCik"`
	Transactions []struct {
		Date   string `xml:"transactionDate>value"`
		Code   string `xml:"transactionCoding>transactionCode"`
		Shares string `xml:"transactionAmounts>transactionShares>value"`
	} `xml:"nonDerivativeTable>nonDerivativeTransaction"`
}

// Form4 reads a Form 4's non-derivative transactions as one point per transaction code and day: series
// sec.form4.<issuer CIK>.<owner CIK>.<code>, the shares, summed when a filing has several such transactions in a day.
// A joint filing is put under its first reporting owner, so a fund and its affiliates count as one insider.
func Form4(filing []byte) ([]Observation, error) {
	docs := documents(filing)
	if len(docs) == 0 {
		return nil, errors.New("no ownership document")
	}
	var doc ownershipDocument
	if err := xml.Unmarshal(docs[0], &doc); err != nil {
		return nil, err
	}
	issuer := cik(doc.IssuerCIK)
	if issuer == "" || len(doc.OwnerCIKs) == 0 {
		return nil, errors.New("ownership document without its issuer or reporting owner")
	}
	owner := cik(doc.OwnerCIKs[0])
	type key struct{ code, day string }
	sum, count := map[key]float64{}, map[key]int{}
	var order []key
	for _, t := range doc.Transactions {
		shares, err := strconv.ParseFloat(strings.TrimSpace(t.Shares), 64)
		k := key{strings.TrimSpace(t.Code), strings.TrimSpace(t.Date)}
		if err != nil || k.code == "" {
			continue // no figure is never a zero
		}
		if count[k] == 0 {
			order = append(order, k)
		}
		sum[k] += shares
		count[k]++
	}
	var out []Observation
	for _, k := range order {
		at, err := time.Parse("2006-01-02", k.day[:min(len(k.day), 10)])
		if err != nil {
			continue
		}
		tr := []string{fmt.Sprintf("Form 4 nonDerivativeTransaction transactionShares with transactionCode %s on %s", k.code, k.day)}
		if count[k] > 1 {
			tr = append(tr, fmt.Sprintf("sum of %d transactions", count[k]))
		}
		if len(doc.OwnerCIKs) > 1 {
			tr = append(tr, fmt.Sprintf("joint filing of %d reporting owners, put under the first", len(doc.OwnerCIKs)))
		}
		out = append(out, Observation{SourceID: "sec-edgar", Series: fmt.Sprintf("sec.form4.%s.%s.%s", issuer, owner, k.code),
			ObservedAt: at, Value: sum[k], Unit: "shares", Transforms: tr})
	}
	return out, nil
}

// Form13F reads a 13F-HR's information table as one point per security: series sec.13f.<filer CIK>.<CUSIP>, the
// shares held at the end of the quarter reported. Principal amounts and options (put or call) are not shares held and
// are left out.
func Form13F(filing []byte, filer string) ([]Observation, error) {
	var period time.Time
	sum, count := map[string]float64{}, map[string]int{}
	var order []string
	for _, d := range documents(filing) {
		var doc struct {
			XMLName xml.Name
			Period  string `xml:"headerData>filerInfo>periodOfReport"`
			Rows    []struct {
				CUSIP   string `xml:"cusip"`
				Amount  string `xml:"shrsOrPrnAmt>sshPrnamt"`
				Type    string `xml:"shrsOrPrnAmt>sshPrnamtType"`
				PutCall string `xml:"putCall"`
			} `xml:"infoTable"`
		}
		if err := xml.Unmarshal(d, &doc); err != nil {
			return nil, err
		}
		switch doc.XMLName.Local {
		case "edgarSubmission":
			p, err := time.Parse("01-02-2006", strings.TrimSpace(doc.Period))
			if err != nil {
				return nil, fmt.Errorf("13F period of report %q: %w", doc.Period, err)
			}
			period = p
		case "informationTable":
			for _, r := range doc.Rows {
				shares, err := strconv.ParseFloat(strings.TrimSpace(r.Amount), 64)
				c := strings.ToUpper(strings.TrimSpace(r.CUSIP))
				if err != nil || c == "" || strings.TrimSpace(r.Type) != "SH" || strings.TrimSpace(r.PutCall) != "" {
					continue
				}
				if count[c] == 0 {
					order = append(order, c)
				}
				sum[c] += shares
				count[c]++
			}
		}
	}
	if period.IsZero() {
		return nil, errors.New("13F without its period of report")
	}
	var out []Observation
	for _, c := range order {
		tr := []string{"13F informationTable sshPrnamt of rows with sshPrnamtType SH and no putCall, CUSIP " + c}
		if count[c] > 1 {
			tr = append(tr, fmt.Sprintf("sum of %d rows", count[c]))
		}
		out = append(out, Observation{SourceID: "sec-edgar", Series: fmt.Sprintf("sec.13f.%s.%s", cik(filer), c),
			ObservedAt: period, Value: sum[c], Unit: "shares", Transforms: tr})
	}
	return out, nil
}

// InsiderBuy is one stored open-market purchase: an insider's code-P shares on a day.
type InsiderBuy struct {
	Point         int64
	Issuer, Owner string
	At            time.Time
}

// Cluster is ClusterInsiders or more distinct insiders of one issuer buying within ClusterWindow of the first buy.
type Cluster struct {
	Issuer      string
	First, Last time.Time
	Insiders    int
	Buys        []int64
}

// FindClusters finds every cluster among buys. Clusters of one issuer do not overlap: a buy belongs to the first
// cluster whose window holds it.
func FindClusters(buys []InsiderBuy) []Cluster {
	by := map[string][]InsiderBuy{}
	var issuers []string
	for _, b := range buys {
		if by[b.Issuer] == nil {
			issuers = append(issuers, b.Issuer)
		}
		by[b.Issuer] = append(by[b.Issuer], b)
	}
	sort.Strings(issuers)
	var out []Cluster
	for _, issuer := range issuers {
		bs := by[issuer]
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].At.Before(bs[j].At) })
		for i := 0; i < len(bs); {
			j, owners := i, map[string]bool{}
			for ; j < len(bs) && bs[j].At.Sub(bs[i].At) <= ClusterWindow; j++ {
				owners[bs[j].Owner] = true
			}
			if len(owners) < ClusterInsiders {
				i++
				continue
			}
			c := Cluster{Issuer: issuer, First: bs[i].At, Last: bs[j-1].At, Insiders: len(owners)}
			for _, b := range bs[i:j] {
				c.Buys = append(c.Buys, b.Point)
			}
			out = append(out, c)
			i = j
		}
	}
	return out
}

// RecordClusters finds the clusters among the buys stored since `since` and records each one not yet recorded as a
// derived figure: series sec.form4.cluster.<issuer CIK>, at the first buy, the number of insiders, its buys as inputs.
// A cluster that grows is recorded again with its new count.
func (s *Store) RecordClusters(ctx context.Context, since time.Time) error {
	rows, err := s.pool.Query(ctx, `SELECT id, series, observed_at FROM scan_points WHERE kind = 'observed'
		AND source_id = 'sec-edgar' AND series LIKE 'sec.form4.%.P' AND observed_at >= $1`, since)
	if err != nil {
		return err
	}
	var buys []InsiderBuy
	for rows.Next() {
		var b InsiderBuy
		var series string
		if err := rows.Scan(&b.Point, &series, &b.At); err != nil {
			rows.Close()
			return err
		}
		if f := strings.Split(series, "."); len(f) == 5 {
			b.Issuer, b.Owner = f[2], f[3]
			buys = append(buys, b)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range FindClusters(buys) {
		series := "sec.form4.cluster." + c.Issuer
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scan_points WHERE kind = 'derived' AND series = $1
			AND observed_at = $2 AND value = $3)`, series, c.First, float64(c.Insiders)).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := s.Derive(ctx, Derivation{Series: series, ObservedAt: c.First, Value: float64(c.Insiders), Unit: "insiders",
			Transforms: []string{fmt.Sprintf("distinct insiders buying on the open market (Form 4 code P) from %s to %s, "+
				"within %d days of the first buy; a cluster is %d or more", day(c.First), day(c.Last),
				int(ClusterWindow.Hours()/24), ClusterInsiders)},
			Inputs: c.Buys}); err != nil {
			return err
		}
	}
	return nil
}
