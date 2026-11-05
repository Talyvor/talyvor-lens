package market

import "fmt"

// table_pdf.go — B48.14: a document that is a titled table, such as a wallet's monthly statement, drawn as the receipt
// is: the light theme, the mark's own vectors, Helvetica for words and Courier for figures.

// PDFColumn is one column of a PDFTable: Width in points; a Right column holds figures, right-aligned.
type PDFColumn struct {
	Name  string
	Width float64
	Right bool
}

// PDFTable is a document of one table: Label beside the mark, Title under it, Lines of context, then Rows under
// Columns, repeated at the head of every page, and Footer at the foot of each.
type PDFTable struct {
	Label, Title string
	Lines        []string
	Columns      []PDFColumn
	Rows         [][]string
	Footer       string
}

// PDF renders t as an A4 document.
func (t PDFTable) PDF() []byte {
	const (
		width, height = 595.28, 841.89
		left, rightX  = 48.0, 547.28
		bottom        = 64.0
		size          = 8.5
	)
	var pages []*pdfPage
	p := &pdfPage{}
	pages = append(pages, p)
	y := height - 48.0
	p.mark(left, y, 30)
	p.text("F1", 7.5, left+42, y-9, pdfLabel, t.Label)
	p.text("F2", 18, left+42, y-30, pdfInk, t.Title)
	p.rect(left+42, y-40, 32, 2, pdfAccent)
	y -= 58
	for _, l := range t.Lines {
		p.text("F1", 9, left, y, pdfMuted, l)
		y -= 13
	}
	y -= 12
	header := func() {
		x := left
		for _, c := range t.Columns {
			if c.Right {
				p.text("F1", 7, x+c.Width-4-4.0*float64(len(c.Name)), y, pdfLabel, c.Name)
			} else {
				p.text("F1", 7, x, y, pdfLabel, c.Name)
			}
			x += c.Width
		}
		y -= 8
		p.rect(left, y, rightX-left, 0.75, pdfLine)
		y -= 13
	}
	header()
	for _, row := range t.Rows {
		if y < bottom {
			p = &pdfPage{}
			pages = append(pages, p)
			y = height - 56
			header()
		}
		x := left
		for i, c := range t.Columns {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			// A cell is cut to its column: Helvetica averages about half the size per glyph, Courier is 0.6 of it.
			if c.Right {
				p.right(size, x+c.Width-4, y, pdfInk, clip(cell, int((c.Width-4)/(0.6*size))))
			} else {
				p.text("F1", size, x, y, pdfInk, clip(cell, int((c.Width-6)/(0.5*size))))
			}
			x += c.Width
		}
		y -= 13
	}
	for i, pg := range pages {
		pg.text("F1", 8, left, 36, pdfMuted, t.Footer)
		pg.right(8, rightX, 36, pdfMuted, fmt.Sprintf("%d/%d", i+1, len(pages)))
	}
	return pdfDocument(width, height, pages)
}

// clip cuts s to n characters, the last of them an ellipsis when it is cut.
func clip(s string, n int) string {
	r := []rune(s)
	if n < 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
