package market

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strconv"
	"strings"

	"github.com/talyvor/lens/internal/brand"
)

// receipt_render.go — B32.40: a marketplace receipt as a page (HTML) and as a document (PDF), both in the brand: the
// flat mark, Space Grotesk for words and IBM Plex Mono for figures on the page; on paper the light theme, the mark's
// own vectors, Helvetica for words and Courier for figures (the PDF's built-in faces, so it embeds no font).

// usdText prints µUSD as US dollars: to the cent when it is whole cents, otherwise to as many places as it needs.
func usdText(micros int64) string {
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	if micros%10_000 == 0 {
		return fmt.Sprintf("%s$%d.%02d", sign, micros/1_000_000, micros%1_000_000/10_000)
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", micros%1_000_000), "0")
	return fmt.Sprintf("%s$%d.%s", sign, micros/1_000_000, frac)
}

// rateText prints basis points as a percentage: 2000 → 20%, 550 → 5.5%.
func rateText(bps int) string {
	return strconv.FormatFloat(float64(bps)/100, 'f', -1, 64) + "%"
}

var currencySymbols = map[string]string{"GBP": "£", "EUR": "€", "USD": "$"}

// minorText prints an amount in a currency's minor unit with its symbol, or its code.
func minorText(currency string, minor int64) string {
	digits := 2
	switch currency {
	case "JPY", "KRW", "ISK":
		digits = 0
	}
	s := strconv.FormatInt(minor, 10)
	if digits > 0 {
		for len(s) <= digits {
			s = "0" + s
		}
		s = s[:len(s)-digits] + "." + s[len(s)-digits:]
	}
	if sym, ok := currencySymbols[currency]; ok {
		return sym + s
	}
	return currency + " " + s
}

// LocalTaxText is the receipt's tax in the buyer's currency, with the rate it was converted at; "" without one.
func (r Receipt) LocalTaxText() string {
	if r.TaxLocal == nil || r.TaxLocal.Currency == "USD" {
		return ""
	}
	at := ""
	if r.TaxLocal.RateDate != nil {
		at = " of " + r.TaxLocal.RateDate.Format("2 January 2006")
	}
	return fmt.Sprintf("VAT in %s: %s, at the ECB reference rate%s (1 USD = %s %s)", r.TaxLocal.Currency,
		minorText(r.TaxLocal.Currency, r.TaxLocal.AmountMinor), at, r.TaxLocal.Rate, r.TaxLocal.Currency)
}

// SupplierVAT is the supplier's VAT number as the receipt prints it.
func (r Receipt) SupplierVAT() string {
	if r.Supplier.VATNumber == "" {
		return VATRegistrationPending
	}
	return r.Supplier.VATNumber
}

// addressLines splits an address into the lines it was written in, or at its commas.
func addressLines(parts ...string) []string {
	var out []string
	for _, p := range parts {
		for _, l := range strings.FieldsFunc(p, func(c rune) bool { return c == '\n' || c == ',' }) {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

func (r Receipt) supplierLines() []string { return addressLines(r.Supplier.Address) }
func (r Receipt) buyerLines() []string {
	return addressLines(r.Buyer.Address, strings.TrimSpace(r.Buyer.PostalCode+" "+r.Buyer.Country))
}

var receiptHTML = template.Must(template.New("receipt").Funcs(template.FuncMap{
	"usd":  usdText,
	"rate": rateText,
	"date": func(r Receipt) string { return r.IssuedAt.Format("2 January 2006") },
	"paid": func(r Receipt) string { return r.PaidAt.Format("2 January 2006") },
	"gross": func(l ReceiptLine) string {
		return usdText(l.NetUSDMicros + l.TaxUSDMicros)
	},
	"supplierLines": Receipt.supplierLines,
	"buyerLines":    Receipt.buyerLines,
}).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Receipt {{.Number}} — Talyvor</title>
<style>
` + brand.TokensCSS + brand.PrintCSS + `
  *{box-sizing:border-box}
  body{margin:0;padding:40px 24px;background:var(--tv-canvas);color:var(--tv-ink);
       font-family:var(--tv-font-sans);font-size:15px;line-height:24px;-webkit-font-smoothing:antialiased}
  main{max-width:880px;margin:0 auto}
  .num{font-family:var(--tv-font-mono);font-variant-numeric:tabular-nums}
  header{display:flex;align-items:flex-start;gap:16px;margin-bottom:24px}
  header .tv-mark{width:40px;height:40px;margin-top:4px}
  .eyebrow{font-size:12px;line-height:16px;font-weight:500;letter-spacing:.22em;text-transform:uppercase;color:var(--tv-label);margin:0}
  h1{margin:4px 0 6px;font-size:28px;line-height:34px;font-weight:500;letter-spacing:-.01em}
  header .tv-rule{margin-top:14px}
  .meta{color:var(--tv-ink-muted);font-size:13px;line-height:20px}
  .meta .num{overflow-wrap:anywhere}
  .preview{display:flex;align-items:center;gap:12px;margin:0 0 16px;padding:12px 16px;border:1px solid var(--tv-line-strong);
           border-radius:var(--tv-radius-md);background:var(--tv-surface);font-weight:500}
  .preview::before{content:"";width:10px;height:10px;flex:none;border-radius:var(--tv-radius-pill);background:var(--tv-caution)}
  .parties{display:grid;grid-template-columns:1fr 1fr;gap:16px;margin-bottom:16px}
  section{background:var(--tv-surface);border:1px solid var(--tv-line);border-radius:var(--tv-radius-md);
          padding:18px 20px;margin-bottom:16px;break-inside:avoid}
  .parties section{margin:0}
  section h2{margin:0 0 8px}
  .party p{margin:0}
  .party .name{font-weight:500}
  .party .vat{margin-top:8px;color:var(--tv-ink-muted);font-size:13px;line-height:20px}
  table{border-collapse:collapse;width:100%;font-size:14px;line-height:20px}
  th,td{text-align:left;padding:9px 8px;border-bottom:1px solid var(--tv-line);vertical-align:top}
  tbody tr:last-child td{border-bottom:0}
  th{color:var(--tv-label);font-weight:500;font-size:12px;letter-spacing:.08em;text-transform:uppercase}
  td.num,th.num{text-align:right;white-space:nowrap}
  .totals{margin:12px 0 0 auto;max-width:320px}
  .totals div{display:flex;justify-content:space-between;gap:16px;padding:4px 8px}
  .totals .grand{border-top:1px solid var(--tv-line-strong);margin-top:4px;padding-top:10px;font-weight:600}
  .local{color:var(--tv-ink-muted);font-size:13px;line-height:20px;margin:12px 8px 0;text-align:right}
  .notes p{margin:0 0 4px}
  footer{color:var(--tv-ink-muted);font-size:13px;line-height:20px;margin-top:28px}
  @media (max-width:600px){
    body{padding:28px 16px}
    h1{font-size:24px;line-height:30px}
    header .tv-mark{width:32px;height:32px}
    .parties{grid-template-columns:1fr}
    section{padding:14px 12px}
    table{font-size:13px}
    th,td{padding:8px 4px}
    th{letter-spacing:.04em}
    .hide-narrow{display:none}
    .totals{max-width:none}
    .local{text-align:left}
  }
  @page{margin:16mm}
  @media print{
    body{padding:0}
    section,.preview{border-color:var(--tv-line-strong)}
  }
</style></head><body>
<main>
<header>
  ` + brand.Mark + `
  <div>
    <p class="eyebrow">Talyvor · Marketplace receipt</p>
    <h1>Receipt <span class="num">{{.Number}}</span></h1>
    <div class="meta">Issued {{date .}} · paid {{paid .}} · invoice <span class="num">{{.InvoiceID}}</span></div>
    <span class="tv-rule"></span>
  </div>
</header>
{{if .Preview}}<p class="preview">{{.PreviewReason}}</p>{{end}}
<div class="parties">
<section class="party">
  <h2 class="eyebrow">Supplier</h2>
  <p class="name">{{.Supplier.LegalName}}</p>
  {{range supplierLines .}}<p>{{.}}</p>{{end}}
  <p class="vat">{{if .Supplier.VATNumber}}VAT number: <span class="num">{{.Supplier.VATNumber}}</span>{{else}}{{.SupplierVAT}}{{end}}</p>
</section>
<section class="party">
  <h2 class="eyebrow">Customer</h2>
  <p class="name">{{.Buyer.Name}}</p>
  {{range buyerLines .}}<p>{{.}}</p>{{end}}
  {{if .Buyer.VATNumber}}<p class="vat">VAT number: <span class="num">{{.Buyer.VATNumber}}</span></p>{{end}}
</section>
</div>
<section>
<h2 class="eyebrow">What you paid for</h2>
<table>
<thead><tr><th>Description</th><th class="num">Net</th><th class="num">VAT rate</th><th class="num">VAT</th><th class="num hide-narrow">Total</th></tr></thead>
<tbody>
{{range .Lines}}<tr><td>{{.Description}}</td><td class="num">{{usd .NetUSDMicros}}</td><td class="num">{{rate .RateBps}}</td><td class="num">{{usd .TaxUSDMicros}}</td><td class="num hide-narrow">{{gross .}}</td></tr>
{{end}}</tbody>
</table>
<div class="totals">
  <div><span>Net</span><span class="num">{{usd .NetUSDMicros}}</span></div>
  <div><span>VAT</span><span class="num">{{usd .TaxUSDMicros}}</span></div>
  <div class="grand"><span>Total (USD)</span><span class="num">{{usd .GrossUSDMicros}}</span></div>
</div>
{{with .LocalTaxText}}<p class="local">{{.}}</p>{{end}}
</section>
{{if .Notes}}<section class="notes">
<h2 class="eyebrow">Notes</h2>
{{range .Notes}}<p>{{.}}</p>{{end}}
</section>{{end}}
<footer>Talyvor is the supplier of the marketplace services on this receipt. Amounts are in US dollars.</footer>
</main>
</body></html>
`))

// HTML renders the receipt as a page, printable as it is.
func (r Receipt) HTML() ([]byte, error) {
	var b bytes.Buffer
	if err := receiptHTML.Execute(&b, r); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ── the PDF ──────────────────────────────────────────────────────────────────────────────────────────────────────

// Paper colours: the light column of brand-v4/tokens/tokens.css, as PDF RGB.
const (
	pdfInk     = "0.024 0.039 0.071" // #060A12
	pdfMuted   = "0.275 0.345 0.431" // #46586E
	pdfLabel   = "0.392 0.420 0.475" // #646B79
	pdfAccent  = "0.059 0.478 0.424" // #0F7A6C
	pdfCaution = "0.541 0.416 0.071" // #8A6A12
	pdfLine    = "0.898 0.910 0.925" // rgba(6,10,18,.10) on white
)

// pdfPage is one A4 page's content stream as it is drawn.
type pdfPage struct{ b bytes.Buffer }

// text draws s at (x, y) in font (F1 Helvetica, F2 Helvetica-Bold, F3 Courier) of size, in colour.
func (p *pdfPage) text(font string, size, x, y float64, colour, s string) {
	fmt.Fprintf(&p.b, "BT /%s %.1f Tf %s rg %.2f %.2f Td (%s) Tj ET\n", font, size, colour, x, y, pdfString(s))
}

// right draws s in Courier, its right edge at x: Courier's every glyph is 600/1000 of the size wide.
func (p *pdfPage) right(size, x, y float64, colour, s string) {
	p.text("F3", size, x-0.6*size*float64(len(winAnsi(s))), y, colour, s)
}

func (p *pdfPage) rect(x, y, w, h float64, colour string) {
	fmt.Fprintf(&p.b, "%s rg %.2f %.2f %.2f %.2f re f\n", colour, x, y, w, h)
}

// winAnsi is s in the PDF's WinAnsiEncoding: Latin-1 as it is, the few typographic marks a receipt prints, and ? for
// anything else.
func winAnsi(s string) []byte {
	special := map[rune]byte{'€': 0x80, '…': 0x85, '‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94, '–': 0x96, '—': 0x97}
	var out []byte
	for _, r := range s {
		switch b, ok := special[r]; {
		case ok:
			out = append(out, b)
		case r < 0x80 || (r >= 0xA0 && r <= 0xFF):
			out = append(out, byte(r))
		default:
			out = append(out, '?')
		}
	}
	return out
}

func pdfString(s string) string {
	var b strings.Builder
	for _, c := range winAnsi(s) {
		switch c {
		case '(', ')', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			if c >= 0x80 {
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// wrap breaks s into lines of at most n characters, at spaces where it can.
func wrap(s string, n int) []string {
	var lines []string
	for len([]rune(s)) > n {
		r := []rune(s)
		cut := strings.LastIndex(string(r[:n]), " ")
		if cut <= 0 {
			cut = len(string(r[:n]))
		}
		lines = append(lines, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	return append(lines, s)
}

var (
	svgViewBox = regexp.MustCompile(`viewBox="([-\d.]+) ([-\d.]+) ([-\d.]+) ([-\d.]+)"`)
	svgPath    = regexp.MustCompile(`<path fill="#([0-9A-Fa-f]{6})" d="([^"]+)"`)
	svgToken   = regexp.MustCompile(`[MLQCZ]|-?\d+(?:\.\d+)?`)
)

// mark draws the brand's flat mark (brand.MarkLightSVG, its own vectors) in a size×size box whose top left is (x, y).
// The file's paths use M, L, Q, C and Z only; a quadratic segment is drawn as the cubic that is exactly it.
func (p *pdfPage) mark(x, y, size float64) {
	svg := brand.MarkLightSVG()
	vb := svgViewBox.FindStringSubmatch(svg)
	if vb == nil {
		return
	}
	minX, _ := strconv.ParseFloat(vb[1], 64)
	minY, _ := strconv.ParseFloat(vb[2], 64)
	w, _ := strconv.ParseFloat(vb[3], 64)
	s := size / w
	fmt.Fprintf(&p.b, "q %.5f 0 0 %.5f %.3f %.3f cm\n", s, -s, x-minX*s, y+minY*s)
	for _, m := range svgPath.FindAllStringSubmatch(svg, -1) {
		rgb, _ := strconv.ParseUint(m[1], 16, 32)
		fmt.Fprintf(&p.b, "%.3f %.3f %.3f rg\n", float64(rgb>>16)/255, float64(rgb>>8&0xFF)/255, float64(rgb&0xFF)/255)
		toks := svgToken.FindAllString(m[2], -1)
		var cx, cy float64
		num := func(i int) float64 { v, _ := strconv.ParseFloat(toks[i], 64); return v }
		for i := 0; i < len(toks); {
			switch toks[i] {
			case "M":
				cx, cy = num(i+1), num(i+2)
				fmt.Fprintf(&p.b, "%g %g m\n", cx, cy)
				i += 3
			case "L":
				cx, cy = num(i+1), num(i+2)
				fmt.Fprintf(&p.b, "%g %g l\n", cx, cy)
				i += 3
			case "C":
				fmt.Fprintf(&p.b, "%g %g %g %g %g %g c\n", num(i+1), num(i+2), num(i+3), num(i+4), num(i+5), num(i+6))
				cx, cy = num(i+5), num(i+6)
				i += 7
			case "Q":
				qx, qy, ex, ey := num(i+1), num(i+2), num(i+3), num(i+4)
				fmt.Fprintf(&p.b, "%.4f %.4f %.4f %.4f %g %g c\n", cx+2*(qx-cx)/3, cy+2*(qy-cy)/3, ex+2*(qx-ex)/3, ey+2*(qy-ey)/3, ex, ey)
				cx, cy = ex, ey
				i += 5
			case "Z":
				p.b.WriteString("h\n")
				i++
			default:
				i++
			}
		}
		p.b.WriteString("f\n")
	}
	p.b.WriteString("Q\n")
}

// PDF renders the receipt as an A4 document.
func (r Receipt) PDF() []byte {
	const (
		width, height = 595.28, 841.89
		left, rightX  = 48.0, 547.28
		bottom        = 64.0
	)
	var pages []*pdfPage
	p := &pdfPage{}
	pages = append(pages, p)
	y := height - 48.0

	p.mark(left, y, 30)
	p.text("F1", 7.5, left+42, y-9, pdfLabel, "TALYVOR  ·  MARKETPLACE RECEIPT")
	p.text("F2", 18, left+42, y-30, pdfInk, "Receipt "+r.Number)
	p.rect(left+42, y-40, 32, 2, pdfAccent)
	y -= 58
	p.text("F1", 9, left, y, pdfMuted, fmt.Sprintf("Issued %s  ·  paid %s  ·  invoice %s", r.IssuedAt.Format("2 January 2006"),
		r.PaidAt.Format("2 January 2006"), r.InvoiceID))
	y -= 22
	if r.Preview {
		p.text("F2", 10, left, y, pdfCaution, r.PreviewReason)
		y -= 22
	}

	party := func(x float64, label, name string, lines []string, vat string) float64 {
		yy := y
		p.text("F1", 7.5, x, yy, pdfLabel, label)
		yy -= 15
		p.text("F2", 10, x, yy, pdfInk, name)
		for _, l := range lines {
			for _, w := range wrap(l, 44) {
				yy -= 13
				p.text("F1", 10, x, yy, pdfInk, w)
			}
		}
		if vat != "" {
			yy -= 15
			p.text("F1", 9, x, yy, pdfMuted, vat)
		}
		return yy
	}
	supplierVAT, buyerVAT := VATRegistrationPending, ""
	if r.Supplier.VATNumber != "" {
		supplierVAT = "VAT number: " + r.Supplier.VATNumber
	}
	if r.Buyer.VATNumber != "" {
		buyerVAT = "VAT number: " + r.Buyer.VATNumber
	}
	y1 := party(left, "SUPPLIER", r.Supplier.LegalName, r.supplierLines(), supplierVAT)
	y2 := party(310, "CUSTOMER", r.Buyer.Name, r.buyerLines(), buyerVAT)
	y = min(y1, y2) - 30

	cols := []float64{330, 400, 470, rightX} // the right edges of Net, VAT rate, VAT and Total
	header := func() {
		p.text("F1", 7.5, left, y, pdfLabel, "DESCRIPTION")
		for i, h := range []string{"NET", "VAT RATE", "VAT", "TOTAL"} {
			p.text("F1", 7.5, cols[i]-4.2*float64(len(h)), y, pdfLabel, h)
		}
		y -= 8
		p.rect(left, y, rightX-left, 0.75, pdfLine)
		y -= 14
	}
	header()
	for _, l := range r.Lines {
		desc := wrap(l.Description, 46)
		if y-13*float64(len(desc)) < bottom {
			p = &pdfPage{}
			pages = append(pages, p)
			y = height - 56
			header()
		}
		for i, d := range desc {
			p.text("F1", 9.5, left, y-13*float64(i), pdfInk, d)
		}
		for i, v := range []string{usdText(l.NetUSDMicros), rateText(l.RateBps), usdText(l.TaxUSDMicros), usdText(l.NetUSDMicros + l.TaxUSDMicros)} {
			p.right(9.5, cols[i], y, pdfInk, v)
		}
		y -= 13*float64(len(desc)) + 6
		p.rect(left, y+4, rightX-left, 0.5, pdfLine)
		y -= 8
	}
	if y < bottom+120 {
		p = &pdfPage{}
		pages = append(pages, p)
		y = height - 56
	}
	y -= 6
	for _, t := range []struct {
		label, value string
		bold         bool
	}{{"Net", usdText(r.NetUSDMicros), false}, {"VAT", usdText(r.TaxUSDMicros), false}, {"Total (USD)", usdText(r.GrossUSDMicros), true}} {
		font := "F1"
		if t.bold {
			font = "F2"
			p.rect(370, y+12, rightX-370, 0.75, pdfLine)
		}
		p.text(font, 10, 370, y, pdfInk, t.label)
		p.right(10, rightX, y, pdfInk, t.value)
		y -= 16
	}
	if s := r.LocalTaxText(); s != "" {
		y -= 6
		for _, w := range wrap(s, 96) {
			p.text("F1", 9, left, y, pdfMuted, w)
			y -= 12
		}
	}
	if len(r.Notes) > 0 {
		y -= 12
		p.text("F1", 7.5, left, y, pdfLabel, "NOTES")
		for _, n := range r.Notes {
			for _, w := range wrap(n, 96) {
				y -= 13
				p.text("F1", 9.5, left, y, pdfInk, w)
			}
		}
	}
	for i, pg := range pages {
		pg.text("F1", 8, left, 36, pdfMuted, "Talyvor is the supplier of the marketplace services on this receipt. Amounts are in US dollars.")
		pg.right(8, rightX, 36, pdfMuted, fmt.Sprintf("%s  %d/%d", r.Number, i+1, len(pages)))
	}
	return pdfDocument(width, height, pages)
}

// pdfDocument writes the pages as a PDF 1.4 file: a catalogue, the page tree, the three built-in fonts, and each page
// with its content stream.
func pdfDocument(width, height float64, pages []*pdfPage) []byte {
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 6+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Courier /Encoding /WinAnsiEncoding >>")
	for i, p := range pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] /Resources << /Font << /F1 3 0 R /F2 4 0 R /F3 5 0 R >> >> /Contents %d 0 R >>",
			width, height, 7+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", p.b.Len(), p.b.String()))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}
