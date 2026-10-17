package safehttp

import "testing"

func TestCheckURL(t *testing.T) {
	cases := map[string]bool{ // url → allowed
		"https://node.example.com":          true,
		"http://10.0.0.5:11434/v1":          true, // private: left to the allowlist-aware dial guard
		"http://[2001:db8::1]:8080":         true,
		"http://169.254.169.254/latest":     false, // cloud metadata
		"http://[fe80::1]/":                 false, // link-local v6
		"http://0.0.0.0:8080":               false, // unspecified
		"http://224.0.0.1":                  false, // multicast
		"file:///etc/passwd":                false,
		"gopher://node.example.com":         false,
		"http://user:pw@node.example.com":   false, // userinfo can hide the real host
		"http://node.example.com/a b":       false,
		"http://node.example.com/\r\nHost:": false,
		"//node.example.com":                false,
		"":                                  false,
	}
	for raw, want := range cases {
		if got := CheckURL(raw) == nil; got != want {
			t.Errorf("CheckURL(%q) allowed=%v, want %v", raw, got, want)
		}
	}
}
