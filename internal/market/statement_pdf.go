package market

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"time"
)

// statement_pdf.go — B61.131: the branded statement (DESIGN.md 16.3; statement.css holds every measure, in points).
// A4, three embedded TrueType fonts (statement_assets, see FONTS.md) with WinAnsiEncoding and the widths the design
// measured for them, the logo top left on every page and the streams top right on the first. Each section is one
// balance's statement: its title, who and what, the four figures, then its lines from the opening to the closing
// balance and how they add up. Rows never split, and the closing row never opens a page alone.

// BrandedRow is one line of a statement.
type BrandedRow struct {
	Day, Time string    // "03 Oct", "10:41"
	Name      string    // who it was with, or what it was
	State     string    // "Pending", "Failed" or "Returned"; "" for a completed line
	Detail    string    // "Card payment · Ref INV-2034"
	Task      string    // "The agent says: …", or ""
	Amount    int64     // money in positive, money out negative
	Balance   int64     // the balance after it
	At        time.Time // when it moved (the Excel sheet's Date and Time)
	What      string    // what it was, "Card payment"
	Reference string    // the rest of Detail after What: "Ref INV-2034"
}

// BrandedSection is one balance's statement over one period: Opening + In − Out = Closing.
type BrandedSection struct {
	Title, Source, Range string   // "October 2026", "US dollar balance", "1 to 31 October 2026"
	Holder, HolderKind   string   // "Northwind Studio", "Business account"
	WhoHead, WhoName     string   // "Account" or "Agent", and its name
	WhoLines             []string // the account's details, or what the agent pays in
	Issued, Period       string   // "Issued 26 November 2026", "1 October 2026 to 31 October 2026"
	CurrencyWords        string   // "US dollars"
	OpenDay, CloseDay    string   // "01 Oct", "31 Oct"
	Opening, In, Out     int64
	Closing              int64
	Money                func(int64) string // "$13,315.97", "-$4.00"
	Figure               func(int64) string // a non-negative amount with no sign or currency: "13,315.97"
	Number               func(int64) string // an amount as a plain decimal a spreadsheet reads: "-13315.97"
	From, Last           time.Time          // the period's first and last days (the Excel sheet's dates)
	Rows                 []BrandedRow
}

// BrandedStatement is a statement document: one or more sections, its pages numbered through. Sandbox says, on every
// page, that no real money moved.
type BrandedStatement struct {
	Sandbox  bool
	Sections []BrandedSection
}

// The statement's colours (DESIGN.md 16.3) besides the receipt's ink, slate (pdfMuted), grey (pdfLabel), line and
// tangerine (pdfAccent).
const (
	stCloud      = "0.953 0.957 0.965" // #F3F4F6
	stChipLine   = "0.788 0.804 0.839" // #C9CDD6
	stDanger     = "0.753 0.149 0.176" // #C0262D
	stDangerFill = "0.984 0.914 0.918" // #FBE9EA
	stWhite      = "1 1 1"
)

// The page and the table, in points from the top of the page (statement.css).
const (
	stW, stH, stM          = 595.28, 841.89, 48.0
	stRight                = stW - stM
	stRow2, stRow3, stRowB = 28.0, 37.0, 24.0 // a line, a line with what the agent said, an opening or closing row
	stHead, stNote         = 22.0, 58.0       // the column heads, how it adds up
	stTop1, stTopN, stEnd  = 352.0, 100.0, 770.0
	stFoot                 = 784.0
)

// The table's columns: Date 54, Details the rest, then the right edges of Money out, Money in and Balance (8 of air
// after Balance).
const (
	stDetailsX = stM + 54
	stOutR     = stRight - 84 - 78
	stInR      = stRight - 84
	stBalR     = stRight - 8
	stDetailsW = stOutR - 78 - stDetailsX - 8
)

// The fonts: F1 Figtree Statement Regular, F2 Figtree Statement SemiBold, F3 Bricolage Statement Bold.
var (
	//go:embed statement_assets/FigtreeStatement-Regular.ttf
	stFigtreeRegular []byte
	//go:embed statement_assets/FigtreeStatement-SemiBold.ttf
	stFigtreeSemiBold []byte
	//go:embed statement_assets/BricolageStatement-Bold.ttf
	stBricolageBold []byte
	//go:embed statement_assets/widths.json
	stWidthsJSON []byte
	//go:embed statement_assets/statement-logo.png
	stLogoPNG []byte
	//go:embed statement_assets/statement-streams.png
	stStreamsPNG []byte
)

type stFont struct {
	name, file string
	data       []byte
	m          struct {
		FirstChar  int     `json:"first_char"`
		LastChar   int     `json:"last_char"`
		Widths     []int   `json:"widths"`
		Ascent     int     `json:"ascent"`
		Descent    int     `json:"descent"`
		CapHeight  int     `json:"cap_height"`
		BBox       [4]int  `json:"bbox"`
		ItalicAng  float64 `json:"italic_angle"`
		StemV      int     `json:"stem_v"`
		Flags      int     `json:"flags"`
		FontWeight int     `json:"weight"`
	}
	packed []byte // the file, zlib-compressed
}

var stFonts = func() map[string]*stFont {
	fonts := map[string]*stFont{
		"F1": {name: "FigtreeStatement-Regular", file: "FigtreeStatement-Regular.ttf", data: stFigtreeRegular},
		"F2": {name: "FigtreeStatement-SemiBold", file: "FigtreeStatement-SemiBold.ttf", data: stFigtreeSemiBold},
		"F3": {name: "BricolageStatement-Bold", file: "BricolageStatement-Bold.ttf", data: stBricolageBold},
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(stWidthsJSON, &all); err != nil {
		panic("statement widths: " + err.Error())
	}
	for _, f := range fonts {
		if err := json.Unmarshal(all[f.file], &f.m); err != nil || len(f.m.Widths) != f.m.LastChar-f.m.FirstChar+1 {
			panic(fmt.Sprintf("statement widths for %s: %v", f.file, err))
		}
		f.packed = deflate(f.data)
	}
	return fonts
}()

// stImage is a PNG as a PDF image: its size and its colour, zlib-compressed.
type stImage struct {
	w, h int
	rgb  []byte
}

func stDecode(b []byte) stImage {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		panic("statement image: " + err.Error())
	}
	return stImageOf(img)
}

func stImageOf(img image.Image) stImage {
	b := img.Bounds()
	rgb := make([]byte, 0, 3*b.Dx()*b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			// On white paper: a pixel's alpha is its mix with white.
			mix := func(v uint8) byte { return byte((int(v)*int(c.A) + 255*(255-int(c.A))) / 255) }
			rgb = append(rgb, mix(c.R), mix(c.G), mix(c.B))
		}
	}
	return stImage{w: b.Dx(), h: b.Dy(), rgb: deflate(rgb)}
}

var stLogo, stStreams = stDecode(stLogoPNG), stDecode(stStreamsPNG)

// stWidth is s's advance in font at size with spacing after every character, in points.
func stWidth(font string, size, spacing float64, s string) float64 {
	f := stFonts[font]
	w := 0.0
	for _, c := range winAnsi(s) {
		if i := int(c) - f.m.FirstChar; i >= 0 && i < len(f.m.Widths) {
			w += float64(f.m.Widths[i]) * size / 1000
		}
		w += spacing
	}
	return w
}

// stFit is s cut with an ellipsis to fit max points, never wrapped.
func stFit(font string, size, spacing, max float64, s string) string {
	if stWidth(font, size, spacing, s) <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && stWidth(font, size, spacing, strings.TrimRight(string(r), " ")+"…") > max {
		r = r[:len(r)-1]
	}
	return strings.TrimRight(string(r), " ") + "…"
}

// stPage draws in points from the top of the page.
type stPage struct{ pdfPage }

// text draws s with its baseline at top, its left edge at x.
func (p *stPage) text(font string, size, spacing, x, top float64, colour, s string) {
	fmt.Fprintf(&p.b, "BT /%s %.2f Tf %.2f Tc %s rg %.2f %.2f Td (%s) Tj ET\n", font, size, spacing, colour, x, stH-top, pdfString(s))
}

// right draws s with its baseline at top, its right edge at x.
func (p *stPage) right(font string, size, spacing, x, top float64, colour, s string) {
	p.text(font, size, spacing, x-stWidth(font, size, spacing, s)+spacing, top, colour, s)
}

func (p *stPage) fill(x, top, w, h float64, colour string) {
	fmt.Fprintf(&p.b, "%s rg %.2f %.2f %.2f %.2f re f\n", colour, x, stH-top-h, w, h)
}

// rounded adds a rectangle with corners of radius r to the path.
func (p *stPage) rounded(x, top, w, h, r float64) {
	const k = 0.5523
	x0, y0, x1, y1 := x, stH-top-h, x+w, stH-top
	fmt.Fprintf(&p.b, "%.2f %.2f m %.2f %.2f l %.2f %.2f %.2f %.2f %.2f %.2f c %.2f %.2f l %.2f %.2f %.2f %.2f %.2f %.2f c "+
		"%.2f %.2f l %.2f %.2f %.2f %.2f %.2f %.2f c %.2f %.2f l %.2f %.2f %.2f %.2f %.2f %.2f c h\n",
		x0+r, y0, x1-r, y0, x1-r+k*r, y0, x1, y0+r-k*r, x1, y0+r,
		x1, y1-r, x1, y1-r+k*r, x1-r+k*r, y1, x1-r, y1,
		x0+r, y1, x0+r-k*r, y1, x0, y1-r+k*r, x0, y1-r,
		x0, y0+r, x0, y0+r-k*r, x0+r-k*r, y0, x0+r, y0)
}

func (p *stPage) roundedFill(x, top, w, h, r float64, colour string) {
	fmt.Fprintf(&p.b, "%s rg\n", colour)
	p.rounded(x, top, w, h, r)
	p.b.WriteString("f\n")
}

func (p *stPage) image(name string, x, top, w, h float64) {
	fmt.Fprintf(&p.b, "q %.3f 0 0 %.3f %.3f %.3f cm /%s Do Q\n", w, h, x, stH-top-h, name)
}

// PDF renders the statement as an A4 document.
func (s BrandedStatement) PDF() []byte {
	var pages []*stPage
	type started struct {
		sec   *BrandedSection
		first bool
	}
	var heads []started
	for i := range s.Sections {
		sec := &s.Sections[i]
		for j, items := range stPaginate(sec.Rows) {
			p := &stPage{}
			pages = append(pages, p)
			heads = append(heads, started{sec, j == 0})
			top := stTopN
			if j == 0 {
				stFirstPage(p, sec, s.Sandbox)
				top = stTop1
			} else {
				stRunningHead(p, sec)
			}
			stTable(p, sec, items, top)
		}
	}
	for i, p := range pages {
		stFootOf(p, heads[i].sec, s.Sandbox, i+1, len(pages))
	}
	return stDocument(pages)
}

// stItem is one block of the table: the opening row, a line, or the closing row with how it adds up.
type stItem struct {
	kind   byte // 'o' opening, 'l' a line, 'c' closing
	height float64
	row    *BrandedRow
}

// stPaginate fills each page down to stEnd. Rows never split, and the closing row and how it adds up stay together,
// taking the last line with them rather than open a page alone.
func stPaginate(rows []BrandedRow) [][]stItem {
	items := []stItem{{kind: 'o', height: stRowB}}
	for i := range rows {
		h := stRow2
		if rows[i].Task != "" {
			h = stRow3
		}
		items = append(items, stItem{kind: 'l', height: h, row: &rows[i]})
	}
	items = append(items, stItem{kind: 'c', height: stRowB + stNote})
	var pages [][]stItem
	var cur []stItem
	y := stTop1 + stHead
	for _, it := range items {
		if y+it.height > stEnd {
			if it.kind == 'c' && len(cur) > 1 && cur[len(cur)-1].kind == 'l' {
				carry := cur[len(cur)-1]
				pages, cur, y = append(pages, cur[:len(cur)-1]), []stItem{carry}, stTopN+stHead+carry.height
			} else {
				pages, cur, y = append(pages, cur), nil, stTopN+stHead
			}
		}
		cur = append(cur, it)
		y += it.height
	}
	return append(pages, cur)
}

// stFirstPage is the logo, the streams, what this is, who and what it is for, and the four figures.
func stFirstPage(p *stPage, sec *BrandedSection, sandbox bool) {
	p.image("Im2", stW-300, 0, 300, 170)
	p.image("Im1", stM, 40, 120, 120*float64(stLogo.h)/float64(stLogo.w))
	p.text("F2", 8, 1.12, stM, 111.8, pdfLabel, "STATEMENT")
	if sandbox {
		x := stM + stWidth("F2", 8, 1.12, "STATEMENT") + 8
		w := 6 + 7 + 3.5 + stWidth("F2", 7, 0.14, "Sandbox") + 6
		fmt.Fprintf(&p.b, "%s RG 0.75 w\n", stChipLine)
		p.rounded(x, 102, w, 14, 7)
		p.b.WriteString("S\n")
		// The chip's dashed square, as the design's 12-unit icon drawn 7 wide.
		fmt.Fprintf(&p.b, "%s RG 0.88 w [1.28 0.93] 0 d\n", pdfMuted)
		p.rounded(x+6+7*1.5/12, 105.5+7*1.5/12, 7*9/12, 7*9/12, 7*2/12)
		p.b.WriteString("S [] 0 d\n")
		p.text("F2", 7, 0.14, x+6+7+3.5, 111.5, pdfMuted, "Sandbox")
	}
	p.text("F3", 30, -0.6, stM, 142.4, pdfInk, stFit("F3", 30, -0.6, stRight-stM, sec.Title))
	p.text("F1", 10.5, 0, stM, 165.5, pdfMuted, stFit("F1", 10.5, 0, stRight-stM, sec.Source+" · "+sec.Range))

	colW := (stRight - stM - 32) / 3
	column := func(i int, head, value string, lines []string) {
		x := stM + float64(i)*(colW+16)
		p.text("F2", 7, 0.7, x, 197, pdfLabel, strings.ToUpper(head))
		p.text("F2", 10, 0, x, 213, pdfInk, stFit("F2", 10, 0, colW, value))
		if len(lines) > 4 { // four lines fit above the band; the rest share the last
			lines = append(lines[:3:3], strings.Join(lines[3:], " · "))
		}
		for j, l := range lines {
			p.text("F1", 8.5, 0, x, 225+11.2*float64(j), pdfMuted, stFit("F1", 8.5, 0, colW, l))
		}
	}
	column(0, "Account holder", sec.Holder, []string{sec.HolderKind})
	column(1, sec.WhoHead, sec.WhoName, sec.WhoLines)
	n := fmt.Sprintf("%d lines", len(sec.Rows))
	if len(sec.Rows) == 1 {
		n = "1 line"
	}
	period := sec.Period
	if stWidth("F1", 8.5, 0, period) > colW { // "1 November 2026 to 30 November 2026" is too long: "1 to 30 November 2026"
		period = sec.Range
	}
	column(2, "Statement", sec.Issued, []string{period, n})

	// The band: three figures on cloud, the closing balance on ink with the page's one tangerine dot.
	unit := (stRight - stM) / 4.12
	p.roundedFill(stM, 262, stRight-stM, 66, 14, stCloud)
	p.roundedFill(stM+3*unit, 262, 1.12*unit, 66, 14, pdfInk)
	figures := []struct {
		label string
		v     int64
	}{{"Opening balance", sec.Opening}, {"Money in", sec.In}, {"Money out", sec.Out}, {"Closing balance", sec.Closing}}
	for i, f := range figures {
		x, w, label, ink := stM+float64(i)*unit+16, unit-30, pdfLabel, pdfInk
		if i == 3 {
			w, label, ink = 1.12*unit-30, stChipLine, stWhite
			fmt.Fprintf(&p.b, "%s rg\n", pdfAccent)
			p.rounded(x, 278.6, 6, 6, 3)
			p.b.WriteString("f\n")
			x += 11
			w -= 11
		}
		p.text("F2", 7, 0.7, x, 284, label, strings.ToUpper(f.label))
		if i == 3 {
			x -= 11
			w += 11
		}
		size, s := 15.0, sec.Money(f.v)
		for size > 9 && stWidth("F3", size, -0.02*size, s) > w {
			size -= 0.5
		}
		p.text("F3", size, -0.02*size, x, 305.5, ink, s)
	}
}

// stRunningHead is, on every page after the first, the logo and a short line saying what this is.
func stRunningHead(p *stPage, sec *BrandedSection) {
	p.image("Im1", stM, 40, 120, 120*float64(stLogo.h)/float64(stLogo.w))
	p.right("F2", 9.5, 0, stRight, 55.5, pdfInk, stFit("F2", 9.5, 0, 300, "Statement · "+sec.Title))
	p.right("F1", 8, 0, stRight, 68.8, pdfMuted, stFit("F1", 8, 0, 300, sec.Holder+" · "+sec.Source))
}

// stFigure writes an amount for a column: no sign or currency, a minus only for a balance below zero.
func stFigure(sec *BrandedSection, v int64) string {
	if v < 0 {
		return "-" + sec.Figure(-v)
	}
	return sec.Figure(v)
}

// stTable draws the column heads at top and then items.
func stTable(p *stPage, sec *BrandedSection, items []stItem, top float64) {
	head := func(s string, x float64, right bool) {
		if right {
			p.right("F2", 7, 0.7, x, top+13.45, pdfLabel, s)
		} else {
			p.text("F2", 7, 0.7, x, top+13.45, pdfLabel, s)
		}
	}
	head("DATE", stM, false)
	head("DETAILS", stDetailsX, false)
	head("MONEY OUT", stOutR, true)
	head("MONEY IN", stInR, true)
	head("BALANCE", stBalR, true)
	p.fill(stM, top+stHead-0.75, stRight-stM, 0.75, pdfInk)
	y := top + stHead
	for _, it := range items {
		switch it.kind {
		case 'o':
			p.roundedFill(stM, y, stRight-stM, stRowB, 7, stCloud)
			p.text("F2", 9, 0, stM+8, y+15.15, pdfInk, sec.OpenDay)
			p.text("F2", 9.5, 0, stDetailsX+8, y+15.3, pdfInk, "Opening balance")
			p.right("F2", 9.5, 0, stBalR, y+15.3, pdfInk, stFigure(sec, sec.Opening))
		case 'l':
			stLine(p, sec, it.row, y, it.height)
		case 'c':
			p.roundedFill(stM, y-0.5, stRight-stM, stRowB, 7, stCloud)
			p.text("F2", 9, 0, stM+8, y+14.65, pdfInk, sec.CloseDay)
			p.text("F2", 9.5, 0, stDetailsX+8, y+14.8, pdfInk, "Closing balance")
			p.right("F2", 9.5, 0, stOutR, y+14.8, pdfInk, sec.Figure(sec.Out))
			p.right("F2", 9.5, 0, stInR, y+14.8, pdfInk, sec.Figure(sec.In))
			p.right("F2", 9.5, 0, stBalR, y+14.8, pdfInk, stFigure(sec, sec.Closing))
			stAddsUp(p, sec, y+stRowB)
		}
		y += it.height
	}
}

// stLine is one line: its day and time, who and what, its figure in or out, and the balance after it.
func stLine(p *stPage, sec *BrandedSection, r *BrandedRow, y, h float64) {
	p.text("F2", 9, 0, stM, y+14, pdfInk, r.Day)
	p.text("F1", 7.5, 0, stM, y+24.4, pdfLabel, r.Time)
	chipW := 0.0
	if r.State != "" {
		chipW = 6 + 5 + stWidth("F2", 6.8, 0, r.State) + 5
	}
	name := stFit("F2", 9.5, 0, stDetailsW-chipW, r.Name)
	p.text("F2", 9.5, 0, stDetailsX, y+14.26, pdfInk, name)
	if r.State != "" {
		x := stDetailsX + stWidth("F2", 9.5, 0, name) + 6
		fill, ink := stDangerFill, stDanger
		if r.State == "Pending" {
			fill, ink = pdfAccent, pdfInk
		}
		p.roundedFill(x, y+6.3, chipW-6, 11, 5.5, fill)
		p.text("F2", 6.8, 0, x+5, y+14.2, ink, r.State)
	}
	p.text("F1", 8, 0, stDetailsX, y+24.9, pdfMuted, stFit("F1", 8, 0, stDetailsW, r.Detail))
	if r.Task != "" {
		p.text("F1", 8, 0, stDetailsX, y+35.3-1.6, pdfLabel, stFit("F1", 8, 0, stDetailsW, r.Task))
	}
	switch {
	case r.Amount < 0:
		p.right("F1", 9.5, 0, stOutR, y+14.26, pdfInk, sec.Figure(-r.Amount))
	case r.Amount > 0:
		p.right("F1", 9.5, 0, stInR, y+14.26, pdfInk, sec.Figure(r.Amount))
	}
	p.right("F2", 9.5, 0, stBalR, y+14.26, pdfInk, stFigure(sec, r.Balance))
	p.fill(stM, y+h-0.5, stRight-stM, 0.5, pdfLine)
}

// stAddsUp is how the statement adds up: opening + in − out = closing, the closing figure over a tangerine rule.
func stAddsUp(p *stPage, sec *BrandedSection, y float64) {
	p.text("F2", 7, 0.7, stM+8, y+23, pdfLabel, "HOW THIS ADDS UP")
	x, base := stM+8, y+40
	part := func(font string, size float64, colour, s string, after float64) float64 {
		at := x
		p.text(font, size, 0, x, base, colour, s)
		x += stWidth(font, size, 0, s) + after
		return at
	}
	part("F2", 9.5, pdfInk, sec.Money(sec.Opening), 4)
	part("F1", 8, pdfLabel, "opening", 8)
	part("F2", 9.5, pdfLabel, "+", 8)
	part("F2", 9.5, pdfInk, sec.Money(sec.In), 4)
	part("F1", 8, pdfLabel, "in", 8)
	part("F2", 9.5, pdfLabel, "–", 8)
	part("F2", 9.5, pdfInk, sec.Money(sec.Out), 4)
	part("F1", 8, pdfLabel, "out", 8)
	part("F2", 9.5, pdfLabel, "=", 8)
	closing := sec.Money(sec.Closing)
	at := part("F2", 9.5, pdfInk, closing, 4)
	p.fill(at, base+3, stWidth("F2", 9.5, 0, closing), 1.5, pdfAccent)
	part("F1", 8, pdfLabel, "closing", 0)
}

// stFootOf is the foot of every page: the Sandbox line while no real money moved, what the amounts are, the company
// line, and the page's number.
func stFootOf(p *stPage, sec *BrandedSection, sandbox bool, n, of int) {
	p.fill(stM, stFoot, stRight-stM, 0.5, pdfLine)
	page := fmt.Sprintf("Page %d of %d", n, of)
	p.right("F2", 7.5, 0, stRight, stFoot+15.3, pdfMuted, page)
	max := stRight - stM - stWidth("F2", 7.5, 0, page) - 16
	x := stM
	if sandbox {
		const line = "Sandbox statement. No real money moved."
		p.text("F2", 6.8, 0, x, stFoot+15, pdfInk, line)
		x += stWidth("F2", 6.8, 0, line+" ")
	}
	rest := "Times are UTC. Amounts are in " + sec.CurrencyWords + ". Questions go to hello@talyvor.com"
	p.text("F1", 6.8, 0, x, stFoot+15, pdfLabel, stFit("F1", 6.8, 0, stM+max-x, rest))
	p.text("F1", 6.8, 0, stM, stFoot+25.2, pdfLabel, stFit("F1", 6.8, 0, max, StatementCompanyLine))
}

// StatementCompanyLine is who issues the statement, as the foot of every page writes it.
const StatementCompanyLine = "Talyvor Ltd, registered in England and Wales, company number 17299143. 71-75 Shelton Street, Covent Garden, London WC2H 9JQ"

// stDocument writes the pages as a PDF 1.4 file: a catalogue, the page tree, the three fonts each with its
// descriptor and its file, the logo and the streams, and each page with its content stream.
func stDocument(pages []*stPage) []byte {
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) int {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
		return len(offsets)
	}
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	const first = 14 // 1 catalogue, 2 pages, 3 to 11 the fonts, 12 and 13 the images
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", first+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	var fontRefs []string
	for _, key := range []string{"F1", "F2", "F3"} {
		f := stFonts[key]
		ws := make([]string, len(f.m.Widths))
		for i, w := range f.m.Widths {
			ws[i] = fmt.Sprint(w)
		}
		n := len(offsets) + 1
		obj(fmt.Sprintf("<< /Type /Font /Subtype /TrueType /BaseFont /%s /FirstChar %d /LastChar %d /Widths [%s] /FontDescriptor %d 0 R /Encoding /WinAnsiEncoding >>",
			f.name, f.m.FirstChar, f.m.LastChar, strings.Join(ws, " "), n+1))
		obj(fmt.Sprintf("<< /Type /FontDescriptor /FontName /%s /Flags %d /FontBBox [%d %d %d %d] /ItalicAngle %g /Ascent %d /Descent %d /CapHeight %d /StemV %d /FontWeight %d /FontFile2 %d 0 R >>",
			f.name, f.m.Flags, f.m.BBox[0], f.m.BBox[1], f.m.BBox[2], f.m.BBox[3], f.m.ItalicAng, f.m.Ascent, f.m.Descent, f.m.CapHeight, f.m.StemV, f.m.FontWeight, n+2))
		obj(fmt.Sprintf("<< /Length %d /Length1 %d /Filter /FlateDecode >>\nstream\n%s\nendstream", len(f.packed), len(f.data), f.packed))
		fontRefs = append(fontRefs, fmt.Sprintf("/%s %d 0 R", key, n))
	}
	for _, im := range []stImage{stLogo, stStreams} {
		obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream",
			im.w, im.h, len(im.rgb), im.rgb))
	}
	for i, p := range pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] /Resources << /Font << %s >> /XObject << /Im1 12 0 R /Im2 13 0 R >> >> /Contents %d 0 R >>",
			stW, stH, strings.Join(fontRefs, " "), first+1+2*i))
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
