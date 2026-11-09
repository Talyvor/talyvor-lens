package api

// refusal.go — B55.51: a 4xx message as a person reads it.

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// refusalKinds are the kinds a package names after itself ("rooms: conflict: …"); the web and the
// phone used to strip them one screen at a time.
var refusalKinds = []string{"invalid request: ", "not allowed: ", "conflict: ", "not found: ", "invalid listing: ", "invalid tax details: "}

// Refusal drops the leading package names ("economy: ", "rooms: ", then "spending: " and the like)
// and the kind after them, and starts a sentence with a capital.
func Refusal(msg string) string {
	s := msg
	for {
		word, rest, ok := strings.Cut(s, ": ")
		if !ok || rest == "" || !packageWord(word) {
			break
		}
		s = rest
	}
	for _, kind := range refusalKinds {
		if rest, ok := strings.CutPrefix(s, kind); ok && rest != "" {
			s = rest
			break
		}
	}
	if word, _, _ := strings.Cut(s, " "); !plainWord(word) {
		return s
	}
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

// plainWord reports whether w is a word of a sentence, so a field name ("workspace_id") or a
// package name ("economy:") keeps its case.
func plainWord(w string) bool {
	for _, c := range w {
		if !unicode.IsLower(c) && c != '\'' {
			return false
		}
	}
	return w != ""
}

// packageWord reports whether w reads as a Go package name: one lower-case word of letters and digits.
func packageWord(w string) bool {
	if w == "" || w[0] < 'a' || w[0] > 'z' {
		return false
	}
	for _, c := range w {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
