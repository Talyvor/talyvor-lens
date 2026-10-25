package jcs

import (
	"math"
	"testing"
)

// RFC 8785 §3.2.2–3.2.3: the worked example, and §3.2.3's sorting example, byte for byte.
func TestCanonicalizeMatchesRFC8785Examples(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"§3.2.2", `{
  "numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
  "literals": [null, true, false]
}`, `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`},
		{"§3.2.3", `{
  "\u20ac": "Euro Sign",
  "\r": "Carriage Return",
  "\ufb33": "Hebrew Letter Dalet With Dagesh",
  "1": "One",
  "\ud83d\ude00": "Emoji: Grinning Face",
  "\u0080": "Control",
  "\u00f6": "Latin Small Letter O With Diaeresis"
}`, "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"ö\":\"Latin Small Letter O With Diaeresis\"," +
			"\"€\":\"Euro Sign\",\"😀\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}"},
	} {
		got, err := Canonicalize([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// RFC 8785 Appendix B: the IEEE 754 test values and the text each must become.
func TestNumberMatchesRFC8785AppendixB(t *testing.T) {
	for _, c := range []struct {
		bits uint64
		want string
	}{
		{0x0000000000000000, "0"}, {0x8000000000000000, "0"},
		{0x0000000000000001, "5e-324"}, {0x8000000000000001, "-5e-324"},
		{0x7fefffffffffffff, "1.7976931348623157e+308"}, {0xffefffffffffffff, "-1.7976931348623157e+308"},
		{0x4340000000000000, "9007199254740992"}, {0xc340000000000000, "-9007199254740992"},
		{0x4430000000000000, "295147905179352830000"},
		{0x44b52d02c7e14af5, "9.999999999999997e+22"}, {0x44b52d02c7e14af6, "1e+23"}, {0x44b52d02c7e14af7, "1.0000000000000001e+23"},
		{0x444b1ae4d6e2ef4e, "999999999999999700000"}, {0x444b1ae4d6e2ef4f, "999999999999999900000"}, {0x444b1ae4d6e2ef50, "1e+21"},
		{0x3eb0c6f7a0b5ed8c, "9.999999999999997e-7"}, {0x3eb0c6f7a0b5ed8d, "0.000001"},
		{0x41b3de4355555553, "333333333.3333332"}, {0x41b3de4355555554, "333333333.33333325"},
		{0x41b3de4355555555, "333333333.3333333"}, {0x41b3de4355555556, "333333333.3333334"},
		{0x41b3de4355555557, "333333333.33333343"},
		{0xbecbf647612f3696, "-0.0000033333333333333333"}, {0x43143ff3c1cb0959, "1424953923781206.2"},
	} {
		got, err := Number(math.Float64frombits(c.bits))
		if err != nil || got != c.want {
			t.Errorf("%016x = %q, %v; want %q", c.bits, got, err, c.want)
		}
	}
	for _, bits := range []uint64{0x7fffffffffffffff, 0x7ff0000000000000} { // NaN, Infinity
		if _, err := Number(math.Float64frombits(bits)); err == nil {
			t.Errorf("%016x has no JSON form and was written", bits)
		}
	}
}
