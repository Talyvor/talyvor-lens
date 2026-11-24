package main

import (
	"encoding/csv"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// B28.332 — every wallet list (an agent's transfers, the approvals, the schedules, the earnings payouts) is also a CSV:
// the same route with ?format=csv. One row per item of the JSON list, in its order, under a header of the item's JSON
// field names; a nested object's fields are columns of their own, prefixed with its name (payee_kind, paid_from_name),
// and empty when it is null. Amounts stay integers in the units their column names (amount_ulxc, amount_minor), times
// are RFC 3339 in UTC, and a text cell a spreadsheet would read as a formula is written with a leading apostrophe.

// wantsCSV reports whether the caller asked for the list as a CSV.
func wantsCSV(req *http.Request) bool {
	return strings.EqualFold(req.URL.Query().Get("format"), "csv")
}

// writeListCSV writes rows, a slice of structs, as name.csv.
func writeListCSV(w http.ResponseWriter, name string, rows any) {
	v := reflect.ValueOf(rows)
	cols := csvColumns(v.Type().Elem(), "")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.name
	}
	_ = cw.Write(header)
	for i := 0; i < v.Len(); i++ {
		rec := make([]string, len(cols))
		for j, c := range cols {
			rec[j] = csvListCell(v.Index(i), c.path)
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

type csvColumn struct {
	name string
	path []int
}

// csvColumns lists t's JSON fields in declaration order, a nested struct's flattened under its own name.
func csvColumns(t reflect.Type, prefix string) []csvColumn {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var cols []csvColumn
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || tag == "-" {
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}) {
			for _, sub := range csvColumns(ft, prefix+tag+"_") {
				cols = append(cols, csvColumn{name: sub.name, path: append([]int{i}, sub.path...)})
			}
			continue
		}
		cols = append(cols, csvColumn{name: prefix + tag, path: []int{i}})
	}
	return cols
}

// csvListCell is the value at path in v, "" where a pointer on the way is nil.
func csvListCell(v reflect.Value, path []int) string {
	for _, i := range path {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return ""
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	switch x := v.Interface().(type) {
	case time.Time:
		if x.IsZero() {
			return ""
		}
		return x.UTC().Format(time.RFC3339)
	case string:
		return csvCell(x)
	case bool:
		return strconv.FormatBool(x)
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.String:
		return csvCell(v.String())
	}
	return ""
}
