package market

import (
	"regexp"
	"sort"
	"strings"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
)

// scan.go — what publishing checks (B20.1). The rule:
//
//   - a SECRET anywhere in a listing refuses it: an API key, a token or a private key published is a
//     credential given away, whoever wrote it;
//   - PERSONAL DATA refuses it too (internal/pii: emails, phone numbers, card numbers, national
//     insurance/SSN-shaped numbers, IP addresses, dates of birth, labelled names) — a listing is read by
//     strangers;
//   - PROMPT INJECTION at the blocking level (internal/injection's default policy) refuses every kind
//     but an evaluation, whose test cases are allowed to be attacks; below that level, or in an
//     evaluation, it is recorded on the version and not refused.
//
// Every version keeps what its scan found.

// secretPatterns are credential shapes that are never ordinary text.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private_key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY(?: BLOCK)?-----`)},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"openai_key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`)},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"stripe_key", regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`)},
	{"github_token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,})`)},
	{"slack_token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"talyvor_key", regexp.MustCompile(`\btlv_[A-Za-z0-9_-]{20,}`)},
}

// Scan is what a version's publish scan found.
type Scan struct {
	Secrets       []string `json:"secrets,omitempty"`       // kinds of credential found
	PersonalData  []string `json:"personal_data,omitempty"` // kinds of personal data found
	InjectionRisk float64  `json:"injection_risk"`
	Injection     []string `json:"injection,omitempty"` // the patterns that matched
	Refused       string   `json:"refused,omitempty"`   // why it may not be published; empty when it may
	Held          string   `json:"held,omitempty"`      // why a person must review it before anyone else may use it (B20.4)
	Similar       *Similar `json:"similar,omitempty"`   // B32.46: the listing it copies without declaring it as a parent (similarity.go)

	Gym *economy.NegotiationGymResult `json:"gym,omitempty"` // B52.16: a Counter playbook's gym result (playbooks.go)
}

var (
	piiDetector       = pii.New()
	injectionDetector = injection.New(injection.DefaultPolicy())
)

// ScanText is the publish scan of texts for a listing of kind; rooms scan a public room's messages with it (B32.30).
func ScanText(kind string, texts []string) Scan {
	var s Scan
	secrets, personal, injected := map[string]bool{}, map[string]bool{}, map[string]bool{}
	blocked, warned := false, false
	for _, t := range texts {
		for _, p := range secretPatterns {
			if p.re.MatchString(t) {
				secrets[p.name] = true
			}
		}
		for _, typ := range piiDetector.Detect(t).Types {
			personal[typ] = true
		}
		r := injectionDetector.Detect(t)
		s.InjectionRisk = max(s.InjectionRisk, r.RiskScore)
		for _, p := range r.Patterns {
			injected[p] = true
		}
		blocked = blocked || r.Action == injection.ActionBlock
		warned = warned || r.Action == injection.ActionWarn
	}
	s.Secrets, s.PersonalData, s.Injection = sortedKeys(secrets), sortedKeys(personal), sortedKeys(injected)
	switch {
	case len(s.Secrets) > 0:
		s.Refused = "it contains a secret (" + s.Secrets[0] + ") — remove it and publish again"
	case len(s.PersonalData) > 0:
		s.Refused = "it contains personal data (" + s.PersonalData[0] + ") — remove it and publish again"
	case blocked && kind != "evaluation":
		s.Refused = "it reads as a prompt injection — only an evaluation may carry attacks"
	case warned && kind != "evaluation":
		s.Held = "it may be a prompt injection (" + strings.Join(s.Injection, ", ") + ")"
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// strings collects every string in a decoded JSON value.
func stringsIn(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = stringsIn(e, out)
		}
	case map[string]any:
		for k, e := range x {
			out = append(out, k)
			out = stringsIn(e, out)
		}
	}
	return out
}
