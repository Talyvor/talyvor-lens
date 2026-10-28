// Package redact cuts credentials out of text before it is logged or written to an audit trail (B50.2, B50.3).
package redact

import "regexp"

// Mark replaces every credential Secrets finds.
const Mark = "[REDACTED]"

var secretValue = regexp.MustCompile(`tlv_[A-Za-z0-9_]{8,}` + // Talyvor workspace and agent keys
	`|\b(?:sk|rk|pk|whsec)_(?:live|test)_[A-Za-z0-9]{8,}` + // Stripe keys and webhook secrets
	`|\bsk-[A-Za-z0-9_-]{16,}` + // provider keys
	`|\bAIza[0-9A-Za-z_-]{30,}` + // Google API keys
	`|(?i:bearer)\s+[A-Za-z0-9._~+/=-]{8,}` +
	`|\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*` + // JWTs
	`|(?i:password|passwd|secret)["']?\s*[=:]\s*["']?[^\s"',&]+`)

// Secrets returns s with every API key, bearer token, JWT and password assignment replaced by Mark.
func Secrets(s string) string { return secretValue.ReplaceAllString(s, Mark) }
