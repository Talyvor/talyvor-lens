// Package ecbrate keeps the European Central Bank's euro foreign exchange reference rates (B19.12).
//
// The ECB publishes, around 16:00 CET on each TARGET working day, how many units of each currency one
// euro buys. Lens fetches the daily file, keeps every day it has seen in ecb_reference_rates (migration
// 0156), and prices a card authorisation at "the day's" rate: the latest one published on or before its
// date. Any currency's USD value follows from two of them: USD per unit = (USD per EUR) ÷ (unit per EUR).
package ecbrate

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DailyURL is the ECB's file of the latest reference rates.
const DailyURL = "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml"

// ErrNoRate: no reference rate for the currency on or before the date.
var ErrNoRate = errors.New("ecbrate: no ECB reference rate for this currency on or before this date")

// Day is one day's published rates: each currency per 1 EUR, as the decimal the ECB printed.
type Day struct {
	Date   time.Time
	PerEUR map[string]string // upper-case ISO code → decimal string
}

type envelope struct {
	Cube struct {
		Days []struct {
			Time  string `xml:"time,attr"`
			Rates []struct {
				Currency string `xml:"currency,attr"`
				Rate     string `xml:"rate,attr"`
			} `xml:"Cube"`
		} `xml:"Cube"`
	} `xml:"Cube"`
}

// Parse reads the ECB's eurofxref XML (the daily file, or the 90-day one) into its days.
func Parse(r io.Reader) ([]Day, error) {
	var env envelope
	if err := xml.NewDecoder(r).Decode(&env); err != nil {
		return nil, fmt.Errorf("ecbrate: parse: %w", err)
	}
	var days []Day
	for _, d := range env.Cube.Days {
		date, err := time.Parse("2006-01-02", d.Time)
		if err != nil {
			return nil, fmt.Errorf("ecbrate: date %q: %w", d.Time, err)
		}
		day := Day{Date: date, PerEUR: map[string]string{}}
		for _, r := range d.Rates {
			rat, ok := new(big.Rat).SetString(r.Rate)
			if !ok || rat.Sign() <= 0 {
				return nil, fmt.Errorf("ecbrate: %s rate %q is not a positive decimal", r.Currency, r.Rate)
			}
			day.PerEUR[strings.ToUpper(r.Currency)] = r.Rate
		}
		days = append(days, day)
	}
	if len(days) == 0 {
		return nil, errors.New("ecbrate: the file holds no rates")
	}
	return days, nil
}

// Book keeps the rates in Postgres and fetches them from the ECB.
type Book struct {
	pool   *pgxpool.Pool
	client *http.Client
	url    string
}

// New keeps rates in pool, fetched from url (DailyURL in production).
func New(pool *pgxpool.Pool, url string) *Book {
	return &Book{pool: pool, client: &http.Client{Timeout: 10 * time.Second}, url: url}
}

// Refresh fetches the ECB's file and stores every day in it; a day already stored is left as it is.
func (b *Book) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url, nil)
	if err != nil {
		return err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("ecbrate: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ecbrate: fetch: %s", resp.Status)
	}
	days, err := Parse(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	for _, d := range days {
		for ccy, rate := range d.PerEUR {
			if _, err := b.pool.Exec(ctx, `INSERT INTO ecb_reference_rates (rate_date, currency, per_eur) VALUES ($1, $2, $3::numeric)
				ON CONFLICT (rate_date, currency) DO NOTHING`, d.Date, ccy, rate); err != nil {
				return fmt.Errorf("ecbrate: store: %w", err)
			}
		}
	}
	return nil
}

// Conversion is an amount's USD value at the day's reference rate, and the rate it was priced at.
type Conversion struct {
	RateDate       *time.Time // nil for USD, which needs no rate
	USDPerEUR      string     // as the ECB printed it; "" for USD
	CurrencyPerEUR string     // "1" for EUR; "" for USD
	USDMicros      int64      // rounded up to the micro-dollar
}

// perEUR is the rate of ccy on the latest date on or before `on` that has one.
func (b *Book) perEUR(ctx context.Context, ccy string, on time.Time) (time.Time, string, error) {
	var date time.Time
	var rate string
	err := b.pool.QueryRow(ctx, `SELECT rate_date, per_eur::text FROM ecb_reference_rates
		WHERE currency = $1 AND rate_date <= $2 ORDER BY rate_date DESC LIMIT 1`, ccy, on).Scan(&date, &rate)
	if errors.Is(err, pgx.ErrNoRows) {
		return date, "", ErrNoRate
	}
	return date, rate, err
}

// ToUSD prices amountMinor (the smallest unit of a two-decimal currency: usd, eur or gbp) in USD at the
// reference rate of the day `at` falls on (UTC). USD and EUR must come from the same ECB day, so the
// currency's rate fixes the date and USD's is read on it.
func (b *Book) ToUSD(ctx context.Context, amountMinor int64, currency string, at time.Time) (Conversion, error) {
	ccy := strings.ToUpper(currency)
	switch ccy {
	case "USD":
		return Conversion{USDMicros: amountMinor * 10_000}, nil
	case "EUR", "GBP":
	default:
		return Conversion{}, fmt.Errorf("ecbrate: cannot price %s", currency)
	}
	on := at.UTC()
	date, usd, err := b.perEUR(ctx, "USD", on)
	if err != nil {
		return Conversion{}, err
	}
	c := Conversion{RateDate: &date, USDPerEUR: usd, CurrencyPerEUR: "1"}
	if ccy != "EUR" {
		var d time.Time
		if d, c.CurrencyPerEUR, err = b.perEUR(ctx, ccy, date); err != nil {
			return Conversion{}, err
		}
		if !d.Equal(date) {
			return Conversion{}, fmt.Errorf("%w: the ECB's %s rate for %s is missing", ErrNoRate, ccy, date.Format("2006-01-02"))
		}
	}
	c.USDMicros = USDMicros(amountMinor, c.USDPerEUR, c.CurrencyPerEUR)
	return c, nil
}

// USDMicros is ceil(amountMinor ÷ 100 × 10⁶ × usdPerEUR ÷ ccyPerEUR), exactly.
func USDMicros(amountMinor int64, usdPerEUR, ccyPerEUR string) int64 {
	usd, _ := new(big.Rat).SetString(usdPerEUR)
	ccy, _ := new(big.Rat).SetString(ccyPerEUR)
	v := new(big.Rat).SetInt64(amountMinor * 10_000)
	v.Mul(v, usd).Quo(v, ccy)
	q, r := new(big.Int).QuoRem(v.Num(), v.Denom(), new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// Converted is a US-dollar amount in another currency at a day's reference rate (B32.40: a receipt's tax in the
// buyer's currency).
type Converted struct {
	Currency    string     `json:"currency"`
	AmountMinor int64      `json:"amount_minor"`        // rounded half-up to the currency's minor unit
	Rate        string     `json:"rate"`                // units of the currency per US dollar, to six places; "1" for USD
	RateDate    *time.Time `json:"rate_date,omitempty"` // the ECB day the rate is from; nil for USD
	Source      string     `json:"source"`              // "ecb", or "none" for USD
}

// FromUSD prices usdMicros in currency (any the ECB publishes, or EUR or USD) at the reference rate of the latest ECB
// day on or before `at` (UTC): amount = usdMicros ÷ 10⁶ × (currency per EUR) ÷ (USD per EUR), rounded half-up to the
// currency's minor unit.
func (b *Book) FromUSD(ctx context.Context, usdMicros int64, currency string, at time.Time) (Converted, error) {
	r, err := b.USDRate(ctx, currency, at)
	if err != nil {
		return Converted{}, err
	}
	return r.Convert(usdMicros), nil
}

// USDRate is one day's rate from US dollars into a currency, read once to convert many amounts (B32.51: every offer of
// a catalog page).
type USDRate struct {
	currency string
	rate     *big.Rat // units of the currency per US dollar
	date     *time.Time
	source   string
}

// USDRate reads the rate FromUSD converts at: that of the latest ECB day on or before `at` (UTC).
func (b *Book) USDRate(ctx context.Context, currency string, at time.Time) (USDRate, error) {
	ccy := strings.ToUpper(currency)
	if ccy == "USD" {
		return USDRate{currency: ccy, rate: big.NewRat(1, 1), source: "none"}, nil
	}
	date, usd, err := b.perEUR(ctx, "USD", at.UTC())
	if err != nil {
		return USDRate{}, err
	}
	per := "1"
	if ccy != "EUR" {
		var d time.Time
		if d, per, err = b.perEUR(ctx, ccy, date); err != nil {
			return USDRate{}, err
		}
		if !d.Equal(date) {
			return USDRate{}, fmt.Errorf("%w: the ECB's %s rate for %s is missing", ErrNoRate, ccy, date.Format("2006-01-02"))
		}
	}
	usdRat, _ := new(big.Rat).SetString(usd)
	perRat, _ := new(big.Rat).SetString(per)
	return USDRate{currency: ccy, rate: new(big.Rat).Quo(perRat, usdRat), date: &date, source: "ecb"}, nil
}

// PerUSD is how many units of currency one US dollar buys at the latest ECB day (B30.29: the Test FX partner's rate).
func (b *Book) PerUSD(ctx context.Context, currency string) (*big.Rat, error) {
	r, err := b.USDRate(ctx, currency, time.Now())
	if err != nil {
		return nil, err
	}
	return new(big.Rat).Set(r.rate), nil
}

// Date is the ECB day the rate is from; nil for USD.
func (r USDRate) Date() *time.Time { return r.date }

// Convert prices usdMicros at the rate, rounded half-up to the currency's minor unit.
func (r USDRate) Convert(usdMicros int64) Converted {
	scale := int64(1)
	for range minorDigits(r.currency) {
		scale *= 10
	}
	v := new(big.Rat).SetFrac64(usdMicros*scale, 1_000_000)
	v.Mul(v, r.rate)
	c := Converted{Currency: r.currency, AmountMinor: roundHalfUp(v), Rate: r.rate.FloatString(6), RateDate: r.date, Source: r.source}
	if r.source == "none" {
		c.Rate = "1"
	}
	return c
}

// minorDigits is how many decimal places the currency's minor unit has (ISO 4217): none for the yen, the won and the
// króna, two for every other currency the ECB publishes.
func minorDigits(ccy string) int {
	switch ccy {
	case "JPY", "KRW", "ISK":
		return 0
	}
	return 2
}

// roundHalfUp rounds a non-negative rational to the nearest integer, halves up.
func roundHalfUp(v *big.Rat) int64 {
	twice := new(big.Int).Mul(v.Num(), big.NewInt(2))
	twice.Add(twice, v.Denom())
	return new(big.Int).Quo(twice, new(big.Int).Mul(v.Denom(), big.NewInt(2))).Int64()
}
