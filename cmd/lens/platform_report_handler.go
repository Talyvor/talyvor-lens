package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/talyvor/lens/internal/platformreport"
	"github.com/talyvor/lens/internal/sellertax"
)

// B32.44 — the annual platform-reporting export, for the operator.

func writePlatformReportErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, platformreport.ErrInvalid):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, sellertax.ErrNoCustody):
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}

// newPlatformReportExportHandler — POST /v1/admin/platform-reports {year, funding, format, actor}: the year's export as
// the file itself (format csv, the default, or json; funding live, the default, or test), recorded in platform_reports
// and the operator audit trail. The file holds the sellers' TINs in clear, so only the global admin key reaches it.
func newPlatformReportExportHandler(g *platformreport.Generator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Year    int    `json:"year"`
			Funding string `json:"funding"`
			Format  string `json:"format"`
			Actor   string `json:"actor"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "body must be {year, funding, format, actor}: "+err.Error())
			return
		}
		actor, ok := contractActor(req, in.Actor)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "actor in the body is not the operator "+moderatorOperatorHeader+" names")
			return
		}
		if in.Funding == "" {
			in.Funding = platformreport.FundingLive
		}
		if in.Format == "" {
			in.Format = platformreport.FormatCSV
		}
		file, run, err := g.Export(req.Context(), in.Year, in.Funding, in.Format, actor)
		if err != nil {
			writePlatformReportErr(w, err)
			return
		}
		name := fmt.Sprintf("platform-report-%d", run.Year)
		if run.Funding == platformreport.FundingTest {
			name += "-test"
		}
		contentType := "text/csv; charset=utf-8"
		if run.Format == platformreport.FormatJSON {
			contentType = "application/json"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+"."+run.Format+`"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Platform-Report-Id", run.ID)
		w.Header().Set("X-Platform-Report-Sha256", run.SHA256)
		w.Header().Set("X-Platform-Report-Rows", strconv.Itoa(run.Rows))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(file) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- a CSV/JSON attachment with its own content type; nosniff is set on every response
	})
}

// newPlatformReportRunsHandler — GET /v1/admin/platform-reports[?year=]: the files written, newest first, each with
// its operator, record count and sha256 — never its contents.
func newPlatformReportRunsHandler(g *platformreport.Generator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		year := 0
		if v := req.URL.Query().Get("year"); v != "" {
			y, err := strconv.Atoi(v)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "year is a year such as 2026")
				return
			}
			year = y
		}
		runs, err := g.Runs(req.Context(), year)
		if err != nil {
			writePlatformReportErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"runs": runs})
	})
}
