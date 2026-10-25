// Package jcs is the JSON Canonicalization Scheme, RFC 8785: the one byte form of a JSON value that a signature over
// it covers (B45.23 signs A2A Agent Cards over it). Object members are sorted by their names' UTF-16 code units, and
// strings and numbers are written as ECMAScript's JSON.stringify writes them, with no whitespace.
package jcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Canonicalize is the RFC 8785 form of the JSON value in raw.
func Canonicalize(raw []byte) ([]byte, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return Value(v)
}

// Parse reads the one I-JSON value (RFC 7493) in raw, as RFC 8785 requires: valid UTF-8, no name twice in one object,
// nothing after it. Objects are map[string]any, numbers json.Number. A signature checked over a parse that kept one of
// two duplicate names would vouch for a card another parser reads differently.
func Parse(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("jcs: not valid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := parse(d)
	if err != nil {
		return nil, fmt.Errorf("jcs: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("jcs: more than one JSON value")
	}
	return v, nil
}

func parse(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			name := k.(string)
			if _, dup := m[name]; dup {
				return nil, fmt.Errorf("the name %q appears twice in one object", name)
			}
			if m[name], err = parse(d); err != nil {
				return nil, err
			}
		}
		_, err := d.Token() // '}'
		return m, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			e, err := parse(d)
			if err != nil {
				return nil, err
			}
			a = append(a, e)
		}
		_, err := d.Token() // ']'
		return a, err
	}
	return t, nil
}

// Marshal is the RFC 8785 form of v as encoding/json marshals it.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// Value writes a value decoded with UseNumber — map[string]any, []any, string, json.Number, bool or nil.
func Value(v any) ([]byte, error) {
	var b bytes.Buffer
	err := write(&b, v)
	return b.Bytes(), err
}

func write(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeString(b, x)
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return fmt.Errorf("jcs: %s is not an IEEE 754 double", x)
		}
		s, err := Number(f)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		names := make([]string, 0, len(x))
		for k := range x {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool { return lessUTF16(names[i], names[j]) })
		b.WriteByte('{')
		for i, k := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			if err := write(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("jcs: cannot write a %T", v)
	}
	return nil
}

func lessUTF16(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// writeString escapes only what JSON requires: '"', '\\' and the controls below U+0020, the five with a short form
// using it and the rest as lower-case \u00xx. Everything else is written as it is.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// Number is f as ECMAScript's Number.prototype.toString writes it (ECMA-262 Number::toString): the shortest digits
// that read back as f, fixed notation when the decimal exponent is in [-6, 21), exponent notation otherwise.
func Number(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errors.New("jcs: NaN and Infinity have no JSON form")
	}
	if f == 0 {
		return "0", nil // -0 too
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// 'e' with precision -1 is the shortest round-trip form: d.ddddde±xx.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	k := len(digits)
	x, _ := strconv.Atoi(exp)
	n := x + 1 // the position of the decimal point after the first digit, as ECMA-262 counts it
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k), nil
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:], nil
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits, nil
	}
	es := "+"
	if n-1 < 0 {
		es = "-"
	}
	m := digits[:1]
	if k > 1 {
		m += "." + digits[1:]
	}
	return sign + m + "e" + es + strconv.Itoa(abs(n-1)), nil
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
