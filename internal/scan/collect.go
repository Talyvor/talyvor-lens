package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Collector reads one source of the register into the store (B52.21).
type Collector interface {
	// Source is the collector's id in the register.
	Source() string
	// Setting is the server setting holding the collector's API key, or "" when the source needs none.
	Setting() string
	// Collect reads every figure observed since `since` and hands each to put, fetching only through f.
	Collect(ctx context.Context, f *Fetcher, since, now time.Time, put func(Observation) error) error
}

// Backfill is how far back a collector reads on its first run, where the source keeps that much.
const Backfill = 5 * 365 * 24 * time.Hour

// overlap is re-read on every run, so a figure published late for an earlier day is still collected.
const overlap = 14 * 24 * time.Hour

// defaultPace spaces requests to a source whose register entry holds no rate limit.
const defaultPace = 2 * time.Second

// Fetcher makes a collector's HTTP requests, no faster than its source's rate limit, and hashes what it read.
type Fetcher struct {
	client *http.Client
	gap    time.Duration
	last   time.Time
	now    func() time.Time
}

// NewFetcher spaces requests to at most perMinute a minute, or one every defaultPace when perMinute is 0.
func NewFetcher(client *http.Client, perMinute int) *Fetcher {
	gap := defaultPace
	if perMinute > 0 {
		gap = time.Minute / time.Duration(perMinute)
	}
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// A redirect off the source's host would carry its API key (GIE's x-key header) to that host.
		if req.URL.Host != via[0].URL.Host || len(via) >= 5 {
			return errors.New("redirected off the source's host")
		}
		return nil
	}
	return &Fetcher{client: &c, gap: gap, now: time.Now}
}

// Get fetches req and returns the body, its hex SHA-256 and when it was retrieved. A status other than 200 is an
// error carrying ErrNotPublished for a 404. The error never carries the URL, which can hold an API key.
func (f *Fetcher) Get(ctx context.Context, req *http.Request) (body []byte, hash string, at time.Time, err error) {
	if wait := f.gap - f.now().Sub(f.last); wait > 0 {
		select {
		case <-ctx.Done():
			return nil, "", time.Time{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	f.last = f.now()
	resp, err := f.client.Do(req.WithContext(ctx))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, "", time.Time{}, fmt.Errorf("%s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("%s: %w", req.URL.Host, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", time.Time{}, fmt.Errorf("%s %s: %w", req.URL.Host, req.URL.Path, ErrNotPublished)
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
		return nil, "", time.Time{}, fmt.Errorf("%s %s: status %d", req.URL.Host, req.URL.Path, resp.StatusCode)
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), f.now().UTC(), nil
}

// ErrNotPublished is a 404: the source has not published (or no longer keeps) what was asked for.
var ErrNotPublished = errors.New("scan: not published")

// CollectorStatus is one collector as the status page shows it.
type CollectorStatus struct {
	Source        string     `json:"source"`
	Name          string     `json:"name"`
	On            bool       `json:"on"`
	Note          string     `json:"note,omitempty"`
	LastRetrieved *time.Time `json:"last_retrieved"`
	Failing       bool       `json:"failing"` // the last run on this replica failed; the error itself is in the log
}

// Collectors runs every collector on a schedule and says how each is doing.
type Collectors struct {
	store  *Store
	client *http.Client
	keys   map[string]string // setting → value
	list   []Collector
	now    func() time.Time

	mu      sync.Mutex
	failing map[string]bool // source → whether its last run on this replica failed
}

// NewCollectors builds the runner. keys maps each collector's setting to its value from the server's settings.
func NewCollectors(store *Store, client *http.Client, keys map[string]string, list ...Collector) *Collectors {
	return &Collectors{store: store, client: client, keys: keys, list: list, now: time.Now, failing: map[string]bool{}}
}

// Energy is B52.21's collectors, reading their API keys from keys.
func Energy(keys map[string]string) []Collector {
	return []Collector{
		&AGSI{Key: keys[SettingGIE]}, &ALSI{Key: keys[SettingGIE]}, &ENTSOG{}, &EIA{Key: keys[SettingEIA]}, &ECMWF{},
	}
}

func (c *Collectors) ready(col Collector) bool {
	return col.Setting() == "" || c.keys[col.Setting()] != ""
}

// Run collects from every collector once. A collector without its key is switched off and skipped; one that fails
// is logged and the others still run.
func (c *Collectors) Run(ctx context.Context) {
	for _, col := range c.list {
		err := c.runOne(ctx, col)
		c.mu.Lock()
		c.failing[col.Source()] = err != nil
		c.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			slog.Warn("scan: collector failed", slog.String("source", col.Source()), slog.String("err", err.Error()))
		}
	}
}

func (c *Collectors) runOne(ctx context.Context, col Collector) error {
	ready := c.ready(col)
	if err := c.store.SetEnabled(ctx, col.Source(), ready); err != nil || !ready {
		return err
	}
	src, err := c.store.Source(ctx, col.Source())
	if err != nil {
		return err
	}
	perMinute := 0
	if src.RequestsPerMinute != nil {
		perMinute = *src.RequestsPerMinute
	}
	now := c.now().UTC()
	since := now.Add(-Backfill)
	var last *time.Time
	if err := c.store.pool.QueryRow(ctx, `SELECT max(observed_at) FROM scan_points WHERE kind = 'observed' AND source_id = $1`,
		col.Source()).Scan(&last); err != nil {
		return err
	}
	if last != nil && last.Add(-overlap).After(since) {
		since = last.Add(-overlap)
	}
	return col.Collect(ctx, NewFetcher(c.client, perMinute), since, now, func(o Observation) error {
		o.SourceID = col.Source()
		_, err := c.store.Record(ctx, o)
		return err
	})
}

// Every runs the collectors now and then once every interval until ctx ends.
func (c *Collectors) Every(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.Run(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Status is every collector: on or off and why, and when its source last gave a figure.
func (c *Collectors) Status(ctx context.Context) []CollectorStatus {
	last := map[string]time.Time{}
	rows, err := c.store.pool.Query(ctx, `SELECT source_id, max(retrieved_at) FROM scan_points WHERE kind = 'observed'
		GROUP BY source_id`)
	if err == nil {
		for rows.Next() {
			var id string
			var at time.Time
			if rows.Scan(&id, &at) == nil {
				last[id] = at
			}
		}
		rows.Close()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CollectorStatus, 0, len(c.list))
	for _, col := range c.list {
		st := CollectorStatus{Source: col.Source(), Name: col.Source(), On: c.ready(col), Failing: c.failing[col.Source()]}
		if src, err := c.store.Source(ctx, col.Source()); err == nil {
			st.Name = src.Name
			if st.On && !src.PublicDisplay {
				st.Note = "collected for internal use only"
			}
		}
		if !st.On {
			st.Note = "off: needs its API key in the server setting " + col.Setting()
		}
		if at, ok := last[col.Source()]; ok {
			st.LastRetrieved = &at
		}
		out = append(out, st)
	}
	return out
}
