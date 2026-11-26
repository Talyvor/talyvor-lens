package main

import (
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/talyvor/lens/internal/market"
)

// statement_xlsx.go — B61.132: the money, agent and wallet statements as an Excel workbook (DESIGN.md 16.4), made
// from the same statement document as the PDF.

// statementFileName is a statement file's name without its extension (DESIGN.md 16.1): talyvor-statement-USD-2026-10
// for a whole month, talyvor-statement-USD-2026-08-03-to-2026-10-31 otherwise, with the agent's name before the
// currency on an agent's statement. A month so far is named for its month, as its title is.
func statementFileName(agent, currency string, from, to time.Time) string {
	from, to = from.UTC(), to.UTC()
	last := to.Add(-time.Nanosecond)
	if last.Before(from) {
		last = from
	}
	parts := []string{"talyvor-statement"}
	var slug strings.Builder
	for _, r := range strings.ToLower(agent) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			slug.WriteRune(r)
		case slug.Len() > 0 && !strings.HasSuffix(slug.String(), "-"):
			slug.WriteByte('-')
		}
	}
	if s := strings.Trim(slug.String(), "-"); s != "" {
		parts = append(parts, s)
	}
	if currency != "" {
		parts = append(parts, currency)
	}
	if from.Equal(time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)) && !to.After(from.AddDate(0, 1, 0)) {
		parts = append(parts, from.Format("2006-01"))
	} else {
		parts = append(parts, from.Format(time.DateOnly)+"-to-"+last.Format(time.DateOnly))
	}
	return strings.Join(parts, "-")
}

// writeStatementXLSX answers with doc as an Excel workbook named name.xlsx.
func writeStatementXLSX(w http.ResponseWriter, name string, doc market.BrandedStatement) {
	writeStatementFile(w, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", name+".xlsx", doc.XLSX())
}

// writeStatementFile answers with a statement document Lens rendered, a PDF or an Excel workbook, to download as filename.
func writeStatementFile(w http.ResponseWriter, contentType, filename string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(body) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- application/pdf or an Excel workbook rendered by Lens
}
