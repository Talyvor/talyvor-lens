package scan

import (
	"context"
	"encoding/json"
	"time"
)

// published.go — B52.33: SCAN'S PUBLISHED REPORTS, FOR ANYONE TO READ.
//
// Only an approved report is published. A reader gets its title, its four sections, its figures (each with the
// stored point's value, unit and date where it has one, for the charts), its sources and the "How this report was
// made" box — never the refusal, the reviewer, the review note, who wrote the draft or what the second model said.

// ReportFamilies are the families a reader can pick, each the series prefixes it covers. No filings report is
// written yet (filings are events, not levels), so that family lists none until one is.
var ReportFamilies = map[string][]string{
	"energy":  {"gie.agsi.", "entsog.flow."},
	"oil":     {"eia.stocks."},
	"crops":   {"crop.nass."},
	"filings": {"sec.", "usaspending."},
}

// PublishedFigure is one of a published report's figures; Value, Unit and At come from the stored point behind it.
type PublishedFigure struct {
	Name  string     `json:"name"`
	Text  string     `json:"text"`
	Value *float64   `json:"value,omitempty"`
	Unit  string     `json:"unit,omitempty"`
	At    *time.Time `json:"at,omitempty"`
}

// PublishedReport is an approved report as a reader sees it.
type PublishedReport struct {
	ID              int64             `json:"id"`
	Series          string            `json:"series"`
	Family          string            `json:"family"`
	PublishedAt     time.Time         `json:"published_at"`
	Title           string            `json:"title"`
	WhatChanged     string            `json:"what_changed"`
	WhyItMatters    string            `json:"why_it_matters"`
	Confidence      string            `json:"confidence"`
	WhatWouldChange string            `json:"what_would_change"`
	Figures         []PublishedFigure `json:"figures"`
	Sources         []string          `json:"sources"`
	Box             HowMade           `json:"box"`
}

func familyName(series string) string {
	for name, prefixes := range ReportFamilies {
		for _, p := range prefixes {
			if len(series) > len(p) && series[:len(p)] == p {
				return name
			}
		}
	}
	return ""
}

// Published is every approved report, newest published first; only family's when family is one of ReportFamilies.
func (w *Writer) Published(ctx context.Context, family string) ([]PublishedReport, error) {
	// shortcut: the newest 200 in one reply, a few a week; page it when the library outgrows that.
	return w.published(ctx, `status = 'approved' AND ($1::text[] IS NULL OR EXISTS (
		SELECT 1 FROM unnest($1::text[]) p WHERE starts_with(series, p))) ORDER BY reviewed_at DESC, id DESC LIMIT 200`,
		ReportFamilies[family])
}

// PublishedReport is one approved report; ErrReportNotFound for any other, whatever its status.
func (w *Writer) PublishedReport(ctx context.Context, id int64) (PublishedReport, error) {
	out, err := w.published(ctx, `status = 'approved' AND id = $1`, id)
	if err != nil {
		return PublishedReport{}, err
	}
	if len(out) == 0 {
		return PublishedReport{}, ErrReportNotFound
	}
	return out[0], nil
}

// published reads the reports where says, with the stored point behind each figure.
func (w *Writer) published(ctx context.Context, where string, arg any) ([]PublishedReport, error) {
	rows, err := w.store.pool.Query(ctx, `SELECT id, series, reviewed_at, draft FROM scan_reports WHERE `+where, arg)
	if err != nil {
		return nil, err
	}
	out := []PublishedReport{}
	var drafts []Draft
	var points []int64
	for rows.Next() {
		var p PublishedReport
		var raw []byte
		var d Draft
		if err := rows.Scan(&p.ID, &p.Series, &p.PublishedAt, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			rows.Close()
			return nil, err
		}
		for _, f := range d.Figures {
			if f.Point != 0 {
				points = append(points, f.Point)
			}
		}
		out, drafts = append(out, p), append(drafts, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	type point struct {
		value float64
		unit  string
		at    time.Time
	}
	byID := map[int64]point{}
	if len(points) > 0 {
		prow, err := w.store.pool.Query(ctx, `SELECT id, value, unit, observed_at FROM scan_points WHERE id = ANY($1)`, points)
		if err != nil {
			return nil, err
		}
		for prow.Next() {
			var id int64
			var pt point
			if err := prow.Scan(&id, &pt.value, &pt.unit, &pt.at); err != nil {
				prow.Close()
				return nil, err
			}
			byID[id] = pt
		}
		prow.Close()
		if err := prow.Err(); err != nil {
			return nil, err
		}
	}
	for i, d := range drafts {
		p := &out[i]
		p.Family, p.Title, p.WhatChanged, p.WhyItMatters, p.Confidence, p.WhatWouldChange, p.Sources, p.Box =
			familyName(p.Series), d.Title, d.WhatChanged, d.WhyItMatters, d.Confidence, d.WhatWouldChange, d.Sources, d.Box
		if p.Sources == nil {
			p.Sources = []string{}
		}
		p.Figures = make([]PublishedFigure, len(d.Figures))
		for j, f := range d.Figures {
			p.Figures[j] = PublishedFigure{Name: f.Name, Text: f.Text}
			if pt, ok := byID[f.Point]; ok {
				v, at := pt.value, pt.at
				p.Figures[j].Value, p.Figures[j].Unit, p.Figures[j].At = &v, pt.unit, &at
			}
		}
	}
	return out, nil
}
