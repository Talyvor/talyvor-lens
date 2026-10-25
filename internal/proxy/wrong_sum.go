package proxy

import (
	"regexp"
	"strconv"
	"strings"
)

// sumRE is one integer sum, difference or product: "5312 + 8686", "12*7", "40 − 3".
var sumRE = regexp.MustCompile(`(\d{1,9})\s*([-+*×−])\s*(\d{1,9})`)

// wrongSum reports an answer that is a lone number and is not the value of the one sum its question
// asks. B17.238: the pool served "14998" for "What is 5312 + 8686?" to every workspace, every night —
// one wrong answer, stored once, became every asker's. A stored answer Lens can check itself is
// checked, at the write (never stored) and at the read (an older row is not served; the request goes
// upstream and its fresh answer replaces it). A question with any other number in it, or an answer
// in words, is not judged.
func wrongSum(question string, body []byte) bool {
	m := sumRE.FindAllStringSubmatchIndex(question, -1)
	if len(m) != 1 || strings.ContainsAny(question[:m[0][0]]+question[m[0][1]:], "0123456789") {
		return false
	}
	answer := strings.TrimRight(strings.TrimSpace(extractAnthropicText(body)+extractResponseContent(body)), ".")
	got, err := strconv.ParseInt(strings.ReplaceAll(answer, ",", ""), 10, 64)
	if err != nil {
		return false
	}
	a, _ := strconv.ParseInt(question[m[0][2]:m[0][3]], 10, 64)
	b, _ := strconv.ParseInt(question[m[0][6]:m[0][7]], 10, 64)
	want := a + b
	switch question[m[0][4]:m[0][5]] {
	case "-", "−":
		want = a - b
	case "*", "×":
		want = a * b
	}
	return got != want
}
