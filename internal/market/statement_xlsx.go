package market

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// statement_xlsx.go — B61.132: the branded statement as an Excel workbook (DESIGN.md 16.4; make_sheets.py holds the
// exact cells), written with the standard library: a zip of a few XML parts. A Statement sheet for each section, then
// About. Arial throughout. The summary and every balance are formulas, each with the value it works out to stored
// beside it so a preview that never calculates still shows the figures. Words are always text cells, never formulas,
// whatever they start with. The heads stay in view and filter, and it prints landscape on A4, one page wide.

// The sheet's colours (DESIGN.md 16.3), as Excel writes them.
const (
	xInk, xSlate, xGrey, xLine, xCloud, xTangerine, xWhite = "FF111216", "FF4A5061", "FF646B7B", "FFE4E6EB", "FFF3F4F6", "FFFF7A1A", "FFFFFFFF"
)

// The Statement sheet's rows: the heads, then the opening balance, the lines and the closing balance.
const (
	xHead  = 13
	xFirst = xHead + 2
)

// xFont, xStyle: a cell's look; the workbook's styles.xml is the set its cells use.
type xFont struct {
	size  float64
	bold  bool
	color string
}

type xStyle struct {
	font      xFont
	fill      string // "" for none
	bottom    bool   // a thin line beneath
	numFmt    int    // 0 general, 4 #,##0.00, xDate, xTime
	align     string // "", "left", "right"
	indent    int
	top, wrap bool
	center    bool // vertically
}

const (
	xDate = 164 // d mmm yyyy
	xTime = 165 // hh:mm
)

type xBook struct {
	styles []xStyle
	index  map[xStyle]int
}

func (b *xBook) style(s xStyle) int {
	if b.index == nil {
		b.index = map[xStyle]int{{font: xFont{size: 10, color: xInk}}: 0}
		b.styles = []xStyle{{font: xFont{size: 10, color: xInk}}}
	}
	if i, ok := b.index[s]; ok {
		return i
	}
	b.index[s] = len(b.styles)
	b.styles = append(b.styles, s)
	return len(b.styles) - 1
}

// xCell is one cell: text, a number, or a formula with the value it works out to.
type xCell struct {
	col     int // 1 for A
	style   int
	text    string
	num     string // a plain decimal
	formula string
	isText  bool // the formula's value is text
	isNum   bool
}

type xRow struct {
	r      int
	height float64
	cells  []xCell
}

type xComment struct {
	ref      string
	row, col int // 0-based, for the note's drawing
	text     string
}

type xSheet struct {
	name     string
	cols     []float64
	rows     []*xRow
	merges   []string
	comments []xComment
	logo     bool
	frozen   int // the rows above stay in view
	heads    int // the heads row: it filters down to the last line, and prints on every page
	lastLine int
	lastRow  int  // the print area's last row
	setup    bool // landscape A4, one page wide
}

func (s *xSheet) row(r int, height float64) *xRow {
	for _, x := range s.rows {
		if x.r == r {
			if height > 0 {
				x.height = height
			}
			return x
		}
	}
	x := &xRow{r: r, height: height}
	s.rows = append(s.rows, x)
	return x
}

func (x *xRow) text(col, style int, v string) {
	x.cells = append(x.cells, xCell{col: col, style: style, text: v})
}

func (x *xRow) num(col, style int, v string) {
	x.cells = append(x.cells, xCell{col: col, style: style, num: v, isNum: true})
}

func (x *xRow) formula(col, style int, f, value string, isText bool) {
	x.cells = append(x.cells, xCell{col: col, style: style, formula: f, num: value, isText: isText, isNum: !isText})
}

func (x *xRow) blank(col, style int) {
	x.cells = append(x.cells, xCell{col: col, style: style})
}

func xCol(c int) string {
	s := ""
	for ; c > 0; c = (c - 1) / 26 {
		s = string(rune('A'+(c-1)%26)) + s
	}
	return s
}

func xRef(col, row int) string { return xCol(col) + strconv.Itoa(row) }

// xSerial is t's day as Excel counts it, days since 30 December 1899.
func xSerial(t time.Time) string {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return strconv.FormatInt(int64(d.Sub(time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC))/(24*time.Hour)), 10)
}

// xClock is t's time of day, to the minute, as the fraction of a day Excel keeps.
func xClock(t time.Time) string {
	return strconv.FormatFloat(float64(t.Hour()*60+t.Minute())/1440, 'f', -1, 64)
}

// XLSX renders the statement as an Excel workbook.
func (s BrandedStatement) XLSX() []byte {
	var b xBook
	b.style(xStyle{font: xFont{size: 10, color: xInk}})
	var sheets []*xSheet
	for i := range s.Sections {
		name := "Statement"
		if i > 0 {
			name = fmt.Sprintf("Statement %d", i+1)
		}
		sheets = append(sheets, xStatementSheet(&b, &s.Sections[i], name, s.Sandbox))
	}
	if len(sheets) == 0 {
		sheets = append(sheets, xStatementSheet(&b, &BrandedSection{}, "Statement", s.Sandbox))
	}
	var first BrandedSection
	if len(s.Sections) > 0 {
		first = s.Sections[0]
	}
	sheets = append(sheets, xAboutSheet(&b, first, len(s.Sections), s.Sandbox))
	return xWorkbook(&b, sheets)
}

func xStatementSheet(b *xBook, sec *BrandedSection, name string, sandbox bool) *xSheet {
	number := sec.Number
	if number == nil {
		number = func(v int64) string { return strconv.FormatInt(v, 10) }
	}
	f := func(size float64, bold bool, color string) xFont { return xFont{size: size, bold: bold, color: color} }
	sh := &xSheet{name: name, cols: []float64{13, 11, 30, 22, 13, 17, 11, 15, 15, 16}, logo: true, setup: true}

	band := sh.row(1, 5)
	for c := 1; c <= 10; c++ {
		band.blank(c, b.style(xStyle{font: f(10, false, xInk), fill: xTangerine}))
	}
	for r := 2; r <= 4; r++ {
		sh.row(r, 17)
	}

	// Who and when, top left.
	sh.row(6, 0).text(1, b.style(xStyle{font: f(9, true, xGrey)}), "Statement")
	sh.row(7, 30).text(1, b.style(xStyle{font: f(20, true, xInk)}), sec.Title)
	holder := sec.HolderKind
	if sec.Holder != "" {
		holder = strings.TrimSuffix(sec.Holder+", "+sec.HolderKind, ", ")
	}
	info := [][2]string{{"Account holder", holder}, {sec.WhoHead, sec.WhoName}, {"Period", sec.Period}}
	if sandbox {
		info = append(info, [2]string{"Sandbox", "Sandbox statement. No real money moved."})
	}
	for i, kv := range info {
		x := sh.row(8+i, 0)
		x.text(1, b.style(xStyle{font: f(9, false, xGrey)}), kv[0])
		x.text(3, b.style(xStyle{font: f(10, i < 2, xInk)}), kv[1])
	}

	// The lines, worked out once: what the formulas below come to.
	last := xFirst + len(sec.Rows) - 1
	if len(sec.Rows) == 0 {
		last = xFirst // one row that says nothing moved
	}
	closeRow := last + 1
	var in, out int64
	for _, r := range sec.Rows {
		if r.Amount > 0 {
			in += r.Amount
		} else {
			out -= r.Amount
		}
	}
	closing := sec.Opening + in - out

	// The summary, top right. Only the opening balance is typed in.
	sh.row(6, 0).text(8, b.style(xStyle{font: f(9, true, xGrey)}), "Summary, in "+sec.CurrencyWords)
	check := "Adds up" // closing is the last balance by construction; the formula says so again after any edit
	summary := []struct {
		label, formula, value string
		text                  bool
	}{
		{"Opening balance", "", number(sec.Opening), false},
		{"Money in", fmt.Sprintf("SUM(I%d:I%d)", xFirst, last), number(in), false},
		{"Money out", fmt.Sprintf("SUM(H%d:H%d)", xFirst, last), number(out), false},
		{"Closing balance", "J7+J8-J9", number(closing), false},
		{"Check", fmt.Sprintf(`IF(ROUND(J10-J%d,2)=0,"Adds up","Does not add up")`, last), check, true},
	}
	for i, it := range summary {
		r := 7 + i
		tot := it.label == "Closing balance"
		fill, labelColor, figColor, size := xCloud, xSlate, xInk, 10.0
		if tot {
			fill, labelColor, figColor, size = xInk, xWhite, xWhite, 11
		}
		x := sh.row(r, 0)
		x.text(8, b.style(xStyle{font: f(10, tot, labelColor), fill: fill, indent: 1}), it.label)
		x.blank(9, b.style(xStyle{font: f(10, false, xInk), fill: fill}))
		fig := b.style(xStyle{font: f(size, true, figColor), fill: fill, numFmt: 4, align: "right"})
		if it.formula == "" {
			x.num(10, fig, it.value)
		} else {
			x.formula(10, fig, it.formula, it.value, it.text)
		}
		sh.merges = append(sh.merges, fmt.Sprintf("H%d:I%d", r, r))
	}
	sh.comments = append(sh.comments, xComment{ref: "J7", row: 6, col: 9, text: "The balance at the start of the period, from the ledger."})

	// The heads, on ink.
	heads := []string{"Date", "Time (UTC)", "Details", "What", "Paid by", "Reference", "Status", "Money out", "Money in", "Balance"}
	hx := sh.row(xHead, 22)
	for c, h := range heads {
		st := xStyle{font: f(9, true, xWhite), fill: xInk, align: "left", indent: 1, center: true}
		if c >= 7 {
			st.align, st.indent = "right", 0
		}
		hx.text(c+1, b.style(st), h)
	}

	bandRow := func(r int, label string, day time.Time) *xRow {
		x := sh.row(r, 0)
		cell := b.style(xStyle{font: f(10, true, xInk), fill: xCloud})
		x.num(1, b.style(xStyle{font: f(10, true, xInk), fill: xCloud, numFmt: xDate, align: "left", indent: 1}), xSerial(day))
		x.blank(2, cell)
		x.text(3, cell, label)
		for c := 4; c <= 7; c++ {
			x.blank(c, cell)
		}
		return x
	}
	figure := b.style(xStyle{font: f(10, true, xInk), fill: xCloud, numFmt: 4, align: "right"})
	open := bandRow(xHead+1, "Opening balance", sec.From)
	open.blank(8, figure)
	open.blank(9, figure)
	open.formula(10, figure, "J7", number(sec.Opening), false)

	paidBy := ""
	if sec.WhoHead == "Agent" {
		paidBy = sec.WhoName
	}
	words := func(bold bool) int { return b.style(xStyle{font: f(10, bold, xInk), bottom: true}) }
	money := func(bold bool) int {
		return b.style(xStyle{font: f(10, bold, xInk), bottom: true, numFmt: 4, align: "right"})
	}
	balance := sec.Opening
	for i, l := range sec.Rows {
		r := xFirst + i
		x := sh.row(r, 0)
		x.num(1, b.style(xStyle{font: f(10, false, xInk), bottom: true, numFmt: xDate, align: "left", indent: 1}), xSerial(l.At))
		x.num(2, b.style(xStyle{font: f(10, false, xInk), bottom: true, numFmt: xTime, align: "left"}), xClock(l.At))
		by := ""
		if l.Amount < 0 {
			by = paidBy
		}
		for c, v := range []string{l.Name, l.What, by, l.Reference, l.State} {
			x.text(3+c, words(c == 0), v)
		}
		if l.Amount < 0 {
			x.num(8, money(false), number(-l.Amount))
			x.blank(9, money(false))
		} else {
			x.blank(8, money(false))
			if l.Amount > 0 {
				x.num(9, money(false), number(l.Amount))
			} else {
				x.blank(9, money(false))
			}
		}
		balance += l.Amount
		x.formula(10, money(true), fmt.Sprintf("J%d+N(I%d)-N(H%d)", r-1, r, r), number(balance), false)
		if l.Task != "" {
			sh.comments = append(sh.comments, xComment{ref: xRef(3, r), row: r - 1, col: 2, text: l.Task})
		}
	}
	if len(sec.Rows) == 0 {
		x := sh.row(xFirst, 0)
		x.text(3, words(false), "Nothing moved in this period.")
		x.formula(10, money(true), fmt.Sprintf("J%d+N(I%d)-N(H%d)", xFirst-1, xFirst, xFirst), number(sec.Opening), false)
	}
	cl := bandRow(closeRow, "Closing balance", sec.Last)
	cl.formula(8, figure, fmt.Sprintf("SUM(H%d:H%d)", xFirst, last), number(out), false)
	cl.formula(9, figure, fmt.Sprintf("SUM(I%d:I%d)", xFirst, last), number(in), false)
	cl.formula(10, figure, fmt.Sprintf("J%d", last), number(closing), false)

	foot := b.style(xStyle{font: f(8, false, xGrey)})
	sh.row(closeRow+2, 0).text(1, foot, "Times are UTC. Amounts are in "+sec.CurrencyWords+". Questions go to hello@talyvor.com")
	sh.row(closeRow+3, 0).text(1, foot, StatementCompanyLine)

	sh.frozen, sh.heads, sh.lastLine, sh.lastRow = xHead, xHead, last, closeRow+3
	return sh
}

func xAboutSheet(b *xBook, sec BrandedSection, sections int, sandbox bool) *xSheet {
	sh := &xSheet{name: "About", cols: []float64{24, 92}}
	what := "A statement of the " + sec.Source
	if sections > 1 {
		what = "A statement of each balance, one sheet each"
	}
	if sec.Holder != "" {
		what += " of " + sec.Holder
	}
	what += ", from " + sec.Period + "."
	rows := [][2]string{{"About this file", ""}, {"What it is", what}}
	if sandbox {
		rows = append(rows, [2]string{"Sandbox", "This statement is from the Talyvor sandbox. No real money moved."})
	}
	rows = append(rows,
		[2]string{"How it adds up", "Opening balance + Money in − Money out = Closing balance. The Check cell on the Statement sheet says so."},
		[2]string{"Formulas", "Money in, Money out, Closing balance and every Balance are formulas. Opening balance is the one figure typed in."},
		[2]string{"Date and Time (UTC)", "When the money moved. Times are UTC."},
		[2]string{"Details", "Who was paid, or who paid in. A note on the cell holds what the agent said the payment was for, in its own words."},
		[2]string{"What", "The kind of line: Card payment, Paid out, Paid in, Invoice paid, Refund, Returned."},
		[2]string{"Paid by", "The agent that paid. Empty when the account holder paid."},
		[2]string{"Status", "Empty when the line is complete. Otherwise Pending, Failed or Returned."},
		[2]string{"Questions", "hello@talyvor.com"},
		[2]string{"From", StatementCompanyLine},
	)
	for i, kv := range rows {
		x := sh.row(i+2, 0)
		size := 10.0
		if i == 0 {
			size = 14
		}
		x.text(1, b.style(xStyle{font: xFont{size: size, bold: true, color: xInk}, top: true}), kv[0])
		x.text(2, b.style(xStyle{font: xFont{size: 10, color: xSlate}, top: true, wrap: true}), kv[1])
	}
	return sh
}

// xEsc is s as XML text; a character XML cannot carry is replaced, never dropped silently.
func xEsc(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func xQuote(name string) string { return "'" + strings.ReplaceAll(name, "'", "''") + "'" }

func xWorkbook(b *xBook, sheets []*xSheet) []byte {
	const (
		ns    = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
		rels  = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
		rtype = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
		head  = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	)
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	put := func(name, body string) {
		w, _ := z.Create(name)
		_, _ = w.Write([]byte(head + body))
	}

	var types strings.Builder
	types.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Default Extension="vml" ContentType="application/vnd.openxmlformats-officedocument.vmlDrawing"/>` +
		`<Default Extension="png" ContentType="image/png"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	var book, bookRels, names strings.Builder
	book.WriteString(`<workbook ` + ns + `><bookViews><workbookView activeTab="0"/></bookViews><sheets>`)
	bookRels.WriteString(`<Relationships ` + rels + `>`)
	logoUsed := false
	for i, sh := range sheets {
		n := i + 1
		fmt.Fprintf(&types, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n)
		fmt.Fprintf(&book, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xEsc(sh.name), n, n)
		fmt.Fprintf(&bookRels, `<Relationship Id="rId%d" Type="%sworksheet" Target="worksheets/sheet%d.xml"/>`, n, rtype, n)
		q := xEsc(xQuote(sh.name))
		if sh.heads > 0 {
			fmt.Fprintf(&names, `<definedName name="_xlnm._FilterDatabase" localSheetId="%d" hidden="1">%s!$A$%d:$J$%d</definedName>`, i, q, sh.heads, sh.lastLine)
			fmt.Fprintf(&names, `<definedName name="_xlnm.Print_Area" localSheetId="%d">%s!$A$1:$J$%d</definedName>`, i, q, sh.lastRow)
			fmt.Fprintf(&names, `<definedName name="_xlnm.Print_Titles" localSheetId="%d">%s!$%d:$%d</definedName>`, i, q, sh.heads, sh.heads)
		}

		var srels strings.Builder
		if sh.logo {
			logoUsed = true
			fmt.Fprintf(&types, `<Override PartName="/xl/drawings/drawing%d.xml" ContentType="application/vnd.openxmlformats-officedocument.drawing+xml"/>`, n)
			fmt.Fprintf(&srels, `<Relationship Id="rId1" Type="%sdrawing" Target="../drawings/drawing%d.xml"/>`, rtype, n)
			put(fmt.Sprintf("xl/drawings/drawing%d.xml", n), xLogoDrawing())
			put(fmt.Sprintf("xl/drawings/_rels/drawing%d.xml.rels", n),
				`<Relationships `+rels+`><Relationship Id="rId1" Type="`+rtype+`image" Target="../media/image1.png"/></Relationships>`)
		}
		if len(sh.comments) > 0 {
			fmt.Fprintf(&types, `<Override PartName="/xl/comments%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.comments+xml"/>`, n)
			fmt.Fprintf(&srels, `<Relationship Id="rId2" Type="%scomments" Target="../comments%d.xml"/>`, rtype, n)
			fmt.Fprintf(&srels, `<Relationship Id="rId3" Type="%svmlDrawing" Target="../drawings/vmlDrawing%d.vml"/>`, rtype, n)
			put(fmt.Sprintf("xl/comments%d.xml", n), xComments(sh.comments))
			put(fmt.Sprintf("xl/drawings/vmlDrawing%d.vml", n), xNotesDrawing(n, sh.comments))
		}
		if srels.Len() > 0 {
			put(fmt.Sprintf("xl/worksheets/_rels/sheet%d.xml.rels", n), `<Relationships `+rels+`>`+srels.String()+`</Relationships>`)
		}
		put(fmt.Sprintf("xl/worksheets/sheet%d.xml", n), xSheetXML(sh, ns))
	}
	book.WriteString(`</sheets>`)
	if names.Len() > 0 {
		book.WriteString(`<definedNames>` + names.String() + `</definedNames>`)
	}
	book.WriteString(`<calcPr calcId="191029" fullCalcOnLoad="1"/></workbook>`)
	fmt.Fprintf(&bookRels, `<Relationship Id="rId%d" Type="%sstyles" Target="styles.xml"/></Relationships>`, len(sheets)+1, rtype)
	types.WriteString(`</Types>`)

	put("[Content_Types].xml", types.String())
	put("_rels/.rels", `<Relationships `+rels+`><Relationship Id="rId1" Type="`+rtype+`officeDocument" Target="xl/workbook.xml"/></Relationships>`)
	put("xl/workbook.xml", book.String())
	put("xl/_rels/workbook.xml.rels", bookRels.String())
	put("xl/styles.xml", xStylesXML(b))
	if logoUsed {
		w, _ := z.Create("xl/media/image1.png")
		_, _ = w.Write(stLogoPNG)
	}
	_ = z.Close()
	return buf.Bytes()
}

func xSheetXML(sh *xSheet, ns string) string {
	var s strings.Builder
	s.WriteString(`<worksheet ` + ns + `>`)
	if sh.setup {
		s.WriteString(`<sheetPr><pageSetUpPr fitToPage="1"/></sheetPr>`)
	}
	s.WriteString(`<sheetViews><sheetView showGridLines="0" workbookViewId="0">`)
	if sh.frozen > 0 {
		fmt.Fprintf(&s, `<pane ySplit="%d" topLeftCell="A%d" activePane="bottomLeft" state="frozen"/><selection pane="bottomLeft" activeCell="A%d" sqref="A%d"/>`,
			sh.frozen, sh.frozen+1, sh.frozen+1, sh.frozen+1)
	}
	s.WriteString(`</sheetView></sheetViews><sheetFormatPr defaultRowHeight="15"/><cols>`)
	for i, w := range sh.cols {
		fmt.Fprintf(&s, `<col min="%d" max="%d" width="%g" customWidth="1"/>`, i+1, i+1, w)
	}
	s.WriteString(`</cols><sheetData>`)
	rows := append([]*xRow(nil), sh.rows...)
	for i := 1; i < len(rows); i++ { // rows in order, as Excel requires
		for j := i; j > 0 && rows[j].r < rows[j-1].r; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	for _, x := range rows {
		fmt.Fprintf(&s, `<row r="%d"`, x.r)
		if x.height > 0 {
			fmt.Fprintf(&s, ` ht="%g" customHeight="1"`, x.height)
		}
		s.WriteString(`>`)
		cells := append([]xCell(nil), x.cells...)
		for i := 1; i < len(cells); i++ {
			for j := i; j > 0 && cells[j].col < cells[j-1].col; j-- {
				cells[j], cells[j-1] = cells[j-1], cells[j]
			}
		}
		for _, c := range cells {
			ref := xRef(c.col, x.r)
			switch {
			case c.formula != "" && c.isText:
				fmt.Fprintf(&s, `<c r="%s" s="%d" t="str"><f>%s</f><v>%s</v></c>`, ref, c.style, xEsc(c.formula), xEsc(c.num))
			case c.formula != "":
				fmt.Fprintf(&s, `<c r="%s" s="%d"><f>%s</f><v>%s</v></c>`, ref, c.style, xEsc(c.formula), c.num)
			case c.isNum:
				fmt.Fprintf(&s, `<c r="%s" s="%d"><v>%s</v></c>`, ref, c.style, c.num)
			case c.text != "":
				// Words are an inline string: a text cell Excel never reads as a formula, whatever it starts with.
				fmt.Fprintf(&s, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, c.style, xEsc(c.text))
			default:
				fmt.Fprintf(&s, `<c r="%s" s="%d"/>`, ref, c.style)
			}
		}
		s.WriteString(`</row>`)
	}
	s.WriteString(`</sheetData>`)
	if sh.heads > 0 {
		fmt.Fprintf(&s, `<autoFilter ref="A%d:J%d"/>`, sh.heads, sh.lastLine)
	}
	if len(sh.merges) > 0 {
		fmt.Fprintf(&s, `<mergeCells count="%d">`, len(sh.merges))
		for _, m := range sh.merges {
			fmt.Fprintf(&s, `<mergeCell ref="%s"/>`, m)
		}
		s.WriteString(`</mergeCells>`)
	}
	s.WriteString(`<pageMargins left="0.5" right="0.5" top="0.6" bottom="0.6" header="0.3" footer="0.3"/>`)
	if sh.setup {
		s.WriteString(`<pageSetup paperSize="9" orientation="landscape" fitToWidth="1" fitToHeight="0"/>`)
	}
	if sh.logo {
		s.WriteString(`<drawing r:id="rId1"/>`)
	}
	if len(sh.comments) > 0 {
		s.WriteString(`<legacyDrawing r:id="rId3"/>`)
	}
	s.WriteString(`</worksheet>`)
	return s.String()
}

// xLogoDrawing is the logo at A2, 151 pixels wide (the PNG is 480 by 156).
func xLogoDrawing() string {
	const emu = 9525 // per pixel
	return fmt.Sprintf(`<xdr:wsDr xmlns:xdr="http://schemas.openxmlformats.org/drawingml/2006/spreadsheetDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`+
		`<xdr:oneCellAnchor><xdr:from><xdr:col>0</xdr:col><xdr:colOff>0</xdr:colOff><xdr:row>1</xdr:row><xdr:rowOff>0</xdr:rowOff></xdr:from><xdr:ext cx="%d" cy="%d"/>`+
		`<xdr:pic><xdr:nvPicPr><xdr:cNvPr id="2" name="Talyvor" descr="Talyvor"/><xdr:cNvPicPr><a:picLocks noChangeAspect="1"/></xdr:cNvPicPr></xdr:nvPicPr>`+
		`<xdr:blipFill><a:blip r:embed="rId1"/><a:stretch><a:fillRect/></a:stretch></xdr:blipFill>`+
		`<xdr:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></xdr:spPr></xdr:pic><xdr:clientData/></xdr:oneCellAnchor></xdr:wsDr>`,
		151*emu, 49*emu, 151*emu, 49*emu)
}

func xComments(cs []xComment) string {
	var s strings.Builder
	s.WriteString(`<comments xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><authors><author>Talyvor</author></authors><commentList>`)
	for _, c := range cs {
		fmt.Fprintf(&s, `<comment ref="%s" authorId="0"><text><t xml:space="preserve">%s</t></text></comment>`, c.ref, xEsc(c.text))
	}
	s.WriteString(`</commentList></comments>`)
	return s.String()
}

// xNotesDrawing is the shapes Excel draws a sheet's notes in, hidden until the cell is pointed at.
func xNotesDrawing(sheet int, cs []xComment) string {
	var s strings.Builder
	fmt.Fprintf(&s, `<xml xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office" xmlns:x="urn:schemas-microsoft-com:office:excel">`+
		`<o:shapelayout v:ext="edit"><o:idmap v:ext="edit" data="%d"/></o:shapelayout>`+
		`<v:shapetype id="_x0000_t202" coordsize="21600,21600" o:spt="202" path="m,l,21600r21600,l21600,xe"><v:stroke joinstyle="miter"/><v:path gradientshapeok="t" o:connecttype="rect"/></v:shapetype>`, sheet)
	for i, c := range cs {
		fmt.Fprintf(&s, `<v:shape id="_x0000_s%d" type="#_x0000_t202" style="position:absolute;margin-left:59.25pt;margin-top:1.5pt;width:144pt;height:59pt;z-index:%d;visibility:hidden" fillcolor="#ffffe1" o:insetmode="auto">`+
			`<v:fill color2="#ffffe1"/><v:shadow on="t" color="black" obscured="t"/><v:path o:connecttype="none"/><v:textbox style="mso-direction-alt:auto"><div style="text-align:left"></div></v:textbox>`+
			`<x:ClientData ObjectType="Note"><x:MoveWithCells/><x:SizeWithCells/><x:Anchor>%d, 15, %d, 2, %d, 15, %d, 16</x:Anchor><x:AutoFill>False</x:AutoFill><x:Row>%d</x:Row><x:Column>%d</x:Column></x:ClientData></v:shape>`,
			sheet*1024+1+i, i+1, c.col+1, c.row, c.col+3, c.row+4, c.row, c.col)
	}
	s.WriteString(`</xml>`)
	return s.String()
}

func xStylesXML(b *xBook) string {
	var fonts []xFont
	fontAt := map[xFont]int{}
	fills := []string{"", "gray125"}
	fillAt := map[string]int{"": 0}
	for _, st := range b.styles {
		if _, ok := fontAt[st.font]; !ok {
			fontAt[st.font] = len(fonts)
			fonts = append(fonts, st.font)
		}
		if _, ok := fillAt[st.fill]; !ok {
			fillAt[st.fill] = len(fills)
			fills = append(fills, st.fill)
		}
	}
	var s strings.Builder
	s.WriteString(`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<numFmts count="2"><numFmt numFmtId="164" formatCode="d mmm yyyy"/><numFmt numFmtId="165" formatCode="hh:mm"/></numFmts>`)
	fmt.Fprintf(&s, `<fonts count="%d">`, len(fonts))
	for _, f := range fonts {
		s.WriteString(`<font>`)
		if f.bold {
			s.WriteString(`<b/>`)
		}
		fmt.Fprintf(&s, `<sz val="%g"/><color rgb="%s"/><name val="Arial"/><family val="2"/></font>`, f.size, f.color)
	}
	fmt.Fprintf(&s, `</fonts><fills count="%d">`, len(fills))
	for i, f := range fills {
		switch i {
		case 0:
			s.WriteString(`<fill><patternFill patternType="none"/></fill>`)
		case 1:
			s.WriteString(`<fill><patternFill patternType="gray125"/></fill>`)
		default:
			fmt.Fprintf(&s, `<fill><patternFill patternType="solid"><fgColor rgb="%s"/><bgColor rgb="%s"/></patternFill></fill>`, f, f)
		}
	}
	s.WriteString(`</fills><borders count="2"><border><left/><right/><top/><bottom/><diagonal/></border>` +
		`<border><left/><right/><top/><bottom style="thin"><color rgb="` + xLine + `"/></bottom><diagonal/></border></borders>` +
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>`)
	fmt.Fprintf(&s, `<cellXfs count="%d">`, len(b.styles))
	for _, st := range b.styles {
		border := 0
		if st.bottom {
			border = 1
		}
		fmt.Fprintf(&s, `<xf numFmtId="%d" fontId="%d" fillId="%d" borderId="%d" xfId="0" applyFont="1" applyFill="1" applyBorder="1" applyNumberFormat="1" applyAlignment="1"><alignment`,
			st.numFmt, fontAt[st.font], fillAt[st.fill], border)
		if st.align != "" {
			fmt.Fprintf(&s, ` horizontal="%s"`, st.align)
		}
		if st.indent > 0 {
			fmt.Fprintf(&s, ` indent="%d"`, st.indent)
		}
		switch {
		case st.top:
			s.WriteString(` vertical="top"`)
		case st.center:
			s.WriteString(` vertical="center"`)
		}
		if st.wrap {
			s.WriteString(` wrapText="1"`)
		}
		s.WriteString(`/></xf>`)
	}
	s.WriteString(`</cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`)
	return s.String()
}
