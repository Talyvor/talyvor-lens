package main

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/talyvor/lens/internal/redact"
)

// B50.2 — what may never reach the logs. redactLogAttr is the logger's ReplaceAttr: an attribute named like a
// credential loses its value, and every string value and message (log.Printf arrives as one) has keys, bearer
// tokens, IBANs and card numbers cut out. Numbers logged as numbers are left alone.

const redacted = redact.Mark

var (
	secretAttrName = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|secret|token|authorization|api_?key|cookie|private_?key)$`)
	ibanValue      = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]){11,30}\b`)
	cardValue      = regexp.MustCompile(`\b[2-6](?:[ -]?[0-9]){12,18}\b`)
)

func redactLogAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	if a.Key != slog.MessageKey && secretAttrName.MatchString(a.Key) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redactLogText(a.Value.String()))
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case error, fmt.Stringer:
			return slog.String(a.Key, redactLogText(fmt.Sprint(v)))
		}
	}
	return a
}

func redactLogText(s string) string {
	s = redact.Secrets(s)
	s = ibanValue.ReplaceAllString(s, redacted)
	return cardValue.ReplaceAllStringFunc(s, func(m string) string {
		if luhn(m) {
			return redacted
		}
		return m
	})
}

// luhn reports whether the digits in s pass the card-number checksum, so a long id or amount is left alone.
func luhn(s string) bool {
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
	sum := 0
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if (len(d)-1-i)%2 == 1 {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}
